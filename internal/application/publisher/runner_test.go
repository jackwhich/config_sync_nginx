package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/state"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"nginx_updata_config/internal/infrastructure/lock"
)

type fakeRuntime struct {
	test   func(context.Context) error
	reload func(context.Context) error
	verify func(context.Context, string, bool) error
}

func (f *fakeRuntime) Test(c context.Context) error {
	if f.test != nil {
		return f.test(c)
	}
	return c.Err()
}
func (f *fakeRuntime) Reload(c context.Context) error {
	if f.reload != nil {
		return f.reload(c)
	}
	return c.Err()
}
func (f *fakeRuntime) Verify(c context.Context, _ config.Target, id string, initial bool) error {
	if f.verify != nil {
		return f.verify(c, id, initial)
	}
	return c.Err()
}

type fixture struct {
	cfg  config.Config
	repo string
	r    *Runner
	rt   *fakeRuntime
	t    *testing.T
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, e := fsutil.Canonical(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	repo := filepath.Join(root, "repo")
	if e = os.Mkdir(repo, 0700); e != nil {
		t.Fatal(e)
	}
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "user.name", "Test")
	gitRun(t, repo, "config", "user.email", "test@example.invalid")
	cfg := config.Config{ListenAddr: "127.0.0.1:0", Env: "test", NodeID: "test-node", DataDir: filepath.Join(root, "state"), LockFile: filepath.Join(root, "node.lock"), ReleaseAuthTokens: map[string]string{"test": "secret"}, Repos: map[string]config.Repo{"config": {URL: "file://" + repo, AllowLocal: true, AllowedBranches: []string{"main"}}}, Targets: []config.Target{{Type: release.ReleaseTypeConfig, ServerName: "site", PathDest: filepath.Join(root, "deploy"), HealthChecks: []config.HealthCheck{{URL: "http://127.0.0.1/", Contains: "{commit}"}}, InitialHealthChecks: []config.HealthCheck{{URL: "http://127.0.0.1/", Status: 404}}}}, StepTimeout: config.Duration(5 * time.Second), ExecutionTimeout: config.Duration(15 * time.Second), RecoveryTimeout: config.Duration(5 * time.Second)}
	if e = cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	rt := &fakeRuntime{}
	r, e := NewWithRuntime(cfg, rt)
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{cfg: cfg, repo: repo, r: r, rt: rt, t: t}
	t.Cleanup(func() {
		if f.r != nil {
			f.r.Close()
		}
	})
	return f
}
func (f *fixture) commit(body string) string {
	f.t.Helper()
	p := filepath.Join(f.repo, "site")
	if e := os.MkdirAll(p, 0755); e != nil {
		f.t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(p, "site.conf"), []byte(body), 0644); e != nil {
		f.t.Fatal(e)
	}
	gitRun(f.t, f.repo, "add", ".")
	gitRun(f.t, f.repo, "commit", "-m", "fixture")
	return gitRun(f.t, f.repo, "rev-parse", "HEAD")
}
func (f *fixture) request(commit string) release.ApplyRequest {
	f.t.Helper()
	st, e := f.r.store.Load(f.cfg.Targets[0].ID)
	if e != nil {
		f.t.Fatal(e)
	}
	return release.ApplyRequest{ReleaseID: release.ID(), ExpectedStateRevision: st.Revision, Env: "test", Type: release.ReleaseTypeConfig, Branch: "main", CommitID: commit, Params: map[string]string{"path_dest": f.cfg.Targets[0].PathDest, "server_name": "site"}}
}
func (f *fixture) apply(commit string) release.Result {
	f.t.Helper()
	res := f.r.Apply(context.Background(), f.request(commit))
	if res.Status != release.NodeStatusSucceeded {
		f.t.Fatalf("apply: %+v", res)
	}
	return res
}
func (f *fixture) current() (*state.TargetState, string) {
	f.t.Helper()
	st, e := f.r.store.Load(f.cfg.Targets[0].ID)
	if e != nil {
		f.t.Fatal(e)
	}
	base, e := openTarget(f.cfg.Targets[0])
	if e != nil {
		f.t.Fatal(e)
	}
	defer base.Close()
	link, e := fsutil.Link(base)
	if e != nil {
		f.t.Fatal(e)
	}
	return st, link
}
func (f *fixture) restart() {
	f.t.Helper()
	f.r.Close()
	f.r = nil
	r, e := NewWithRuntime(f.cfg, f.rt)
	if e != nil {
		f.t.Fatal(e)
	}
	f.r = r
}

