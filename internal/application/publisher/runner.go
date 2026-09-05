package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"nginx_updata_config/internal/infrastructure/nginx"
	"nginx_updata_config/internal/infrastructure/oras"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/state"
	"nginx_updata_config/internal/infrastructure/applog"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"nginx_updata_config/internal/infrastructure/git"
	"nginx_updata_config/internal/infrastructure/lock"
	"nginx_updata_config/internal/infrastructure/prom"
	statestore "nginx_updata_config/internal/infrastructure/state"
)

// Runner executes one synchronous transaction at a time across every target on a node.
// JSON snapshots published in views are immutable; only the lock owner mutates transactions.
type ArtifactSource interface {
	Pull(context.Context, config.Target, string, string) (string, error)
}
type Runner struct {
	artifacts ArtifactSource
	cfg       config.Config
	git       git.Client
	runtime   Runtime
	store     *statestore.Store
	nodeLock  *lock.Lock
	busy      chan struct{}
	mu        sync.RWMutex
	views     map[string]*state.TargetState
	blocked   string
	stopping  bool
}

func New(cfg config.Config) (*Runner, error) { return NewWithRuntime(cfg, &nginx.Runtime{}) }
func NewWithRuntime(cfg config.Config, rt Runtime) (*Runner, error) {
	store, e := statestore.Open(cfg.DataDir)
	if e != nil {
		return nil, e
	}
	nl, e := lock.Open(cfg.LockFile)
	if e != nil {
		store.Close()
		return nil, e
	}
	r := &Runner{cfg: cfg, artifacts: oras.Client{Config: cfg.ORAS, DataDir: cfg.DataDir, MaxBytes: cfg.MaxArchiveBytes, MaxFiles: cfg.MaxArchiveFiles}, git: git.Client{DataDir: cfg.DataDir, Repos: cfg.Repos, MaxBytes: cfg.MaxArchiveBytes, MaxFiles: cfg.MaxArchiveFiles}, runtime: rt, store: store, nodeLock: nl, busy: make(chan struct{}, 1), views: map[string]*state.TargetState{}}

	if e = nl.Try(); e != nil {
		r.Close()
		return nil, fmt.Errorf("startup requires idle node lock: %w", e)
	}
	defer nl.Unlock()
	if e = r.load(); e != nil {
		r.Close()
		return nil, e
	}
	startupTargets := map[string]config.Target{}
	for _, t := range cfg.Targets {
		if !t.IsTemplate() {
			startupTargets[t.ID] = t
		}
	}
	for _, st := range r.views {
		env := st.Target.Env
		if env == "" {
			env = cfg.Env
		}
		t, err := cfg.TargetForEnv(st.Target.Type, st.Target.ServerName, st.Target.PathDest, st.Target.Project, env)
		if err == nil && t.ID == st.Target.ID {
			startupTargets[t.ID] = t
		}
	}
	for _, t := range startupTargets {
		st, err := r.ensureTarget(t)
		if err != nil {
			r.Close()
			return nil, err
		}
		if st.ActiveID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.RecoveryTimeout.Value())
			r.recoverStartup(ctx, t, st)
			cancel()
		} else if st.Current != nil {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.StepTimeout.Value())
			err := r.checkStoredBaseline(ctx, t, st)
			cancel()
			st.RecoveryRequired = err != nil
			if err := r.save(st); err != nil {
				r.block(err.Error())
			}
		}
	}
	// Pending work for targets removed from configuration cannot be recovered safely.
	r.mu.RLock()
	for _, st := range r.views {
		if (st.ActiveID != "" || st.RecoveryRequired) && !r.configured(st.Target.ID) {
			e = fmt.Errorf("unconfigured target needs recovery: %s", st.Target.ID)
		}
	}
	r.mu.RUnlock()
	if e != nil {
		r.block(e.Error())
	}
	r.Health()
	return r, nil
}
func (r *Runner) Close() {
	if r.nodeLock != nil {
		r.nodeLock.Close()
	}
	if r.store != nil {
		r.store.Close()
	}
}
func (r *Runner) Stop() { r.mu.Lock(); r.stopping = true; r.mu.Unlock() }
func (r *Runner) Wait(ctx context.Context) error {
	select {
	case r.busy <- struct{}{}:
		<-r.busy
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *Runner) configured(id string) bool {
	st := r.views[id]
	if st == nil {
		return false
	}
	env := st.Target.Env
	if env == "" {
		env = r.cfg.Env
	}
	t, err := r.cfg.TargetForEnv(st.Target.Type, st.Target.ServerName, st.Target.PathDest, st.Target.Project, env)
	return err == nil && t.ID == id
}
func (r *Runner) block(reason string) { r.mu.Lock(); r.blocked = reason; r.mu.Unlock() }
func clone(st *state.TargetState) *state.TargetState {
	b, _ := json.Marshal(st)
	var out state.TargetState
	_ = json.Unmarshal(b, &out)
	return &out
}
func (r *Runner) publish(st *state.TargetState) {
	cp := clone(st)
	r.mu.Lock()
	r.views[st.Target.ID] = cp
	r.mu.Unlock()
}
func (r *Runner) save(st *state.TargetState) error {
	if e := r.store.Save(st); e != nil {
		prom.PersistFailure(st.Target.Env, string(st.Target.Type), st.Target.ID)
		applog.LogError("发布状态写入失败", "state_persist_failed", map[string]any{"env": st.Target.Env, "target_id": st.Target.ID, "error": e.Error()})
		return e
	}
	r.publish(st)
	return nil
}
func (r *Runner) load() error {
	states, e := r.store.All()
	if e != nil {
		return e
	}
	next := map[string]*state.TargetState{}
	ids := map[string]bool{}
	for _, st := range states {
		for id := range st.Records {
			if ids[id] {
				return fmt.Errorf("duplicate release_id in persistent state")
			}
			ids[id] = true
		}
		next[st.Target.ID] = st
	}
	r.mu.Lock()
	r.views = next
	r.mu.Unlock()
	return nil
}
func (r *Runner) Health() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ready := !r.stopping && r.blocked == ""
	reason := r.blocked
	for _, st := range r.views {
		verifiedAt := time.Time{}
		commitID := ""
		if st.Current != nil {
			verifiedAt = st.Current.VerifiedAt
			commitID = st.Current.CommitID
		}
		prom.TargetState(st.Target.Env, string(st.Target.Type), st.Target.ServerName, st.Target.ID, st.RecoveryRequired, verifiedAt, commitID)
	}
	prom.Ready(r.cfg.Env, r.cfg.NodeID, ready)
	envs := []string{}
	for env := range r.cfg.ReleaseAuthTokens {
		if r.cfg.AcceptsEnv(env) {
			envs = append(envs, env)
		}
	}
	return map[string]any{"enabled_envs": envs, "status": "ok", "release_contract": 2, "capabilities": []string{release.FrontendORASCapability, "request_targets_v1", "nginx_commands_v1"}, "node_id": r.cfg.NodeID, "env": r.cfg.Env, "publish_ready": ready, "busy": len(r.busy) > 0, "reason": reason}
}
func (r *Runner) Target(id string) (config.Target, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := r.views[id]
	if st == nil {
		return config.Target{}, false
	}
	env := st.Target.Env
	if env == "" {
		env = r.cfg.Env
	}
	t, err := r.cfg.TargetForEnv(st.Target.Type, st.Target.ServerName, st.Target.PathDest, st.Target.Project, env)
	return t, err == nil && t.ID == id
}
func (r *Runner) Resolve(typ release.ReleaseType, site, root, project string, environments ...string) (config.Target, error) {
	env := r.cfg.Env
	if len(environments) > 0 {
		env = environments[0]
	}
	t, err := r.cfg.TargetForEnv(typ, site, root, project, env)
	if err != nil {
		return config.Target{}, err
	}
	if existing, ok := r.Target(t.ID); ok {
		return existing, nil
	}
	select {
	case r.busy <- struct{}{}:
		defer func() { <-r.busy }()
	default:
		return config.Target{}, lock.ErrBusy
	}
	if err := r.nodeLock.Try(); err != nil {
		return config.Target{}, err
	}
	defer r.nodeLock.Unlock()
	if err := r.load(); err != nil {
		return config.Target{}, err
	}
	if _, err := r.ensureTarget(t); err != nil {
		return config.Target{}, err
	}
	return t, nil
}

