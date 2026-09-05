package prom

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"strconv"
	"time"
)

var requests = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "nginx_updata_config", Name: "http_requests_total", Help: "HTTP responses by bounded handler and code."}, []string{"handler", "code"})
var duration = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "nginx_updata_config", Name: "http_request_duration_seconds", Help: "HTTP request duration."}, []string{"handler"})
var terminal = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "nginx_updata_config", Name: "release_terminal_total", Help: "Accepted release terminal outcomes."}, []string{"env", "release_type", "target_id", "status"})
var steps = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "nginx_updata_config", Name: "release_step_duration_seconds", Help: "Accepted release phase durations.", Buckets: []float64{.01, .1, 1, 5, 15, 30, 60, 120, 300}}, []string{"env", "release_type", "target_id", "step", "status"})

func MetricsHandler() http.Handler { return promhttp.Handler() }
func RecordHTTP(h string, t time.Duration, code int) {
	requests.WithLabelValues(h, strconv.Itoa(code)).Inc()
	duration.WithLabelValues(h).Observe(t.Seconds())
}
func Terminal(env, typ, target, status string) {
	terminal.WithLabelValues(env, typ, target, status).Inc()
}
func Step(env, typ, target, step, status string, t time.Duration) {
	steps.WithLabelValues(env, typ, target, step, status).Observe(t.Seconds())
	if status == "failed" {
		stepFailures.WithLabelValues(env, typ, target, step).Inc()
	}
}

var ready = promauto.NewGaugeVec(prometheus.GaugeOpts{Namespace: "nginx_updata_config", Name: "publish_ready", Help: "HTTP service health, independent of release steps (1 or 0)."}, []string{"env", "node_id"})

func Ready(env, node string, value bool) {
	v := 0.0
	if value {
		v = 1
	}
	ready.WithLabelValues(env, node).Set(v)
}

var stepFailures = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "nginx_updata_config", Name: "release_step_failures_total", Help: "Failed accepted publication steps."}, []string{"env", "release_type", "target_id", "step"})
var rollback = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "nginx_updata_config", Name: "rollback_total", Help: "Automatic local rollback attempts by verified outcome."}, []string{"env", "release_type", "target_id", "status"})
var cleanupFailures = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "nginx_updata_config", Name: "cleanup_failures_total", Help: "Snapshot or export cleanup failures."}, []string{"env", "release_type", "target_id"})
var persistFailures = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "nginx_updata_config", Name: "state_persist_failures_total", Help: "Failed authoritative state writes."}, []string{"env", "release_type", "target_id"})
var recovery = promauto.NewGaugeVec(prometheus.GaugeOpts{Namespace: "nginx_updata_config", Name: "target_recovery_required", Help: "Persisted target recovery flag (1 or 0)."}, []string{"env", "release_type", "target_id"})
var active = promauto.NewGaugeVec(prometheus.GaugeOpts{Namespace: "nginx_updata_config", Name: "release_in_progress", Help: "Active publication transaction (1 or 0)."}, []string{"env", "release_type", "target_id"})
var started = promauto.NewGaugeVec(prometheus.GaugeOpts{Namespace: "nginx_updata_config", Name: "release_started_timestamp_seconds", Help: "Start time of the active publication; zero when idle."}, []string{"env", "release_type", "target_id"})
var lastSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{Namespace: "nginx_updata_config", Name: "last_success_timestamp_seconds", Help: "Verification time of current persisted version; zero if never deployed."}, []string{"env", "release_type", "target_id"})

var phaseNames = []string{"verify_baseline", "fetch", "oras_pull", "prepare_snapshot", "verify_candidate", "switch", "nginx_test", "reload", "verify_activation"}

// Initialize finite configured series at zero before the first scrape/publication.
func InitTarget(env, typ, target string) {
	for _, status := range []string{"succeeded", "skipped", "failed"} {
		terminal.WithLabelValues(env, typ, target, status).Add(0)
	}
	for _, status := range []string{"succeeded", "failed"} {
		rollback.WithLabelValues(env, typ, target, status).Add(0)
	}
	for _, name := range phaseNames {
		stepFailures.WithLabelValues(env, typ, target, name).Add(0)
	}
	cleanupFailures.WithLabelValues(env, typ, target).Add(0)
	persistFailures.WithLabelValues(env, typ, target).Add(0)
	Active(env, typ, target, false)
	TargetState(env, typ, target, false, time.Time{})
}
func Rollback(env, typ, target, status string) {
	rollback.WithLabelValues(env, typ, target, status).Inc()
}
func CleanupFailure(env, typ, target string) { cleanupFailures.WithLabelValues(env, typ, target).Inc() }
func PersistFailure(env, typ, target string) { persistFailures.WithLabelValues(env, typ, target).Inc() }
func Active(env, typ, target string, value bool) {
	v, timestamp := 0.0, 0.0
	if value {
		v, timestamp = 1, float64(time.Now().UnixNano())/1e9
	}
	active.WithLabelValues(env, typ, target).Set(v)
	started.WithLabelValues(env, typ, target).Set(timestamp)
}
func TargetState(env, typ, target string, needsRecovery bool, verifiedAt time.Time) {
	v, timestamp := 0.0, 0.0
	if needsRecovery {
		v = 1
	}
	if !verifiedAt.IsZero() {
		timestamp = float64(verifiedAt.UnixNano()) / 1e9
	}
	recovery.WithLabelValues(env, typ, target).Set(v)
	lastSuccess.WithLabelValues(env, typ, target).Set(timestamp)
}
