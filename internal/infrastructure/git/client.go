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
	// Keep a promisor remote so a filtered fetch transfers commits and trees
	// first, then git archive retrieves only blobs required by the requested
	// site directory. The release request remains pinned and reachability is
	// still checked against the named branch below.
	if err = ensurePartialRemote(run, repoDir, repo.URL); err != nil {
		return "", "", err
	}
	refspec := "+" + ref + ":" + ref
	if branch == "" {
		refspec = "+refs/heads/*:refs/heads/*"
	}
	if err = fetchBranch(run, repoDir, refspec); err != nil {
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
	if err = c.extract(ctx, repoDir, actual, site, dest, env, secrets); err != nil {
		_ = data.RemoveAll(rel)
		return "", "", err
	}
	return dest, actual, nil
}

func fetchBranch(run func(...string) (string, error), repoDir, refspec string) error {
	base := []string{"--git-dir=" + repoDir, "fetch", "--prune", "--force", "--no-tags", "origin", refspec}
	filtered := append([]string{base[0], base[1], "--filter=blob:none"}, base[2:]...)
	output, err := run(filtered...)
	if err == nil {
		return nil
	}
	// Some release nodes still use a Git client from before partial clone.
	// Retry only that known incompatibility; network and authentication errors
	// must remain visible to the caller.
	if !filterUnsupported(output) {
		return err
	}
	_, err = run(base...)
	return err
}

func filterUnsupported(output string) bool {
	return strings.Contains(output, "unknown option") && strings.Contains(output, "filter=blob:none")
}

func ensurePartialRemote(run func(...string) (string, error), repoDir, repositoryURL string) error {
	const remote = "origin"
	// `git remote get-url` is not available in older Git clients. Reading the
	// remote URL from config works on those versions as well.
	current, err := run("--git-dir="+repoDir, "config", "--get", "remote."+remote+".url")
	if err != nil {
		remotes, listErr := run("--git-dir="+repoDir, "remote")
		if listErr != nil {
			return listErr
		}
		if remoteExists(remotes, remote) {
			return fmt.Errorf("read %s remote URL: %w", remote, err)
		}
		if _, err = run("--git-dir="+repoDir, "remote", "add", remote, repositoryURL); err != nil {
			return err
		}
	} else if strings.TrimSpace(current) != repositoryURL {
		if _, err = run("--git-dir="+repoDir, "remote", "set-url", remote, repositoryURL); err != nil {
			return err
		}
	}
	if _, err = run("--git-dir="+repoDir, "config", "remote."+remote+".promisor", "true"); err != nil {
		return err
	}
	_, err = run("--git-dir="+repoDir, "config", "remote."+remote+".partialclonefilter", "blob:none")
	return err
}

func remoteExists(remotes, name string) bool {
	for _, remote := range strings.Fields(remotes) {
		if remote == name {
			return true
		}
	}
	return false
}

func (c Client) extract(ctx context.Context, repoDir, commit, site, dest string, env, secrets []string) error {
	cmd := process.Command(ctx, "git", "--git-dir="+repoDir, "archive", "--format=tar", commit, site)
	cmd.Env = append(os.Environ(), env...)
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
		message := stderr.String()
		for _, secret := range secrets {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		return fmt.Errorf("git archive: %w: %s", waitErr, message)
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
