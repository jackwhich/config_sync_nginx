package release

import "strings"

func EffectiveVersion(r ApplyRequest) string {
	if r.Version != "" {
		return r.Version
	}
	return r.CommitID
}
func ServerIdentity(p map[string]string) string { return strings.TrimSpace(p["server_name"]) }

func ValidTargetID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
