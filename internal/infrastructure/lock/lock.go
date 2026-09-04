package lock

import (
	"errors"
	"golang.org/x/sys/unix"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"os"
	"path/filepath"
)

var ErrBusy = errors.New("node is publishing")

type Lock struct{ file *os.File }

func Open(path string) (*Lock, error) {
	r, err := fsutil.OpenDir(filepath.Dir(path), 0755)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	name := filepath.Base(path)
	if st, e := r.Lstat(name); e == nil && !st.Mode().IsRegular() {
		return nil, errors.New("lock must be a regular file")
	}
	f, err := r.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return &Lock{f}, nil
}
func (l *Lock) Try() error {
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrBusy
	}
	return err
}
func (l *Lock) Unlock()      { _ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN) }
func (l *Lock) Close() error { return l.file.Close() }
