package nginx

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/state"
	"nginx_updata_config/internal/infrastructure/process"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Runtime struct {
	Config        config.Config
	beforeWorkers map[int]bool
	reloaded      bool
}

func (n *Runtime) args(extra ...string) []string {
	a := []string{}
	if n.Config.Nginx.ConfigFile != "" {
		a = append(a, "-c", n.Config.Nginx.ConfigFile)
	}
	if n.Config.Nginx.Prefix != "" {
		a = append(a, "-p", n.Config.Nginx.Prefix)
	}
	if n.Config.Nginx.GlobalDirectives != "" {
		a = append(a, "-g", n.Config.Nginx.GlobalDirectives)
	}
	if n.Config.Nginx.ErrorLog != "" {
		a = append(a, "-e", n.Config.Nginx.ErrorLog)
	}
	return append(a, extra...)
}
func (n *Runtime) Test(ctx context.Context) error {
	_, e := process.Run(ctx, "", nil, nil, n.Config.Nginx.Binary, n.args("-t")...)
	return e
}
func (n *Runtime) workers(ctx context.Context) (map[int]bool, error) {
	master := n.Config.Nginx.MasterPID
	if master == 0 {
		b, e := os.ReadFile(n.Config.Nginx.PIDFile)
		if e != nil {
			return nil, e
		}
		master, e = strconv.Atoi(strings.TrimSpace(string(b)))
		if e != nil || master <= 1 {
			return nil, fmt.Errorf("invalid nginx master pid")
		}
	}
	out, e := process.Run(ctx, "", nil, nil, "ps", "-eo", "pid=,ppid=,args=")
	if e != nil {
		return nil, e
	}
	pids := map[int]bool{}
	masterFound := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		parent, _ := strconv.Atoi(f[1])
		if pid == master && strings.Contains(line, "nginx: master process") {
			masterFound = true
		}
		if parent == master && strings.Contains(line, "nginx: worker process") && !strings.Contains(line, "shutting down") {
			pids[pid] = true
		}
	}
	if !masterFound || len(pids) == 0 {
		return nil, fmt.Errorf("nginx master or active workers missing")
	}
	return pids, nil
}
func (n *Runtime) Reload(ctx context.Context) error {
	p, e := n.workers(ctx)
	if e != nil {
		return e
	}
	n.beforeWorkers = p
	n.reloaded = true
	if n.Config.Nginx.MasterPID > 1 {
		return syscall.Kill(n.Config.Nginx.MasterPID, syscall.SIGHUP)
	}
	_, e = process.Run(ctx, "", nil, nil, n.Config.Nginx.Binary, n.args("-s", "reload")...)
	return e
}
func (n *Runtime) Verify(ctx context.Context, t config.Target, commit string, initial bool) error {
	checks := t.HealthChecks
	if initial {
		checks = t.InitialHealthChecks
		if len(checks) == 0 && !t.Dynamic {
			return fmt.Errorf("initial baseline health checks not configured")
		}
	}
	var last error
	for {
		last = nil
		{
			p, e := n.workers(ctx)
			last = e
			if e == nil && n.reloaded {
				fresh := false
				for pid := range p {
					if !n.beforeWorkers[pid] {
						fresh = true
					}
				}
				if !fresh {
					last = fmt.Errorf("new nginx workers have not started")
				}
			}
		}
		if last == nil {
			for _, h := range checks {
				if e := Probe(ctx, h, commit, ""); e != nil {
					last = e
					break
				}
			}
		}
		if last == nil {
			n.reloaded = false
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("activation verification failed: %v: %w", last, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func Probe(ctx context.Context, h config.HealthCheck, commit, digest string) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: h.TLSServerName}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, e := http.NewRequestWithContext(ctx, "GET", strings.ReplaceAll(h.URL, "{commit}", commit), nil)
	if e != nil {
		return e
	}
	request.Host = h.Host
	request.Header.Set("Cache-Control", "no-cache")
	request.Close = true
	response, e := client.Do(request)
	if e != nil {
		return e
	}
	defer response.Body.Close()
	expected := h.Status
	if expected == 0 {
		expected = 200
	}
	if response.StatusCode != expected {
		return fmt.Errorf("health status %d, expected %d", response.StatusCode, expected)
	}
	if digest != "" {
		hash := sha256.New()
		n, e := io.Copy(hash, io.LimitReader(response.Body, 1<<30))
		if e != nil {
			return e
		}
		if n >= 1<<30 || hex.EncodeToString(hash.Sum(nil)) != digest {
			return fmt.Errorf("HTTP resource digest mismatch: %s", h.URL)
		}
		return nil
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("health response too large")
	}
	if !strings.Contains(string(b), strings.ReplaceAll(h.Contains, "{commit}", commit)) {
		return fmt.Errorf("health content mismatch: %s", h.URL)
	}
	return nil
}
func VerifyFrontendHTTP(ctx context.Context, t config.Target, m *state.Manifest) error {
	base := strings.TrimRight(t.PublicBaseURL, "/")
	for name, file := range m.Files {
		if name == ".release-version" {
			continue
		}
		h := config.HealthCheck{URL: base + "/" + name, Host: t.PublicHost, Status: 200}
		if e := Probe(ctx, h, m.CommitID, file.SHA256); e != nil {
			return e
		}
	}
	return nil
}
