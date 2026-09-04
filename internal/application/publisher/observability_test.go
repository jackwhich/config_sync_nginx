package publisher

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"nginx_updata_config/internal/domain/release"
)

func metricValue(t *testing.T, suffix, target string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "nginx_updata_config_"+suffix {
			continue
		}
		for _, metric := range family.Metric {
			actual := map[string]string{}
			for _, label := range metric.Label {
				actual[label.GetName()] = label.GetValue()
			}
			if actual["target_id"] != target {
				continue
			}
			matches := true
			for k, v := range labels {
				if actual[k] != v {
					matches = false
				}
			}
			if !matches {
				continue
			}
			if metric.Counter != nil {
				return metric.Counter.GetValue()
			}
			return metric.Gauge.GetValue()
		}
	}
	t.Fatalf("missing metric %s for %s", suffix, target)
	return 0
}

func TestPublicationFailureMetricsAndReplay(t *testing.T) {
	f := newFixture(t)
	target := f.cfg.Targets[0].ID
	check := func(suffix string, labels map[string]string, want float64) {
		t.Helper()
		if got := metricValue(t, suffix, target, labels); got != want {
			t.Fatalf("%s = %v, want %v", suffix, got, want)
		}
	}
	check("release_terminal_total", map[string]string{"status": "failed"}, 0)
	a := f.commit("A")
	f.apply(a)
	b := f.commit("B")
	req := f.request(b)
	calls := 0
	f.rt.test = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("nginx -t failed")
		}
		return nil
	}
	result := f.r.Apply(context.Background(), req)
	if result.Status != release.NodeStatusFailed || result.RollbackStatus != "succeeded" {
		t.Fatal(result)
	}
	replay := f.r.Apply(context.Background(), req)
	if !replay.Replayed {
		t.Fatal(replay)
	}
	check("release_terminal_total", map[string]string{"status": "failed"}, 1)
	check("release_step_failures_total", map[string]string{"step": "nginx_test"}, 1)
	check("rollback_total", map[string]string{"status": "succeeded"}, 1)
	check("release_in_progress", nil, 0)
	check("release_started_timestamp_seconds", nil, 0)
	f.rt.test = func(context.Context) error { return errors.New("nginx unavailable") }
	result = f.r.Apply(context.Background(), f.request(b))
	if result.Status != release.NodeStatusRecoveryRequired {
		t.Fatal(result)
	}
	f.r.Health()
	check("rollback_total", map[string]string{"status": "failed"}, 1)
	check("target_recovery_required", nil, 1)
}
