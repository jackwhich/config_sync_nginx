package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/service/fsutil"
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
type HealthCheck struct {
	URL           string `yaml:"url" json:"url"`
	Host          string `yaml:"host" json:"host,omitempty"`
	TLSServerName string `yaml:"tls_server_name" json:"tls_server_name,omitempty"`
	Status        int    `yaml:"status" json:"status"`
	Contains      string `yaml:"contains" json:"contains,omitempty"`
}
type Nginx struct {
	Binary     string `yaml:"binary"`
	ConfigFile string `yaml:"config_file"`
	Prefix     string `yaml:"prefix"`
	PIDFile    string `yaml:"pid_file"`
}
type Target struct {
	Type                release.ReleaseType `yaml:"type" json:"type"`
	ServerName          string              `yaml:"server_name" json:"server_name"`
	PathDest            string              `yaml:"path_dest" json:"path_dest"`
	Project             string              `yaml:"project" json:"project,omitempty"`
	RequiredFiles       []string            `yaml:"required_files" json:"-"`
	HealthChecks        []HealthCheck       `yaml:"health_checks" json:"-"`
	InitialHealthChecks []HealthCheck       `yaml:"initial_health_checks" json:"-"`
	PublicBaseURL       string              `yaml:"public_base_url" json:"-"`
	PublicHost          string              `yaml:"public_host" json:"-"`
	FileMode            string              `yaml:"file_mode" json:"-"`
	ID                  string              `yaml:"-" json:"target_id"`
	Dir                 string              `yaml:"-" json:"deployment_dir"`
}

func (t Target) Mode() os.FileMode {
	v, _ := strconv.ParseUint(t.FileMode, 8, 32)
	return os.FileMode(v)
}

type Config struct {
	ListenAddr            string            `yaml:"listen_addr"`
	NodeID                string            `yaml:"node_id"`
	Env                   string            `yaml:"env"`
	DataDir               string            `yaml:"data_dir"`
	LockFile              string            `yaml:"lock_file"`
	LogFile               string            `yaml:"log_file"`
	ReleaseAuthTokens     map[string]string `yaml:"release_auth_tokens"`
	Repos                 map[string]Repo   `yaml:"repos"`
	Targets               []Target          `yaml:"targets"`
	Nginx                 Nginx             `yaml:"nginx"`
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
	return c, c.Validate()
}
func (c *Config) Validate() error {
	if c.ListenAddr == "" || c.Env == "" || c.NodeID == "" {
		return fmt.Errorf("listen_addr, env, node_id are required")
	}
	if len(c.NodeID) > 128 || len(c.Env) > 64 {
		return fmt.Errorf("node_id/env too long")
	}
	if strings.TrimSpace(c.ReleaseAuthTokens[c.Env]) == "" {
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
	if len(c.Targets) == 0 {
		return fmt.Errorf("at least one explicit target is required")
	}
	seen := map[string]bool{}
	needNginx := false
	for i := range c.Targets {
		t := &c.Targets[i]
		if e = release.ValidateRepoSiteDirectoryName(t.ServerName); e != nil {
			return e
		}
		switch t.Type {
		case release.ReleaseTypeConfig, release.ReleaseTypeWhitelist:
			needNginx = true
		case release.ReleaseTypeFrontendStatic:
		default:
			return fmt.Errorf("unsupported target type")
		}
		t.PathDest, e = fsutil.Canonical(t.PathDest)
		if e != nil {
			return e
		}
		if t.PathDest == string(os.PathSeparator) {
			return fmt.Errorf("path_dest may not be filesystem root")
		}
		t.Dir = filepath.Join(t.PathDest, string(t.Type), t.ServerName)
		if fsutil.Within(t.Dir, c.LockFile) {
			return fmt.Errorf("lock_file cannot reside within a deployment target")
		}
		if fsutil.Within(c.DataDir, t.Dir) || fsutil.Within(t.Dir, c.DataDir) {
			return fmt.Errorf("deployment directory overlaps data_dir")
		}
		for _, other := range c.Targets[:i] {
			if fsutil.Within(t.Dir, other.Dir) || fsutil.Within(other.Dir, t.Dir) {
				return fmt.Errorf("overlapping targets")
			}
		}
		t.ID = release.Digest([]string{c.Env, t.Dir})
		if seen[t.ID] {
			return fmt.Errorf("duplicate target")
		}
		seen[t.ID] = true
		if t.FileMode == "" {
			t.FileMode = "0644"
		}
		v, err := strconv.ParseUint(t.FileMode, 8, 32)
		if err != nil || v > 0777 || v&0022 != 0 {
			return fmt.Errorf("file_mode must not permit group/world writes")
		}
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
		if len(repo.AllowedBranches) == 0 {
			return fmt.Errorf("allowed_branches required for %s", t.Type)
		}
		if len(t.HealthChecks) == 0 {
			return fmt.Errorf("health_checks required for target %s", t.ServerName)
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
			if c.AssetRetention <= 0 {
				return fmt.Errorf("frontend requires positive asset_retention")
			}
			u, err := url.Parse(t.PublicBaseURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				return fmt.Errorf("frontend requires public_base_url")
			}
		}
		for _, f := range t.RequiredFiles {
			if !filepath.IsLocal(f) || strings.Contains(f, "\\") {
				return fmt.Errorf("invalid required_files entry")
			}
		}
	}
	if needNginx {
		if !filepath.IsAbs(c.Nginx.Binary) || !filepath.IsAbs(c.Nginx.ConfigFile) || !filepath.IsAbs(c.Nginx.PIDFile) {
			return fmt.Errorf("nginx binary, config_file and pid_file must be explicit absolute paths")
		}
		if c.Nginx.Prefix != "" && !filepath.IsAbs(c.Nginx.Prefix) {
			return fmt.Errorf("nginx prefix must be absolute")
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
func (c Config) Target(typ release.ReleaseType, site, root, project string) (Target, error) {
	real, e := fsutil.Canonical(root)
	if e != nil {
		return Target{}, e
	}
	for _, t := range c.Targets {
		if t.Type == typ && t.ServerName == site && t.PathDest == real {
			if t.Project != "" && project != "" && project != t.Project {
				return Target{}, fmt.Errorf("project not authorized")
			}
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("target or deployment path not authorized")
}
