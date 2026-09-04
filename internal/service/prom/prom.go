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
}

var ready = promauto.NewGaugeVec(prometheus.GaugeOpts{Namespace: "nginx_updata_config", Name: "publish_ready", Help: "Node ready to accept publications (1 or 0)."}, []string{"env", "node_id"})

func Ready(env, node string, value bool) {
	v := 0.0
	if value {
		v = 1
	}
	ready.WithLabelValues(env, node).Set(v)
}
