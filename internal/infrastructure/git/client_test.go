package git

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
)

func TestExtractRejectsUnsafeEntriesAndHonorsLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header tar.Header
		limit  int64
	}{
		{"traversal", tar.Header{Name: "../escape", Mode: 0644, Size: 1, Typeflag: tar.TypeReg}, 100},
		{"absolute", tar.Header{Name: "/escape", Mode: 0644, Size: 1, Typeflag: tar.TypeReg}, 100},
		{"symlink", tar.Header{Name: "site/link", Linkname: "../../escape", Typeflag: tar.TypeSymlink}, 100},
		{"hardlink", tar.Header{Name: "site/link", Linkname: "elsewhere", Typeflag: tar.TypeLink}, 100},
		{"oversized", tar.Header{Name: "site/file", Mode: 0644, Size: 2, Typeflag: tar.TypeReg}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			if e := tw.WriteHeader(&tc.header); e != nil {
				t.Fatal(e)
			}
			if tc.header.Size > 0 {
				_, _ = tw.Write(bytes.Repeat([]byte{'x'}, int(tc.header.Size)))
			}
			tw.Close()
			root, e := os.OpenRoot(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			defer root.Close()
			if e = Extract(context.Background(), &b, root, tc.limit, 10); e == nil {
				t.Fatal("unsafe entry accepted")
			}
		})
	}
}
func TestRealGitArchivePAXAndRejectedPipeDoesNotHang(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	run("init", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	if e := os.Mkdir(filepath.Join(repo, "site"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(repo, "site", "config"), []byte("valid"), 0644); e != nil {
		t.Fatal(e)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	commit := run("rev-parse", "HEAD")
	// Make the local upload-pack advertise partial-clone filtering. This lets
	// Checkout prove that archive can retrieve required blobs on demand.
	run("config", "uploadpack.allowFilter", "true")
	data := t.TempDir()
	c := Client{DataDir: data, Repos: map[string]config.Repo{"config": {URL: "file://" + repo, AllowLocal: true, AllowedBranches: []string{"main"}}}, MaxBytes: 10 << 20, MaxFiles: 100}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	work, actual, e := c.Checkout(ctx, "config", "main", commit, "site")
	if e != nil {
		t.Fatal("valid Git PAX archive failed", e)
	}
	defer os.RemoveAll(work)
	if actual != commit {
		t.Fatal("commit not pinned")
	}
	if _, e = os.Stat(filepath.Join(work, "site", "config")); e != nil {
		t.Fatal(e)
	}
	repoDir := filepath.Join(data, "repos", release.Digest([]string{"config", "file://" + repo})+".git")
	if got := runGit(t, "--git-dir="+repoDir, "config", "--get", "remote.origin.promisor"); got != "true" {
		t.Fatalf("partial clone promisor = %q, want true", got)
	}
	if got := runGit(t, "--git-dir="+repoDir, "config", "--get", "remote.origin.partialclonefilter"); got != "blob:none" {
		t.Fatalf("partial clone filter = %q, want blob:none", got)
	}
	if _, _, e = c.Checkout(ctx, "config", "other", commit, "site"); e == nil {
		t.Fatal("unlisted branch accepted")
	}
	// The rejected first symlink precedes a large stream that would fill the pipe.
	if e = os.Symlink("/etc/passwd", filepath.Join(repo, "site", "aaa")); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(repo, "site", "zzz"), bytes.Repeat([]byte{'x'}, 2<<20), 0644); e != nil {
		t.Fatal(e)
	}
	run("add", ".")
	run("commit", "-m", "unsafe")
	bad := run("rev-parse", "HEAD")
	started := time.Now()
	_, _, e = c.Checkout(ctx, "config", "main", bad, "site")
	if e == nil {
		t.Fatal("symlink accepted")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("archive pipe stalled instead of closing/killing producer")
	}
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
