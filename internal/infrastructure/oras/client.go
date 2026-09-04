// Package oras pulls a pinned dist artifact using the standalone ORAS executable.
package oras

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/domain/release"
	"nginx_updata_config/internal/infrastructure/archive"
	"nginx_updata_config/internal/infrastructure/fsutil"
	"nginx_updata_config/internal/infrastructure/process"
)

const ArtifactType = "application/vnd.nginx.frontend.dist.v1"
const titleAnnotation = "org.opencontainers.image.title"

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}
type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

type Client struct {
	Config   config.ORAS
	DataDir  string
	MaxBytes int64
	MaxFiles int
	// run is injectable only inside package tests. Production always uses process.Run.
	run func(context.Context, string, []string) error
}

func (c Client) command(ctx context.Context, dir string, args ...string) error {
	args = append(args, "--registry-config", c.Config.RegistryConfig)
	if c.Config.CAFile != "" {
		args = append(args, "--ca-file", c.Config.CAFile)
	}
	if c.run != nil {
		return c.run(ctx, dir, args)
	}
	// Cloud hosts must not inherit the CI's egress proxy or ORAS disk cache.
	env := []string{"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=", "NO_PROXY=*", "no_proxy=*", "ORAS_CACHE="}
	_, err := process.Run(ctx, dir, env, nil, c.Config.Binary, args...)
	return err
}

// Resolve reads the immutable-SHA tag once; Pull then uses only the returned digest.
func (c Client) Resolve(ctx context.Context, target config.Target, commit string) (string, error) {
	if err := config.ValidateArtifactRepository(target.ArtifactRepository); err != nil {
		return "", err
	}
	if !release.IsCommit(commit) {
		return "", fmt.Errorf("invalid commit ID")
	}
	root, err := os.OpenRoot(c.DataDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	rel := "worktrees/oras-resolve-" + release.ID()
	if err := fsutil.EnsureDirs(root, rel, 0700); err != nil {
		return "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = fsutil.RemoveTree(cleanup, root, rel)
	}()
	work := filepath.Join(c.DataDir, rel)
	if err := c.command(ctx, work, "manifest", "fetch", target.ArtifactRepository+":"+commit, "--output", filepath.Join(work, "manifest.json")); err != nil {
		return "", err
	}
	dir, err := root.OpenRoot(rel)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	raw, err := readLimited(dir, "manifest.json", 128<<10)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(hash[:])
	if _, err := validateManifest(raw, commit, digest, c.MaxBytes); err != nil {
		return "", err
	}
	return digest, nil
}

func (c Client) Pull(ctx context.Context, target config.Target, commit, digest string) (work string, err error) {
	if err = config.ValidateArtifactRepository(target.ArtifactRepository); err != nil {
		return "", err
	}
	if !release.IsCommit(commit) || !release.IsArtifactDigest(digest) || c.MaxBytes <= 0 || c.MaxFiles <= 0 {
		return "", fmt.Errorf("invalid artifact identity or resource limits")
	}
	data, err := os.OpenRoot(c.DataDir)
	if err != nil {
		return "", err
	}
	defer data.Close()
	rel := "worktrees/oras-" + release.ID()
	if err = fsutil.EnsureDirs(data, rel+"/download", 0700); err != nil {
		return "", err
	}
	work = filepath.Join(c.DataDir, rel)
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = fsutil.RemoveTree(cleanup, data, rel)
			// Return the path so the caller can retry bounded cleanup and record a warning.
		}
	}()
	ref := target.ArtifactRepository + "@" + digest
	manifestPath := filepath.Join(work, "oci-manifest.json")
	if err = c.command(ctx, work, "manifest", "fetch", ref, "--output", manifestPath); err != nil {
		return work, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return work, err
	}
	defer root.Close()
	raw, err := readLimited(root, "oci-manifest.json", 128<<10)
	if err != nil {
		return work, err
	}
	m, err := validateManifest(raw, commit, digest, c.MaxBytes)
	if err != nil {
		return work, err
	}
	download := filepath.Join(work, "download")
	if err = c.command(ctx, work, "pull", ref, "--output", download, "--concurrency", "1"); err != nil {
		return work, err
	}
	dest, err := root.OpenRoot("download")
	if err != nil {
		return work, err
	}
	defer dest.Close()
	entries, err := os.ReadDir(download)
	if err != nil {
		return work, err
	}
	if len(entries) != len(m.Layers) {
		return work, fmt.Errorf("unexpected files from ORAS pull")
	}
	for _, layer := range m.Layers {
		if err = verifyLayer(ctx, dest, layer); err != nil {
			return work, err
		}
	}
	if err = verifyOptionalMetadata(dest, m, commit); err != nil {
		return work, err
	}
	if err = fsutil.EnsureDirs(root, target.ServerName, 0700); err != nil {
		return work, err
	}
	site, err := root.OpenRoot(target.ServerName)
	if err != nil {
		return work, err
	}
	defer site.Close()
	file, err := dest.Open("dist.tar.gz")
	if err != nil {
		return work, err
	}
	defer file.Close()
	if err = ExtractGzip(ctx, file, site, c.MaxBytes, c.MaxFiles); err != nil {
		return work, err
	}
	index, err := site.Lstat("index.html")
	if err != nil || !index.Mode().IsRegular() || index.Size() == 0 {
		return work, fmt.Errorf("dist.tar.gz must contain a nonempty index.html at its root")
	}
	return work, nil
}

