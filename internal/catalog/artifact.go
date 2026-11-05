package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }
func sumReader(r io.Reader) (string, error) {
	h := sha256.New()
	_, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), err
}

// Inspect streams the payload without materializing a rootfs image on disk.
// Metadata-only reads are cheap; full reads verify the actual xdelta source.
func Inspect(ctx context.Context, filename, variant string, full bool) (Artifact, error) {
	a := Artifact{Variant: variant}
	f, err := os.Open(filename)
	if err != nil {
		return a, err
	}
	defer f.Close()
	tr := tar.NewReader(contextReader{ctx, f})
	var manifest []byte
	payloads := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return a, err
		}
		switch h.Name {
		case "manifest":
			if h.Size > 1<<20 {
				return a, fmt.Errorf("oversized artifact manifest")
			}
			manifest, err = io.ReadAll(tr)
			if err != nil {
				return a, err
			}
			a.ManifestSHA256 = Hash(manifest)
		case "header.tar.gz":
			if h.Size > 4<<20 {
				return a, fmt.Errorf("oversized artifact header")
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return a, err
			}
			a.HeaderSHA256 = Hash(data)
			gz, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return a, err
			}
			ht := tar.NewReader(io.LimitReader(gz, 8<<20))
			for {
				hh, e := ht.Next()
				if e == io.EOF {
					break
				}
				if e != nil {
					gz.Close()
					return a, e
				}
				if hh.Name == "header-info" {
					var info struct {
						Provides struct {
							Name string `json:"artifact_name"`
						} `json:"artifact_provides"`
						Depends struct {
							Devices []string `json:"device_type"`
						} `json:"artifact_depends"`
					}
					if e := json.NewDecoder(ht).Decode(&info); e != nil {
						gz.Close()
						return a, e
					}
					a.Name = info.Provides.Name
					a.Devices = info.Depends.Devices
				}
			}
			gz.Close()
		default:
			if strings.HasPrefix(h.Name, "data/") && h.Typeflag == tar.TypeReg {
				payloads++
				if full {
					gz, err := gzip.NewReader(tr)
					if err != nil {
						return a, err
					}
					hash := sha256.New()
					count := &countWriter{}
					r := io.TeeReader(gz, io.MultiWriter(hash, count))
					pt := tar.NewReader(r)
					files := 0
					for {
						ph, e := pt.Next()
						if e == io.EOF {
							break
						}
						if e != nil {
							gz.Close()
							return a, e
						}
						if ph.Typeflag == tar.TypeReg {
							files++
							a.RootfsSHA256, e = sumReader(pt)
							if e != nil {
								gz.Close()
								return a, e
							}
						}
					}
					if _, err = io.Copy(io.Discard, r); err != nil {
						gz.Close()
						return a, err
					}
					a.PayloadSize = count.n
					gz.Close()
					if files != 1 {
						return a, fmt.Errorf("expected one rootfs image")
					}
					a.PayloadSHA256 = hex.EncodeToString(hash.Sum(nil))
				}
			}
		}
		if !full && a.Name != "" && a.ManifestSHA256 != "" {
			break
		}
	}
	if a.Name == "" || a.ManifestSHA256 == "" || a.HeaderSHA256 == "" || !strings.HasPrefix(a.Name, "release-") {
		return a, fmt.Errorf("incomplete rootfs artifact metadata")
	}
	compatible := false
	for _, d := range a.Devices {
		compatible = compatible || d == variant
	}
	if !compatible {
		return a, fmt.Errorf("artifact incompatible with %s", variant)
	}
	if full {
		if payloads != 1 {
			return a, fmt.Errorf("expected one rootfs payload")
		}
		match := false
		for _, line := range strings.Split(string(manifest), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == a.RootfsSHA256 && strings.HasPrefix(fields[1], "data/") {
				match = true
			}
		}
		if !match {
			return a, fmt.Errorf("rootfs does not match manifest")
		}
	}
	a.Version = strings.TrimPrefix(a.Name, "release-")
	a.ID = a.Identity()
	return a, nil
}
func Verify(ctx context.Context, filename string, want Artifact) error {
	got, err := Inspect(ctx, filename, want.Variant, true)
	if err != nil {
		return err
	}
	if got.ID != want.ID || got.RootfsSHA256 != want.RootfsSHA256 || got.PayloadSHA256 != want.PayloadSHA256 || got.PayloadSize != want.PayloadSize {
		return fmt.Errorf("artifact bytes do not match %s", want.ID)
	}
	return nil
}
