package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"context"
	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/auth"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/infrastructure/applog"
	"nginx_updata_config/internal/infrastructure/prom"
)

// Publisher is the HTTP adapter's application contract.
type Publisher interface {
	Apply(context.Context, release.ApplyRequest) release.Result
	Stage(context.Context, release.ApplyRequest) release.Result
	NginxTest(context.Context, release.NginxCommandRequest) release.Result
	NginxReload(context.Context, release.NginxCommandRequest) release.Result
	Abort(context.Context, release.NginxCommandRequest) release.Result
	Health() map[string]any
	Target(string) (config.Target, bool)
	Resolve(release.ReleaseType, string, string, string, ...string) (config.Target, error)
	State(string, string) (map[string]any, int, error)
}
type Server struct {
	runner Publisher
	cfg    config.Config
	mux    *http.ServeMux
	slots  chan struct{}
}

func New(r Publisher, cfg config.Config) *Server {
	s := &Server{runner: r, cfg: cfg, mux: http.NewServeMux(), slots: make(chan struct{}, cfg.MaxConcurrentRequests)}
	s.mux.HandleFunc("POST /api/v1/releases/apply", s.apply)
	s.mux.HandleFunc("POST /api/v1/releases/nginx/test", s.nginxTest)
	s.mux.HandleFunc("POST /api/v1/releases/nginx/reload", s.nginxReload)
	s.mux.HandleFunc("POST /api/v1/releases/abort", s.abort)
	s.mux.HandleFunc("GET /api/v1/releases/state", s.state)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.runner.Health()) })
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) { s.runner.Health(); prom.MetricsHandler().ServeHTTP(w, r) })
	return s
}
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		cw := &captureWriter{ResponseWriter: w}
		clientIP := effectiveClientIP(r, s.cfg.Access)
		requestID := release.ID()
		cw.Header().Set("X-Request-ID", requestID)
		handler := "unknown"
		switch r.URL.Path {
		case "/api/v1/releases/apply":
			handler = "apply"
		case "/api/v1/releases/nginx/test":
			handler = "nginx_test"
		case "/api/v1/releases/nginx/reload":
			handler = "nginx_reload"
		case "/api/v1/releases/abort":
			handler = "abort"
		case "/api/v1/releases/state":
			handler = "state"
		case "/healthz":
			handler = "healthz"
		case "/metrics":
			handler = "metrics"
		}
		defer func() {
			panicked := recover() != nil
			if panicked && cw.code == 0 {
				writeError(cw, 500, "INTERNAL_ERROR", "internal server error")
			}
			elapsed := time.Since(started)
			prom.RecordHTTP(handler, elapsed, cw.status())
			ip := ""
			if clientIP != nil {
				ip = clientIP.String()
			}
			peer, _ := hostOnlyFromRemoteAddr(r.RemoteAddr)
			fields := map[string]any{
				"request_id": requestID, "client_ip": ip, "peer_ip": peer,
				"method": r.Method, "path": r.URL.EscapedPath(), "status_code": cw.status(),
				"duration_ms": float64(elapsed.Microseconds()) / 1000, "bytes_written": cw.bytes,
				"node_id": s.cfg.NodeID, "env": s.cfg.Env,
			}
			if cw.releaseID != "" {
				fields["release_id"] = cw.releaseID
			}
			if cw.targetID != "" {
				fields["target_id"] = cw.targetID
			}
			if cw.errorCode != "" {
				fields["error_code"] = cw.errorCode
			}
			if cw.releaseStatus != "" {
				fields["release_status"] = cw.releaseStatus
			}
			if panicked {
				fields["panic"] = true
			}
			switch {
			case panicked || cw.status() >= 500:
				applog.LogError("HTTP 请求完成", "http_access", fields)
			case cw.status() >= 400:
				applog.LogWarn("HTTP 请求完成", "http_access", fields)
			default:
				applog.LogInfo("HTTP 请求完成", "http_access", fields)
			}
		}()
		if !clientIPAllowed(clientIP, s.cfg.Access) {
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
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, environments ...string) bool {
	// Authenticate before decoding attacker-controlled JSON or producing target metric labels.
	token := r.Header.Get(auth.ReleaseAuthHeader)
	env := ""
	if len(environments) > 0 {
		env = environments[0]
	}
	matched := 0
	for key, expected := range s.cfg.ReleaseAuthTokens {
		if !s.cfg.AcceptsEnv(key) || (env != "" && env != key) {
			continue
		}
		matched |= subtle.ConstantTimeCompare([]byte(token), []byte(strings.TrimSpace(expected)))
	}
	if token == "" || matched != 1 {
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
	if strings.TrimSpace(req.Env) == "" {
		writeError(w, 400, "INVALID_REQUEST", "env is required")
		return
	}
	if !s.cfg.AcceptsEnv(strings.TrimSpace(req.Env)) {
		writeError(w, 403, "ENV_NOT_ALLOWED", "environment not enabled")
		return
	}
	if !s.authorize(w, r, strings.TrimSpace(req.Env)) {
		return
	}
	result := s.runner.Stage(r.Context(), req)
	s.writeReleaseResult(w, result)
}

func (s *Server) nginxTest(w http.ResponseWriter, r *http.Request) {
	s.nginxCommand(w, r, s.runner.NginxTest)
}

func (s *Server) nginxReload(w http.ResponseWriter, r *http.Request) {
	s.nginxCommand(w, r, s.runner.NginxReload)
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	s.nginxCommand(w, r, s.runner.Abort)
}

func (s *Server) nginxCommand(w http.ResponseWriter, r *http.Request, run func(context.Context, release.NginxCommandRequest) release.Result) {
	if !s.authorize(w, r) {
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	var req release.NginxCommandRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
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
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if strings.TrimSpace(req.Env) == "" || !s.cfg.AcceptsEnv(strings.TrimSpace(req.Env)) {
		writeError(w, http.StatusForbidden, "ENV_NOT_ALLOWED", "environment does not match this node")
		return
	}
	if !s.authorize(w, r, strings.TrimSpace(req.Env)) {
		return
	}
	s.writeReleaseResult(w, run(r.Context(), req))
}

func (s *Server) writeReleaseResult(w http.ResponseWriter, result release.Result) {
	if cw, ok := w.(*captureWriter); ok {
		if release.IsID(result.ReleaseID) {
			cw.releaseID = result.ReleaseID
		}
		cw.targetID, cw.errorCode, cw.releaseStatus = result.TargetID, result.ErrorCode, string(result.Status)
	}
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
	if !s.cfg.AcceptsEnv(q.Get("env")) {
		writeError(w, 403, "ENV_NOT_ALLOWED", "environment does not match this node")
		return
	}
	if !s.authorize(w, r, q.Get("env")) {
		return
	}
	id := q.Get("target_id")
	releaseID := q.Get("release_id")
	if releaseID != "" && !release.IsID(releaseID) {
		writeError(w, 400, "INVALID_REQUEST", "release_id must be UUID")
		return
	}
	if id != "" {
		if !release.ValidTargetID(id) {
			writeError(w, 400, "INVALID_REQUEST", "invalid target_id")
			return
		}
		if q.Get("type") != "" || q.Get("server_name") != "" || q.Get("path_dest") != "" {
			writeError(w, 400, "INVALID_REQUEST", "use target_id or the full target tuple")
			return
		}
		t, ok := s.runner.Target(id)
		if !ok || t.Env != q.Get("env") || (t.Project != "" && q.Get("project") != "" && q.Get("project") != t.Project) {
			writeError(w, 403, "TARGET_NOT_ALLOWED", "target not authorized")
			return
		}
	} else {
		if q.Get("type") == "" || q.Get("server_name") == "" || q.Get("path_dest") == "" {
			writeError(w, 400, "INVALID_REQUEST", "type, server_name and path_dest required")
			return
		}
		t, e := s.runner.Resolve(release.ReleaseType(q.Get("type")), q.Get("server_name"), q.Get("path_dest"), q.Get("project"), q.Get("env"))
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
	if cw, ok := w.(*captureWriter); ok {
		cw.errorCode = key
	}
	writeJSON(w, code, map[string]string{"error_code": key, "error": message})
}

type captureWriter struct {
	http.ResponseWriter
	code                                          int
	bytes                                         int64
	releaseID, targetID, errorCode, releaseStatus string
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
	n, err := c.ResponseWriter.Write(b)
	c.bytes += int64(n)
	return n, err
}
func (c *captureWriter) status() int {
	if c.code == 0 {
		return 200
	}
	return c.code
}
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
