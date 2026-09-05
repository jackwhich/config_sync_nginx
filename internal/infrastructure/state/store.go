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
	"path/filepath"
	"sync"
	"time"
)

type Store struct {
	Root       *os.Root
	dataDir    string
	mu         sync.Mutex
	BeforeSave func(*state.TargetState) error
}

func Open(dataDir string) (*Store, error) {
	var e error
	dataDir, e = fsutil.Canonical(dataDir)
	if e != nil {
		return nil, e
	}
	sub, e := openRoot(dataDir)
	if e != nil {
		return nil, e
	}
	return &Store{Root: sub, dataDir: dataDir}, nil
}

func openRoot(dataDir string) (*os.Root, error) {
	r, e := fsutil.OpenDir(dataDir, 0700)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	if e = fsutil.EnsureDirs(r, "state", 0700); e != nil {
		return nil, e
	}
	return r.OpenRoot("state")
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Root == nil {
		return nil
	}
	e := s.Root.Close()
	s.Root = nil
	return e
}

// reopenIfRemovedLocked restores the state directory if it was removed after
// the process opened it. A missing state record is not treated as a removed
// state directory, so normal first-time target initialization is unchanged.
func (s *Store) reopenIfRemovedLocked() (bool, error) {
	if s.Root == nil {
		return false, fmt.Errorf("state store is closed")
	}
	info, e := os.Stat(filepath.Join(s.dataDir, "state"))
	if e == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("state path is not a directory")
		}
		return false, nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return false, e
	}
	newRoot, e := openRoot(s.dataDir)
	if e != nil {
		return false, e
	}
	oldRoot := s.Root
	s.Root = newRoot
	if e = oldRoot.Close(); e != nil {
		return false, e
	}
	return true, nil
}

func (s *Store) Load(id string) (*state.TargetState, error) {
	if !release.ValidTargetID(id) {
		return nil, fmt.Errorf("invalid target_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

func (s *Store) loadLocked(id string) (*state.TargetState, error) {
	if _, e := s.reopenIfRemovedLocked(); e != nil {
		return nil, e
	}
	b, e := s.Root.ReadFile(id + ".json")
	if errors.Is(e, os.ErrNotExist) {
		reopened, reopenErr := s.reopenIfRemovedLocked()
		if reopenErr != nil {
			return nil, reopenErr
		}
		if reopened {
			b, e = s.Root.ReadFile(id + ".json")
		}
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, e := s.reopenIfRemovedLocked(); e != nil {
		return e
	}
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
	e = fsutil.AtomicWrite(s.Root, st.Target.ID+".json", b, 0600)
	if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	reopened, reopenErr := s.reopenIfRemovedLocked()
	if reopenErr != nil || !reopened {
		return firstError(reopenErr, e)
	}
	return fsutil.AtomicWrite(s.Root, st.Target.ID+".json", b, 0600)
}
func (s *Store) All() ([]*state.TargetState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allLocked()
}

func (s *Store) allLocked() ([]*state.TargetState, error) {
	if _, e := s.reopenIfRemovedLocked(); e != nil {
		return nil, e
	}
	entries, e := fs.ReadDir(s.Root.FS(), ".")
	if errors.Is(e, os.ErrNotExist) {
		reopened, reopenErr := s.reopenIfRemovedLocked()
		if reopenErr != nil {
			return nil, reopenErr
		}
		if reopened {
			entries, e = fs.ReadDir(s.Root.FS(), ".")
		}
	}
	if e != nil {
		return nil, e
	}
	var out []*state.TargetState
	for _, ent := range entries {
		n := ent.Name()
		if len(n) != 69 || n[64:] != ".json" {
			continue
		}
		st, e := s.loadLocked(n[:64])
		if e != nil {
			return nil, e
		}
		out = append(out, st)
	}
	return out, nil
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}
func IsMissing(e error) bool { return errors.Is(e, os.ErrNotExist) }