// Called only with the node lock; registered targets survive service restarts.
func (r *Runner) ensureTarget(t config.Target) (*state.TargetState, error) {
	r.mu.RLock()
	count := len(r.views)
	var conflict bool
	for id, other := range r.views {
		if id != t.ID && (fsutil.Within(t.Dir, other.Target.Dir) || fsutil.Within(other.Target.Dir, t.Dir)) {
			conflict = true
		}
	}
	r.mu.RUnlock()
	if conflict {
		return nil, fmt.Errorf("deployment directory overlaps a registered target")
	}
	st, err := r.store.Load(t.ID)
	if err != nil && !statestore.IsMissing(err) {
		return nil, err
	}
	if statestore.IsMissing(err) {
		if count >= r.cfg.MaxDynamicTargets && t.Dynamic {
			return nil, fmt.Errorf("registered target limit reached")
		}
		st = state.New(t)
		if err := r.claimTarget(t); err != nil {
			return nil, err
		}
		base, err := openTarget(t)
		if err != nil {
			return nil, err
		}
		entries, err := fs.ReadDir(base.FS(), ".")
		base.Close()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Name() != ".publisher.json" {
				st.RecoveryRequired = true
			}
		}
	} else {
		if st.Target.Dir != t.Dir || st.Target.Type != t.Type {
			return nil, fmt.Errorf("state target mismatch")
		}
		if err := r.claimTarget(t); err != nil {
			return nil, err
		}
		st.Target = t
	}
	if err := r.save(st); err != nil {
		return nil, err
	}
	prom.InitTarget(t.Env, string(t.Type), t.ServerName, t.ID)
	return st, nil
}

// State includes the live revision independently of a historical release result.
func (r *Runner) State(id, releaseID string) (map[string]any, int, error) {
	t, ok := r.Target(id)
	if !ok {
		return nil, 403, fmt.Errorf("target not authorized")
	}
	st, e := r.store.Load(id)
	if e != nil {
		return nil, 503, e
	}
	r.mu.RLock()
	if r.blocked != "" {
		if local := r.views[id]; local != nil {
			st = local
		}
	}
	r.mu.RUnlock()
	out := map[string]any{"release_contract": 2, "capabilities": []string{release.FrontendORASCapability, "request_targets_v1", "nginx_commands_v1"}, "node_id": r.cfg.NodeID, "env": t.Env, "target": t, "target_id": id, "state_revision": st.Revision, "current": st.Current, "previous": st.Previous, "active_release_id": st.ActiveID, "recovery_required": st.RecoveryRequired, "current_commit_id": "", "previous_commit_id": ""}
	if st.Current != nil {
		out["current_commit_id"] = st.Current.CommitID
	}
	if st.Previous != nil {
		out["previous_commit_id"] = st.Previous.CommitID
	}
	base, e := openTarget(t)
	if e != nil {
		return nil, 503, e
	}
	link, e := fsutil.Link(base)
	base.Close()
	if e != nil {
		out["observed_link_error"] = e.Error()
	} else {
		out["observed_link"] = link
	}
	if releaseID != "" {
		rec := st.Records[releaseID]
		if rec == nil {
			return nil, 404, fmt.Errorf("release_id not found")
		}
		out["release"] = rec.Result
		out["release_http_status"] = rec.HTTPStatus
		out["baseline"] = rec.Baseline
		out["before_link"] = rec.BeforeLink
	}
	return out, 200, nil
}
func reject(req release.ApplyRequest, code int, key, msg string) release.Result {
	return release.Result{ReleaseID: req.ReleaseID, Status: release.NodeStatusFailed, ActivationStatus: "unchanged", RollbackStatus: "not_needed", ErrorCode: key, Error: msg, HTTPStatus: code, StartedAt: time.Now().UTC()}
}

func leftoverCommit(rec *state.Record) string {
	if rec.Request.CommitID != "" {
		return rec.Request.CommitID
	}
	return rec.Result.CommitID
}

