package release

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type ReleaseType string

const (
	ReleaseTypeConfig         ReleaseType = "config"
	ReleaseTypeWhitelist      ReleaseType = "whitelist"
	ReleaseTypeFrontendStatic ReleaseType = "frontend_static"
)

type NodeStatus string

const (
	NodeStatusRunning          NodeStatus = "running"
	NodeStatusSucceeded        NodeStatus = "succeeded"
	NodeStatusSkipped          NodeStatus = "skipped"
	NodeStatusFailed           NodeStatus = "failed"
	NodeStatusRecoveryRequired NodeStatus = "recovery_required"
)

const FrontendORASCapability = "frontend_oras_v1"

type ApplyRequest struct {
	ArtifactDigest        string            `json:"artifact_digest,omitempty"`
	ReleaseID             string            `json:"release_id"`
	ExpectedStateRevision string            `json:"expected_state_revision"`
	RestoreOf             string            `json:"restore_of,omitempty"`
	App                   string            `json:"app,omitempty"`
	Env                   string            `json:"env"`
	Type                  ReleaseType       `json:"type"`
	SourceRepo            string            `json:"source_repo,omitempty"`
	Branch                string            `json:"branch"`
	CommitID              string            `json:"commit_id"`
	Version               string            `json:"version,omitempty"`
	Project               string            `json:"project,omitempty"`
	Params                map[string]string `json:"params"`
	Operator              string            `json:"operator,omitempty"`
	BuildURL              string            `json:"build_url,omitempty"`
}
type Step struct {
	Name       string     `json:"name"`
	Status     NodeStatus `json:"status"`
	Message    string     `json:"message,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	DurationMS int64      `json:"duration_ms"`
}
type Result struct {
	ArtifactDigest      string      `json:"artifact_digest,omitempty"`
	ReleaseID           string      `json:"release_id,omitempty"`
	TargetID            string      `json:"target_id,omitempty"`
	Status              NodeStatus  `json:"status"`
	Phase               string      `json:"phase,omitempty"`
	ActivationStatus    string      `json:"activation_status"`
	RollbackStatus      string      `json:"rollback_status"`
	StateRevisionBefore string      `json:"state_revision_before,omitempty"`
	StateRevisionAfter  string      `json:"state_revision_after,omitempty"`
	ErrorCode           string      `json:"error_code,omitempty"`
	Error               string      `json:"error,omitempty"`
	Warnings            []string    `json:"warnings,omitempty"`
	Replayed            bool        `json:"replayed,omitempty"`
	Env                 string      `json:"env,omitempty"`
	Type                ReleaseType `json:"type,omitempty"`
	Project             string      `json:"project,omitempty"`
	ServerName          string      `json:"server_name,omitempty"`
	Version             string      `json:"version,omitempty"`
	CommitID            string      `json:"commit_id,omitempty"`
	PreviousCommitID    string      `json:"previous_commit_id,omitempty"`
	StartedAt           time.Time   `json:"started_at"`
	FinishedAt          time.Time   `json:"finished_at,omitempty"`
	Steps               []Step      `json:"steps,omitempty"`
	HTTPStatus          int         `json:"-"`
}

func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func Digest(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func (r Result) Terminal() bool {
	return r.Status == NodeStatusSucceeded || r.Status == NodeStatusSkipped || r.Status == NodeStatusFailed
}
