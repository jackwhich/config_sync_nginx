package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/service/config"
	"nginx_updata_config/internal/service/state"
)

func frontendFixture(t *testing.T) (*fixture, *atomic.Bool) {
	f := newFixture(t)
	f.r.Close()
	f.r = nil
	f.cfg.Targets[0].Type = release.ReleaseTypeFrontendStatic
	f.cfg.Repos["frontend_static"] = f.cfg.Repos["config"]
	f.cfg.AssetRetention = config.Duration(7 * 24 * time.Hour)
	targetDir := filepath.Join(f.cfg.Targets[0].PathDest, "frontend_static", "site")
	wrongRoute := &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relative := strings.TrimPrefix(r.URL.Path, "/")
		if relative == "" {
			relative = "index.html"
		}
		root := filepath.Join(targetDir, "latest")
		if strings.HasPrefix(relative, "assets/") && !wrongRoute.Load() {
			root = targetDir
		}
		data, e := os.ReadFile(filepath.Join(root, relative))
		if e != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	f.cfg.Targets[0].PublicBaseURL = server.URL
	if e := f.cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	r, e := NewWithRuntime(f.cfg, f.rt)
	if e != nil {
		t.Fatal(e)
	}
	f.r = r
	return f, wrongRoute
}
func (f *fixture) frontendCommit(body, extra string) (string, string) {
	t := f.t
	t.Helper()
	site := filepath.Join(f.repo, "site")
	if e := os.RemoveAll(site); e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(hash[:])
	name := "assets/app." + digest[:8] + ".js"
	if e := os.MkdirAll(filepath.Join(site, "assets"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(site, name), []byte(body), 0644); e != nil {
		t.Fatal(e)
	}
	html := `<!doctype html><script src="/` + name + `"></script>` + extra
	if e := os.WriteFile(filepath.Join(site, "index.html"), []byte(html), 0644); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(map[string]any{"assets": map[string]string{name: digest}})
	if e := os.WriteFile(filepath.Join(site, "frontend-manifest.json"), b, 0644); e != nil {
		t.Fatal(e)
	}
	gitRun(t, f.repo, "add", "-A")
	gitRun(t, f.repo, "commit", "-m", "frontend")
	return gitRun(t, f.repo, "rev-parse", "HEAD"), name
}
func frontendRequest(f *fixture, id string) release.ApplyRequest {
	req := f.request(id)
	req.Type = release.ReleaseTypeFrontendStatic
	return req
}
func TestFrontendOldAssetsRemainAvailableAndProtected(t *testing.T) {
	f, _ := frontendFixture(t)
	a, oldAsset := f.frontendCommit("console.log('A')", "")
	first := f.r.Apply(context.Background(), frontendRequest(f, a))
	if first.Status != release.NodeStatusSucceeded {
		t.Fatalf("%+v", first)
	}
	b, _ := f.frontendCommit("console.log('B')", "")
	second := f.r.Apply(context.Background(), frontendRequest(f, b))
	if second.Status != release.NodeStatusSucceeded {
		t.Fatalf("%+v", second)
	}
	// Clear previous to demonstrate protection by the client's compatibility window.
	st, _ := f.current()
	st.Previous = nil
	if e := cleanupSnapshots(context.Background(), f.cfg, f.cfg.Targets[0], st); e != nil {
		t.Fatal(e)
	}
	response, e := http.Get(f.cfg.Targets[0].PublicBaseURL + "/" + oldAsset)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("old browser resource disappeared")
	}
}
func TestFrontendRejectsLatestAssetsRoute(t *testing.T) {
	f, wrong := frontendFixture(t)
	a, _ := f.frontendCommit("console.log('A')", "")
	got := f.r.Apply(context.Background(), frontendRequest(f, a))
	if got.Status != release.NodeStatusSucceeded {
		t.Fatal(got)
	}
	wrong.Store(true)
	b, _ := f.frontendCommit("console.log('B')", "")
	got = f.r.Apply(context.Background(), frontendRequest(f, b))
	if got.Status != release.NodeStatusFailed || got.RollbackStatus != "succeeded" {
		t.Fatalf("unsafe route accepted: %+v", got)
	}
	st, _ := f.current()
	if st.Current.CommitID != a {
		t.Fatal("failed frontend did not restore old index")
	}
}
func TestFrontendUndeclaredReferenceAndImmutableCollision(t *testing.T) {
	t.Run("reference", func(t *testing.T) {
		f, _ := frontendFixture(t)
		a, _ := f.frontendCommit("console.log('A')", `<img src="/missing.png">`)
		got := f.r.Apply(context.Background(), frontendRequest(f, a))
		if got.Status != release.NodeStatusFailed || !strings.Contains(got.Error, "undeclared") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("collision", func(t *testing.T) {
		f, _ := frontendFixture(t)
		a, name := f.frontendCommit("console.log('A')", "")
		target := f.cfg.Targets[0]
		if e := os.MkdirAll(filepath.Join(target.Dir, "assets"), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(target.Dir, name), []byte("collision"), 0644); e != nil {
			t.Fatal(e)
		}
		got := f.r.Apply(context.Background(), frontendRequest(f, a))
		if got.Status != release.NodeStatusFailed || !strings.Contains(got.Error, "collision") {
			t.Fatalf("%+v", got)
		}
	})
}
func TestFirstDeploymentRecoveryVerifiesAbsence(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	calls := 0
	f.rt.test = func(context.Context) error {
		calls++
		if calls == 1 {
			return os.ErrInvalid
		}
		return nil
	}
	got := f.r.Apply(context.Background(), f.request(a))
	if got.Status != release.NodeStatusFailed || got.RollbackStatus != "succeeded" {
		t.Fatalf("%+v", got)
	}
	st, link := f.current()
	if st.Current != nil || link != "" {
		t.Fatal("first release invented a baseline")
	}
}
func TestSnapshotRetentionProtectsOldPrevious(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	f.apply(a)
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, f.commit(strings.Repeat("B", i+1)))
		f.apply(ids[len(ids)-1])
	}
	st, _ := f.current()
	base, e := openTarget(f.cfg.Targets[0])
	if e != nil {
		t.Fatal(e)
	}
	defer base.Close()
	// Pin a retained old version as previous; keep_releases must not delete it.
	old := ids[1]
	m, e := loadManifest(base, old)
	if e != nil {
		t.Fatal(e)
	}
	st.Previous = &state.Version{CommitID: old, Source: m.Source, Link: "releases/" + old, ManifestDigest: manifestDigest(m)}
	f.cfg.KeepReleases = 2
	if e = cleanupSnapshots(context.Background(), f.cfg, f.cfg.Targets[0], st); e != nil {
		t.Fatal(e)
	}
	if _, e = verifySnapshot(context.Background(), base, st.Previous); e != nil {
		t.Fatal("previous deleted", e)
	}
}

func TestFrontendStartupDoesNotCommitBrokenOldAssetRoute(t *testing.T) {
	f, wrong := frontendFixture(t)
	a, _ := f.frontendCommit("console.log('A')", "")
	if got := f.r.Apply(context.Background(), frontendRequest(f, a)); got.Status != release.NodeStatusSucceeded {
		t.Fatal(got)
	}
	b, _ := f.frontendCommit("console.log('B')", "")
	wrong.Store(true)
	f.r.store.BeforeSave = func(st *state.TargetState) error {
		if id := st.ActiveID; id != "" && st.Records[id].Result.Phase == "verify_activation" {
			panic("simulated process crash after switch")
		}
		return nil
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash was not injected")
			}
		}()
		f.r.Apply(context.Background(), frontendRequest(f, b))
	}()
	f.restart()
	st, link := f.current()
	if st.Current.CommitID != a || link != "releases/"+a || st.RecoveryRequired {
		t.Fatalf("startup accepted broken asset route: %+v %s", st, link)
	}
}
