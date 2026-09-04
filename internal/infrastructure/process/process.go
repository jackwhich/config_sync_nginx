package process

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type LimitedBuffer struct {
	bytes.Buffer
	Limit int
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.Limit - b.Len()
	if left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
func Run(ctx context.Context, dir string, env []string, secrets []string, name string, args ...string) (string, error) {
	cmd := Command(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out := &LimitedBuffer{Limit: 1 << 20}
	cmd.Stdout = out
	cmd.Stderr = out
	e := cmd.Run()
	s := out.String()
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	if e != nil {
		return s, fmt.Errorf("%s failed: %w: %s", filepathBase(name), e, strings.TrimSpace(s))
	}
	return s, nil
}
func filepathBase(s string) string { i := strings.LastIndexByte(s, '/'); return s[i+1:] }
