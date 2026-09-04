package process

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCancellationTerminatesCommandGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, e := Run(ctx, "", nil, nil, "sh", "-c", "sleep 30 & wait")
	if e == nil {
		t.Fatal("command was not cancelled")
	}
	if time.Since(started) > 1500*time.Millisecond {
		t.Fatal("child process kept output pipes open")
	}
}
func TestOutputIsBoundedAndCredentialsAreRedacted(t *testing.T) {
	b := LimitedBuffer{Limit: 8}
	n, e := b.Write([]byte(strings.Repeat("x", 100)))
	if e != nil || n != 100 || b.Len() != 8 {
		t.Fatal("incorrect bounded writer")
	}
	out, e := Run(context.Background(), "", nil, []string{"test-secret"}, "sh", "-c", "printf 'test-secret'; exit 1")
	if e == nil || strings.Contains(out, "test-secret") || strings.Contains(e.Error(), "test-secret") {
		t.Fatal("credential leaked")
	}
}
