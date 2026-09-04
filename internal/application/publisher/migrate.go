package publisher

import (
	"context"
	"fmt"
	"io/fs"
	"nginx_updata_config/internal/infrastructure/nginx"
	"os"
	"path/filepath"
	"time"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/state"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"nginx_updata_config/internal/infrastructure/git"
	"nginx_updata_config/internal/infrastructure/lock"
	statestore "nginx_updata_config/internal/infrastructure/state"
)

// AdoptBaseline is an offline, explicit migration command. It only accepts a legacy
// latest -> <full-commit> link whose complete contents match the configured Git source.
// Its durable intent is recovered by the normal HTTP service after an interruption.
func AdoptBaseline(ctx context.Context, cfg config.Config, targetID, branch, commit string) error {
	var target config.Target
	for _, t := range cfg.Targets {
		if t.ID == targetID {
			target = t
		}
	}
	if target.ID == "" || !release.IsCommit(commit) {
		return fmt.Errorf("configured target_id and full commit required")
	}
	if target.Type == release.ReleaseTypeFrontendStatic {
		return fmt.Errorf("frontend ORAS migration requires a new empty target and explicit Nginx root cutover; old Git snapshots are not adopted")
	}
	nl, e := lock.Open(cfg.LockFile)
	if e != nil {
		return e
	}
	defer nl.Close()
	if e = nl.Try(); e != nil {
		return e
	}
	defer nl.Unlock()
	stStore, e := statestore.Open(cfg.DataDir)
	if e != nil {
		return e
	}
	defer stStore.Close()
	st, e := stStore.Load(target.ID)
	if statestore.IsMissing(e) {
		st = state.New(target)
	} else if e != nil {
		return e
	}
	if st.ActiveID != "" || st.Current != nil || len(st.Records) != 0 {
		return fmt.Errorf("migration requires an unmanaged target without active or recorded releases")
	}
	base, e := openTarget(target)
	if e != nil {
		return e
	}
	defer base.Close()
	if e = claim(base, ".publisher.json", owner{cfg.NodeID, cfg.Env, cfg.DataDir, cfg.LockFile}); e != nil {
		return e
	}
	oldLink, e := fsutil.Link(base)
	if e != nil {
		return e
	}
	// Restrict legacy sources to a direct child; never follow arbitrary old absolute links.
	if oldLink != commit && oldLink != filepath.Join(target.Dir, commit) {
		return fmt.Errorf("legacy latest must point to the target’s supplied full-commit child; observed %q", oldLink)
	}
	info, e := base.Lstat(commit)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("legacy snapshot must be a real directory")
	}
	gc := git.Client{DataDir: cfg.DataDir, Repos: cfg.Repos, MaxBytes: cfg.MaxArchiveBytes, MaxFiles: cfg.MaxArchiveFiles}
	work, actual, e := gc.Checkout(ctx, string(target.Type), branch, commit, target.ServerName)
	if e != nil {
		return e
	}
	defer os.RemoveAll(work)
	candidate, e := prepareSnapshot(ctx, cfg, target, work, actual, cfg.Repos[string(target.Type)].URL, commit)
	if e != nil {
		return e
	}
	manifest, e := verifySnapshot(ctx, base, candidate)
	if e != nil {
		return e
	}
	legacy, e := base.OpenRoot(commit)
	if e != nil {
		return e
	}
	defer legacy.Close()
	seen := 0
	e = fs.WalkDir(legacy.FS(), ".", func(name string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		expected, ok := manifest.Files[name]
		if !ok {
			return fmt.Errorf("legacy extra file: %s", name)
		}
		got, e := hashFile(ctx, legacy, name)
		if e != nil {
			return e
		}
		if got != expected {
			return fmt.Errorf("legacy differs from Git: %s", name)
		}
		seen++
		return nil
	})
	if e != nil {
		return e
	}
	if seen != len(manifest.Files)-1 {
		return fmt.Errorf("legacy is missing source files (the generated .release-version is excluded)")
	}
	rt := &nginx.Runtime{Config: cfg}
	if e = rt.Test(ctx); e != nil {
		return e
	}
	// The legacy files are identical. A local baseline under releases/ is now valid.
	// Persist an ordinary switch intent before touching latest, so startup can finish it.
	id := release.ID()
	req := release.ApplyRequest{ReleaseID: id, ExpectedStateRevision: st.Revision, Env: cfg.Env, Type: target.Type, SourceRepo: string(target.Type), Branch: branch, CommitID: commit, Version: commit, Project: target.Project, Params: map[string]string{"path_dest": target.PathDest, "server_name": target.ServerName}}
	rec := &state.Record{Request: req, Fingerprint: release.Digest(struct {
		Request      release.ApplyRequest
		Target, Repo string
	}{req, target.ID, cfg.Repos[string(target.Type)].URL}), Candidate: candidate, Baseline: candidate, BeforeLink: candidate.Link, Intent: true, HTTPStatus: 409, Result: release.Result{ReleaseID: id, TargetID: target.ID, Env: cfg.Env, Type: target.Type, Project: target.Project, ServerName: target.ServerName, Version: commit, CommitID: commit, Status: release.NodeStatusRunning, Phase: "baseline_migration", ActivationStatus: "unknown", RollbackStatus: "not_needed", StateRevisionBefore: st.Revision, StateRevisionAfter: st.Revision, StartedAt: time.Now().UTC()}}
	st.ActiveID = id
	st.RecoveryRequired = true
	st.Records[id] = rec
	// Allow startup recovery to recognize the legacy link without ever using it as an unchecked destination.
	rec.LegacyLink = oldLink
	if e = stStore.Save(st); e != nil {
		return e
	}
	if e = fsutil.Switch(base, candidate.Link); e != nil {
		return e
	}
	if e = rt.Test(ctx); e == nil {
		e = rt.Reload(ctx)
	}
	if e == nil {
		e = rt.Verify(ctx, target, commit, false)
	}
	if e != nil {
		return fmt.Errorf("migration requires recovery; start the service to reconcile: %w", e)
	}
	candidate.VerifiedAt = time.Now().UTC()
	st.Current = candidate
	st.Previous = nil
	st.Revision = release.ID()
	st.ActiveID = ""
	st.RecoveryRequired = false
	rec.Result.Status = release.NodeStatusSucceeded
	rec.Result.Phase = "complete"
	rec.Result.ActivationStatus = "verified"
	rec.Result.StateRevisionAfter = st.Revision
	rec.Result.FinishedAt = time.Now().UTC()
	rec.HTTPStatus = 200
	return stStore.Save(st)
}
