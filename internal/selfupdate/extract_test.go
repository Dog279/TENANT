package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type entry struct {
	name string
	data []byte
}

func writeTarGz(t *testing.T, path string, entries []entry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := c(); err != nil {
			t.Fatal(err)
		}
	}
}

func writeZip(t *testing.T, path string, entries []entry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []func() error{zw.Close, f.Close} {
		if err := c(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractBinary(t *testing.T) {
	big := make([]byte, 2<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		entries []entry
		want    []byte // nil: an error is expected
		errWant string
	}{
		{"only the member", []entry{{"README.md", []byte("readme")}, {"tenant", []byte("binary")}, {"LICENSE", []byte("mit")}}, []byte("binary"), ""},
		{"member under a directory", []entry{{"tenant_1.0/tenant", []byte("nested")}}, []byte("nested"), ""},
		{"2 MB member streams", []entry{{"tenant", big}}, big, ""},
		{"missing member", []entry{{"README.md", []byte("readme")}}, nil, "no entry named tenant"},
		{"dotdot refused", []entry{{"../tenant", []byte("evil")}}, nil, "refusing"},
		{"absolute refused", []entry{{"/tenant", []byte("evil")}}, nil, "refusing"},
	} {
		for ext, build := range map[string]func(*testing.T, string, []entry){".tar.gz": writeTarGz, ".zip": writeZip} {
			t.Run(tc.name+ext, func(t *testing.T) {
				dir := t.TempDir()
				archive := filepath.Join(dir, "a"+ext)
				build(t, archive, tc.entries)
				dest := filepath.Join(dir, "tenant")
				err := ExtractBinary(archive, dest, "tenant")
				if tc.want == nil {
					if err == nil || !strings.Contains(err.Error(), tc.errWant) {
						t.Fatalf("err = %v, want one mentioning %q", err, tc.errWant)
					}
					if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("a refused archive must write nothing; stat = %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got, _ := os.ReadFile(dest); !bytes.Equal(got, tc.want) {
					t.Fatalf("extracted %d bytes, want %d", len(got), len(tc.want))
				}
				if names := dirNames(t, dir); !slices.Equal(names, []string{"a" + ext, "tenant"}) {
					t.Fatalf("dir holds %v; only the member may be written, and no temp file left", names)
				}
				if info, _ := os.Stat(dest); runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
					t.Fatalf("mode = %o, want 0755", info.Mode().Perm())
				}
			})
		}
	}
	if err := ExtractBinary(filepath.Join(t.TempDir(), "a.rar"), "x", "tenant"); err == nil {
		t.Fatal("an unknown archive type must be refused")
	}
}