// takeover lets a later commit own the target. RecoveryRequired is a stored
// outcome of leftover work, not a node lock. A new commit_id proceeds; the
// previous release_id is only closed out when the commit itself changed.
func (r *Runner) takeover(st *state.TargetState, req release.ApplyRequest) {
	if id := st.ActiveID; id != "" && id != req.ReleaseID {
		if rec := st.Records[id]; rec != nil && !rec.Result.Terminal() && leftoverCommit(rec) != req.CommitID {
			rec.Result.Status = release.NodeStatusFailed
			rec.Result.ErrorCode = "SUPERSEDED"
			rec.Result.Error = "superseded by a newer commit"
			rec.Result.FinishedAt = time.Now().UTC()
			rec.HTTPStatus = http.StatusConflict
			applog.LogWarn("新提交接管未完成的旧任务", "release_superseded", map[string]any{"env": rec.Request.Env, "node_id": r.cfg.NodeID, "target_id": st.Target.ID, "release_id": id, "commit_id": leftoverCommit(rec), "incoming_commit_id": req.CommitID})
		}
	}
	st.ActiveID = ""
	st.RecoveryRequired = false
}
func (r *Runner) duplicate(req release.ApplyRequest, fingerprint string) (release.Result, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, st := range r.views {
		if rec := st.Records[req.ReleaseID]; rec != nil {
			if rec.Fingerprint != fingerprint {
				return reject(req, 409, "RELEASE_ID_CONFLICT", "release_id was already used with different parameters"), true
			}
			result := rec.Result
			result.HTTPStatus = rec.HTTPStatus
			if result.Terminal() {
				result.Replayed = true
				return result, true
			}
			if result.Status == release.NodeStatusRecoveryRequired {
				return release.Result{}, false
			}
			result.HTTPStatus = 409
			result.ErrorCode = "RELEASE_RUNNING"
			return result, true
		}
	}
	return release.Result{}, false
}

// Apply retains the in-process synchronous workflow for callers that use the
// Go API directly. HTTP callers use Stage followed by the separate nginx
// command endpoints.
func (r *Runner) Apply(ctx context.Context, req release.ApplyRequest) release.Result {
	return r.apply(ctx, req, false)
}

// Stage fetches and verifies a candidate, then switches latest. It is a
// completed Git action: NginxTest and NginxReload are separate completed
// actions which use this release's retained baseline if they need to roll back.
func (r *Runner) Stage(ctx context.Context, req release.ApplyRequest) release.Result {
	return r.apply(ctx, req, true)
}

