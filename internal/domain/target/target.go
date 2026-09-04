package target

import (
	"nginx_updata_config/internal/domain/release"
	"os"
	"strconv"
)

type HealthCheck struct {
	URL           string `yaml:"url" json:"url"`
	Host          string `yaml:"host" json:"host,omitempty"`
	TLSServerName string `yaml:"tls_server_name" json:"tls_server_name,omitempty"`
	Status        int    `yaml:"status" json:"status"`
	Contains      string `yaml:"contains" json:"contains,omitempty"`
}
type Target struct {
	ArtifactRepository  string              `yaml:"artifact_repository" json:"artifact_repository,omitempty"`
	SharedAssets        bool                `yaml:"shared_assets" json:"-"`
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

// SnapshotLink is relative to <path_dest>/<server_name> for frontend releases.
func (t Target) SnapshotLink(commit string) string {
	if t.Type == release.ReleaseTypeFrontendStatic {
		return commit
	}
	return "releases/" + commit
}
