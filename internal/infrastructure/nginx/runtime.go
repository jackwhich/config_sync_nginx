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
	"strings"
	"time"
)

// Runtime only invokes the nginx command already provided by the host's PATH.
// Optional HTTP checks do not inspect or manage Nginx processes.
type Runtime struct{}

func (n *Runtime) Test(ctx context.Context) error {
	_, err := n.TestOutput(ctx)
	return err
}

func (n *Runtime) TestOutput(ctx context.Context) (string, error) {
	out, err := process.Run(ctx, "", nil, nil, "nginx", "-t")
	if err != nil {
		return out, fmt.Errorf("nginx -t: %w", err)
	}
	return out, nil
}
func (n *Runtime) Reload(ctx context.Context) error {
	_, err := n.ReloadOutput(ctx)
	return err
}

func (n *Runtime) ReloadOutput(ctx context.Context) (string, error) {
	out, err := process.Run(ctx, "", nil, nil, "nginx", "-s", "reload")
	if err != nil {
		return out, fmt.Errorf("nginx -s reload: %w", err)
	}
	return out, nil
}
func (n *Runtime) Verify(ctx context.Context, t config.Target, commit string, initial bool) error {
	checks := t.HealthChecks
	if initial {
		checks = t.InitialHealthChecks
	}
	if len(checks) == 0 {
		return ctx.Err()
	}
	for {
		var last error
		for _, h := range checks {
			if err := Probe(ctx, h, commit, ""); err != nil {
				last = err
				break
			}
		}
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("HTTP verification failed: %v: %w", last, ctx.Err())
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