func (r *Runner) apply(ctx context.Context, req release.ApplyRequest, deferNginxCommands bool) release.Result {
	if e := release.ValidateApplyRequest(&req); e != nil {
		return reject(req, 400, "INVALID_REQUEST", e.Error())
	}
	if !r.cfg.AcceptsEnv(req.Env) {
		return reject(req, 403, "ENV_NOT_ALLOWED", "environment does not match this node")
	}
	if req.DataDir != "" {
		path, err := fsutil.Canonical(req.DataDir)
		if err != nil || path != r.cfg.DataDir {
			return reject(req, 400, "INVALID_DATA_DIR", "data_dir must match the service configuration")
		}
		req.DataDir = ""
	}
	t, e := r.Resolve(req.Type, release.ServerIdentity(req.Params), req.Params["path_dest"], req.Project, req.Env)
	if e != nil {
		if errors.Is(e, lock.ErrBusy) {
			return reject(req, 409, "NODE_BUSY", e.Error())
		}
		return reject(req, 403, "TARGET_NOT_ALLOWED", e.Error())
	}
	req.Params["path_dest"] = t.PathDest
	if req.Project == "" {
		req.Project = t.Project
	}
	req.Version = release.EffectiveVersion(req)
	source := r.source(req, t)
	fingerprint := release.Digest(struct {
		Request      release.ApplyRequest
		Target, Repo string
	}{req, t.ID, source})
	if result, ok := r.duplicate(req, fingerprint); ok {
		return result
	}
	select {
	case r.busy <- struct{}{}:
		defer func() { <-r.busy }()
	default:
		return reject(req, 409, "NODE_BUSY", "another release owns the node lock")
	}
	if e = r.nodeLock.Try(); e != nil {
		if errors.Is(e, lock.ErrBusy) {
			return reject(req, 409, "NODE_BUSY", e.Error())
		}
		return reject(req, 503, "LOCK_FAILED", e.Error())
	}
	defer r.nodeLock.Unlock()
	if e = r.load(); e != nil {
		r.block(e.Error())
		return reject(req, 503, "STATE_UNAVAILABLE", e.Error())
	}
	if result, ok := r.duplicate(req, fingerprint); ok {
		return result
	}
	r.mu.RLock()
	blocked := r.blocked
	stopping := r.stopping
	r.mu.RUnlock()
	if blocked != "" || stopping {
		return reject(req, 503, "RECOVERY_REQUIRED", "publication unavailable: "+blocked)
	}
	st, e := r.store.Load(t.ID)
	if e != nil {
		return reject(req, 503, "STATE_UNAVAILABLE", e.Error())
	}
	if req.ExpectedStateRevision != "" && req.ExpectedStateRevision != st.Revision {
		return reject(req, 409, "STATE_REVISION_CONFLICT", "target revision has changed; query live state")
	}
	if req.RestoreOf != "" {
		original := st.Records[req.RestoreOf]
		if original == nil || original.Result.Status != release.NodeStatusSucceeded || original.Result.StateRevisionAfter != st.Revision || original.Baseline == nil || st.Current == nil || original.Candidate == nil || st.Current.CommitID != original.Candidate.CommitID || req.CommitID != original.Baseline.CommitID {
			return reject(req, 409, "RESTORE_BASELINE_CONFLICT", "restore requires the successful release's retained baseline and unchanged resulting revision")
		}
	}
	if req.RestoreOf != "" && req.Type == release.ReleaseTypeFrontendStatic && req.ArtifactDigest == "" {
		req.ArtifactDigest = st.Records[req.RestoreOf].Baseline.ArtifactDigest
	}
	if req.RestoreOf != "" && req.Type == release.ReleaseTypeFrontendStatic && st.Records[req.RestoreOf].Baseline.ArtifactDigest != req.ArtifactDigest {
		return reject(req, 409, "RESTORE_BASELINE_CONFLICT", "artifact digest must match the recorded local baseline")
	}
	r.takeover(st, req)
	prom.Active(t.Env, string(t.Type), t.ID, true)
	defer prom.Active(t.Env, string(t.Type), t.ID, false)
	rec := &state.Record{Request: req, Fingerprint: fingerprint, Baseline: st.Current, BaselinePrevious: st.Previous, HTTPStatus: 409}
	rec.Result = release.Result{ArtifactDigest: req.ArtifactDigest, ReleaseID: req.ReleaseID, TargetID: t.ID, Env: req.Env, Type: req.Type, Project: req.Project, ServerName: t.ServerName, Version: req.Version, CommitID: req.CommitID, Status: release.NodeStatusRunning, Phase: "accepted", ActivationStatus: "unchanged", RollbackStatus: "not_needed", StateRevisionBefore: st.Revision, StateRevisionAfter: st.Revision, StartedAt: time.Now().UTC()}
	if st.Current != nil {
		rec.Result.PreviousCommitID = st.Current.CommitID
	}
	st.Records[req.ReleaseID] = rec
	st.ActiveID = req.ReleaseID
	if e = r.save(st); e != nil {
		return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
	}
	deadline := rec.Result.StartedAt.Add(r.cfg.ExecutionTimeout.Value())
	prep, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	base, e := openTarget(t)
	if e != nil {
		return r.fail(st, rec, "TARGET_UNAVAILABLE", e)
	}
	defer base.Close()
	// Observe live latest for rollback. Never block a publication on stored
	// baseline drift: identity is the requested commit_id.
	var liveOK bool
	var liveCommit, liveSource string
	e = r.step(prep, st, rec, "verify_baseline", func(c context.Context) error {
		if err := c.Err(); err != nil {
			return err
		}
		link, live := r.observeLiveCommit(c, t, base)
		rec.BeforeLink = link
		if live != nil {
			liveOK = true
			liveCommit = live.CommitID
			liveSource = live.Source
			if st.Current != nil && st.Current.CommitID != live.CommitID {
				st.Previous = st.Current
			}
			st.Current = live
			rec.Baseline = live
			rec.Result.PreviousCommitID = live.CommitID
		} else if st.Current != nil {
			rec.Baseline = st.Current
			rec.Result.PreviousCommitID = st.Current.CommitID
		}
		return nil
	})
	if e != nil {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return r.fail(st, rec, "BASELINE_CHECK_CANCELLED", e)
		}
		return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
	}
	if liveOK && liveCommit == req.CommitID && liveSource == source {
		for _, existing := range st.Records {
			if existing.Candidate != nil && existing.Candidate.CommitID == req.CommitID && existing.Candidate.Source == source && existing.Result.Status == release.NodeStatusSucceeded && resumableNginxPhase(existing.Result.Phase) {
				result := existing.Result
				result.Replayed = true
				result.HTTPStatus = http.StatusOK
				return result
			}
		}
		rec.Result.Status = release.NodeStatusSkipped
		rec.Result.Phase = "complete"
		rec.Result.FinishedAt = time.Now().UTC()
		rec.HTTPStatus = 200
		st.ActiveID = ""
		if e = r.save(st); e != nil {
			return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
		}
		return r.result(rec)
	}
	if req.RestoreOf != "" {
		original := st.Records[req.RestoreOf]
		candidate := *original.Baseline
		candidate.Version = req.Version
		rec.Candidate = &candidate
		e = r.step(prep, st, rec, "prepare_snapshot", func(c context.Context) error { _, e := verifySnapshot(c, base, rec.Candidate); return e })
	} else {
		var work, actual string
		fetchStep := "fetch"
		if req.Type == release.ReleaseTypeFrontendStatic {
			fetchStep = "oras_pull"
		}
		e = r.step(prep, st, rec, fetchStep, func(c context.Context) error {
			var err error
			if req.Type == release.ReleaseTypeFrontendStatic {
				actual = req.CommitID
				if req.ArtifactDigest == "" {
					resolver, ok := r.artifacts.(interface {
						Resolve(context.Context, config.Target, string) (string, error)
					})
					if !ok {
						return fmt.Errorf("artifact source does not support SHA tag resolution")
					}
					req.ArtifactDigest, err = resolver.Resolve(c, t, req.CommitID)
					if err != nil {
						return err
					}
					if !release.IsArtifactDigest(req.ArtifactDigest) {
						return fmt.Errorf("invalid resolved artifact digest")
					}
					source = r.source(req, t)
					rec.Result.ArtifactDigest = req.ArtifactDigest
					if err := r.save(st); err != nil {
						return err
					}
				}
				work, err = r.artifacts.Pull(c, t, req.CommitID, req.ArtifactDigest)
			} else {
				work, actual, err = r.git.Checkout(c, req.SourceRepo, req.Branch, req.CommitID, t.ServerName)
			}
			return err
		})
		if work != "" {
			defer r.cleanupExport(t, work)
		}
		if e == nil {
			e = r.step(prep, st, rec, "prepare_snapshot", func(c context.Context) error {
				var err error
				rec.Candidate, err = prepareSnapshot(c, r.cfg, t, work, actual, source, req.Version)
				return err
			})
		}
	}
	if e != nil {
		return r.fail(st, rec, "PREPARE_FAILED", e)
	}
	e = r.step(prep, st, rec, "verify_candidate", func(c context.Context) error {
		m, e := verifySnapshot(c, base, rec.Candidate)
		if e != nil {
			return e
		}
		return installAssets(c, base, rec.Candidate, m)
	})
	if e != nil {
		return r.fail(st, rec, "CANDIDATE_INVALID", e)
	}
	if e = prep.Err(); e != nil {
		return r.fail(st, rec, "PREPARE_CANCELLED", e)
	}
	// Critical work has its own deadline: disconnects can no longer interrupt activation/recovery.
	critical, criticalCancel := context.WithDeadline(context.Background(), deadline)
	defer criticalCancel()
	if currentLink, err := fsutil.Link(base); err != nil || currentLink != rec.BeforeLink {
		return r.uncertain(st, rec, "BASELINE_UNVERIFIED", fmt.Errorf("latest changed during preparation"))
	}
	rec.Intent = true
	rec.Result.Phase = "switch_intent"
	if e = r.save(st); e != nil {
		return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
	}
	e = r.step(critical, st, rec, "switch", func(c context.Context) error {
		if e := c.Err(); e != nil {
			return e
		}
		return fsutil.Switch(base, rec.Candidate.Link)
	})
	if e != nil {
		return r.restore(t, st, rec, "ACTIVATION_FAILED", e)
	}
	if deferNginxCommands {
		return r.commitGitStage(st, rec)
	}
	if e == nil {
		e = r.stepWithOutput(critical, st, rec, "nginx_test", r.nginxTest)
		if e != nil {
			return r.restore(t, st, rec, "NGINX_TEST_FAILED", e)
		}
	}
	if e == nil {
		e = r.stepWithOutput(critical, st, rec, "reload", r.nginxReload)
		if e != nil {
			return r.restore(t, st, rec, "NGINX_RELOAD_FAILED", e)
		}
	}
	if e == nil {
		e = r.step(critical, st, rec, "verify_activation", func(c context.Context) error {
			if e := r.verify(c, t, base, rec.Candidate); e != nil {
				return e
			}
			return r.verifyRetainedAssets(c, t, base, st, rec)
		})
	}
	if e != nil {
		return r.restore(t, st, rec, "ACTIVATION_FAILED", e)
	}
	return r.commit(t, st, rec)
}

