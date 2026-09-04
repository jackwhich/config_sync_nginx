package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/service/config"
	"nginx_updata_config/internal/service/fsutil"
	"os"
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
	Target           config.Target      `json:"target"`
	Revision         string             `json:"state_revision"`
	Current          *Version           `json:"current,omitempty"`
	Previous         *Version           `json:"previous,omitempty"`
	ActiveID         string             `json:"active_release_id,omitempty"`
	RecoveryRequired bool               `json:"recovery_required"`
	Records          map[string]*Record `json:"records"`
	UpdatedAt        time.Time          `json:"updated_at"`
}
type Store struct {
	Root       *os.Root
	BeforeSave func(*TargetState) error
}

func Open(dataDir string) (*Store, error) {
	r, e := fsutil.OpenDir(dataDir, 0700)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	if e = fsutil.EnsureDirs(r, "state", 0700); e != nil {
		return nil, e
	}
	sub, e := r.OpenRoot("state")
	return &Store{Root: sub}, e
}
func (s *Store) Close() error { return s.Root.Close() }
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
func (s *Store) Load(id string) (*TargetState, error) {
	if !ValidTargetID(id) {
		return nil, fmt.Errorf("invalid target_id")
	}
	b, e := s.Root.ReadFile(id + ".json")
	if e != nil {
		return nil, e
	}
	var st TargetState
	if e = json.Unmarshal(b, &st); e != nil {
		return nil, e
	}
	if st.Schema != 2 || st.Target.ID != id || st.Records == nil || !release.IsID(st.Revision) {
		return nil, fmt.Errorf("invalid state schema for %s", id)
	}
	for id, rec := range st.Records {
		if rec == nil || id != rec.Request.ReleaseID || !release.IsID(id) || rec.Result.TargetID != st.Target.ID {
			return nil, fmt.Errorf("invalid release record")
		}
	}
	return &st, nil
}
func (s *Store) Save(st *TargetState) error {
	if s.BeforeSave != nil {
		if e := s.BeforeSave(st); e != nil {
			return e
		}
	}
	st.UpdatedAt = time.Now().UTC()
	b, e := json.Marshal(st)
	if e != nil {
		return e
	}
	return fsutil.AtomicWrite(s.Root, st.Target.ID+".json", b, 0600)
}
func (s *Store) All() ([]*TargetState, error) {
	entries, e := fs.ReadDir(s.Root.FS(), ".")
	if e != nil {
		return nil, e
	}
	var out []*TargetState
	for _, ent := range entries {
		n := ent.Name()
		if len(n) != 69 || n[64:] != ".json" {
			continue
		}
		st, e := s.Load(n[:64])
		if e != nil {
			return nil, e
		}
		out = append(out, st)
	}
	return out, nil
}
func New(t config.Target) *TargetState {
	return &TargetState{Schema: 2, Target: t, Revision: release.ID(), Records: map[string]*Record{}}
}
func IsMissing(e error) bool { return errors.Is(e, os.ErrNotExist) }
