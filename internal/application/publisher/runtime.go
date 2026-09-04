package publisher

import (
	"context"
	"fmt"
	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/state"
	"nginx_updata_config/internal/infrastructure/nginx"
	"os"
	"strings"
	"time"
)

// Runtime tests and reloads via the existing nginx command and runs optional HTTP probes.
type Runtime interface {
	Test(context.Context) error
	Reload(context.Context) error
	Verify(context.Context, config.Target, string, bool) error
}

// Probe old assets as well: a route accidentally rooted at latest/assets must fail
// activation before it can strand browsers that loaded the previous index.
func (r *Runner) verifyRetainedAssets(ctx context.Context, t config.Target, base *os.Root, st *state.TargetState, current *state.Record) error {
	if t.Type != release.ReleaseTypeFrontendStatic || !t.SharedAssets {
		return nil
	}
	versions := map[string]*state.Version{}
	keep := func(v *state.Version) {
		if v != nil {
			versions[v.CommitID] = v
		}
	}
	keep(st.Current)
	keep(st.Previous)
	for _, rec := range st.Records {
		if !rec.Result.Terminal() || (rec.Intent && time.Since(rec.Result.FinishedAt) < r.cfg.AssetRetention.Value()) {
			keep(rec.Baseline)
			keep(rec.Candidate)
		}
	}
	for _, v := range versions {
		if current != nil && current.Candidate != nil && v.CommitID == current.Candidate.CommitID {
			continue
		}
		m, e := loadManifest(base, v.CommitID)
		if e != nil {
			return e
		}
		for name, digest := range m.Assets {
			h := config.HealthCheck{URL: strings.TrimRight(t.PublicBaseURL, "/") + "/" + name, Host: t.PublicHost, Status: 200}
			if e = nginx.Probe(ctx, h, v.CommitID, digest); e != nil {
				return fmt.Errorf("previous client resource unavailable: %w", e)
			}
		}
	}
	return nil
}