func resumableNginxPhase(phase string) bool {
	switch phase {
	case "latest_switched", "nginx_test", "nginx_test_succeeded", "reload", "verify_activation":
		return true
	default:
		return false
	}
}
func (r *Runner) step(ctx context.Context, st *state.TargetState, rec *state.Record, name string, fn func(context.Context) error) error {
	return r.stepWithOutput(ctx, st, rec, name, func(c context.Context) (string, error) {
		return "", fn(c)
	})
}

func (r *Runner) stepWithOutput(ctx context.Context, st *state.TargetState, rec *state.Record, name string, fn func(context.Context) (string, error)) error {
	rec.Result.Phase = name
	if e := r.save(st); e != nil {
		return e
	}
	started := time.Now()
	stepCtx, cancel := context.WithTimeout(ctx, r.cfg.StepTimeout.Value())
	defer cancel()
	output := ""
	err := stepCtx.Err()
	if err == nil {
		output, err = fn(stepCtx)
	}
	output = commandOutput(output, err)
	status := release.NodeStatusSucceeded
	message := output
	if err != nil {
		status = release.NodeStatusFailed
		if message == "" {
			message = err.Error()
		}
	}
	rec.Result.Steps = append(rec.Result.Steps, release.Step{Name: name, Status: status, Message: message, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds()})
	prom.Step(st.Target.Env, string(st.Target.Type), st.Target.ID, name, string(status), time.Since(started))
	fields := map[string]any{"env": rec.Request.Env, "node_id": r.cfg.NodeID, "release_id": rec.Request.ReleaseID, "target_id": st.Target.ID, "step": name, "status": status, "duration_ms": time.Since(started).Milliseconds()}
	if output != "" {
		fields["command_output"] = output
	}
	if err != nil {
		fields["error"] = message
		applog.LogError("发布步骤失败", "release_step", fields)
	} else {
		applog.LogInfo("发布步骤完成", "release_step", fields)
	}
	return err
}

func (r *Runner) nginxTest(ctx context.Context) (string, error) {
	if reporter, ok := r.runtime.(NginxCommandReporter); ok {
		output, err := reporter.TestOutput(ctx)
		return nginxCommandOutput("nginx -t", output, err), err
	}
	err := r.runtime.Test(ctx)
	return nginxCommandOutput("nginx -t", "", err), err
}

func (r *Runner) nginxReload(ctx context.Context) (string, error) {
	if reporter, ok := r.runtime.(NginxCommandReporter); ok {
		output, err := reporter.ReloadOutput(ctx)
		return nginxCommandOutput("nginx -s reload", output, err), err
	}
	err := r.runtime.Reload(ctx)
	return nginxCommandOutput("nginx -s reload", "", err), err
}

func nginxCommandOutput(command, output string, err error) string {
	output = strings.TrimSpace(output)
	if output != "" {
		return command + " output: " + output
	}
	if err != nil {
		return err.Error()
	}
	return command + " succeeded"
}

func commandOutput(output string, err error) string {
	output = strings.TrimSpace(output)
	if output == "" || err != nil && output == err.Error() {
		return output
	}
	const maxOutputBytes = 8192
	if len(output) > maxOutputBytes {
		return output[:maxOutputBytes] + " [truncated]"
	}
	return output
}

// commitGitStage records a completed Git/snapshot switch.  It deliberately
// leaves no active transaction behind: nginx -t and reload are independently
// callable actions, while the recorded baseline remains available to either
// action for rollback.
func (r *Runner) commitGitStage(st *state.TargetState, rec *state.Record) release.Result {
	st.Previous = rec.Baseline
	st.Current = rec.Candidate
	st.Revision = release.ID()
	st.ActiveID = ""
	st.RecoveryRequired = false
	rec.Result.Status = release.NodeStatusSucceeded
	rec.Result.Phase = "latest_switched"
	rec.Result.ActivationStatus = "latest_switched"
	rec.Result.RollbackStatus = "not_needed"
	rec.Result.ErrorCode = ""
	rec.Result.Error = ""
	rec.Result.StateRevisionAfter = st.Revision
	rec.Result.FinishedAt = time.Now().UTC()
	rec.HTTPStatus = http.StatusOK
	if e := r.save(st); e != nil {
		return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
	}
	return r.result(rec)
}

