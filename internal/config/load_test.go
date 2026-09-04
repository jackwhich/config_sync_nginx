package config

import (
	"nginx_updata_config/internal/domain/release"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTypeOnlyConfigurationAndRequestPaths(t *testing.T) {
	root := t.TempDir()
	yaml := `listen_addr: ":9166"
hostname: node-uat
app: update-nginx-http
data_dir: ` + filepath.Join(root, "state") + `
release_auth_tokens:
  uat: secret-uat
  prod: secret-prod
repos:
  config:
    url: https://gitlab.example.com/ops/config.git
    gitlab_token: test-token
oras:
  binary: /usr/local/bin/oras
  registry_config: /etc/oras/auth.json
  repository: harbor.example.com/web/{server_name}-dist
targets:
  - config
  - frontend_static
`
	file := filepath.Join(root, "service.yaml")
	load := func(body string) (Config, error) {
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(file)
	}
	c, err := load(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeID != "node-uat" || c.Env != "" || c.LockFile != filepath.Join(c.DataDir, "publish.lock") {
		t.Fatalf("unexpected defaults: node=%s env=%s lock=%s", c.NodeID, c.Env, c.LockFile)
	}
	for _, env := range []string{"uat", "prod"} {
		target, err := c.TargetForEnv("frontend_static", "web", filepath.Join(root, "deploy"), "project", env)
		if err != nil {
			t.Fatal(err)
		}
		if !target.Dynamic || target.Env != env || target.ArtifactRepository != "harbor.example.com/web/web-dist" || target.Dir != filepath.Join(target.PathDest, "web") {
			t.Fatalf("bad target: %+v", target)
		}
	}
	for _, body := range []string{yaml + "---\nunknown: true\n", yaml + "nginx:\n  pid_file: /run/nginx.pid\n", strings.Replace(yaml, "  - config", "  - type: config\n    typo: true", 1), strings.Replace(yaml, "  - config", "  - type: config\n    health_checks:\n      - url: http://127.0.0.1\n        contains: ok\n        typo: true", 1), strings.Split(yaml, "targets:")[0] + "targets: []\n"} {
		if _, err := load(body); err == nil {
			t.Fatal("invalid YAML accepted")
		}
	}
	for _, args := range [][4]string{{"config", "../escape", filepath.Join(root, "deploy"), "uat"}, {"whitelist", "site", filepath.Join(root, "deploy"), "uat"}, {"config", "site", "relative", "uat"}, {"config", "site", c.DataDir, "uat"}, {"config", "site", filepath.Join(root, "deploy"), "other"}} {
		if _, err := c.TargetForEnv(release.ReleaseType(args[0]), args[1], args[2], "", args[3]); err == nil {
			t.Fatal("invalid target accepted", args)
		}
	}
}
