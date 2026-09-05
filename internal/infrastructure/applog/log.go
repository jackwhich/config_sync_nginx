package applog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	logMu     sync.Mutex
	logOut    io.Writer = os.Stdout
	logHandle *os.File
)

// Init selects stdout or an append-only JSON-lines file.
func Init(path string) error {
	path = strings.TrimSpace(path)
	logMu.Lock()
	defer logMu.Unlock()
	if logHandle != nil {
		_ = logHandle.Close()
		logHandle = nil
	}
	logOut = os.Stdout
	if path == "" {
		return nil
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("log_file 须为绝对路径，当前为 %q", path)
	}
	if err := os.MkdirAll(filepath.Dir(cleaned), 0755); err != nil {
		return fmt.Errorf("创建日志目录: %w", err)
	}
	f, err := os.OpenFile(cleaned, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开日志文件: %w", err)
	}
	logHandle, logOut = f, f
	return nil
}

func LogInfo(message, event string, fields map[string]any) { writeLog("info", message, event, fields) }
func LogWarn(message, event string, fields map[string]any) { writeLog("warn", message, event, fields) }
func LogError(message, event string, fields map[string]any) {
	writeLog("error", message, event, fields)
}

// Application fields are flattened for log search; they cannot replace the schema fields.
func writeLog(level, message, event string, fields map[string]any) {
	content, err := json.Marshal(entry{Time: time.Now().UTC().Format(time.RFC3339Nano), Level: level, Message: message, Event: event, Fields: fields})
	if err != nil {
		content, _ = json.Marshal(entry{Time: time.Now().UTC().Format(time.RFC3339Nano), Level: "error", Message: "日志序列化失败", Event: "log_encoding_failed", Fields: map[string]any{"error": err.Error()}})
	}
	logMu.Lock()
	defer logMu.Unlock()
	if _, err = fmt.Fprintln(logOut, string(content)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, string(content))
	}
}

// ErrorWriter routes net/http's internal errors through the same JSON encoder.
type ErrorWriter struct{}

func (ErrorWriter) Write(p []byte) (int, error) {
	LogError(strings.TrimSpace(string(p)), "http_server_error", nil)
	return len(p), nil
}

// entry preserves a human-friendly JSON Lines order while retaining flattened
// fields for log search. JSON object order is not semantically meaningful, but
// putting the timestamp first makes raw tail output easier to read.
type entry struct {
	Time    string
	Level   string
	Message string
	Event   string
	Fields  map[string]any
}

func (e entry) MarshalJSON() ([]byte, error) {
	values := []struct {
		key   string
		value any
	}{
		{"time", e.Time},
		{"level", e.Level},
		{"message", e.Message},
		{"event", e.Event},
	}
	reserved := map[string]bool{"time": true, "level": true, "message": true, "event": true}
	keys := make([]string, 0, len(e.Fields))
	for key := range e.Fields {
		if !reserved[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		values = append(values, struct {
			key   string
			value any
		}{key, e.Fields[key]})
	}

	var out bytes.Buffer
	out.WriteByte('{')
	for i, value := range values {
		if i > 0 {
			out.WriteByte(',')
		}
		key, err := json.Marshal(value.key)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(value.value)
		if err != nil {
			return nil, err
		}
		out.Write(key)
		out.WriteByte(':')
		out.Write(encoded)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