func readLimited(root *os.Root, name string, max int64) ([]byte, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, fmt.Errorf("invalid or oversized %s", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if len(b) > int(max) {
		return nil, fmt.Errorf("oversized %s", name)
	}
	return b, err
}

func validateManifest(raw []byte, commit, digest string, maxBytes int64) (*manifest, error) {
	sum := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("OCI manifest digest mismatch")
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.SchemaVersion != 2 || m.MediaType != "application/vnd.oci.image.manifest.v1+json" || m.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("expected a frontend dist OCI artifact")
	}
	if m.Annotations["org.opencontainers.image.revision"] != commit {
		return nil, fmt.Errorf("artifact revision does not match requested full commit")
	}
	if m.Config.Size < 0 || m.Config.Size > 4096 || !release.IsArtifactDigest(m.Config.Digest) {
		return nil, fmt.Errorf("invalid OCI config descriptor")
	}
	seen := map[string]bool{}
	var total int64
	for _, layer := range m.Layers {
		name := layer.Annotations[titleAnnotation]
		if name != "dist.tar.gz" && name != "dist.tar.gz.sha256" && name != "manifest.json" {
			return nil, fmt.Errorf("unexpected artifact file %q", name)
		}
		if seen[name] || !release.IsArtifactDigest(layer.Digest) || layer.Size <= 0 || layer.Size > maxBytes-total {
			return nil, fmt.Errorf("invalid or oversized artifact layer")
		}
		if _, ok := layer.Annotations["io.deis.oras.content.unpack"]; ok {
			return nil, fmt.Errorf("ORAS auto-unpack artifacts prohibited")
		}
		if name == "dist.tar.gz" && layer.MediaType != "application/gzip" {
			return nil, fmt.Errorf("dist.tar.gz layer must use application/gzip")
		}
		if name != "dist.tar.gz" && (layer.Size > 64<<10 || (layer.MediaType != "application/json" && layer.MediaType != "text/plain" && layer.MediaType != "application/octet-stream")) {
			return nil, fmt.Errorf("invalid metadata layer")
		}
		seen[name] = true
		total += layer.Size
	}
	if !seen["dist.tar.gz"] {
		return nil, fmt.Errorf("dist.tar.gz layer required")
	}
	return &m, nil
}
func verifyLayer(ctx context.Context, root *os.Root, d descriptor) error {
	name := d.Annotations[titleAnnotation]
	st, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != d.Size {
		return fmt.Errorf("artifact file size/type mismatch: %s", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, fsutil.Reader(ctx, io.LimitReader(f, d.Size+1)))
	if err != nil {
		return err
	}
	if n != d.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != d.Digest {
		return fmt.Errorf("artifact digest mismatch: %s", name)
	}
	return nil
}
func verifyOptionalMetadata(root *os.Root, m *manifest, commit string) error {
	tarDigest := ""
	for _, d := range m.Layers {
		if d.Annotations[titleAnnotation] == "dist.tar.gz" {
			tarDigest = strings.TrimPrefix(d.Digest, "sha256:")
		}
	}
	for _, d := range m.Layers {
		name := d.Annotations[titleAnnotation]
		if name == "dist.tar.gz" {
			continue
		}
		raw, err := readLimited(root, name, 64<<10)
		if err != nil {
			return err
		}
		if name == "dist.tar.gz.sha256" {
			parts := strings.Fields(string(raw))
			if len(parts) != 2 || parts[0] != tarDigest || strings.TrimPrefix(parts[1], "*") != "dist.tar.gz" {
				return fmt.Errorf("invalid dist checksum file")
			}
		} else {
			var meta struct {
				CommitID string `json:"commit_id"`
				SHA256   string `json:"sha256"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil || meta.CommitID != commit || meta.SHA256 != tarDigest {
				return fmt.Errorf("manifest.json commit_id/sha256 mismatch")
			}
		}
	}
	return nil
}
func ExtractGzip(ctx context.Context, input io.Reader, dest *os.Root, maxBytes int64, maxFiles int) error {
	if maxBytes <= 0 || maxBytes > 1<<50 || maxFiles <= 0 || maxFiles > 10000000 {
		return fmt.Errorf("invalid gzip extraction limits")
	}
	gz, err := gzip.NewReader(fsutil.Reader(ctx, input))
	if err != nil {
		return err
	}
	defer gz.Close()
	// Bound payload plus tar headers/padding as well as uncompressed file contents.
	limited := &io.LimitedReader{R: gz, N: maxBytes + int64(maxFiles)*1024 + (1 << 20)}
	if err = archive.Extract(ctx, limited, dest, maxBytes, maxFiles); err != nil {
		return err
	}
	trailing, err := io.ReadAll(io.LimitReader(fsutil.Reader(ctx, limited), (1<<20)+1))
	if err != nil {
		return err
	} // Includes gzip footer/checksum errors.
	if limited.N == 0 || len(trailing) > 1<<20 || len(bytes.Trim(trailing, "\x00")) != 0 {
		return fmt.Errorf("invalid trailing tar payload")
	}
	return nil
}
