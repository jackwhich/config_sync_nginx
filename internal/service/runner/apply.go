package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"io/fs"
	"net/url"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/service/config"
	"nginx_updata_config/internal/service/fsutil"
	"nginx_updata_config/internal/service/state"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func openTarget(t config.Target) (*os.Root, error) {
	real, e := fsutil.Canonical(t.PathDest)
	if e != nil {
		return nil, e
	}
	if real != t.PathDest {
		return nil, fmt.Errorf("configured deployment root changed")
	}
	root, e := fsutil.OpenDir(t.PathDest, 0755)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	sub := filepath.Join(string(t.Type), t.ServerName)
	if e = fsutil.EnsureDirs(root, sub, 0755); e != nil {
		return nil, e
	}
	return root.OpenRoot(sub)
}
func ensureSpace(root *os.Root, required uint64) error {
	f, e := root.Open(".")
	if e != nil {
		return e
	}
	defer f.Close()
	var stat unix.Statfs_t
	if e = unix.Fstatfs(int(f.Fd()), &stat); e != nil {
		return e
	}
	if uint64(stat.Bavail)*uint64(stat.Bsize) < required {
		return fmt.Errorf("insufficient free disk space")
	}
	return nil
}
func hashFile(ctx context.Context, r *os.Root, name string) (state.File, error) {
	st, e := r.Lstat(name)
	if e != nil {
		return state.File{}, e
	}
	if !st.Mode().IsRegular() {
		return state.File{}, fmt.Errorf("non-regular file %s", name)
	}
	f, e := r.Open(name)
	if e != nil {
		return state.File{}, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, fsutil.Reader(ctx, f))
	return state.File{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, e
}
func manifestDigest(m *state.Manifest) string {
	return release.Digest(struct {
		Commit, Source string
		Files          map[string]state.File
		Assets         map[string]string
	}{m.CommitID, m.Source, m.Files, m.Assets})
}
func loadManifest(base *os.Root, commit string) (*state.Manifest, error) {
	if !release.IsCommit(commit) {
		return nil, fmt.Errorf("invalid manifest commit")
	}
	b, e := base.ReadFile(".manifests/" + commit + ".json")
	if e != nil {
		return nil, e
	}
	var m state.Manifest
	e = json.Unmarshal(b, &m)
	if e != nil || m.CommitID != commit || m.Files == nil {
		return nil, fmt.Errorf("invalid snapshot manifest")
	}
	for name, file := range m.Files {
		if len(file.SHA256) != 64 || !release.IsCommit(file.SHA256) || file.Size < 0 {
			return nil, fmt.Errorf("invalid manifest digest or size")
		}
		if !fs.ValidPath(name) || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("unsafe manifest path")
		}
	}
	for name, digest := range m.Assets {
		if !strings.HasPrefix(name, "assets/") || m.Files[name].SHA256 != digest || len(digest) != 64 {
			return nil, fmt.Errorf("invalid asset manifest")
		}
	}
	return &m, nil
}
func verifySnapshot(ctx context.Context, base *os.Root, v *state.Version) (*state.Manifest, error) {
	if v == nil {
		return nil, fmt.Errorf("missing snapshot")
	}
	if v.Link != "releases/"+v.CommitID {
		return nil, fmt.Errorf("invalid snapshot link")
	}
	m, e := loadManifest(base, v.CommitID)
	if e != nil {
		return nil, e
	}
	if m.Source != v.Source || manifestDigest(m) != v.ManifestDigest {
		return nil, fmt.Errorf("snapshot source or manifest changed")
	}
	root, e := base.OpenRoot(v.Link)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	seen := 0
	e = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		expected, ok := m.Files[name]
		if !ok {
			return fmt.Errorf("unexpected snapshot file %s", name)
		}
		got, e := hashFile(ctx, root, name)
		if e != nil {
			return e
		}
		if expected != got {
			return fmt.Errorf("snapshot integrity mismatch %s", name)
		}
		seen++
		return nil
	})
	if e == nil && seen != len(m.Files) {
		e = fmt.Errorf("snapshot files missing")
	}
	return m, e
}
func prepareSnapshot(ctx context.Context, c config.Config, t config.Target, work, commit, source, version string) (*state.Version, error) {
	base, e := openTarget(t)
	if e != nil {
		return nil, e
	}
	defer base.Close()
	for _, dir := range []string{".staging", "releases", ".manifests"} {
		if e = fsutil.EnsureDirs(base, dir, 0755); e != nil {
			return nil, e
		}
	}
	if m, e := loadManifest(base, commit); e == nil {
		v := &state.Version{CommitID: commit, Version: version, Source: source, Link: "releases/" + commit, ManifestDigest: manifestDigest(m)}
		_, e = verifySnapshot(ctx, base, v)
		return v, e
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if _, e = base.Lstat("releases/" + commit); e == nil {
		return nil, fmt.Errorf("snapshot exists without a trusted manifest; recovery required")
	}
	src, e := os.OpenRoot(work)
	if e != nil {
		return nil, e
	}
	defer src.Close()
	site, e := src.OpenRoot(t.ServerName)
	if e != nil {
		return nil, e
	}
	defer site.Close()
	stage := ".staging/stage-" + release.ID()
	if e = base.Mkdir(stage, 0755); e != nil {
		return nil, e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), c.CleanupTimeout.Value())
		defer cancel()
		_ = fsutil.RemoveTree(cleanup, base, stage)
	}()
	dst, e := base.OpenRoot(stage)
	if e != nil {
		return nil, e
	}
	defer dst.Close()
	m := &state.Manifest{CommitID: commit, Source: source, CreatedAt: time.Now().UTC(), Files: map[string]state.File{}}
	var bytes uint64
	count := 0
	e = fs.WalkDir(site.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks prohibited")
		}
		if d.IsDir() {
			return fsutil.EnsureDirs(dst, name, 0755)
		}
		if name == ".release-version" {
			return fmt.Errorf("reserved .release-version file")
		}
		if t.Type == release.ReleaseTypeFrontendStatic && name == "frontend-manifest.json" {
			return nil
		}
		f, err := hashFile(ctx, site, name)
		if err != nil {
			return err
		}
		bytes += uint64(f.Size)
		count++
		if bytes > uint64(c.MaxArchiveBytes) || count > c.MaxArchiveFiles {
			return fmt.Errorf("snapshot limit exceeded")
		}
		if err = ensureSpace(base, c.MinFreeBytes+uint64(f.Size)); err != nil {
			return err
		}
		if err = fsutil.CopyFile(ctx, site, dst, name, name, t.Mode()); err != nil {
			return err
		}
		m.Files[name] = f
		return nil
	})
	if e != nil {
		return nil, e
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("empty site")
	}
	for _, name := range t.RequiredFiles {
		if _, ok := m.Files[name]; !ok {
			return nil, fmt.Errorf("required file missing: %s", name)
		}
	}
	if t.Type == release.ReleaseTypeFrontendStatic {
		if e = validateFrontend(ctx, site, m); e != nil {
			return nil, e
		}
	}
	marker := []byte(commit + "\n")
	if e = dst.WriteFile(".release-version", marker, t.Mode()); e != nil {
		return nil, e
	}
	markerFile, e := dst.Open(".release-version")
	if e != nil {
		return nil, e
	}
	e = markerFile.Sync()
	markerFile.Close()
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256(marker)
	m.Files[".release-version"] = state.File{SHA256: hex.EncodeToString(h[:]), Size: int64(len(marker))}
	if e = syncTree(dst); e != nil {
		return nil, e
	}
	if e = base.Rename(stage, "releases/"+commit); e != nil {
		return nil, e
	}
	if e = fsutil.SyncDir(base, "releases"); e != nil {
		return nil, e
	}
	b, _ := json.Marshal(m)
	if e = fsutil.AtomicWrite(base, ".manifests/"+commit+".json", b, 0600); e != nil {
		return nil, e
	}
	v := &state.Version{CommitID: commit, Version: version, Source: source, Link: "releases/" + commit, ManifestDigest: manifestDigest(m)}
	return v, nil
}
func syncTree(root *os.Root) error {
	var dirs []string
	e := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, e error) error {
		if e == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return e
	})
	if e != nil {
		return e
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if e = fsutil.SyncDir(root, dirs[i]); e != nil {
			return e
		}
	}
	return nil
}

