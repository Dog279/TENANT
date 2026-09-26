package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// testFiles is the staged content the stream tests round-trip.
var testFiles = []struct {
	path, body string
	kind       Kind
}{
	{"config/config.json", `{"model":"local"}`, KindFile},
	{"data/episodic.db", "SQLite format 3\x00 pretend", KindSQLite},
	{"data/archive/2026-09.jsonl", "{\"a\":1}\n{\"a\":2}\n", KindLog},
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// stage writes testFiles under a temp dir and returns a manifest for them and
// an opener for writeStream.
func stage(t *testing.T) (*Manifest, func(Entry) (*os.File, error)) {
	t.Helper()
	dir := t.TempDir()
	m := &Manifest{Format: FormatVersion, TenantVersion: "test", CreatedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), Platform: "test/test"}
	for _, f := range testFiles {
		p := filepath.Join(dir, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
		m.Entries = append(m.Entries, Entry{Path: f.path, Kind: f.kind, Size: int64(len(f.body)), SHA256: sum(f.body)})
	}
	return m, func(e Entry) (*os.File, error) { return os.Open(filepath.Join(dir, filepath.FromSlash(e.Path))) }
}

func readAll(t *testing.T, data []byte, ids ...age.Identity) (map[string]string, *Reader, error) {
	t.Helper()
	r, err := NewReader(bytes.NewReader(data), ids)
	if err != nil {
		return nil, nil, err
	}
	got := map[string]string{}
	err = r.Each(func(e Entry, content io.Reader) error {
		b, err := io.ReadAll(content)
		got[e.Path] = string(b)
		return err
	})
	return got, r, err
}

func TestStreamRoundTripPlain(t *testing.T) {
	m, open := stage(t)
	var buf bytes.Buffer
	if err := writeStream(&buf, m, open, nil); err != nil {
		t.Fatal(err)
	}
	got, r, err := readAll(t, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if r.Encrypted() {
		t.Fatal("plain backup reported as encrypted")
	}
	for _, f := range testFiles {
		if got[f.path] != f.body {
			t.Fatalf("%s = %q, want %q", f.path, got[f.path], f.body)
		}
	}
	if r.Manifest().TenantVersion != "test" || len(r.Manifest().Entries) != len(testFiles) {
		t.Fatalf("manifest not carried: %+v", r.Manifest())
	}
}

func scrypt(t *testing.T, pass string) (*age.ScryptRecipient, *age.ScryptIdentity) {
	t.Helper()
	rcp, err := age.NewScryptRecipient(pass)
	if err != nil {
		t.Fatal(err)
	}
	rcp.SetWorkFactor(10) // fast for tests
	id, err := age.NewScryptIdentity(pass)
	if err != nil {
		t.Fatal(err)
	}
	return rcp, id
}

func TestStreamRoundTripPassphrase(t *testing.T) {
	m, open := stage(t)
	rcp, id := scrypt(t, "correct horse")
	var buf bytes.Buffer
	if err := writeStream(&buf, m, open, []age.Recipient{rcp}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("episodic.db")) || bytes.Contains(buf.Bytes(), []byte(`"model"`)) {
		t.Fatal("encrypted backup leaks plaintext names or content")
	}
	if _, _, err := readAll(t, buf.Bytes()); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("no identity: err = %v, want ErrEncrypted", err)
	}
	_, wrong := scrypt(t, "wrong horse")
	if _, _, err := readAll(t, buf.Bytes(), wrong); err == nil {
		t.Fatal("wrong passphrase decrypted the backup")
	}
	got, r, err := readAll(t, buf.Bytes(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Encrypted() || got["config/config.json"] != testFiles[0].body {
		t.Fatalf("encrypted round trip failed: encrypted=%v got=%v", r.Encrypted(), got)
	}
}

func TestStreamRoundTripX25519AndArmor(t *testing.T) {
	m, open := stage(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var bin bytes.Buffer
	if err := writeStream(&bin, m, open, []age.Recipient{id.Recipient()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAll(t, bin.Bytes(), id); err != nil {
		t.Fatalf("binary: %v", err)
	}
	// The age CLI can armor a backup; Tenant reads that form too.
	var armored bytes.Buffer
	aw := armor.NewWriter(&armored)
	if _, err := aw.Write(bin.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAll(t, armored.Bytes(), id); err != nil {
		t.Fatalf("armored: %v", err)
	}
}

// rawTar builds a backup stream entry by entry, so tests can write what
// writeStream never would.
type rawEntry struct {
	name, body string
	typ        byte
}

func rawBackup(t *testing.T, m *Manifest, entries []rawEntry, trailing string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	all := append([]rawEntry{{name: manifestName, body: string(mb)}}, entries...)
	for _, e := range all {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Typeflag: typ, Name: e.name, Size: int64(len(e.body)), Mode: 0o600}
		if typ == tar.TypeSymlink {
			hdr.Size, hdr.Linkname = 0, e.body
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteString(trailing)
	return buf.Bytes()
}

func manifestFor(files ...rawEntry) *Manifest {
	m := &Manifest{Format: FormatVersion, CreatedAt: time.Now().UTC()}
	for _, f := range files {
		m.Entries = append(m.Entries, Entry{Path: f.name, Kind: KindFile, Size: int64(len(f.body)), SHA256: sum(f.body)})
	}
	return m
}

func TestStreamRejectsDamage(t *testing.T) {
	a := rawEntry{name: "data/a.json", body: "alpha"}
	b := rawEntry{name: "data/b.json", body: "bravo"}
	m := manifestFor(a, b)
	cases := []struct {
		name     string
		data     []byte
		contains string
	}{
		{"altered content", rawBackup(t, m, []rawEntry{a, {name: b.name, body: "brav0"}}, ""), "checksum"},
		{"reordered", rawBackup(t, m, []rawEntry{b, a}, ""), "expected data/a.json"},
		{"missing entry", rawBackup(t, m, []rawEntry{a}, ""), "truncated"},
		{"extra entry", rawBackup(t, m, []rawEntry{a, b, {name: "data/c.json", body: "x"}}, ""), "extra entry"},
		{"wrong size", rawBackup(t, m, []rawEntry{a, {name: b.name, body: "bravo!"}}, ""), "manifest says"},
		{"symlink entry", rawBackup(t, manifestFor(rawEntry{name: "data/l", body: ""}), []rawEntry{{name: "data/l", body: "/etc/passwd", typ: tar.TypeSymlink}}, ""), "not a regular file"},
		{"trailing bytes", rawBackup(t, m, []rawEntry{a, b}, "junk"), "unexpected bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readAll(t, tc.data)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.contains)
			}
		})
	}
	good := rawBackup(t, m, []rawEntry{a, b}, "")
	for _, cut := range []int{len(good) - 1, len(good) - 9, len(good) / 2} {
		if _, _, err := readAll(t, good[:cut]); err == nil {
			t.Fatalf("a backup cut to %d of %d bytes was accepted", cut, len(good))
		}
	}
	if _, _, err := readAll(t, good); err != nil {
		t.Fatalf("intact raw backup rejected: %v", err)
	}
}

func TestStreamRejectsTruncatedEncryption(t *testing.T) {
	m, open := stage(t)
	rcp, id := scrypt(t, "pw")
	var buf bytes.Buffer
	if err := writeStream(&buf, m, open, []age.Recipient{rcp}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if _, _, err := readAll(t, data[:len(data)-1], id); err == nil {
		t.Fatal("truncated encrypted backup accepted")
	}
}

func TestStreamRejectsPlaintextSecrets(t *testing.T) {
	s := rawEntry{name: "config/credentials.json", body: `{"k":"v"}`}
	m := manifestFor(s)
	m.Secrets = true
	m.Entries[0].Kind = KindSecret
	_, _, err := readAll(t, rawBackup(t, m, []rawEntry{s}, ""))
	if err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("err = %v, want a refusal of plaintext secrets", err)
	}
}

func TestStreamRejectsNonBackup(t *testing.T) {
	if _, err := NewReader(strings.NewReader("PK\x03\x04 a zip file"), nil); err == nil {
		t.Fatal("accepted a non-backup")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "data/x", Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if _, err := NewReader(&buf, nil); err == nil || !strings.Contains(err.Error(), manifestName) {
		t.Fatalf("tar without a manifest: err = %v", err)
	}
}

func TestWriteStreamDetectsChangedStagedFile(t *testing.T) {
	m, open := stage(t)
	m.Entries[1].SHA256 = sum("something else")
	if err := writeStream(io.Discard, m, open, nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v, want a changed-file error", err)
	}
}

func TestManifestValidate(t *testing.T) {
	ok := sum("x")
	base := func(entries ...Entry) *Manifest {
		return &Manifest{Format: FormatVersion, Entries: entries}
	}
	e := func(p string) Entry { return Entry{Path: p, Kind: KindFile, SHA256: ok} }
	bad := map[string]*Manifest{
		"no format":         {Entries: []Entry{e("data/x")}},
		"newer format":      {Format: FormatVersion + 1},
		"parent traversal":  base(e("data/../x")),
		"inner traversal":   base(e("config/a/../../b")),
		"absolute":          base(e("/data/x")),
		"backslash":         base(e("data\\x")),
		"drive colon":       base(e("data/C:x")),
		"other root":        base(e("home/x")),
		"root only":         base(e("data")),
		"root slash":        base(e("data/")),
		"dot element":       base(e("data/./x")),
		"duplicate":         base(e("data/x"), e("data/x")),
		"case collision":    base(e("data/Readme"), e("data/README")),
		"unknown kind":      base(Entry{Path: "data/x", Kind: "exe", SHA256: ok}),
		"short sha":         base(Entry{Path: "data/x", Kind: KindFile, SHA256: "abc"}),
		"non-hex sha":       base(Entry{Path: "data/x", Kind: KindFile, SHA256: strings.Repeat("z", 64)}),
		"negative size":     base(Entry{Path: "data/x", Kind: KindFile, Size: -1, SHA256: ok}),
		"undeclared secret": base(Entry{Path: "config/credentials.json", Kind: KindSecret, SHA256: ok}),
	}
	for name, m := range bad {
		if err := m.validate(); err == nil {
			t.Errorf("%s: manifest accepted", name)
		}
	}
	good := base(e("data/x"), e("config/sub/y.json"), Entry{Path: "data/z.db", Kind: KindSQLite, SHA256: ok})
	if err := good.validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	withSecret := base(Entry{Path: "config/credentials.json", Kind: KindSecret, SHA256: ok})
	withSecret.Secrets = true
	if err := withSecret.validate(); err != nil {
		t.Fatalf("declared secret rejected: %v", err)
	}
}
