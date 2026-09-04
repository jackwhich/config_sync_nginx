package applog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	entry := make(map[string]any, len(fields)+4)
	for key, value := range fields {
		entry[key] = value
	}
	entry["time"], entry["level"], entry["message"], entry["event"] = time.Now().UTC().Format(time.RFC3339Nano), level, message, event
	content, err := json.Marshal(entry)
	if err != nil {
		content, _ = json.Marshal(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "level": "error", "message": "日志序列化失败", "event": "log_encoding_failed", "error": err.Error()})
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
