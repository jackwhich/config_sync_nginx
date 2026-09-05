package release

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var ErrInvalidRequest = errors.New("请求无效")
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var artifactDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func IsArtifactDigest(s string) bool { return artifactDigestPattern.MatchString(s) }

var sitePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func IsID(v string) bool     { return uuidPattern.MatchString(v) }
func IsCommit(v string) bool { return commitPattern.MatchString(v) }

func ValidateNginxCommandRequest(r *NginxCommandRequest) error {
	r.ReleaseID = strings.TrimSpace(r.ReleaseID)
	r.Env = strings.TrimSpace(r.Env)
	if !IsID(r.ReleaseID) {
		return fieldError("release_id", "须为 UUID")
	}
	if r.Env == "" || len(r.Env) > 64 {
		return fieldError("env", "不能为空或超长")
	}
	return nil
}

func ValidateRollbackRequest(r *RollbackRequest) error {
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	r.Env = strings.TrimSpace(r.Env)
	r.Project = strings.TrimSpace(r.Project)
	if r.Env == "" || len(r.Env) > 64 || len(r.Project) > 128 {
		return fieldError("env/project", "不能为空或超长")
	}
	if r.Type != ReleaseTypeConfig && r.Type != ReleaseTypeWhitelist && r.Type != ReleaseTypeFrontendStatic {
		return fieldError("type", "不支持的发布类型")
	}
	if err := ValidateRepoSiteDirectoryName(ServerIdentity(r.Params)); err != nil {
		return err
	}
	raw := strings.TrimSpace(r.Params["path_dest"])
	if !filepath.IsAbs(raw) || len(raw) > 4096 || strings.IndexByte(raw, 0) >= 0 {
		return fieldError("params.path_dest", "须为有效绝对路径")
	}
	for k := range r.Params {
		if k != "path_dest" && k != "server_name" {
			return fieldError("params", "不支持字段 "+k)
		}
	}
	r.Params = map[string]string{"path_dest": filepath.Clean(raw), "server_name": ServerIdentity(r.Params)}
	return nil
}

func ValidateApplyRequest(r *ApplyRequest) error {
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	merge := func(dest *string, alias string, name string) error {
		if alias != "" && *dest != "" && alias != *dest {
			return fieldError(name, "重复字段值不一致")
		}
		if *dest == "" {
			*dest = alias
		}
		return nil
	}
	if err := merge(&r.CommitID, r.CommitAlias, "commit_id/commitid"); err != nil {
		return err
	}
	for _, field := range []struct{ key, value string }{{"server_name", r.ServerName}, {"server_name", r.ServerNameAlias}, {"path_dest", r.PathDest}} {
		value := r.Params[field.key]
		if err := merge(&value, field.value, field.key); err != nil {
			return err
		}
		r.Params[field.key] = value
	}
	r.CommitAlias, r.ServerName, r.ServerNameAlias, r.PathDest = "", "", "", ""
	if r.ReleaseID == "" {
		r.ReleaseID = ID()
	}
	r.Env = strings.TrimSpace(r.Env)
	r.Branch = strings.TrimSpace(r.Branch)
	r.Project = strings.TrimSpace(r.Project)
	r.CommitID = strings.ToLower(strings.TrimSpace(r.CommitID))
	r.Version = strings.TrimSpace(r.Version)
	r.ArtifactDigest = strings.TrimSpace(r.ArtifactDigest)
	if !IsID(r.ReleaseID) || (r.ExpectedStateRevision != "" && !IsID(r.ExpectedStateRevision)) {
		return fieldError("release_id/expected_state_revision", "须为 UUID，要求 HTTP 发布协议 2")
	}
	if r.RestoreOf != "" && (!IsID(r.RestoreOf) || r.ExpectedStateRevision == "") {
		return fieldError("restore_of", "须为 UUID")
	}
	if r.Env == "" || len(r.Env) > 64 || len(r.Branch) > 255 {
		return fieldError("env/branch", "不能为空或超长")
	}
	if r.Type == ReleaseTypeFrontendStatic {
		if r.ArtifactDigest != "" && !IsArtifactDigest(r.ArtifactDigest) {
			return fieldError("artifact_digest", "前端须指定 sha256 OCI manifest digest")
		}
	} else if r.ArtifactDigest != "" {
		return fieldError("artifact_digest", "仅用于 frontend_static")
	}
	if !IsCommit(r.CommitID) {
		return fieldError("commit_id", "须为完整的 40 或 64 位十六进制提交 ID")
	}
	if len(r.Project) > 128 || len(r.Version) > 256 || len(r.Operator) > 256 || len(r.BuildURL) > 2048 || len(r.App) > 128 {
		return fieldError("metadata", "字段过长")
	}
	if err := ResolveApplySourceRepo(r); err != nil {
		return err
	}
	if err := ValidateRepoSiteDirectoryName(ServerIdentity(r.Params)); err != nil {
		return err
	}
	raw := strings.TrimSpace(r.Params["path_dest"])
	if !filepath.IsAbs(raw) || len(raw) > 4096 || strings.IndexByte(raw, 0) >= 0 {
		return fieldError("params.path_dest", "须为有效绝对路径")
	}
	for k := range r.Params {
		if k != "path_dest" && k != "server_name" {
			return fieldError("params", "不支持字段 "+k)
		}
	}
	r.Params = map[string]string{"path_dest": filepath.Clean(raw), "server_name": ServerIdentity(r.Params)}
	return nil
}
func ValidateRepoSiteDirectoryName(s string) error {
	if !sitePattern.MatchString(s) || strings.Contains(s, "..") || s == "current" || s == "latest" || s == "releases" || s == "assets" {
		return fieldError("server_name", "须为安全、非保留的单段站点名称")
	}
	return nil
}
func fieldError(f, msg string) error { return fmt.Errorf("%w: %s: %s", ErrInvalidRequest, f, msg) }