var hashName = regexp.MustCompile(`[.-][a-fA-F0-9]{8,64}(?:[.-]|$)`)
var references = regexp.MustCompile(`(?:src=|href=|url\(|from\s+|import\s*\(|new URL\s*\()\s*["']?([^"'\s)>]+)|["']((?:\./|\.\./|/assets/|assets/)[^"'\s]+\.(?:js|css|png|jpg|jpeg|svg|webp|woff2?)(?:\?[^"']*)?)["']`)

func validateFrontend(ctx context.Context, src *os.Root, m *state.Manifest) error {
	b, e := src.ReadFile("frontend-manifest.json")
	if e != nil {
		return fmt.Errorf("frontend-manifest.json required: %w", e)
	}
	var ci struct {
		Assets map[string]string `json:"assets"`
	}
	if e = json.Unmarshal(b, &ci); e != nil {
		return e
	}
	if _, ok := m.Files["index.html"]; !ok || len(ci.Assets) == 0 {
		return fmt.Errorf("frontend needs index.html and immutable assets")
	}
	for name, digest := range ci.Assets {
		f, ok := m.Files[name]
		if !ok || !strings.HasPrefix(name, "assets/") || !hashName.MatchString(path.Base(name)) || f.SHA256 != digest {
			return fmt.Errorf("invalid immutable asset %q", name)
		}
	}
	for name := range m.Files {
		if name != "index.html" {
			if _, ok := ci.Assets[name]; !ok {
				return fmt.Errorf("frontend file not declared as hashed asset: %s", name)
			}
		}
		ext := path.Ext(name)
		if ext != ".html" && ext != ".css" && ext != ".js" {
			continue
		}
		f := m.Files[name]
		if f.Size > 32<<20 {
			return fmt.Errorf("text asset too large")
		}
		b, e = src.ReadFile(name)
		if e != nil {
			return e
		}
		for _, match := range references.FindAllStringSubmatch(string(b), -1) {
			ref := match[1]
			if ref == "" {
				ref = match[2]
			}
			u, e := url.Parse(ref)
			if e != nil {
				return e
			}
			if u.IsAbs() || u.Host != "" || u.Path == "" || strings.HasPrefix(ref, "#") {
				continue
			}
			p := u.Path
			if strings.HasPrefix(p, "/") {
				p = strings.TrimPrefix(p, "/")
			} else {
				p = path.Join(path.Dir(name), p)
			}
			// Navigation URLs are not assets; local script/style/media references must be declared.
			ext := path.Ext(p)
			if ext == "" || ext == ".html" {
				continue
			}
			if _, ok := ci.Assets[p]; !ok {
				return fmt.Errorf("undeclared frontend reference %s in %s", ref, name)
			}
		}
	}
	m.Assets = ci.Assets
	return ctx.Err()
}
func installAssets(ctx context.Context, base *os.Root, v *state.Version, m *state.Manifest) error {
	for name, digest := range m.Assets {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := fsutil.EnsureDirs(base, path.Dir(name), 0755); e != nil {
			return e
		}
		if _, e := base.Lstat(name); e == nil {
			f, e := hashFile(ctx, base, name)
			if e != nil {
				return e
			}
			if f.SHA256 != digest {
				return fmt.Errorf("immutable asset collision: %s", name)
			}
			continue
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e := base.Link(path.Join(v.Link, name), name); e != nil {
			return e
		}
		if e := fsutil.SyncDir(base, path.Dir(name)); e != nil {
			return e
		}
	}
	return nil
}
func cleanupSnapshots(ctx context.Context, c config.Config, t config.Target, st *state.TargetState) error {
	base, e := openTarget(t)
	if e != nil {
		return e
	}
	defer base.Close()
	entries, e := fs.ReadDir(base.FS(), ".manifests")
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	protect := map[string]bool{}
	keep := func(v *state.Version) {
		if v != nil {
			protect[v.CommitID] = true
		}
	}
	keep(st.Current)
	keep(st.Previous)
	link, e := fsutil.Link(base)
	if e != nil {
		return e
	}
	if strings.HasPrefix(link, "releases/") {
		protect[strings.TrimPrefix(link, "releases/")] = true
	}
	for _, r := range st.Records {
		if !r.Result.Terminal() || (t.Type == release.ReleaseTypeFrontendStatic && r.Intent && time.Since(r.Result.FinishedAt) < c.AssetRetention.Value()) {
			keep(r.Baseline)
			keep(r.Candidate)
		}
	}
	var manifests []*state.Manifest
	for _, ent := range entries {
		if e = ctx.Err(); e != nil {
			return e
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		if !release.IsCommit(id) {
			continue
		}
		m, e := loadManifest(base, id)
		if e != nil {
			return e
		}
		manifests = append(manifests, m)
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].CreatedAt.After(manifests[j].CreatedAt) })
	for i, m := range manifests {
		if i < c.KeepReleases {
			protect[m.CommitID] = true
		}
	}
	liveAssets := map[string]bool{}
	for _, m := range manifests {
		if e = ctx.Err(); e != nil {
			return e
		}
		if protect[m.CommitID] {
			for name := range m.Assets {
				liveAssets[name] = true
			}
			continue
		}
		if e = fsutil.RemoveTree(ctx, base, "releases/"+m.CommitID); e != nil {
			return e
		}
		if e = base.Remove(".manifests/" + m.CommitID + ".json"); e != nil {
			return e
		}
	}
	if t.Type == release.ReleaseTypeFrontendStatic {
		e = fs.WalkDir(base.FS(), "assets", func(name string, d fs.DirEntry, e error) error {
			if errors.Is(e, os.ErrNotExist) {
				return nil
			}
			if e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			if d.IsDir() || liveAssets[name] {
				return nil
			}
			st, e := d.Info()
			if e != nil {
				return e
			}
			if time.Since(st.ModTime()) < c.AssetRetention.Value() {
				return nil
			}
			return base.Remove(name)
		})
	}
	return e
}
