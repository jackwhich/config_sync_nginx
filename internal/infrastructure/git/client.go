package git

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/infrastructure/archive"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"nginx_updata_config/internal/infrastructure/process"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Client struct {
	DataDir  string
	Repos    map[string]config.Repo
	MaxBytes int64
	MaxFiles int
}

func (c Client) Checkout(ctx context.Context, key, branch, commit, site string) (string, string, error) {
	repo, ok := c.Repos[key]
	if !ok || (branch != "" && !branchAllowed(repo.AllowedBranches, branch)) {
		return "", "", fmt.Errorf("repository or branch not allowed")
	}
	if !release.IsCommit(commit) {
		return "", "", fmt.Errorf("invalid commit ID")
	}
	if err := release.ValidateRepoSiteDirectoryName(site); err != nil {
		return "", "", err
	}
	data, err := os.OpenRoot(c.DataDir)
	if err != nil {
		return "", "", err
	}
	defer data.Close()
	repoRel := filepath.Join("repos", release.Digest([]string{key, repo.URL})+".git")
	if err = fsutil.EnsureDirs(data, repoRel, 0700); err != nil {
		return "", "", err
	}
	repoDir := filepath.Join(c.DataDir, repoRel)
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	secrets := []string{repo.GitLabToken}
	if repo.GitLabToken != "" {
		credential := base64.StdEncoding.EncodeToString([]byte("oauth2:" + repo.GitLabToken))
		secrets = append(secrets, credential)
		env = append(env, "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+credential, "GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false")
	}
	run := func(args ...string) (string, error) { return process.Run(ctx, "", env, secrets, "git", args...) }
	ref := "refs/heads/" + branch
	if branch != "" {
		if _, err = run("check-ref-format", ref); err != nil {
			return "", "", err
		}
	}
	if _, err = data.Stat(filepath.Join(repoRel, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, err = run("init", "--bare", repoDir); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	refspec := "+" + ref + ":" + ref
	if branch == "" {
		refspec = "+refs/heads/*:refs/heads/*"
	}
	if _, err = run("--git-dir="+repoDir, "fetch", "--prune", "--force", "--no-tags", repo.URL, refspec); err != nil {
		return "", "", err
	}
	kind, err := run("--git-dir="+repoDir, "cat-file", "-t", commit)
	if err != nil || strings.TrimSpace(kind) != "commit" {
		return "", "", fmt.Errorf("requested object is not a commit: %s", commit)
	}
	actual, err := run("--git-dir="+repoDir, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return "", "", err
	}
	actual = strings.TrimSpace(actual)
	if actual != commit {
		return "", "", fmt.Errorf("commit did not resolve to exact ID")
	}
	refs := []string{branch}
	if branch == "" {
		out, e := run("--git-dir="+repoDir, "for-each-ref", "--format=%(refname:strip=2)", "refs/heads/")
		if e != nil {
			return "", "", e
		}
		refs = strings.Fields(out)
	}
	reachable := false
	for _, name := range refs {
		if !branchAllowed(repo.AllowedBranches, name) {
			continue
		}
		if _, e := run("--git-dir="+repoDir, "merge-base", "--is-ancestor", actual, "refs/heads/"+name); e == nil {
			reachable = true
			break
		}
	}
	if !reachable {
		return "", "", fmt.Errorf("commit not reachable from an allowed repository branch")
	}
	rel := filepath.Join("worktrees", "export-"+release.ID())
	if err = fsutil.EnsureDirs(data, rel, 0700); err != nil {
		return "", "", err
	}
	dest := filepath.Join(c.DataDir, rel)
	if err = c.extract(ctx, repoDir, actual, site, dest); err != nil {
		_ = data.RemoveAll(rel)
		return "", "", err
	}
	return dest, actual, nil
}
func (c Client) extract(ctx context.Context, repoDir, commit, site, dest string) error {
	cmd := process.Command(ctx, "git", "--git-dir="+repoDir, "archive", "--format=tar", commit, site)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &process.LimitedBuffer{Limit: 65536}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		_ = stdout.Close()
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return err
	}
	extractErr := Extract(ctx, stdout, root, c.MaxBytes, c.MaxFiles)
	root.Close()
	if extractErr != nil {
		_ = stdout.Close()
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("git archive: %w: %s", waitErr, stderr.String())
	}
	return nil
}
func Extract(ctx context.Context, input io.Reader, dest *os.Root, maxBytes int64, maxFiles int) error {
	return archive.Extract(ctx, input, dest, maxBytes, maxFiles)
}
func branchAllowed(allowed []string, branch string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == branch {
			return true
		}
		if ok, _ := path.Match(a, branch); ok {
			return true
		}
	}
	return false
}
