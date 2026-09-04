package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"nginx_updata_config/internal/domain/auth"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/service/config"
	"nginx_updata_config/internal/service/prom"
	"nginx_updata_config/internal/service/runner"
	"nginx_updata_config/internal/service/state"
)

type Server struct {
	runner *runner.Runner
	cfg    config.Config
	mux    *http.ServeMux
	slots  chan struct{}
}

func New(r *runner.Runner, cfg config.Config) *Server {
	s := &Server{runner: r, cfg: cfg, mux: http.NewServeMux(), slots: make(chan struct{}, cfg.MaxConcurrentRequests)}
	s.mux.HandleFunc("POST /api/v1/releases/apply", s.apply)
	s.mux.HandleFunc("GET /api/v1/releases/state", s.state)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.runner.Health()) })
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) { s.runner.Health(); prom.MetricsHandler().ServeHTTP(w, r) })
	return s
}
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		cw := &captureWriter{ResponseWriter: w}
		handler := "unknown"
		switch r.URL.Path {
		case "/api/v1/releases/apply":
			handler = "apply"
		case "/api/v1/releases/state":
			handler = "state"
		case "/healthz":
			handler = "healthz"
		case "/metrics":
			handler = "metrics"
		}
		defer func() { prom.RecordHTTP(handler, time.Since(started), cw.status()) }()
		if !clientIPAllowed(effectiveClientIP(r, s.cfg.Access), s.cfg.Access) {
			writeError(cw, 403, "IP_NOT_ALLOWED", "client IP not allowed")
			return
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			writeError(cw, 503, "REQUEST_LIMIT", "too many HTTP requests")
			return
		}
		s.mux.ServeHTTP(cw, r)
	})
}
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	// Authenticate before decoding attacker-controlled JSON or producing target metric labels.
	expected := strings.TrimSpace(s.cfg.ReleaseAuthTokens[s.cfg.Env])
	token := r.Header.Get(auth.ReleaseAuthHeader)
	if expected == "" || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		writeError(w, 401, "UNAUTHORIZED", "invalid release token")
		return false
	}
	return true
}
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req release.ApplyRequest
	err := dec.Decode(&req)
	if err == nil {
		var extra any
		if next := dec.Decode(&extra); next != io.EOF {
			if next == nil {
				err = errors.New("only one JSON object is allowed")
			} else {
				err = next
			}
		}
	}
	if err != nil {
		code := 400
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			code = 413
		}
		writeError(w, code, "INVALID_JSON", err.Error())
		return
	}
	result := s.runner.Apply(r.Context(), req)
	code := result.HTTPStatus
	if code == 0 {
		code = 500
	}
	writeJSON(w, code, result)
}
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	q := r.URL.Query()
	if q.Get("env") != s.cfg.Env {
		writeError(w, 403, "ENV_NOT_ALLOWED", "environment does not match this node")
		return
	}
	id := q.Get("target_id")
	releaseID := q.Get("release_id")
	if releaseID != "" && !release.IsID(releaseID) {
		writeError(w, 400, "INVALID_REQUEST", "release_id must be UUID")
		return
	}
	if id != "" {
		if !state.ValidTargetID(id) {
			writeError(w, 400, "INVALID_REQUEST", "invalid target_id")
			return
		}
		if q.Get("type") != "" || q.Get("server_name") != "" || q.Get("path_dest") != "" {
			writeError(w, 400, "INVALID_REQUEST", "use target_id or the full target tuple")
			return
		}
		t, ok := s.runner.Target(id)
		if !ok || (t.Project != "" && q.Get("project") != "" && q.Get("project") != t.Project) {
			writeError(w, 403, "TARGET_NOT_ALLOWED", "target not authorized")
			return
		}
	} else {
		if q.Get("type") == "" || q.Get("server_name") == "" || q.Get("path_dest") == "" {
			writeError(w, 400, "INVALID_REQUEST", "type, server_name and path_dest required")
			return
		}
		t, e := s.runner.Resolve(release.ReleaseType(q.Get("type")), q.Get("server_name"), q.Get("path_dest"), q.Get("project"))
		if e != nil {
			writeError(w, 403, "TARGET_NOT_ALLOWED", e.Error())
			return
		}
		id = t.ID
	}
	result, code, err := s.runner.State(id, releaseID)
	if err != nil {
		writeError(w, code, "STATE_QUERY_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, result)
}
func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, code int, key, message string) {
	writeJSON(w, code, map[string]string{"error_code": key, "error": message})
}

type captureWriter struct {
	http.ResponseWriter
	code int
}

func (c *captureWriter) WriteHeader(code int) {
	if c.code != 0 {
		return
	}
	c.code = code
	c.ResponseWriter.WriteHeader(code)
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.WriteHeader(200)
	}
	return c.ResponseWriter.Write(b)
}
func (c *captureWriter) status() int {
	if c.code == 0 {
		return 200
	}
	return c.code
}
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
