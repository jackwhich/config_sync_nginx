package release

import "strings"

// ResolveApplySourceRepo 将 source_repo 规范为与 type 相同的字符串键（如 "config"），与 YAML repos 的键一致，供本机查找远端配置。
func ResolveApplySourceRepo(req *ApplyRequest) error {
	switch req.Type {
	case ReleaseTypeConfig, ReleaseTypeWhitelist, ReleaseTypeFrontendStatic:
	default:
		return fieldError("type", "不支持的发布类型")
	}
	want := string(req.Type)
	in := strings.TrimSpace(req.SourceRepo)
	if in == "" {
		req.SourceRepo = want
		return nil
	}
	if in != want {
		return fieldError("source_repo", "请省略该字段，或填写为 "+want+"（须与 type 一致）")
	}
	req.SourceRepo = want
	return nil
}
