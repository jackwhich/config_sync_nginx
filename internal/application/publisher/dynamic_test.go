package publisher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/state"
)

type resolvingFixture struct {
	ArtifactSource
	calls  int
	digest string
}

func (a *resolvingFixture) Resolve(context.Context, config.Target, string) (string, error) {
	a.calls++
	return a.digest, nil
}

func TestFrontendSimpleRequestPersistsResolvedDigestAndReplaysOffline(t *testing.T) {
	f, _ := frontendFixtureMode(t, false)
	commit, _ := f.frontendCommit("console.log('ordinary dist')", "")
	source := &resolvingFixture{ArtifactSource: f.r.artifacts, digest: "sha256:" + release.Digest(commit)}
	f.r.artifacts = source
	req := frontendRequest(f, commit)
	req.ArtifactDigest = ""
	req.ExpectedStateRevision = ""
	req.Branch = ""
	got := f.r.Apply(context.Background(), req)
	if got.Status != release.NodeStatusSucceeded || got.ArtifactDigest != source.digest || source.calls != 1 {
		t.Fatal(got, source.calls)
	}
	st, err := f.r.store.Load(got.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current.ArtifactDigest != source.digest || st.Records[req.ReleaseID].Request.ArtifactDigest != "" {
		t.Fatal("resolved digest not separated from original retry request")
	}
	f.r.artifacts = unavailableArtifacts{}
	if replay := f.r.Apply(context.Background(), req); !replay.Replayed || replay.ArtifactDigest != source.digest {
		t.Fatal(replay)
	}
}

func TestTypeOnlyPublishRestartRollbackAndOverlap(t *testing.T) {
	f := newFixture(t)
	a := f.commit("A")
	root := f.cfg.Targets[0].PathDest
	f.r.Close()
	f.cfg.Targets = []config.Target{{Type: release.ReleaseTypeConfig}}
	repo := f.cfg.Repos["config"]
	repo.AllowedBranches = nil
	f.cfg.Repos["config"] = repo
	if err := f.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.r, err = NewWithRuntime(f.cfg, f.rt)
	if err != nil {
		t.Fatal(err)
	}
	request := func(commit string) release.ApplyRequest {
		return release.ApplyRequest{Env: "test", Type: release.ReleaseTypeConfig, CommitAlias: commit, Params: map[string]string{"server_name": "site", "path_dest": root}}
	}
	first := f.r.Apply(context.Background(), request(a))
	if first.Status != release.NodeStatusSucceeded || !release.IsID(first.ReleaseID) {
		t.Fatal(first)
	}
	st, err := f.r.store.Load(first.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Target.Dynamic || st.Current.CommitID != a {
		t.Fatal(st)
	}
	// Same release ID replays even with the optional revision omitted.
	duplicate := request(a)
	duplicate.ReleaseID = first.ReleaseID
	if got := f.r.Apply(context.Background(), duplicate); !got.Replayed {
		t.Fatal(got)
	}
	if _, err := f.r.Resolve(release.ReleaseTypeConfig, "child", st.Target.Dir, "", "test"); err == nil {
		t.Fatal("overlapping dynamic site allowed")
	}
	bad := request(a)
	bad.DataDir = filepath.Join(root, "other-state")
	if got := f.r.Apply(context.Background(), bad); got.ErrorCode != "INVALID_DATA_DIR" {
		t.Fatal(got)
	}
	f.r.Close()
	f.r, err = NewWithRuntime(f.cfg, f.rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.r.Target(first.TargetID); !ok {
		t.Fatal("dynamic target lost on restart")
	}
	b := f.commit("B")
	calls := 0
	f.rt.reload = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("candidate reload failed")
		}
		return nil
	}
	failed := f.r.Apply(context.Background(), request(b))
	if failed.Status != release.NodeStatusFailed || failed.RollbackStatus != "succeeded" {
		t.Fatal(failed)
	}
	link, err := os.Readlink(filepath.Join(st.Target.Dir, "latest"))
	if err != nil || link != "releases/"+a {
		t.Fatal(link, err)
	}
	st, err = f.r.store.Load(first.TargetID)
	if err != nil || st.Current.CommitID != a || st.ActiveID != "" {
		t.Fatal(st, err)
	}
	// Simulate a persistence failure after activation; restart must recover the
	// registered dynamic target even though no site appears in the YAML.
	f.rt.reload = nil
	once := false
	f.r.store.BeforeSave = func(st *state.TargetState) error {
		if !once && st.Current != nil && st.Current.CommitID == b && st.ActiveID == "" {
			once = true
			return errors.New("interrupted state commit")
		}
		return nil
	}
	if got := f.r.Apply(context.Background(), request(b)); got.Status != release.NodeStatusRecoveryRequired {
		t.Fatal(got)
	}
	f.r.Close()
	f.r, err = NewWithRuntime(f.cfg, f.rt)
	if err != nil {
		t.Fatal(err)
	}
	st, err = f.r.store.Load(first.TargetID)
	if err != nil || st.Current.CommitID != b || st.ActiveID != "" || st.RecoveryRequired {
		t.Fatal(st, err)
	}
}
