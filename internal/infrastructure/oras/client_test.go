package oras

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nginx_updata_config/internal/config"
)

func sha(b []byte) string { v := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(v[:]) }
func bundle(t *testing.T, headers []tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte{'x'}, int(h.Size)))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestGzipArchiveValidation(t *testing.T) {
	valid := tar.Header{Name: "./index.html", Typeflag: tar.TypeReg, Size: 2, Mode: 0644}
	for _, tc := range []struct {
		name    string
		headers []tar.Header
		max     int64
		broken  bool
		good    bool
	}{
		{"ordinary-root", []tar.Header{{Name: "./", Typeflag: tar.TypeDir}, valid}, 100, false, true},
		{"traversal", []tar.Header{{Name: "./../escape", Typeflag: tar.TypeReg, Size: 1}}, 100, false, false},
		{"symlink", []tar.Header{{Name: "index.html", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}, 100, false, false},
		{"hardlink", []tar.Header{{Name: "index.html", Typeflag: tar.TypeLink, Linkname: "other"}}, 100, false, false},
		{"duplicate", []tar.Header{valid, valid}, 100, false, false},
		{"expanded-limit", []tar.Header{valid}, 1, false, false},
		{"truncated-footer", []tar.Header{valid}, 100, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bundle(t, tc.headers)
			if tc.broken {
				data = data[:len(data)-4]
			}
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = ExtractGzip(context.Background(), bytes.NewReader(data), root, tc.max, 10)
			if (err == nil) != tc.good {
				t.Fatalf("good=%v err=%v", tc.good, err)
			}
		})
	}
}
func TestPinnedPullValidatesManifestAndCleansFailedWork(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tarBytes := bundle(t, []tar.Header{{Name: "./index.html", Typeflag: tar.TypeReg, Mode: 0644, Size: 4}})
	base := manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", ArtifactType: ArtifactType, Config: descriptor{Size: 2, Digest: sha([]byte("{}"))}, Annotations: map[string]string{"org.opencontainers.image.revision": commit}, Layers: []descriptor{{MediaType: "application/gzip", Digest: sha(tarBytes), Size: int64(len(tarBytes)), Annotations: map[string]string{titleAnnotation: "dist.tar.gz"}}}}
	for _, scenario := range []string{"valid", "wrong-revision", "unsafe-layer", "unpack", "corrupt-file", "pull-failed", "wrong-digest"} {
		t.Run(scenario, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var m manifest
			_ = json.Unmarshal(raw, &m)
			switch scenario {
			case "wrong-revision":
				m.Annotations["org.opencontainers.image.revision"] = strings.Repeat("b", 40)
			case "unsafe-layer":
				m.Layers[0].Annotations[titleAnnotation] = "../escape"
			case "unpack":
				m.Layers[0].Annotations["io.deis.oras.content.unpack"] = "true"
			}
			raw, _ = json.Marshal(m)
			digest := sha(raw)
			if scenario == "wrong-digest" {
				digest = "sha256:" + strings.Repeat("0", 64)
			}
			target := config.Target{ServerName: "site", ArtifactRepository: "harbor.example.com/web/site-dist"}
			calls := 0
			c := Client{DataDir: t.TempDir(), MaxBytes: 1 << 20, MaxFiles: 100, Config: config.ORAS{RegistryConfig: "/etc/oras/auth.json"}}
			c.run = func(ctx context.Context, dir string, args []string) error {
				calls++
				all := strings.Join(args, " ")
				if !strings.Contains(all, target.ArtifactRepository+"@"+digest) || strings.Contains(all, ":"+commit) {
					t.Fatalf("unpinned command %v", args)
				}
				var output string
				for i, arg := range args {
					if arg == "--output" {
						output = args[i+1]
					}
				}
				if args[0] == "manifest" {
					return os.WriteFile(output, raw, 0600)
				}
				if scenario == "pull-failed" {
					return errors.New("Harbor unavailable")
				}
				data := tarBytes
				if scenario == "corrupt-file" {
					data = append([]byte(nil), data...)
					data[0] ^= 0xff
				}
				return os.WriteFile(filepath.Join(output, "dist.tar.gz"), data, 0600)
			}
			work, err := c.Pull(context.Background(), target, commit, digest)
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(work, "site", "index.html")); err != nil {
					t.Fatal(err)
				}
				if calls != 2 {
					t.Fatal(calls)
				}
			} else {
				if err == nil {
					t.Fatal("invalid artifact accepted")
				}
				if _, err := os.Stat(work); !os.IsNotExist(err) {
					t.Fatal("failed work not removed", err)
				}
				if scenario != "pull-failed" && scenario != "corrupt-file" && calls != 1 {
					t.Fatal("unsafe manifest reached pull")
				}
			}
		})
	}
}

func TestResolveSHATagThenPullPinnedDigest(t *testing.T) {
	commit := strings.Repeat("a", 40)
	archive := bundle(t, []tar.Header{{Name: "index.html", Typeflag: tar.TypeReg, Mode: 0644, Size: 4}})
	m := manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", ArtifactType: ArtifactType, Config: descriptor{Size: 2, Digest: sha([]byte("{}"))}, Annotations: map[string]string{"org.opencontainers.image.revision": commit}, Layers: []descriptor{{MediaType: "application/gzip", Digest: sha(archive), Size: int64(len(archive)), Annotations: map[string]string{titleAnnotation: "dist.tar.gz"}}}}
	raw, _ := json.Marshal(m)
	digest := sha(raw)
	target := config.Target{ServerName: "site", ArtifactRepository: "harbor.example.com/web/site-dist"}
	c := Client{DataDir: t.TempDir(), MaxBytes: 1 << 20, MaxFiles: 100}
	calls := 0
	c.run = func(ctx context.Context, dir string, args []string) error {
		calls++
		want := target.ArtifactRepository + "@" + digest
		if calls == 1 {
			want = target.ArtifactRepository + ":" + commit
		}
		if !strings.Contains(strings.Join(args, " "), want) {
			t.Fatal(args)
		}
		output := ""
		for i, arg := range args {
			if arg == "--output" {
				output = args[i+1]
			}
		}
		if args[0] == "manifest" {
			return os.WriteFile(output, raw, 0600)
		}
		return os.WriteFile(filepath.Join(output, "dist.tar.gz"), archive, 0600)
	}
	resolved, err := c.Resolve(context.Background(), target, commit)
	if err != nil || resolved != digest {
		t.Fatal(resolved, err)
	}
	if _, err := c.Pull(context.Background(), target, commit, resolved); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}
