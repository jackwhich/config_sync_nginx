package publisher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
)

// Run the production adapter with a temporary command in PATH. No real Nginx,
// master process, PID file, server config or signals are used by this test.
func TestStandardNginxCommandsAndRollback(t *testing.T) {
	for _, typ := range []release.ReleaseType{release.ReleaseTypeConfig, release.ReleaseTypeWhitelist, release.ReleaseTypeFrontendStatic} {
		t.Run(string(typ), func(t *testing.T) {
			f := newFixture(t)
			f.r.Close()
			f.r = nil
			f.cfg.Targets[0].Type = typ
			f.cfg.Targets[0].HealthChecks = nil
			f.cfg.Targets[0].InitialHealthChecks = nil
			if typ == release.ReleaseTypeWhitelist {
				f.cfg.Repos["whitelist"] = f.cfg.Repos["config"]
			}
			if typ == release.ReleaseTypeFrontendStatic {
				f.cfg.ORAS = config.ORAS{Binary: "/usr/local/bin/oras", RegistryConfig: "/etc/oras/auth.json"}
				f.cfg.Targets[0].ArtifactRepository = "harbor.example.com/web/site-dist"
				if err := os.MkdirAll(filepath.Join(f.repo, "site"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.repo, "site", "index.html"), []byte("<html>dist</html>"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			toolsDir := t.TempDir()
			calls := filepath.Join(toolsDir, "calls")
			links := filepath.Join(toolsDir, "links")
			fail := filepath.Join(toolsDir, "fail-once")
			t.Setenv("TEST_NGINX_CALLS", calls)
			t.Setenv("TEST_NGINX_LINKS", links)
			t.Setenv("TEST_NGINX_FAIL", fail)
			syntaxFail := filepath.Join(toolsDir, "syntax-fail-once")
			t.Setenv("TEST_NGINX_SYNTAX_FAIL", syntaxFail)
			t.Setenv("TEST_NGINX_LATEST", filepath.Join(f.cfg.Targets[0].Dir, "latest"))
			t.Setenv("PATH", toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			command := `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_NGINX_CALLS"
readlink "$TEST_NGINX_LATEST" >> "$TEST_NGINX_LINKS"
if [ "$#" -eq 1 ] && [ "$1" = '-t' ]; then
 if [ -f "$TEST_NGINX_SYNTAX_FAIL" ]; then
  /bin/rm "$TEST_NGINX_SYNTAX_FAIL"
  echo 'nginx: [emerg] unknown directive "bad_directive" in /etc/nginx/conf.d/site.conf:7' >&2
  exit 1
 fi
elif [ "$#" -eq 2 ] && [ "$1" = '-s' ] && [ "$2" = 'reload' ]; then
 if [ -f "$TEST_NGINX_FAIL" ]; then
  /bin/rm "$TEST_NGINX_FAIL"
  echo 'nginx: reload command failed' >&2
  exit 1
 fi
else
 exit 19
fi
`
			if err := os.WriteFile(filepath.Join(toolsDir, "nginx"), []byte(command), 0700); err != nil {
				t.Fatal(err)
			}
			// Any attempt to inspect or control host services must fail this test.
			for _, name := range []string{"ps", "systemctl", "service", "kill"} {
				if err := os.WriteFile(filepath.Join(toolsDir, name), []byte("#!/bin/sh\nprintf 'forbidden-host-control\\n' >> \"$TEST_NGINX_CALLS\"\nexit 21\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			f.r, err = New(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if typ == release.ReleaseTypeFrontendStatic {
				f.r.artifacts = fixtureArtifacts{f}
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("startup ran a host command", err)
			}
			request := func(commit string) release.ApplyRequest {
				req := f.request(commit)
				req.Type = typ
				if typ == release.ReleaseTypeFrontendStatic {
					req.ArtifactDigest = "sha256:" + release.Digest(commit)
				}
				return req
			}
			a := f.commit("A")
			req := request(a)
			got := f.r.Apply(context.Background(), req)
			if got.Status != release.NodeStatusSucceeded || got.ActivationStatus != "reload_requested" {
				t.Fatal(got)
			}
			if replay := f.r.Apply(context.Background(), req); !replay.Replayed {
				t.Fatal(replay)
			}
			b := f.commit("B")
			if err := os.WriteFile(syntaxFail, []byte("reject candidate syntax"), 0600); err != nil {
				t.Fatal(err)
			}
			invalidRequest := request(b)
			got = f.r.Apply(context.Background(), invalidRequest)
			if got.Status != release.NodeStatusFailed || got.HTTPStatus != 500 || got.ErrorCode != "NGINX_TEST_FAILED" || got.RollbackStatus != "succeeded" || !strings.Contains(got.Error, "bad_directive") || !strings.Contains(got.Error, "site.conf:7") {
				t.Fatal(got)
			}
			for _, step := range got.Steps {
				if step.Name == "reload" {
					t.Fatal("candidate reload must not run after nginx -t failure", got)
				}
			}
			if replay := f.r.Apply(context.Background(), invalidRequest); !replay.Replayed || replay.HTTPStatus != 500 || replay.Error != got.Error {
				t.Fatal(replay)
			}
			if err := os.WriteFile(fail, []byte("fail the candidate command"), 0600); err != nil {
				t.Fatal(err)
			}
			got = f.r.Apply(context.Background(), request(b))
			if got.Status != release.NodeStatusFailed || got.HTTPStatus != 500 || got.ErrorCode != "NGINX_RELOAD_FAILED" || got.RollbackStatus != "succeeded" {
				t.Fatal(got)
			}
			st, link := f.current()
			if st.Current.CommitID != a || link != f.cfg.Targets[0].SnapshotLink(a) {
				t.Fatal(st, link)
			}
			raw, err := os.ReadFile(calls)
			if err != nil || string(raw) != "-t\n-s reload\n-t\n-t\n-s reload\n-t\n-s reload\n-t\n-s reload\n" {
				t.Fatal(string(raw), err)
			}
			raw, err = os.ReadFile(links)
			aLink, bLink := f.cfg.Targets[0].SnapshotLink(a), f.cfg.Targets[0].SnapshotLink(b)
			want := strings.Join([]string{aLink, aLink, bLink, aLink, aLink, bLink, bLink, aLink, aLink}, "\n") + "\n"
			if err != nil || string(raw) != want {
				t.Fatal("test/reload order", string(raw), err)
			}
			// Restart validates stored files without executing or discovering Nginx.
			f.r.Close()
			f.r, err = New(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = os.ReadFile(calls)
			if strings.Count(string(raw), "\n") != 9 {
				t.Fatal("restart ran a host command", string(raw))
			}
		})
	}
}
