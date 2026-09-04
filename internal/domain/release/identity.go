package release

import "strings"

func EffectiveVersion(r ApplyRequest) string {
	if r.Version != "" {
		return r.Version
	}
	return r.CommitID
}
func ServerIdentity(p map[string]string) string { return strings.TrimSpace(p["server_name"]) }
