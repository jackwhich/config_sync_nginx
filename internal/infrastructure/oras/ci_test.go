package oras

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCIPackagesBeforeLoginAndDoesNotPromoteProd(t *testing.T) {
	script, err := filepath.Abs("../../../scripts/frontend-artifact.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	_ = os.Mkdir(dist, 0755)
	fake := filepath.Join(root, "oras")
	log := filepath.Join(root, "calls")
	body := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$1" >> "$MOCK_ORAS_LOG"
case "$1" in
 login) cat >/dev/null ;;
 push)
  while [[ $# -gt 0 ]]; do
   if [[ $1 == --export-manifest ]]; then printf '%s' '{"artifact":"test"}' > "$2"; break; fi
   shift
  done ;;
 *) exit 9 ;;
esac
`
	if err := os.WriteFile(fake, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(out string) ([]byte, error) {
		cmd := exec.Command("bash", script, "push", dist, out)
		cmd.Env = append(os.Environ(), "ORAS_BIN="+fake, "MOCK_ORAS_LOG="+log, "HARBOR_REPOSITORY=harbor.example.com/web/app-dist", "RELEASE_COMMIT="+strings.Repeat("a", 40), "HARBOR_USERNAME=robot$ci", "HARBOR_PASSWORD=test-password", "HTTPS_PROXY=http://proxy.internal:8080")
		return cmd.CombinedOutput()
	}
	if _, err := run(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing index accepted")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("login ran before built index existed")
	}
	_ = os.WriteFile(filepath.Join(dist, "index.html"), []byte("hello"), 0644)
	out := filepath.Join(root, "artifact-bundle")
	output, err := run(out)
	if err != nil {
		t.Fatalf("%v %s", err, output)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "login\npush\n" {
		t.Fatalf("unexpected registry mutation %s", calls)
	}
	digest, err := os.ReadFile(filepath.Join(out, "artifact.digest"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(digest)) != sha([]byte(`{"artifact":"test"}`)) {
		t.Fatal("not digest of pushed manifest", string(digest))
	}
	if strings.Contains(string(output), "test-password") {
		t.Fatal("credential leaked")
	}
	if _, err := run(out); err == nil {
		t.Fatal("overwrote existing artifact record")
	}
}
