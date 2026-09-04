package config

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/target"
	"nginx_updata_config/internal/infrastructure/fsutil"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, e := time.ParseDuration(n.Value)
	*d = Duration(v)
	return e
}
func (d Duration) Value() time.Duration { return time.Duration(d) }

type Repo struct {
	URL             string   `yaml:"url"`
	AllowedBranches []string `yaml:"allowed_branches"`
	GitLabToken     string   `yaml:"gitlab_token"`
	AllowLocal      bool     `yaml:"allow_local"`
}
type HealthCheck = target.HealthCheck

type Target = target.Target

type ORAS struct {
	Repository     string `yaml:"repository"`
	Binary         string `yaml:"binary"`
	RegistryConfig string `yaml:"registry_config"`
	CAFile         string `yaml:"ca_file"`
}
type Config struct {
	Hostname              string            `yaml:"hostname"`
	App                   string            `yaml:"app"`
	MaxDynamicTargets     int               `yaml:"max_dynamic_targets"`
	ORAS                  ORAS              `yaml:"oras"`
	ListenAddr            string            `yaml:"listen_addr"`
	NodeID                string            `yaml:"node_id"`
	Env                   string            `yaml:"env"`
	DataDir               string            `yaml:"data_dir"`
	LockFile              string            `yaml:"lock_file"`
	LogFile               string            `yaml:"log_file"`
	ReleaseAuthTokens     map[string]string `yaml:"release_auth_tokens"`
	Repos                 map[string]Repo   `yaml:"repos"`
	Targets               []Target          `yaml:"targets"`
	AllowedClientIPs      []string          `yaml:"allowed_client_ips"`
	TrustedProxyCIDRs     []string          `yaml:"trusted_proxy_cidrs"`
	ExecutionTimeout      Duration          `yaml:"execution_timeout"`
	StepTimeout           Duration          `yaml:"step_timeout"`
	RecoveryTimeout       Duration          `yaml:"recovery_timeout"`
	CleanupTimeout        Duration          `yaml:"cleanup_timeout"`
	MaxRequestBytes       int64             `yaml:"max_request_bytes"`
	MaxConcurrentRequests int               `yaml:"max_concurrent_requests"`
	MaxArchiveBytes       int64             `yaml:"max_archive_bytes"`
	MaxArchiveFiles       int               `yaml:"max_archive_files"`
	MinFreeBytes          uint64            `yaml:"min_free_bytes"`
	KeepReleases          int               `yaml:"keep_releases"`
	AssetRetention        Duration          `yaml:"asset_retention"`
	Access                IPAccessParsed    `yaml:"-"`
}

