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

type LogEntry struct {
	Ts     string         `json:"ts"`
	Level  string         `json:"level"`
	Msg    string         `json:"msg"`
	Event  string         `json:"event,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

var (
	logMu     sync.Mutex
	logOut    io.Writer = os.Stdout
	logHandle *os.File
)

// Init 设置日志写出位置。path 为空或未写时等价于标准输出；非空则须为绝对路径，按行追加写入（自动建目录）。
func Init(path string) error {
	path = strings.TrimSpace(path)
	logMu.Lock()
	defer logMu.Unlock()
	if logHandle != nil {
		_ = logHandle.Close()
		logHandle = nil
	}
	if path == "" {
		logOut = os.Stdout
		return nil
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		logOut = os.Stdout
		return fmt.Errorf("log_file 须为绝对路径，当前为 %q", path)
	}
	if err := os.MkdirAll(filepath.Dir(cleaned), 0o755); err != nil {
		logOut = os.Stdout
		return fmt.Errorf("创建日志目录 %q: %w", filepath.Dir(cleaned), err)
	}
	f, err := os.OpenFile(cleaned, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logOut = os.Stdout
		return fmt.Errorf("打开日志文件 %q: %w", cleaned, err)
	}
	logHandle = f
	logOut = f
	return nil
}

func LogInfo(msg, event string, fields map[string]any) {
	writeLog("info", msg, event, fields)
}

func LogError(msg, event string, fields map[string]any) {
	writeLog("error", msg, event, fields)
}

func writeLog(level, msg, event string, fields map[string]any) {
	entry := LogEntry{
		Ts:     time.Now().Format(time.RFC3339Nano),
		Level:  level,
		Msg:    msg,
		Event:  event,
		Fields: fields,
	}
	content, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ts":"%s","level":"error","msg":"日志序列化失败","event":"序列化失败","fields":{"error":%q}}`+"\n", time.Now().Format(time.RFC3339Nano), err.Error())
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	_, _ = fmt.Fprintln(logOut, string(content))
}