func TestReleaseLifecycleIdempotencyAndABA(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	req := f.request(a)
	req.Version = ".."
	res := f.r.Apply(context.Background(), req)
	if res.Status != release.NodeStatusSucceeded {
		t.Fatalf("%+v", res)
	}
	st, link := f.current()
	if st.Current.CommitID != a || link != a || st.Revision == req.ExpectedStateRevision {
		t.Fatal("activation/state mismatch")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Targets[0].Dir, "releases")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config release unexpectedly created releases directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Targets[0].Dir, a, "site", "site.conf")); err != nil {
		t.Fatalf("Git server directory was not preserved in snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Targets[0].Dir, a, "site.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git server directory was unexpectedly flattened: %v", err)
	}
	replay := f.r.Apply(context.Background(), req)
	if !replay.Replayed || replay.StateRevisionAfter != res.StateRevisionAfter {
		t.Fatalf("replay: %+v", replay)
	}
	changed := req
	changed.Version = "other"
	if got := f.r.Apply(context.Background(), changed); got.HTTPStatus != 409 || got.ErrorCode != "RELEASE_ID_CONFLICT" {
		t.Fatalf("conflict: %+v", got)
	}
	skipReq := f.request(a)
	skipped := f.r.Apply(context.Background(), skipReq)
	if skipped.Status != release.NodeStatusSkipped || skipped.StateRevisionAfter != res.StateRevisionAfter {
		t.Fatalf("skip: %+v", skipped)
	}
	b := f.commit("B")
	f.apply(b)
	f.apply(a)
	stale := f.request(b)
	stale.ExpectedStateRevision = res.StateRevisionAfter
	if got := f.r.Apply(context.Background(), stale); got.ErrorCode != "STATE_REVISION_CONFLICT" {
		t.Fatalf("ABA accepted: %+v", got)
	}
	if _, e := os.Stat(f.cfg.DataDir); e != nil {
		t.Fatal("version metadata deleted data directory")
	}
	f.restart()
	if got := f.r.Apply(context.Background(), req); !got.Replayed {
		t.Fatalf("idempotency lost on restart: %+v", got)
	}
}

func TestLegacyBaselineLinkMigratesToDirectSnapshot(t *testing.T) {
	f := newFixture(t)
	commit := f.commit("A")
	f.apply(commit)
	target := f.cfg.Targets[0]
	if err := os.Remove(filepath.Join(target.Dir, "latest")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target.Dir, commit), filepath.Join(target.Dir, "latest")); err != nil {
		t.Fatal(err)
	}
	st, _ := f.current()
	st.Current.Link = "releases/" + commit
	if err := f.r.store.Save(st); err != nil {
		t.Fatal(err)
	}

	result := f.r.Apply(context.Background(), f.request(commit))
	if result.Status != release.NodeStatusSkipped {
		t.Fatalf("legacy snapshot was not migrated: %+v", result)
	}
	st, link := f.current()
	if st.Current.Link != commit || link != filepath.Join(target.Dir, commit) {
		t.Fatalf("unexpected migrated baseline: %+v, %q", st.Current, link)
	}
}

