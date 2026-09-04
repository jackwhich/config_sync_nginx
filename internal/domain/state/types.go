package state

import (
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/target"
	"time"
)

type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	CommitID   string            `json:"commit_id"`
	Source     string            `json:"source"`
	Files      map[string]File   `json:"files"`
	Assets     map[string]string `json:"assets,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	LastUsedAt time.Time         `json:"last_used_at"`
}
type Version struct {
	ArtifactDigest string    `json:"artifact_digest,omitempty"`
	CommitID       string    `json:"commit_id"`
	Version        string    `json:"version"`
	Source         string    `json:"source"`
	Link           string    `json:"link"`
	VerifiedAt     time.Time `json:"verified_at"`
	ManifestDigest string    `json:"manifest_digest"`
}
type Record struct {
	LegacyLink       string               `json:"legacy_link,omitempty"`
	Request          release.ApplyRequest `json:"request"`
	Fingerprint      string               `json:"fingerprint"`
	Result           release.Result       `json:"result"`
	HTTPStatus       int                  `json:"http_status"`
	Baseline         *Version             `json:"baseline,omitempty"`
	BaselinePrevious *Version             `json:"baseline_previous,omitempty"`
	BeforeLink       string               `json:"before_link"`
	Candidate        *Version             `json:"candidate,omitempty"`
	Intent           bool                 `json:"switch_intent"`
}
type TargetState struct {
	Schema           int                `json:"schema"`
	Target           target.Target      `json:"target"`
	Revision         string             `json:"state_revision"`
	Current          *Version           `json:"current,omitempty"`
	Previous         *Version           `json:"previous,omitempty"`
	ActiveID         string             `json:"active_release_id,omitempty"`
	RecoveryRequired bool               `json:"recovery_required"`
	Records          map[string]*Record `json:"records"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

func New(t target.Target) *TargetState {
	return &TargetState{Schema: 2, Target: t, Revision: release.ID(), Records: map[string]*Record{}}
}
