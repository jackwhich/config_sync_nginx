package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/infrastructure/applog"
)

func TestAccessLogJSONStatusAndTrustedIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.jsonl")
	if err := applog.Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applog.Init("") })
	access, err := config.ParseIPAccessControl([]string{"192.0.2.1"}, []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, config.Config{Env: "test", NodeID: "node", MaxConcurrentRequests: 1, Access: access})
	s.mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); _, _ = w.Write([]byte("ok")) })
	s.mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) { panic("secret-panic-value") })
	for _, tc := range []struct {
		path, remote, xff, level string
		status                   int
	}{
		{"/ok?token=secret-query", "10.0.0.1:123", "192.0.2.1, 10.0.0.2", "info", 201},
		{"/ok", "10.0.0.1:123", "192.0.2.1, 203.0.113.7", "warn", 403},
		{"/missing", "192.0.2.1:123", "spoofed", "warn", 404},
		{"/panic", "192.0.2.1:123", "", "error", 500},
		{"/api/v1/releases/state", "192.0.2.1:123", "", "warn", 401},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-For", tc.xff)
		req.Header.Set("X-Release-Token", "secret-header")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		var entry map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
			t.Fatal(err)
		}
		if _, err := time.Parse(time.RFC3339Nano, entry["time"].(string)); err != nil {
			t.Fatal(err)
		}
		if entry["level"] != tc.level || entry["status_code"] != float64(tc.status) || entry["request_id"] != rec.Header().Get("X-Request-ID") || entry["message"] != "HTTP 请求完成" {
			t.Fatal(entry)
		}
		wantIP := "192.0.2.1"
		if tc.status == 403 {
			wantIP = "203.0.113.7"
		}
		if entry["client_ip"] != wantIP || entry["duration_ms"] == nil {
			t.Fatal(entry)
		}
		for _, secret := range []string{"secret-query", "secret-header", "secret-panic-value"} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("log exposed %s", secret)
			}
		}
	}
}