func TestGitSnapshotAddsServerNameForFlattenedExport(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "site.conf"), []byte("server {}"), 0644); err != nil {
		t.Fatal(err)
	}
	pathDest, err := fsutil.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := config.Target{Type: release.ReleaseTypeConfig, ServerName: "site", PathDest: pathDest, FileMode: "0644"}
	commit := strings.Repeat("a", 40)
	version, err := prepareSnapshot(context.Background(), config.Config{MaxArchiveBytes: 1 << 20, MaxArchiveFiles: 10}, target, work, commit, "file:///repo", commit)
	if err != nil {
		t.Fatal(err)
	}
	if version.Link != commit {
		t.Fatalf("snapshot link = %q", version.Link)
	}
	if _, err := os.Stat(filepath.Join(target.PathDest, "config", "site", commit, "site", "site.conf")); err != nil {
		t.Fatalf("flattened Git export was not placed below server_name: %v", err)
	}
}
func TestReloadFailureRestoresAndRetryIsNotSkipped(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	f.apply(a)
	b := f.commit("B")
	before, _ := f.current()
	calls := 0
	f.rt.reload = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("reload command failed")
		}
		return nil
	}
	failed := f.r.Apply(context.Background(), f.request(b))
	if failed.Status != release.NodeStatusFailed || failed.ActivationStatus != "restored" || failed.RollbackStatus != "succeeded" {
		t.Fatalf("%+v", failed)
	}
	st, link := f.current()
	if st.Current.CommitID != a || link != a || st.Revision == before.Revision {
		t.Fatal("baseline/revision not restored")
	}
	f.rt.reload = nil
	res := f.apply(b)
	if res.Status != release.NodeStatusSucceeded {
		t.Fatal("retry incorrectly skipped")
	}
}
func TestReloadSuccessWithoutActivationIsFailure(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	f.apply(a)
	b := f.commit("B")
	f.rt.verify = func(_ context.Context, id string, _ bool) error {
		if id == b {
			return errors.New("configured HTTP probe returned old content")
		}
		return nil
	}
	got := f.r.Apply(context.Background(), f.request(b))
	if got.Status != release.NodeStatusFailed || got.RollbackStatus != "succeeded" {
		t.Fatalf("%+v", got)
	}
	st, _ := f.current()
	if st.Current.CommitID != a {
		t.Fatal("false success persisted")
	}
}
func TestDisconnectedClientCannotCancelCriticalWorkAndNodeLock(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	req := f.request(a)
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	f.rt.reload = func(c context.Context) error { once.Do(func() { close(entered) }); <-proceed; return c.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan release.Result, 1)
	go func() { done <- f.r.Apply(ctx, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("never entered critical phase")
	}
	cancel()
	if got := f.r.Apply(context.Background(), req); got.HTTPStatus != 409 || got.ErrorCode != "RELEASE_RUNNING" {
		t.Errorf("duplicate running: %+v", got)
	}
	if got := f.r.Apply(context.Background(), f.request(a)); got.ErrorCode != "NODE_BUSY" {
		t.Errorf("concurrent release: %+v", got)
	}
	other, e := lock.Open(f.cfg.LockFile)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	if e = other.Try(); !errors.Is(e, lock.ErrBusy) {
		t.Errorf("cross-process flock not held: %v", e)
	}
	close(proceed)
	if got := <-done; got.Status != release.NodeStatusSucceeded {
		t.Fatalf("disconnect interrupted activation: %+v", got)
	}
}
func TestRecoveryFailureBlocksAndStartupRecovers(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	f.apply(a)
	b := f.commit("B")
	f.rt.reload = func(context.Context) error { return errors.New("nginx unavailable") }
	req := f.request(b)
	got := f.r.Apply(context.Background(), req)
	if got.Status != release.NodeStatusRecoveryRequired || got.HTTPStatus != 503 {
		t.Fatalf("%+v", got)
	}
	if f.r.Health()["publish_ready"] != false {
		t.Fatal("unhealthy node allowed publication")
	}
	if got = f.r.Apply(context.Background(), f.request(b)); got.HTTPStatus != 503 {
		t.Fatalf("new work accepted: %+v", got)
	}
	f.rt.reload = nil
	f.restart()
	st, link := f.current()
	if st.ActiveID != "" || st.RecoveryRequired || st.Current.CommitID != a || link != a {
		t.Fatalf("recovery: %+v %s", st, link)
	}
	if got = f.r.Apply(context.Background(), req); !got.Replayed || got.Status != release.NodeStatusFailed {
		t.Fatalf("recovery result: %+v", got)
	}
}
func TestStateCommitFailureNeverReportsSuccess(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	req := f.request(a)
	injected := false
	f.r.store.BeforeSave = func(st *state.TargetState) error {
		if !injected && st.Records[req.ReleaseID] != nil && st.Records[req.ReleaseID].Result.Status == release.NodeStatusSucceeded {
			injected = true
			return errors.New("disk write failed")
		}
		return nil
	}
	got := f.r.Apply(context.Background(), req)
	if got.Status != release.NodeStatusRecoveryRequired || got.HTTPStatus != 503 {
		t.Fatalf("%+v", got)
	}
	f.restart()
	st, link := f.current()
	if st.Current.CommitID != a || st.ActiveID != "" || link != a {
		t.Fatal("durable intent not recovered")
	}
	got = f.r.Apply(context.Background(), req)
	if !got.Replayed || got.Status != release.NodeStatusSucceeded {
		t.Fatalf("%+v", got)
	}
}
func TestLocalRestoreDoesNotFetchAndHonorsRevision(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	f.apply(a)
	b := f.commit("B")
	original := f.apply(b)
	if e := os.Rename(f.repo, f.repo+"-offline"); e != nil {
		t.Fatal(e)
	}
	req := f.request(a)
	req.RestoreOf = original.ReleaseID
	got := f.r.Apply(context.Background(), req)
	if got.Status != release.NodeStatusSucceeded {
		t.Fatalf("restore used Git or failed: %+v", got)
	}
	req = f.request(a)
	req.RestoreOf = original.ReleaseID
	got = f.r.Apply(context.Background(), req)
	if got.ErrorCode != "RESTORE_BASELINE_CONFLICT" {
		t.Fatalf("stale restore accepted: %+v", got)
	}
}
func TestDriftAndCorruptSnapshotDoNotSkip(t *testing.T) {
	for _, kind := range []string{"link", "content"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			a := f.commit("A")
			f.apply(a)
			target := f.cfg.Targets[0]
			if kind == "content" {
				if e := os.WriteFile(filepath.Join(target.Dir, a, "site", "site.conf"), []byte("changed"), 0644); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.Remove(filepath.Join(target.Dir, "latest")); e != nil {
					t.Fatal(e)
				}
			}
			got := f.r.Apply(context.Background(), f.request(a))
			if got.Status != release.NodeStatusRecoveryRequired {
				t.Fatalf("bad baseline skipped: %+v", got)
			}
		})
	}
}
func TestCleanupProtectsPreviousAndWarnsAfterSuccess(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	f.apply(a)
	base, e := openTarget(f.cfg.Targets[0])
	if e != nil {
		t.Fatal(e)
	}
	defer base.Close()
	// A corrupt historical manifest only affects cleanup, never the authoritative terminal result.
	bad := strings.Repeat("f", 40)
	if e = base.WriteFile(".manifests/"+bad+".json", []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	b := f.commit("B")
	got := f.apply(b)
	if len(got.Warnings) == 0 {
		t.Fatal("cleanup failure was not reported")
	}
	st, link := f.current()
	if st.Previous.CommitID != a || link != b {
		t.Fatal("cleanup rolled back or forgot previous")
	}
}
func TestUnknownExistingTargetIsNotAdopted(t *testing.T) {
	f := newFixture(t)
	f.r.Close()
	f.r = nil
	p := filepath.Join(f.cfg.DataDir, "state", f.cfg.Targets[0].ID+".json")
	if e := os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(f.cfg.Targets[0].Dir, "legacy.conf"), []byte("legacy"), 0644); e != nil {
		t.Fatal(e)
	}
	r, e := NewWithRuntime(f.cfg, f.rt)
	if e != nil {
		t.Fatal(e)
	}
	f.r = r
	if r.Health()["publish_ready"] != false {
		t.Fatal("legacy files silently adopted")
	}
}
func TestInterruptedPreparationBecomesFailed(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	req := f.request(a)
	st, _ := f.current()
	st.ActiveID = req.ReleaseID
	st.Records[req.ReleaseID] = &state.Record{Request: req, Result: release.Result{ReleaseID: req.ReleaseID, TargetID: st.Target.ID, Status: release.NodeStatusRunning}, HTTPStatus: 409}
	if e := f.r.save(st); e != nil {
		t.Fatal(e)
	}
	f.restart()
	st, _ = f.current()
	if st.ActiveID != "" || st.Records[req.ReleaseID].Result.Status != release.NodeStatusFailed {
		b, _ := json.Marshal(st)
		t.Fatal(string(b))
	}
}
func TestIdentityIncludesPathNotProject(t *testing.T) {
	f := newFixture(t)
	original := f.cfg.Targets[0].ID
	c := f.cfg
	c.Targets = append([]config.Target(nil), c.Targets...)
	c.Targets[0].Project = "metadata"
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	if c.Targets[0].ID != original {
		t.Fatal("project changes identity")
	}
	c.Targets[0].PathDest = filepath.Join(filepath.Dir(c.Targets[0].PathDest), "other-deploy")
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	if c.Targets[0].ID == original {
		t.Fatal("different directories collide")
	}
	c.Targets[0].ServerName = "../escape"
	if e := c.Validate(); e == nil {
		t.Fatal("escape accepted")
	}
}

