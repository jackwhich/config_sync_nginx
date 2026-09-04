package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"nginx_updata_config/internal/application/publisher"
	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
)

type noopRuntime struct{}

func (noopRuntime) Test(context.Context) error                                { return nil }
func (noopRuntime) Reload(context.Context) error                              { return nil }
func (noopRuntime) Verify(context.Context, config.Target, string, bool) error { return nil }
func testHandler(t *testing.T) http.Handler {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{ListenAddr: "127.0.0.1:0", NodeID: "node", Env: "test", DataDir: filepath.Join(root, "state"), LockFile: filepath.Join(root, "lock"), ReleaseAuthTokens: map[string]string{"test": "secret"}, MaxRequestBytes: 2048, Repos: map[string]config.Repo{"config": {URL: "file://" + filepath.Join(root, "repo"), AllowLocal: true, AllowedBranches: []string{"main"}}}, Targets: []config.Target{{Type: release.ReleaseTypeConfig, ServerName: "site", PathDest: filepath.Join(root, "deploy"), HealthChecks: []config.HealthCheck{{URL: "http://127.0.0.1", Contains: "{commit}"}}}}, Nginx: config.Nginx{Binary: "/bin/nginx", ConfigFile: "/etc/nginx.conf", PIDFile: "/run/nginx.pid"}}
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	r, e := publisher.NewWithRuntime(cfg, noopRuntime{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(r.Close)
	return New(r, cfg).Handler()
}
func TestStrictJSONAuthAndCapabilities(t *testing.T) {
	h := testHandler(t)
	for _, tc := range []struct {
		body, token string
		status      int
	}{{"invalid", "", 401}, {`{"unknown":1}`, "secret", 400}, {`{} {}`, "secret", 400}, {strings.Repeat(" ", 3000) + "{}", "secret", 413}, {`{"version":".."}`, "secret", 400}} {
		req := httptest.NewRequest("POST", "/api/v1/releases/apply", strings.NewReader(tc.body))
		req.Header.Set("X-Release-Token", tc.token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s: %d %s", tc.body[:min(len(tc.body), 30)], rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	var health map[string]any
	if e := json.Unmarshal(rec.Body.Bytes(), &health); e != nil {
		t.Fatal(e)
	}
	if health["release_contract"] != float64(2) || health["node_id"] != "node" {
		t.Fatal(health)
	}
}
func TestProxyChainIgnoresSpoofedLeftmostIP(t *testing.T) {
	access, e := config.ParseIPAccessControl([]string{"192.0.2.1"}, []string{"10.0.0.0/8"})
	if e != nil {
		t.Fatal(e)
	}
	cases := []struct{ remote, xff, want string }{{"10.0.0.1:123", "192.0.2.1, 203.0.113.20", "203.0.113.20"}, {"10.0.0.1:123", "192.0.2.1, 10.0.0.2", "192.0.2.1"}, {"203.0.113.20:123", "192.0.2.1", "203.0.113.20"}, {"10.0.0.1:123", "192.0.2.1, malformed", ""}}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", tc.xff)
		got := effectiveClientIP(r, access)
		if tc.want == "" {
			if got != nil {
				t.Fatal(got)
			}
		} else if got.String() != tc.want {
			t.Errorf("%v: %s", tc, got)
		}
	}
}
func TestUnauthorizedInputsDoNotBecomeMetricLabels(t *testing.T) {
	h := testHandler(t)
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("POST", "/api/v1/releases/apply", strings.NewReader(`{"env":"unbounded-attacker-label","project":"evil"}`))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Result().Body)
	if strings.Contains(string(b), "unbounded-attacker-label") {
		t.Fatal("unauthorized metric label retained")
	}
}
