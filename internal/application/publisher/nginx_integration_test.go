package publisher

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"nginx_updata_config/internal/infrastructure/nginx"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/infrastructure/process"
)

// Opt-in integration test. The binary must already be installed by the operator.
// The test creates a dedicated master, config, PID file, listen port and deployment tree.
func TestRealNginxActivationAndRecovery(t *testing.T) {
	binary := os.Getenv("NGINX_TEST_BINARY")
	if binary == "" {
		t.Skip("set NGINX_TEST_BINARY to run against a dedicated real Nginx instance")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("NGINX_TEST_BINARY must be absolute")
	}
	f := newFixture(t)
	f.r.Close()
	f.r = nil
	prefix := t.TempDir()
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	conf := filepath.Join(prefix, "nginx.conf")
	pid := filepath.Join(prefix, "nginx.pid")
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	user := ""
	if os.Geteuid() == 0 {
		user = "user root;\n"
	}
	content := fmt.Sprintf(`%sworker_processes 1;
error_log %q notice;
pid %q;
events { worker_connections 64; }
http {
 access_log off;
 server {
  listen 127.0.0.1:%d;
  location = /.release-version { alias %q; open_file_cache off; }
  include %q;
 }
}
`, user, filepath.Join(prefix, "error.log"), pid, port, filepath.Join(f.cfg.Targets[0].Dir, "latest", ".release-version"), filepath.Join(f.cfg.Targets[0].Dir, "latest", "*.conf"))
	if e = os.WriteFile(conf, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	proc := process.Command(context.Background(), binary, "-c", conf, "-p", prefix, "-g", "daemon off;")
	output := &process.LimitedBuffer{Limit: 65536}
	proc.Stdout = output
	proc.Stderr = output
	if e = proc.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-proc.Process.Pid, syscall.SIGQUIT)
		done := make(chan error, 1)
		go func() { done <- proc.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = proc.Cancel()
			<-done
		}
	})
	f.cfg.Nginx = config.Nginx{Binary: binary, ConfigFile: conf, Prefix: prefix, PIDFile: pid}
	f.cfg.Targets[0].HealthChecks = []config.HealthCheck{{URL: url + "/.release-version", Contains: "{commit}", Status: 200}}
	f.cfg.Targets[0].InitialHealthChecks = []config.HealthCheck{{URL: url + "/.release-version", Status: 404}}
	realRuntime := &nginx.Runtime{Config: f.cfg}
	wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = realRuntime.Verify(wait, f.cfg.Targets[0], "", true); e != nil {
		t.Fatal("Nginx did not start", e)
	}
	r, e := NewWithRuntime(f.cfg, realRuntime)
	if e != nil {
		t.Fatal(e)
	}
	f.r = r
	a := f.commit(`location = /effect { return 200 "enabled A"; }`)
	f.apply(a)
	readEffect := func() string {
		t.Helper()
		resp, e := http.Get(url + "/effect")
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		body, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		return string(body)
	}
	if readEffect() != "enabled A" {
		t.Fatal("new workers did not load A")
	}
	b := f.commit(`location = /effect { return 200 "enabled B"; }`)
	f.apply(b)
	if readEffect() != "enabled B" {
		t.Fatal("reload did not load B")
	}
	invalid := f.commit("invalid_nginx_directive;")
	res := r.Apply(context.Background(), f.request(invalid))
	if res.Status != release.NodeStatusFailed || res.RollbackStatus != "succeeded" || readEffect() != "enabled B" {
		t.Fatalf("real Nginx recovery failed: %+v", res)
	}
	// A reload from a syntactically valid release also must be confirmed by live HTTP.
	wrong := f.commit(`location = /effect { return 200 "enabled C"; }`)
	original := f.cfg.Targets[0].HealthChecks
	f.r.cfg.Targets[0].HealthChecks = append(append([]config.HealthCheck(nil), original...), config.HealthCheck{URL: url + "/effect", Contains: "enabled B", Status: 200})
	f.r.cfg.StepTimeout = config.Duration(300 * time.Millisecond)
	res = f.r.Apply(context.Background(), f.request(wrong))
	if res.Status != release.NodeStatusFailed || !strings.Contains(res.Error, "verification") || readEffect() != "enabled B" {
		t.Fatalf("real HTTP mismatch was not restored: %+v", res)
	}
}
