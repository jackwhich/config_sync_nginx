package nginx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandsReportMissingExecutableAndNonzeroExit(t *testing.T) {
	for _, command := range []struct {
		name string
		run  func(context.Context) error
	}{{"nginx -t", (&Runtime{}).Test}, {"nginx -s reload", (&Runtime{}).Reload}} {
		t.Run(command.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PATH", root)
			if err := command.run(context.Background()); err == nil {
				t.Fatal("missing nginx executable accepted")
			}
			if err := os.WriteFile(filepath.Join(root, "nginx"), []byte("#!/bin/sh\necho 'command diagnostic' >&2\nexit 7\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := command.run(context.Background()); err == nil || !strings.Contains(err.Error(), "command diagnostic") || !strings.Contains(err.Error(), command.name) {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			<-ctx.Done()
			if err := command.run(ctx); err == nil {
				t.Fatal("expired command succeeded")
			}
		})
	}
}