func nginxCommandReject(req release.NginxCommandRequest, code int, key, message string) release.Result {
	return release.Result{ReleaseID: req.ReleaseID, Env: req.Env, Status: release.NodeStatusFailed, ActivationStatus: "unchanged", RollbackStatus: "not_needed", ErrorCode: key, Error: message, HTTPStatus: code, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
}

// NginxTest runs nginx -t for a completed Git switch. A failure restores the
// baseline that the switch recorded.
func (r *Runner) NginxTest(ctx context.Context, req release.NginxCommandRequest) release.Result {
	return r.runNginxCommand(ctx, req, "test")
}

// NginxReload runs nginx -s reload only after a successful NginxTest. It
// verifies the activated configuration and finishes the Nginx action.
func (r *Runner) NginxReload(ctx context.Context, req release.NginxCommandRequest) release.Result {
	return r.runNginxCommand(ctx, req, "reload")
}

// Abort is an explicit operator rollback for a Git switch that has not yet
// completed reload. It never changes health or blocks a later publication.
func (r *Runner) Abort(ctx context.Context, req release.NginxCommandRequest) release.Result {
	return r.runNginxCommand(ctx, req, "abort")
}

func (r *Runner) runNginxCommand(ctx context.Context, req release.NginxCommandRequest, command string) release.Result {
	if e := release.ValidateNginxCommandRequest(&req); e != nil {
		return nginxCommandReject(req, http.StatusBadRequest, "INVALID_REQUEST", e.Error())
	}
	if !r.cfg.AcceptsEnv(req.Env) {
		return nginxCommandReject(req, http.StatusForbidden, "ENV_NOT_ALLOWED", "environment does not match this node")
	}
	select {
	case r.busy <- struct{}{}:
		defer func() { <-r.busy }()
	default:
		return nginxCommandReject(req, http.StatusConflict, "NODE_BUSY", "another release owns the node lock")
	}
	if e := r.nodeLock.Try(); e != nil {
		if errors.Is(e, lock.ErrBusy) {
			return nginxCommandReject(req, http.StatusConflict, "NODE_BUSY", e.Error())
		}
		return nginxCommandReject(req, http.StatusServiceUnavailable, "LOCK_FAILED", e.Error())
	}
	defer r.nodeLock.Unlock()
	if e := r.load(); e != nil {
		r.block(e.Error())
		return nginxCommandReject(req, http.StatusServiceUnavailable, "STATE_UNAVAILABLE", e.Error())
	}
	r.mu.RLock()
	blocked, stopping := r.blocked, r.stopping
	r.mu.RUnlock()
	if blocked != "" || stopping {
		return nginxCommandReject(req, http.StatusServiceUnavailable, "RECOVERY_REQUIRED", "publication unavailable: "+blocked)
	}

	var targetID string
	r.mu.RLock()
	for id, view := range r.views {
		if view.Records[req.ReleaseID] != nil {
			targetID = id
			break
		}
	}
	r.mu.RUnlock()
	if targetID == "" {
		return nginxCommandReject(req, http.StatusNotFound, "RELEASE_NOT_FOUND", "release_id not found")
	}
	st, e := r.store.Load(targetID)
	if e != nil {
		return nginxCommandReject(req, http.StatusServiceUnavailable, "STATE_UNAVAILABLE", e.Error())
	}
	rec := st.Records[req.ReleaseID]
	if rec == nil || !rec.Intent || rec.Candidate == nil {
		return nginxCommandReject(req, http.StatusConflict, "RELEASE_NOT_FOUND", "release has no switched candidate")
	}
	env := st.Target.Env
	if env == "" {
		env = r.cfg.Env
	}
	if env != req.Env || rec.Request.Env != req.Env {
		return nginxCommandReject(req, http.StatusForbidden, "ENV_NOT_ALLOWED", "release environment does not match this node")
	}
	target, ok := r.Target(targetID)
	if !ok {
		return nginxCommandReject(req, http.StatusForbidden, "TARGET_NOT_ALLOWED", "release target is no longer authorized")
	}
	base, e := openTarget(target)
	if e != nil {
		return r.uncertain(st, rec, "TARGET_UNAVAILABLE", e)
	}
	defer base.Close()
	if st.Current == nil || st.Current.CommitID != rec.Candidate.CommitID || st.Current.Source != rec.Candidate.Source {
		return nginxCommandReject(req, http.StatusConflict, "RELEASE_NOT_CURRENT", "a newer Git switch is now current")
	}
	if link, e := fsutil.Link(base); e != nil || link != rec.Candidate.Link {
		if e == nil {
			e = fmt.Errorf("latest changed before nginx command")
		}
		return r.uncertain(st, rec, "BASELINE_UNVERIFIED", e)
	}
	deadline, cancel := context.WithTimeout(ctx, r.cfg.ExecutionTimeout.Value())
	defer cancel()
	switch command {
	case "abort":
		if rec.Result.Phase != "latest_switched" && rec.Result.Phase != "nginx_test_succeeded" {
			return nginxCommandReject(req, http.StatusConflict, "RELEASE_NOT_CURRENT", "release has already completed nginx activation")
		}
		return r.restore(target, st, rec, "RELEASE_ABORTED", errors.New("release aborted before nginx activation"))
	case "test":
		if rec.Result.Phase == "nginx_test_succeeded" && rec.NginxTested {
			result := rec.Result
			result.Replayed = true
			result.HTTPStatus = http.StatusOK
			return result
		}
		if rec.Result.Phase != "latest_switched" && rec.Result.Phase != "nginx_test" {
			return nginxCommandReject(req, http.StatusConflict, "NGINX_TEST_NOT_AVAILABLE", "release has not completed its Git switch")
		}
		if e = r.stepWithOutput(deadline, st, rec, "nginx_test", r.nginxTest); e != nil {
			return r.restore(target, st, rec, "NGINX_TEST_FAILED", e)
		}
		rec.NginxTested = true
		rec.Result.Status = release.NodeStatusSucceeded
		rec.Result.Phase = "nginx_test_succeeded"
		rec.Result.ActivationStatus = "nginx_test_passed"
		rec.Result.RollbackStatus = "not_needed"
		rec.Result.FinishedAt = time.Now().UTC()
		rec.HTTPStatus = http.StatusOK
		if e = r.save(st); e != nil {
			return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
		}
		return r.result(rec)
	case "reload":
		if rec.Result.Phase == "complete" && rec.Result.ActivationStatus == "reload_requested" {
			result := rec.Result
			result.Replayed = true
			result.HTTPStatus = http.StatusOK
			return result
		}
		if (rec.Result.Phase != "nginx_test_succeeded" && rec.Result.Phase != "reload" && rec.Result.Phase != "verify_activation") || !rec.NginxTested {
			return nginxCommandReject(req, http.StatusConflict, "NGINX_RELOAD_NOT_AVAILABLE", "release requires a successful nginx -t")
		}
		if e = r.stepWithOutput(deadline, st, rec, "reload", r.nginxReload); e != nil {
			return r.restore(target, st, rec, "NGINX_RELOAD_FAILED", e)
		}
		if e = r.step(deadline, st, rec, "verify_activation", func(c context.Context) error {
			if e := r.verify(c, target, base, rec.Candidate); e != nil {
				return e
			}
			return r.verifyRetainedAssets(c, target, base, st, rec)
		}); e != nil {
			return r.restore(target, st, rec, "ACTIVATION_FAILED", e)
		}
		return r.commit(target, st, rec)
	default:
		return nginxCommandReject(req, http.StatusBadRequest, "INVALID_COMMAND", "unsupported nginx command")
	}
}

func (r *Runner) verify(ctx context.Context, t config.Target, base *os.Root, v *state.Version) error {
	commit := ""
	var m *state.Manifest
	var e error
	if v != nil {
		m, e = verifySnapshot(ctx, base, v)
		if e != nil {
			return e
		}
		commit = v.CommitID
	}
	if e = r.runtime.Verify(ctx, t, commit, v == nil); e != nil {
		return e
	}
	if t.Type == release.ReleaseTypeFrontendStatic && v != nil && t.PublicBaseURL != "" {
		return nginx.VerifyFrontendHTTP(ctx, t, m)
	}
	return nil
}
func (r *Runner) result(rec *state.Record) release.Result {
	result := rec.Result
	result.HTTPStatus = rec.HTTPStatus
	if result.Terminal() {
		prom.Terminal(result.Env, string(result.Type), result.TargetID, string(result.Status))
	}
	fields := map[string]any{"env": result.Env, "node_id": r.cfg.NodeID, "release_id": result.ReleaseID, "target_id": result.TargetID, "type": result.Type, "server_name": result.ServerName, "commit_id": result.CommitID, "phase": result.Phase, "status": result.Status, "status_code": result.HTTPStatus, "error_code": result.ErrorCode, "duration_ms": time.Since(result.StartedAt).Milliseconds(), "rollback_status": result.RollbackStatus}
	if result.ArtifactDigest != "" {
		fields["artifact_digest"] = result.ArtifactDigest
	}
	if result.Status == release.NodeStatusFailed || result.Status == release.NodeStatusRecoveryRequired {
		fields["error"] = result.Error
		applog.LogError("发布执行失败", "release_result", fields)
	} else if result.Status == release.NodeStatusRunning {
		applog.LogInfo("发布等待 Nginx 命令", "release_pending", fields)
	} else {
		applog.LogInfo("发布执行完成", "release_result", fields)
	}
	return result
}
func (r *Runner) fail(st *state.TargetState, rec *state.Record, key string, cause error) release.Result {
	rec.Result.Status = release.NodeStatusFailed
	rec.Result.ErrorCode = key
	rec.Result.Error = cause.Error()
	rec.Result.FinishedAt = time.Now().UTC()
	rec.HTTPStatus = http.StatusInternalServerError
	st.ActiveID = ""
	st.RecoveryRequired = false
	if e := r.save(st); e != nil {
		return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
	}
	return r.result(rec)
}
func (r *Runner) uncertain(st *state.TargetState, rec *state.Record, key string, cause error) release.Result {
	st.RecoveryRequired = true
	st.ActiveID = rec.Request.ReleaseID
	rec.Result.Status = release.NodeStatusRecoveryRequired
	rec.Result.ErrorCode = key
	rec.Result.Error = cause.Error()
	rec.HTTPStatus = 503
	if rec.Intent {
		rec.Result.ActivationStatus = "unknown"
	}
	rec.Result.RollbackStatus = "unavailable"
	if key == "RECOVERY_FAILED" {
		rec.Result.RollbackStatus = "failed"
	}
	r.publish(st)
	// Even when persisting this failure fails, the last durable intent remains recoverable.
	if e := r.save(st); e != nil {
		r.block("state persistence failed: " + e.Error())
	}
	return r.result(rec)
}
func (r *Runner) commit(t config.Target, st *state.TargetState, rec *state.Record) release.Result {
	rec.Candidate.VerifiedAt = time.Now().UTC()
	st.Previous = rec.Baseline
	if rec.LegacyLink != "" {
		st.Previous = nil
	}
	st.Current = rec.Candidate
	st.Revision = release.ID()
	st.ActiveID = ""
	st.RecoveryRequired = false
	rec.Result.Status = release.NodeStatusSucceeded
	rec.Result.Phase = "complete"
	rec.Result.ActivationStatus = "reload_requested"
	rec.Result.RollbackStatus = "not_needed"
	rec.Result.ErrorCode = ""
	rec.Result.Error = ""
	rec.Result.StateRevisionAfter = st.Revision
	rec.Result.FinishedAt = time.Now().UTC()
	rec.HTTPStatus = 200
	if e := r.save(st); e != nil {
		return r.uncertain(st, rec, "STATE_PERSIST_FAILED", e)
	}
	prom.TargetState(t.Env, string(t.Type), t.ServerName, t.ID, false, rec.Candidate.VerifiedAt, rec.Candidate.CommitID)
	cleanup, cancel := context.WithTimeout(context.Background(), r.cfg.CleanupTimeout.Value())
	e := cleanupSnapshots(cleanup, r.cfg, t, st)
	cancel()
	if e != nil {
		prom.CleanupFailure(t.Env, string(t.Type), t.ID)
		applog.LogWarn("历史快照清理失败", "cleanup_warning", map[string]any{"release_id": rec.Request.ReleaseID, "target_id": t.ID, "error": e.Error()})
		rec.Result.Warnings = append(rec.Result.Warnings, "cleanup: "+e.Error())
		if e = r.save(st); e != nil {
			rec.Result.Warnings = append(rec.Result.Warnings, "cleanup warning not persisted: "+e.Error())
		}
	}
	return r.result(rec)
}
func (r *Runner) restore(t config.Target, st *state.TargetState, rec *state.Record, key string, cause error) release.Result {
	outcome := "failed"
	defer func() { prom.Rollback(t.Env, string(t.Type), t.ID, outcome) }()
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.RecoveryTimeout.Value())
	defer cancel()
	base, e := openTarget(t)
	if e != nil {
		return r.uncertain(st, rec, "RECOVERY_FAILED", e)
	}
	defer base.Close()
	e = r.restoreLocal(ctx, t, base, rec)
	if e != nil {
		return r.uncertain(st, rec, "RECOVERY_FAILED", fmt.Errorf("%v; recovery: %w", cause, e))
	}
	outcome = "succeeded"
	st.Current = rec.Baseline
	st.Previous = rec.BaselinePrevious
	st.Revision = release.ID()
	st.ActiveID = ""
	st.RecoveryRequired = false
	rec.Result.ActivationStatus = "restored"
	rec.Result.RollbackStatus = "succeeded"
	rec.Result.StateRevisionAfter = st.Revision
	rec.Result.Phase = "restored"
	return r.fail(st, rec, key, cause)
}
func (r *Runner) restoreLocal(ctx context.Context, t config.Target, base *os.Root, rec *state.Record) error {
	if rec.Baseline != nil {
		if _, e := verifySnapshot(ctx, base, rec.Baseline); e != nil {
			return e
		}
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := fsutil.Switch(base, rec.BeforeLink); e != nil {
		return e
	}
	if _, e := r.nginxTest(ctx); e != nil {
		return e
	}
	if _, e := r.nginxReload(ctx); e != nil {
		return e
	}
	return r.verify(ctx, t, base, rec.Baseline)
}
func (r *Runner) recoverStartup(ctx context.Context, t config.Target, st *state.TargetState) {
	rec := st.Records[st.ActiveID]
	if rec == nil {
		r.block("active release record missing")
		return
	}
	if !rec.Intent {
		if rec.Result.ErrorCode == "BASELINE_UNVERIFIED" {
			if err := r.checkStoredBaseline(ctx, t, st); err != nil {
				return
			}
			st.Revision = release.ID()
			rec.Result.StateRevisionAfter = st.Revision
		}
		st.RecoveryRequired = false
		r.fail(st, rec, "INTERRUPTED", errorMarker{})
		return
	}
	if rec.Result.Phase == "awaiting_nginx_test" || rec.Result.Phase == "awaiting_nginx_reload" {
		r.restore(t, st, rec, "INTERRUPTED", fmt.Errorf("service restarted before the pending nginx command"))
		return
	}
	base, e := openTarget(t)
	if e != nil {
		r.uncertain(st, rec, "RECOVERY_FAILED", e)
		return
	}
	defer base.Close()
	link, e := fsutil.Link(base)
	if e != nil {
		r.uncertain(st, rec, "RECOVERY_FAILED", e)
		return
	}
	if rec.Candidate != nil && link == rec.Candidate.Link {
		if _, e = verifySnapshot(ctx, base, rec.Candidate); e == nil {
			_, e = r.nginxTest(ctx)
		}
		if e == nil {
			_, e = r.nginxReload(ctx)
		}
		if e == nil {
			e = r.verify(ctx, t, base, rec.Candidate)
		}
		if e == nil {
			e = r.verifyRetainedAssets(ctx, t, base, st, rec)
		}
		if e == nil {
			r.commit(t, st, rec)
			return
		}
	} else if link != rec.BeforeLink && (rec.LegacyLink == "" || link != rec.LegacyLink) {
		r.uncertain(st, rec, "EXTERNAL_DRIFT", fmt.Errorf("latest no longer matches candidate or baseline"))
		return
	}
	cause := fmt.Errorf("interrupted activation; restoring local baseline")
	if e != nil {
		cause = fmt.Errorf("interrupted activation: %w", e)
	}
	r.restore(t, st, rec, "INTERRUPTED", cause)
}

type errorMarker struct{}

func (errorMarker) Error() string { return "service stopped before activation" }

// observeLiveCommit reads latest. A verified snapshot is returned for skip and
// rollback; a diverged or missing link never fails the publication.
func (r *Runner) observeLiveCommit(ctx context.Context, t config.Target, base *os.Root) (string, *state.Version) {
	raw, err := fsutil.Link(base)
	if err != nil || raw == "" {
		return "", nil
	}
	live, err := normalizeSnapshotLink(t, raw)
	if err != nil {
		return "", nil
	}
	adopted, err := versionFromLiveSnapshot(ctx, base, live)
	if err != nil {
		return live, nil
	}
	return live, adopted
}

func versionFromLiveSnapshot(ctx context.Context, base *os.Root, link string) (*state.Version, error) {
	commit := snapshotCommit(link)
	if !release.IsCommit(commit) {
		return nil, fmt.Errorf("latest is not a snapshot commit")
	}
	m, err := loadManifest(base, commit)
	if err != nil {
		return nil, err
	}
	v := &state.Version{CommitID: commit, Version: commit, Source: m.Source, ArtifactDigest: artifactDigest(m.Source), Link: link, ManifestDigest: manifestDigest(m)}
	if _, err = verifySnapshot(ctx, base, v); err != nil {
		return nil, err
	}
	return v, nil
}

func normalizeSnapshotLink(t config.Target, link string) (string, error) {
	if link == "" {
		return "", nil
	}
	if filepath.IsAbs(link) {
		var err error
		link, err = filepath.Rel(t.Dir, link)
		if err != nil {
			return "", err
		}
	}
	link = filepath.ToSlash(filepath.Clean(link))
	if release.IsCommit(link) {
		return link, nil
	}
	if strings.HasPrefix(link, "releases/") && release.IsCommit(strings.TrimPrefix(link, "releases/")) {
		return link, nil
	}
	return "", fmt.Errorf("unsafe snapshot link")
}

func snapshotCommit(link string) string {
	if release.IsCommit(link) {
		return link
	}
	return strings.TrimPrefix(link, "releases/")
}

func (r *Runner) checkStoredBaseline(ctx context.Context, t config.Target, st *state.TargetState) error {
	base, err := openTarget(t)
	if err != nil {
		return err
	}
	defer base.Close()
	_, live := r.observeLiveCommit(ctx, t, base)
	if live != nil {
		st.Current = live
	} else if st.Current != nil {
		if _, err = verifySnapshot(ctx, base, st.Current); err != nil {
			return err
		}
	}
	return r.verifyRetainedAssets(ctx, t, base, st, nil)
}

func (r *Runner) cleanupExport(t config.Target, work string) {
	root, e := os.OpenRoot(filepath.Dir(work))
	if e != nil {
		return
	}
	defer root.Close()
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.CleanupTimeout.Value())
	defer cancel()
	if e = fsutil.RemoveTree(ctx, root, filepath.Base(work)); e != nil {
		prom.CleanupFailure(t.Env, string(t.Type), t.ID)
		applog.LogWarn("导出临时目录清理未完成", "cleanup_warning", map[string]any{"target_id": t.ID, "error": e.Error()})
	}
}

func (r *Runner) source(req release.ApplyRequest, t config.Target) string {
	if req.Type == release.ReleaseTypeFrontendStatic {
		return t.ArtifactRepository + "@" + req.ArtifactDigest
	}
	return r.cfg.Repos[req.SourceRepo].URL
}
