// Package fsutil confines operations to directory handles (Go 1.25+).
package fsutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"nginx_updata_config/internal/domain/release"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

func Canonical(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %q", p)
	}
	p = filepath.Clean(p)
	cur := p
	var tail []string
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		tail = append(tail, filepath.Base(cur))
		cur = filepath.Dir(cur)
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	for i := len(tail) - 1; i >= 0; i-- {
		real = filepath.Join(real, tail[i])
	}
	return real, nil
}
func OpenDir(p string, mode os.FileMode) (*os.Root, error) {
	p, err := Canonical(p)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(p, mode); err != nil {
		return nil, err
	}
	return os.OpenRoot(p)
}
func Within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func EnsureDirs(r *os.Root, name string, mode os.FileMode) error {
	if name == "." || name == "" {
		return nil
	}
	if !fs.ValidPath(filepath.ToSlash(name)) {
		return fmt.Errorf("unsafe relative directory %q", name)
	}
	p := ""
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		p = filepath.Join(p, part)
		err := r.Mkdir(p, mode)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		st, err := r.Lstat(p)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("directory contains a symlink or non-directory: %s", p)
		}
	}
	return nil
}
func SyncDir(r *os.Root, name string) error {
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func AtomicWrite(r *os.Root, name string, b []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(name), ".write-"+release.ID())
	defer r.Remove(tmp)
	f, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = r.Rename(tmp, name); err != nil {
		return err
	}
	return SyncDir(r, filepath.Dir(name))
}
func CopyFile(ctx context.Context, src, dst *os.Root, from, to string, mode os.FileMode) error {
	st, err := src.Lstat(from)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", from)
	}
	if err = EnsureDirs(dst, filepath.Dir(to), 0755); err != nil {
		return err
	}
	in, err := src.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dst.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, &contextReader{ctx, in})
	if err == nil {
		err = out.Sync()
	}
	ce := out.Close()
	if err == nil {
		err = ce
	}
	return err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func Reader(ctx context.Context, r io.Reader) io.Reader { return &contextReader{ctx, r} }

// Switch atomically points latest at a snapshot under rootPath.
// The symlink target is an absolute path so Nginx include/root resolutions
// that follow latest keep working after the process cwd changes.
func Switch(rootPath string, r *os.Root, target string) error {
	if target != "" && (!fs.ValidPath(target) || (!strings.HasPrefix(target, "releases/") && !release.IsCommit(target))) {
		return fmt.Errorf("unsafe link target %q", target)
	}
	rootPath, err := Canonical(rootPath)
	if err != nil {
		return err
	}
	if target == "" {
		err := r.Remove("latest")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return SyncDir(r, ".")
	}
	absTarget := filepath.Join(rootPath, filepath.FromSlash(target))
	if !strings.HasPrefix(absTarget, rootPath+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe absolute link target %q", absTarget)
	}
	tmp := ".latest-" + release.ID()
	tmpPath := filepath.Join(rootPath, tmp)
	defer os.Remove(tmpPath)
	if err := os.Symlink(absTarget, tmpPath); err != nil {
		return err
	}
	if err := r.Rename(tmp, "latest"); err != nil {
		return err
	}
	return SyncDir(r, ".")
}

// OwnWWW recursively sets owner/group to www:www for snapshot directories.
// latest is left alone (typically root). No-op when not root or www is missing.
func OwnWWW(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, gid, err := wwwIDs()
	if err != nil {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Lchown(path, uid, gid)
	}
	return filepath.Walk(path, func(p string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Lchown(p, uid, gid)
	})
}

func wwwIDs() (uid, gid int, err error) {
	u, err := user.Lookup("www")
	if err != nil {
		return 0, 0, err
	}
	g, err := user.LookupGroup("www")
	if err != nil {
		return 0, 0, err
	}
	uid, err = strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err = strconv.Atoi(g.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}
func Link(r *os.Root) (string, error) {
	s, err := r.Readlink("latest")
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return s, err
}

// RemoveTree checks cancellation between entries, unlike an uninterruptible RemoveAll.
func RemoveTree(ctx context.Context, r *os.Root, name string) error {
	if name == "." || !fs.ValidPath(filepath.ToSlash(name)) {
		return fmt.Errorf("unsafe cleanup path")
	}
	var entries []string
	err := fs.WalkDir(r.FS(), name, func(p string, d fs.DirEntry, e error) error {
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		entries = append(entries, p)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = r.Remove(entries[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
