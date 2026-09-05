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

type resultPublisher struct {
	Publisher
	result release.Result
}

func (p resultPublisher) Apply(context.Context, release.ApplyRequest) release.Result { return p.result }
func (p resultPublisher) Stage(context.Context, release.ApplyRequest) release.Result { return p.result }
func (p resultPublisher) NginxTest(context.Context, release.NginxCommandRequest) release.Result {
	return p.result
}
func (p resultPublisher) NginxReload(context.Context, release.NginxCommandRequest) release.Result {
	return p.result
}

func TestNginxErrorsReachHTTPClient(t *testing.T) {
	diagnostic := "nginx -t: nginx failed: exit status 1: nginx: [emerg] unknown directive in site.conf:7"
	for _, code := range []int{500, 503} {
		status, errorCode, rollback := release.NodeStatusFailed, "NGINX_TEST_FAILED", "succeeded"
		if code == 503 {
			status, errorCode, rollback = release.NodeStatusRecoveryRequired, "RECOVERY_FAILED", "failed"
		}
		result := release.Result{ReleaseID: release.ID(), HTTPStatus: code, Status: status, ErrorCode: errorCode, Error: diagnostic, RollbackStatus: rollback, Steps: []release.Step{{Name: "nginx_test", Status: release.NodeStatusFailed, Message: diagnostic}}}
		cfg := config.Config{Env: "test", ReleaseAuthTokens: map[string]string{"test": "secret"}, MaxConcurrentRequests: 1, MaxRequestBytes: 2048}
		h := New(resultPublisher{result: result}, cfg).Handler()
		req := httptest.NewRequest("POST", "/api/v1/releases/apply", strings.NewReader(`{"env":"test"}`))
		req.Header.Set("X-Release-Token", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var got release.Result
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != code || got.Status != status || got.ErrorCode != errorCode || got.Error != diagnostic || len(got.Steps) != 1 || got.Steps[0].Message != diagnostic {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
}

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{ListenAddr: "127.0.0.1:0", NodeID: "node", Env: "test", DataDir: filepath.Join(root, "state"), LockFile: filepath.Join(root, "lock"), ReleaseAuthTokens: map[string]string{"test": "secret"}, MaxRequestBytes: 2048, Repos: map[string]config.Repo{"config": {URL: "file://" + filepath.Join(root, "repo"), AllowLocal: true, AllowedBranches: []string{"main"}}}, Targets: []config.Target{{Type: release.ReleaseTypeConfig, ServerName: "site", PathDest: filepath.Join(root, "deploy"), HealthChecks: []config.HealthCheck{{URL: "http://127.0.0.1", Contains: "{commit}"}}}}}
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
