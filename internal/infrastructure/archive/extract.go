package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"os"
	"path/filepath"
	"strings"
)

func Extract(ctx context.Context, input io.Reader, dest *os.Root, maxBytes int64, maxFiles int) error {
	if maxBytes <= 0 || maxFiles <= 0 {
		return fmt.Errorf("invalid extraction limits")
	}
	tr := tar.NewReader(fsutil.Reader(ctx, input))
	var total int64
	files := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}
		name := strings.TrimSuffix(h.Name, "/")
		for strings.HasPrefix(name, "./") {
			name = strings.TrimPrefix(name, "./")
		}
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00\r\n") || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		for _, p := range strings.Split(name, "/") {
			if p == ".git" {
				return fmt.Errorf("archive includes .git")
			}
		}
		files++
		if files > maxFiles || h.Size < 0 || h.Size > maxBytes-total {
			return fmt.Errorf("archive limit exceeded")
		}
		total += h.Size
		switch h.Typeflag {
		case tar.TypeDir:
			if err = fsutil.EnsureDirs(dest, name, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err = fsutil.EnsureDirs(dest, filepath.Dir(name), 0755); err != nil {
				return err
			}
			f, e := dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return e
			}
			_, e = io.Copy(f, fsutil.Reader(ctx, tr))
			if e == nil {
				e = f.Sync()
			}
			ce := f.Close()
			if e == nil {
				e = ce
			}
			if e != nil {
				return e
			}
		default:
			return fmt.Errorf("archive entry %q has prohibited type %q", name, h.Typeflag)
		}
	}
}
