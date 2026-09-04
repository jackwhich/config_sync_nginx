package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/domain/state"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"os"
	"time"
)

type Store struct {
	Root       *os.Root
	BeforeSave func(*state.TargetState) error
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
func (s *Store) Load(id string) (*state.TargetState, error) {
	if !release.ValidTargetID(id) {
		return nil, fmt.Errorf("invalid target_id")
	}
	b, e := s.Root.ReadFile(id + ".json")
	if e != nil {
		return nil, e
	}
	var st state.TargetState
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
func (s *Store) Save(st *state.TargetState) error {
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
func (s *Store) All() ([]*state.TargetState, error) {
	entries, e := fs.ReadDir(s.Root.FS(), ".")
	if e != nil {
		return nil, e
	}
	var out []*state.TargetState
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
func IsMissing(e error) bool { return errors.Is(e, os.ErrNotExist) }