func Load(p string) (Config, error) {
	b, e := os.ReadFile(p)
	if e != nil {
		return Config{}, e
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if e = dec.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one YAML document")
	}
	return c, c.Validate()
}
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		c.ListenAddr = ":9166"
	}
	if c.NodeID != "" && c.Hostname != "" && c.NodeID != c.Hostname {
		return fmt.Errorf("hostname and node_id disagree")
	}
	if c.NodeID == "" {
		c.NodeID = c.Hostname
	}
	if c.NodeID == "" {
		c.NodeID, _ = os.Hostname()
	}
	if c.NodeID == "" {
		return fmt.Errorf("cannot determine node hostname")
	}
	if len(c.ReleaseAuthTokens) == 0 {
		return fmt.Errorf("release_auth_tokens is required")
	}
	for env, token := range c.ReleaseAuthTokens {
		if strings.TrimSpace(env) != env || env == "" || len(env) > 64 || strings.TrimSpace(token) == "" {
			return fmt.Errorf("invalid release_auth_tokens environment or empty token")
		}
	}
	if c.Env == "" && len(c.ReleaseAuthTokens) == 1 {
		for env := range c.ReleaseAuthTokens {
			c.Env = env
		}
	}
	if c.LockFile == "" {
		c.LockFile = filepath.Join(c.DataDir, "publish.lock")
	}
	if c.MaxDynamicTargets == 0 {
		c.MaxDynamicTargets = 1000
	}
	if c.MaxDynamicTargets < 1 {
		return fmt.Errorf("max_dynamic_targets must be positive")
	}
	if len(c.NodeID) > 128 || len(c.Env) > 64 {
		return fmt.Errorf("node_id/env too long")
	}
	if c.Env != "" && strings.TrimSpace(c.ReleaseAuthTokens[c.Env]) == "" {
		return fmt.Errorf("release_auth_tokens must include current env")
	}
	if !filepath.IsAbs(c.DataDir) || !filepath.IsAbs(c.LockFile) {
		return fmt.Errorf("data_dir and shared lock_file must be absolute")
	}
	var e error
	c.DataDir, e = fsutil.Canonical(c.DataDir)
	if e != nil {
		return e
	}
	c.LockFile, e = fsutil.Canonical(c.LockFile)
	if e != nil {
		return e
	}
	if c.LogFile != "" && !filepath.IsAbs(c.LogFile) {
		return fmt.Errorf("log_file must be absolute")
	}
	if c.ExecutionTimeout == 0 {
		c.ExecutionTimeout = Duration(5 * time.Minute)
	}
	if c.StepTimeout == 0 {
		c.StepTimeout = Duration(60 * time.Second)
	}
	if c.RecoveryTimeout == 0 {
		c.RecoveryTimeout = Duration(90 * time.Second)
	}
	if c.CleanupTimeout == 0 {
		c.CleanupTimeout = Duration(5 * time.Second)
	}
	if c.ExecutionTimeout <= 0 || c.StepTimeout <= 0 || c.RecoveryTimeout <= 0 || c.CleanupTimeout <= 0 {
		return fmt.Errorf("timeouts must be positive")
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 64 << 10
	}
	if c.MaxConcurrentRequests == 0 {
		c.MaxConcurrentRequests = 64
	}
	if c.MaxArchiveBytes == 0 {
		c.MaxArchiveBytes = 1 << 30
	}
	if c.MaxArchiveFiles == 0 {
		c.MaxArchiveFiles = 100000
	}
	if c.MinFreeBytes == 0 {
		c.MinFreeBytes = 64 << 20
	}
	if c.KeepReleases == 0 {
		c.KeepReleases = 5
	}
	if c.MaxRequestBytes < 1 || c.MaxConcurrentRequests < 1 || c.MaxArchiveBytes < 1 || c.MaxArchiveFiles < 1 || c.KeepReleases < 2 {
		return fmt.Errorf("invalid resource limits")
	}
	if c.Targets == nil {
		for _, typ := range []release.ReleaseType{release.ReleaseTypeConfig, release.ReleaseTypeWhitelist} {
			if _, ok := c.Repos[string(typ)]; ok {
				c.Targets = append(c.Targets, Target{Type: typ})
			}
		}
		if c.ORAS.Repository != "" {
			c.Targets = append(c.Targets, Target{Type: release.ReleaseTypeFrontendStatic})
		}
	}
	if len(c.Targets) == 0 {
		return fmt.Errorf("targets or a configured release repository is required")
	}
	seen := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		t.Env = c.Env
		if t.IsTemplate() {
			if seen["type:"+string(t.Type)] {
				return fmt.Errorf("duplicate target type")
			}
			seen["type:"+string(t.Type)] = true
		} else if e = release.ValidateRepoSiteDirectoryName(t.ServerName); e != nil {
			return e
		}
		switch t.Type {
		case release.ReleaseTypeConfig, release.ReleaseTypeWhitelist:
		case release.ReleaseTypeFrontendStatic:
			repository := t.ArtifactRepository
			if repository == "" {
				repository = c.ORAS.Repository
			}
			if e := ValidateArtifactRepository(strings.ReplaceAll(repository, "{server_name}", "site")); e != nil {
				return e
			}
			if !filepath.IsAbs(c.ORAS.Binary) || !filepath.IsAbs(c.ORAS.RegistryConfig) {
				return fmt.Errorf("frontend requires explicit absolute oras.binary and oras.registry_config")
			}
			if c.ORAS.CAFile != "" && !filepath.IsAbs(c.ORAS.CAFile) {
				return fmt.Errorf("oras.ca_file must be absolute")
			}
		default:
			return fmt.Errorf("unsupported target type")
		}
		if !t.IsTemplate() {
			if c.Env == "" {
				return fmt.Errorf("explicit site targets require env; use type-only targets for multiple environments")
			}
			if err := c.resolvePaths(t, c.Env); err != nil {
				return err
			}
			if t.Type == release.ReleaseTypeFrontendStatic && t.ArtifactRepository == "" {
				t.ArtifactRepository = strings.ReplaceAll(c.ORAS.Repository, "{server_name}", t.ServerName)
			}
			for _, other := range c.Targets[:i] {
				if !other.IsTemplate() && (fsutil.Within(t.Dir, other.Dir) || fsutil.Within(other.Dir, t.Dir)) {
					return fmt.Errorf("overlapping targets")
				}
			}
			if seen[t.ID] {
				return fmt.Errorf("duplicate target")
			}
			seen[t.ID] = true
		}
		if t.FileMode == "" {
			t.FileMode = "0644"
		}
		v, err := strconv.ParseUint(t.FileMode, 8, 32)
		if err != nil || v > 0777 || v&0022 != 0 {
			return fmt.Errorf("file_mode must not permit group/world writes")
		}
		if t.Type != release.ReleaseTypeFrontendStatic {
			repo, ok := c.Repos[string(t.Type)]
			if !ok {
				return fmt.Errorf("missing repos.%s", t.Type)
			}
			u, err := url.Parse(repo.URL)
			if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("invalid repository URL or embedded credentials")
			}
			if u.Scheme == "file" {
				if !repo.AllowLocal || u.Host != "" || !filepath.IsAbs(u.Path) {
					return fmt.Errorf("local repositories require allow_local and empty host")
				}
			} else if u.Scheme != "https" || u.Host == "" || repo.GitLabToken == "" {
				return fmt.Errorf("repository requires https URL and gitlab_token")
			}

		}
		for j := range t.HealthChecks {
			h := &t.HealthChecks[j]
			if e = validateCheck(h); e != nil {
				return e
			}
			if h.Contains == "" {
				return fmt.Errorf("health_checks must verify content, not status alone")
			}
		}
		for j := range t.InitialHealthChecks {
			if e = validateCheck(&t.InitialHealthChecks[j]); e != nil {
				return e
			}
		}
		if t.Type == release.ReleaseTypeFrontendStatic {
			if t.SharedAssets && c.AssetRetention <= 0 {
				return fmt.Errorf("frontend requires positive asset_retention")
			}
			if t.SharedAssets && t.PublicBaseURL == "" {
				return fmt.Errorf("shared_assets requires public_base_url")
			}
			if t.PublicBaseURL != "" {
				u, err := url.Parse(t.PublicBaseURL)
				if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
					return fmt.Errorf("frontend requires public_base_url")
				}
			}
		}
		for _, f := range t.RequiredFiles {
			if !filepath.IsLocal(f) || strings.Contains(f, "\\") {
				return fmt.Errorf("invalid required_files entry")
			}
		}
	}
	c.Access, e = ParseIPAccessControl(c.AllowedClientIPs, c.TrustedProxyCIDRs)
	return e
}
func validateCheck(h *HealthCheck) error {
	u, e := url.Parse(h.URL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("invalid health URL")
	}
	if h.Status == 0 {
		h.Status = 200
	}
	if h.Status < 100 || h.Status > 599 {
		return fmt.Errorf("invalid health status")
	}
	return nil
}
func (c Config) AcceptsEnv(env string) bool {
	return env != "" && strings.TrimSpace(c.ReleaseAuthTokens[env]) != "" && (c.Env == "" || c.Env == env)
}
func (c Config) resolvePaths(t *Target, env string) error {
	var err error
	t.PathDest, err = fsutil.Canonical(t.PathDest)
	if err != nil {
		return err
	}
	if t.PathDest == string(os.PathSeparator) {
		return fmt.Errorf("path_dest may not be filesystem root")
	}
	t.Dir = filepath.Join(t.PathDest, string(t.Type), t.ServerName)
	if t.Type == release.ReleaseTypeFrontendStatic {
		t.Dir = filepath.Join(t.PathDest, t.ServerName)
	}
	if fsutil.Within(t.Dir, c.LockFile) || fsutil.Within(c.DataDir, t.Dir) || fsutil.Within(t.Dir, c.DataDir) {
		return fmt.Errorf("deployment directory overlaps service state or lock")
	}
	t.Env = env
	t.ID = release.Digest([]string{env, t.Dir})
	return nil
}
func (c Config) Target(typ release.ReleaseType, site, root, project string) (Target, error) {
	return c.TargetForEnv(typ, site, root, project, c.Env)
}
func (c Config) TargetForEnv(typ release.ReleaseType, site, root, project, env string) (Target, error) {
	if !c.AcceptsEnv(env) {
		return Target{}, fmt.Errorf("environment not enabled")
	}
	if err := release.ValidateRepoSiteDirectoryName(site); err != nil {
		return Target{}, err
	}
	real, err := fsutil.Canonical(root)
	if err != nil {
		return Target{}, err
	}
	for _, t := range c.Targets {
		if !t.IsTemplate() && t.Type == typ && t.ServerName == site && t.PathDest == real {
			if t.Project != "" && project != "" && project != t.Project {
				return Target{}, fmt.Errorf("project not authorized")
			}
			return t, nil
		}
	}
	for _, t := range c.Targets {
		if t.IsTemplate() && t.Type == typ {
			t.ServerName, t.PathDest, t.Project, t.Dynamic = site, real, project, true
			if t.Type == release.ReleaseTypeFrontendStatic {
				repository := t.ArtifactRepository
				if repository == "" {
					repository = c.ORAS.Repository
				}
				t.ArtifactRepository = strings.ReplaceAll(repository, "{server_name}", site)
				if err := ValidateArtifactRepository(t.ArtifactRepository); err != nil {
					return Target{}, err
				}
			}
			if err := c.resolvePaths(&t, env); err != nil {
				return Target{}, err
			}
			for _, other := range c.Targets {
				if !other.IsTemplate() && (fsutil.Within(t.Dir, other.Dir) || fsutil.Within(other.Dir, t.Dir)) {
					return Target{}, fmt.Errorf("overlaps configured site")
				}
			}
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("release type or target not enabled")
}

// Repository allowlist comes from node configuration, never an HTTP supplied URL.
func ValidateArtifactRepository(value string) error {
	if strings.ContainsAny(value, "@?#\\ \t\r\n") || strings.Contains(value, "://") || value != strings.ToLower(value) {
		return fmt.Errorf("invalid artifact_repository")
	}
	u, err := url.Parse("https://" + value)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Path == "" || strings.Contains(u.Path, ":") || strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("artifact_repository must be registry/project/app-dist without tag or digest")
	}
	for _, part := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
		if err := release.ValidateRepoSiteDirectoryName(part); err != nil {
			return fmt.Errorf("invalid artifact_repository path: %w", err)
		}
	}
	return nil
}
