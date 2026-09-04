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
var sitePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func IsID(v string) bool     { return uuidPattern.MatchString(v) }
func IsCommit(v string) bool { return commitPattern.MatchString(v) }
func ValidateApplyRequest(r *ApplyRequest) error {
	r.Env = strings.TrimSpace(r.Env)
	r.Branch = strings.TrimSpace(r.Branch)
	r.Project = strings.TrimSpace(r.Project)
	r.CommitID = strings.ToLower(strings.TrimSpace(r.CommitID))
	r.Version = strings.TrimSpace(r.Version)
	if !IsID(r.ReleaseID) || !IsID(r.ExpectedStateRevision) {
		return fieldError("release_id/expected_state_revision", "须为 UUID，要求 HTTP 发布协议 2")
	}
	if r.RestoreOf != "" && !IsID(r.RestoreOf) {
		return fieldError("restore_of", "须为 UUID")
	}
	if r.Env == "" || len(r.Env) > 64 || r.Branch == "" || len(r.Branch) > 255 {
		return fieldError("env/branch", "不能为空或超长")
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
	if !sitePattern.MatchString(s) || strings.Contains(s, "..") || s == "latest" || s == "releases" || s == "assets" {
		return fieldError("server_name", "须为安全、非保留的单段站点名称")
	}
	return nil
}
func fieldError(f, msg string) error { return fmt.Errorf("%w: %s: %s", ErrInvalidRequest, f, msg) }
