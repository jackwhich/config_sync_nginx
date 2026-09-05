package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONLineStartsWithTimeAndKeepsUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.jsonl")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Init("") })
	LogInfo("发布步骤完成", "release_step", map[string]any{"env": "uat", "z": "last"})

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(content)
	if !strings.HasPrefix(line, `{"time":`) {
		t.Fatalf("time is not first: %s", line)
	}
	if !strings.Contains(line, `"message":"发布步骤完成"`) {
		t.Fatalf("UTF-8 message was not preserved: %s", line)
	}
	if strings.Index(line, `"env":"uat"`) > strings.Index(line, `"z":"last"`) {
		t.Fatalf("fields are not deterministic: %s", line)
	}
}