func TestPreActivationCancellationDoesNotBlockNode(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := f.r.Apply(ctx, f.request(a))
	if got.Status != release.NodeStatusFailed || got.ActivationStatus != "unchanged" {
		t.Fatalf("%+v", got)
	}
	if f.r.Health()["publish_ready"] != true {
		t.Fatal("cancelled preparation blocked future publication")
	}
}
func TestDifferentDataDirectoryCannotClaimSameTarget(t *testing.T) {
	f := newFixture(t)
	other := f.cfg
	other.DataDir = filepath.Join(filepath.Dir(f.cfg.DataDir), "other-state")
	other.LockFile = filepath.Join(filepath.Dir(f.cfg.LockFile), "other.lock")
	if e := other.Validate(); e != nil {
		t.Fatal(e)
	}
	r, e := NewWithRuntime(other, f.rt)
	if e == nil {
		r.Close()
		t.Fatal("independent state/lock accepted for same target")
	}
}
func TestPublishDirectorySymlinkCannotEscape(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	outside := t.TempDir()
	target := f.cfg.Targets[0]
	if e := os.Symlink(outside, filepath.Join(target.Dir, ".staging")); e != nil {
		t.Fatal(e)
	}
	got := f.r.Apply(context.Background(), f.request(a))
	if got.Status != release.NodeStatusFailed {
		t.Fatalf("%+v", got)
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatal("wrote outside deployment target")
	}
}
func TestPreparationHonorsDiskAndArchiveLimits(t *testing.T) {
	t.Run("disk", func(t *testing.T) {
		f := newFixture(t)
		a := f.commit("A")
		f.r.cfg.MinFreeBytes = 1 << 62
		got := f.r.Apply(context.Background(), f.request(a))
		if got.Status != release.NodeStatusFailed || !strings.Contains(got.Error, "disk") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("archive", func(t *testing.T) {
		f := newFixture(t)
		a := f.commit(strings.Repeat("a", 100))
		f.r.git.MaxBytes = 10
		got := f.r.Apply(context.Background(), f.request(a))
		if got.Status != release.NodeStatusFailed || !strings.Contains(got.Error, "limit") {
			t.Fatalf("%+v", got)
		}
	})
}
