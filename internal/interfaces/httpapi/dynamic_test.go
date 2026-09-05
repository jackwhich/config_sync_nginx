package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nginx_updata_config/internal/application/publisher"
	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
)

func TestMinimalHTTPPublishAndEnvironmentTokenBinding(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "site"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "site", "site.conf"), []byte("# fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	run("add", ".")
	run("commit", "-m", "fixture")
	commit := run("rev-parse", "HEAD")
	cfg := config.Config{DataDir: filepath.Join(root, "data"), ReleaseAuthTokens: map[string]string{"uat": "uat-token", "prod": "prod-token"}, Repos: map[string]config.Repo{"config": {URL: "file://" + repo, AllowLocal: true}}, Targets: []config.Target{{Type: release.ReleaseTypeConfig}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r, err := publisher.NewWithRuntime(cfg, noopRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h := New(r, cfg).Handler()
	body := map[string]any{"env": "uat", "type": "config", "commitid": commit, "project": "ybf", "params": map[string]string{"server_name": "site", "path_dest": filepath.Join(root, "deploy")}}
	post := func(token string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/v1/releases/apply", bytes.NewReader(b))
		req.Header.Set("X-Release-Token", token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("prod-token"); rec.Code != 401 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec := post("uat-token")
	if rec.Code != 202 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var result release.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != release.NodeStatusRunning || result.Phase != "awaiting_nginx_test" || result.Env != "uat" || !release.IsID(result.ReleaseID) {
		t.Fatal(result)
	}
	command := func(path, token string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(release.NginxCommandRequest{Env: "uat", ReleaseID: result.ReleaseID})
		req := httptest.NewRequest("POST", path, bytes.NewReader(b))
		req.Header.Set("X-Release-Token", token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := command("/api/v1/releases/nginx/test", "uat-token"); rec.Code != 202 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = command("/api/v1/releases/nginx/reload", "uat-token")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != release.NodeStatusSucceeded {
		t.Fatal(result)
	}
	for _, env := range []string{"uat", "prod"} {
		req := httptest.NewRequest("GET", "/api/v1/releases/state?env="+env+"&target_id="+result.TargetID, nil)
		req.Header.Set("X-Release-Token", env+"-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := 200
		if env == "prod" {
			want = 403
		}
		if rec.Code != want {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	body["type"] = "whitelist"
	if rec := post("uat-token"); rec.Code != 403 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "deploy", "whitelist")); !os.IsNotExist(err) {
		t.Fatal("disabled type touched deployment directory")
	}
}
