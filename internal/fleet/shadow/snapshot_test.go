package shadow

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"filippo.io/age"
)

const (
	testHost   = "m0123456789abcdef"
	otherKeyID = "ffffffffffffffffffffffffffffffff"
)

// sealed is a snapshot of someMemory under a fresh key, and what made it.
type sealedFixture struct {
	key       *Key
	recovery  *Key
	to        []age.Recipient
	meta      meta
	ep, sem   []byte
	blob      []byte
	content   string
	recipient string
}

func newSealed(t testing.TB, embeddings bool) *sealedFixture {
	t.Helper()
	f := &sealedFixture{key: testKey(t)}
	text, recipient := testRecovery(t)
	f.recipient = recipient
	var err error
	if f.recovery, err = ParseRecovery(text); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.recovery.Close() })
	f.to = []age.Recipient{f.key.recipient(), f.recovery.recipient()}
	p := someMemory(t)
	f.content = p.content(t)
	f.ep, f.sem = p.images(t)
	f.meta = meta{Host: testHost, KeyID: f.key.ID(), Gen: 7, Content: f.content, Embeddings: embeddings}
	if f.blob, err = seal(f.meta, f.ep, f.sem, f.to, f.key); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *sealedFixture) open(blob []byte, k *Key, host string) (*snapshot, error) {
	return openSnapshot(context.Background(), bytes.NewReader(blob), k, host)
}

// craft builds a snapshot by hand: any first line, any content.
func craft(t testing.TB, by *Key, header string, to []age.Recipient, level int, parts ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString(header)
	enc, err := age.Encrypt(&out, to...)
	if err != nil {
		t.Fatal(err)
	}
	if level == math.MinInt {
		for _, p := range parts {
			_, _ = enc.Write(p)
		}
	} else {
		gz, _ := gzip.NewWriterLevel(enc, level)
		for _, p := range parts {
			_, _ = gz.Write(p)
		}
		_ = gz.Close()
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if by == nil { // made by someone who could encrypt and could not authenticate
		return out.Bytes()
	}
	return authenticate(t, by, out.Bytes())
}

// authenticate appends by's authenticator to body, as seal does.
func authenticate(t testing.TB, by *Key, body []byte) []byte {
	t.Helper()
	h, done, err := by.mac()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	h.Write(body)
	return h.Sum(bytes.Clone(body))
}

func metaLine(t testing.TB, m meta, ep, sem []byte) []byte {
	t.Helper()
	m.Episodic, m.Semantic = int64(len(ep)), int64(len(sem))
	line, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func TestSnapshot_RoundTrip(t *testing.T) {
	for _, embeddings := range []bool{true, false} {
		f := newSealed(t, embeddings)
		keyID, gen, err := Peek(f.blob)
		if err != nil || keyID != f.key.ID() || gen != 7 {
			t.Fatalf("Peek: %q %d %v", keyID, gen, err)
		}
		if !validHash(Hash(f.blob)) || Hash(f.blob) != Hash(bytes.Clone(f.blob)) {
			t.Fatal("Hash is not a stable SHA-256")
		}
		// Encryption is randomized: the same memory sealed twice is two snapshots.
		if again, _ := seal(f.meta, f.ep, f.sem, f.to, f.key); Hash(again) == Hash(f.blob) {
			t.Fatal("two seals of the same memory are byte-identical")
		}
		for name, k := range map[string]*Key{"the fleet key": f.key, "the recovery identity": f.recovery} {
			s, err := f.open(f.blob, k, testHost)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			want := f.meta
			want.Episodic, want.Semantic = int64(len(f.ep)), int64(len(f.sem))
			if s.meta != want {
				t.Errorf("%s: meta %+v, want %+v", name, s.meta, want)
			}
			if got, err := contentHash(context.Background(), s.episodic.DB, s.semantic.DB); err != nil || got != f.content {
				t.Errorf("%s: the memory read back hashes %s (%v), want %s", name, got, err, f.content)
			}
			var n int
			if err := s.semantic.QueryRow(`SELECT count(*) FROM facts WHERE length(embedding) = 128`).Scan(&n); err != nil || n != 2 {
				t.Errorf("%s: %d facts came back with their vectors (%v), want 2", name, n, err)
			}
			s.close()
		}
		// Nothing of the memory is readable in the snapshot.
		for _, plain := range []string{"nginx", "haproxy", "SQLite format 3", "web1", `"host"`, f.content, testHost} {
			if bytes.Contains(f.blob, []byte(plain)) {
				t.Errorf("the snapshot shows %q in the clear", plain)
			}
		}
	}
}

// Whatever is done to a snapshot's bytes, it is refused or it is the same
// memory. It is never something else.
func TestSnapshot_RefusesAlteredBytes(t *testing.T) {
	f := newSealed(t, false)
	header := bytes.IndexByte(f.blob, '\n') + 1
	check := func(what string, blob []byte) {
		t.Helper()
		s, err := f.open(blob, f.key, testHost)
		if err == nil {
			s.close()
			t.Errorf("%s: accepted", what)
			return
		}
		if !errors.Is(err, ErrDamaged) && !errors.Is(err, ErrForeignKey) {
			t.Errorf("%s: %v, want ErrDamaged or ErrForeignKey", what, err)
		}
	}
	// One bit flipped, at every byte of the first line and of age's header,
	// and at steps through the payload to its last byte.
	positions := []int{len(f.blob) - 1, len(f.blob) - 2, len(f.blob) - 17, len(f.blob) / 2}
	for i := 0; i < min(header+400, len(f.blob)); i++ {
		positions = append(positions, i)
	}
	for i := header + 400; i < len(f.blob); i += 97 {
		positions = append(positions, i)
	}
	for _, i := range positions {
		blob := bytes.Clone(f.blob)
		blob[i] ^= 0x04
		check(fmt.Sprintf("bit flipped at byte %d of %d", i, len(blob)), blob)
	}
	// Cut short anywhere.
	for _, n := range []int{0, 1, header - 1, header, header + 1, header + 50, header + 250, len(f.blob) / 2, len(f.blob) - 17, len(f.blob) - 16, len(f.blob) - 1} {
		check(fmt.Sprintf("cut to %d of %d bytes", n, len(f.blob)), f.blob[:n])
	}
	check("a byte added", append(bytes.Clone(f.blob), 0))
	check("said twice", append(bytes.Clone(f.blob), f.blob...))
	check("the first line said twice", append(bytes.Clone(f.blob[:header]), f.blob...))
	check("no first line", f.blob[header:])
	check("not a snapshot", []byte("SQLite format 3\x00"))
	check("empty", nil)
	check("only a line end", []byte("\n"))
	check("a first line with no end", bytes.Repeat([]byte("x"), 4096))
}

func TestSnapshot_Refusals(t *testing.T) {
	f := newSealed(t, false)
	line := func(keyID string, gen uint64) string { return string(appendHeader(nil, keyID, gen)) }
	ours := line(f.key.ID(), 7)
	inner := metaLine(t, f.meta, f.ep, f.sem)
	other := testKey(t)

	// A database that decrypts, that SQLite will open, and that is not sound.
	corrupt := bytes.Clone(f.sem)
	pageSize := int(binary.BigEndian.Uint16(corrupt[16:18]))
	for i := pageSize; i < 2*pageSize; i++ { // the second page: the root of a table
		corrupt[i] = 0x5a
	}
	// And one whose pages are all in order and whose content breaks its own
	// rules, which only the integrity check's report says.
	unsound, err := openMem(context.Background(), f.sem, false)
	must(t, err)
	_, err = unsound.Exec(`PRAGMA ignore_check_constraints = ON; CREATE TABLE ruled (x CHECK (x > 0)); INSERT INTO ruled VALUES (-1)`)
	must(t, err)
	breaksItsRules, _, err := unsound.serialize(context.Background())
	must(t, err)
	must(t, unsound.Close())
	gen8 := func(m meta) meta { m.Gen = 8; return m }
	hostB := func(m meta) meta { m.Host = "m00000000000000ff"; return m }

	for name, tc := range map[string]struct {
		blob []byte
		key  *Key
		want error
	}{
		// The key id says it is not ours. It is not tried, even though this
		// one was in fact encrypted to our key and would have opened.
		"another key's id on a snapshot that would open": {
			craft(t, nil, line(otherKeyID, 7), f.to, gzip.DefaultCompression, metaLine(t, meta{Host: testHost, KeyID: otherKeyID, Gen: 7}, f.ep, f.sem), f.ep, f.sem),
			f.key, ErrForeignKey},
		"another fleet key": {f.blob, other, ErrForeignKey},
		"our key id pasted onto another key's snapshot": {
			craft(t, f.key, ours, []age.Recipient{other.recipient()}, gzip.DefaultCompression, inner, f.ep, f.sem),
			f.key, ErrDamaged},
		"a recovery identity that is not this fleet's": {f.blob, func() *Key {
			text, _ := testRecovery(t)
			k, err := ParseRecovery(text)
			must(t, err)
			t.Cleanup(func() { _ = k.Close() })
			return k
		}(), ErrNotRecovery},
		"generation changed in the first line": {
			craft(t, f.key, line(f.key.ID(), 8), f.to, gzip.DefaultCompression, inner, f.ep, f.sem), f.key, ErrDamaged},
		"generation changed inside": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, metaLine(t, gen8(f.meta), f.ep, f.sem), f.ep, f.sem), f.key, ErrDamaged},
		"another machine's": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, metaLine(t, hostB(f.meta), f.ep, f.sem), f.ep, f.sem), f.key, ErrDamaged},
		"a database that fails the integrity check": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, metaLine(t, f.meta, f.ep, corrupt), f.ep, corrupt), f.key, ErrDamaged},
		"a database that breaks its own constraints": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, metaLine(t, f.meta, f.ep, breaksItsRules), f.ep, breaksItsRules), f.key, ErrDamaged},
		"a database that is not one": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, metaLine(t, f.meta, f.ep, []byte("not a database at all")), f.ep, []byte("not a database at all")), f.key, ErrDamaged},
		"the two databases swapped": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, metaLine(t, f.meta, f.sem, f.ep), f.sem, f.ep), f.key, nil},
		"a database cut short": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, inner, f.ep, f.sem[:len(f.sem)-pageSize]), f.key, ErrDamaged},
		"data after the databases": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, inner, f.ep, f.sem, []byte("more")), f.key, ErrDamaged},
		"a second compressed stream": {
			craft(t, f.key, ours, f.to, math.MinInt, gzipped(inner, f.ep, f.sem), gzipped([]byte("more"))), f.key, ErrDamaged},
		"bytes after the compressed stream": {
			craft(t, f.key, ours, f.to, math.MinInt, gzipped(inner, f.ep, f.sem), []byte("more")), f.key, ErrDamaged},
		"not compressed": {craft(t, f.key, ours, f.to, math.MinInt, inner, f.ep, f.sem), f.key, ErrDamaged},
		"nothing inside": {craft(t, f.key, ours, f.to, gzip.DefaultCompression), f.key, ErrDamaged},
		"no line inside": {craft(t, f.key, ours, f.to, gzip.DefaultCompression, f.ep, f.sem), f.key, ErrDamaged},
		"the line is not JSON": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, []byte("host=x\n"), f.ep, f.sem), f.key, ErrDamaged},
		"the line is too long": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, bytes.Repeat([]byte(" "), maxMetaLine+1), inner, f.ep, f.sem), f.key, ErrDamaged},
		"a field nobody wrote": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, []byte(strings.Replace(string(inner), "{", `{"admin":true,`, 1)), f.ep, f.sem), f.key, ErrDamaged},
		"a size of zero": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, metaLine(t, f.meta, nil, f.sem), f.sem), f.key, ErrDamaged},
		"a size past the limit": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, []byte(strings.Replace(string(inner), fmt.Sprintf(`"episodic":%d`, len(f.ep)), `"episodic":1099511627776`, 1)), f.ep, f.sem), f.key, ErrDamaged},
		"a negative size": {
			craft(t, f.key, ours, f.to, gzip.DefaultCompression, []byte(strings.Replace(string(inner), fmt.Sprintf(`"semantic":%d`, len(f.sem)), `"semantic":-1`, 1)), f.ep, f.sem), f.key, ErrDamaged},
	} {
		s, err := openSnapshot(context.Background(), bytes.NewReader(tc.blob), tc.key, testHost)
		if err == nil {
			s.close()
		}
		if name == "the two databases swapped" {
			// Each database is whole, so this opens: what a database holds is
			// the stores' business. It is here to show it does no harm.
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
		t.Logf("%s: %v", name, err)
	}
	if _, err := f.open(f.blob, f.key, "m00000000000000ff"); !errors.Is(err, ErrDamaged) {
		t.Errorf("opened as another machine's: %v", err)
	}
	// seal refuses to write what open would refuse to read.
	for name, m := range map[string]meta{
		"no machine":        {KeyID: f.key.ID(), Gen: 1},
		"a path as machine": {Host: "../x", KeyID: f.key.ID(), Gen: 1},
		"no key id":         {Host: testHost, Gen: 1},
		"generation":        {Host: testHost, KeyID: f.key.ID(), Gen: maxGen + 1},
		"content hash":      {Host: testHost, KeyID: f.key.ID(), Gen: 1, Content: "abc"},
	} {
		if _, err := seal(m, f.ep, f.sem, f.to, f.key); err == nil {
			t.Errorf("seal with a bad %s: accepted", name)
		}
	}
	if _, err := seal(f.meta, nil, f.sem, f.to, f.key); err == nil {
		t.Error("seal with an empty database: accepted")
	}
	if _, err := seal(f.meta, f.ep, f.sem, nil, f.key); err == nil {
		t.Error("seal to nobody: accepted")
	}
}

func gzipped(parts ...[]byte) []byte {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	for _, p := range parts {
		_, _ = gz.Write(p)
	}
	_ = gz.Close()
	return out.Bytes()
}

func TestPeek(t *testing.T) {
	id := strings.Repeat("ab", keyIDBytes)
	good := snapMagic + " " + id + " "
	for line, want := range map[string]uint64{
		good + "0\nrest":                   0,
		good + "1\n":                       1,
		good + "9223372036854775807\nrest": maxGen,
	} {
		keyID, gen, err := Peek([]byte(line))
		if err != nil || keyID != id || gen != want {
			t.Errorf("%q: %q %d %v", line, keyID, gen, err)
		}
	}
	for _, line := range []string{
		"", "\n", good + "1", // no line end
		good + "\n", good + "01\n", good + "+1\n", good + "-1\n", good + " 1\n", good + "1 \n", good + "1\r\n",
		good + "0x10\n", good + "1e3\n", good + "１\n",
		good + "9223372036854775808\n",  // one past the largest
		good + "18446744073709551615\n", // the largest uint64
		good + "18446744073709551616\n", // overflows it
		good + "99999999999999999999999999\n",
		snapMagic + " " + strings.ToUpper(id) + " 1\n",
		snapMagic + " " + id[:30] + " 1\n",
		snapMagic + " " + id + "ab 1\n",
		snapMagic + " " + id[:30] + "zz 1\n",
		snapMagic + "  " + id + " 1\n",
		snapMagic + "\t" + id + " 1\n",
		"tenant-fleet-snapshot/1 " + id + " 1\n", // the format before the authenticator
		"tenant-fleet-snapshot/3 " + id + " 1\n",
		" " + good + "1\n",
		good + "1" + strings.Repeat(" ", 200) + "\n",
	} {
		if keyID, gen, err := Peek([]byte(line)); !errors.Is(err, ErrDamaged) {
			t.Errorf("%q: accepted as %q %d (%v)", line, keyID, gen, err)
		}
	}
}

func TestValidHost(t *testing.T) {
	for _, id := range []string{"m0123456789abcdef", "a", "0", "web-1", "a_b", strings.Repeat("a", 64), "com", "com10", "lpt", "null"} {
		if !validHost(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "a.b", "A", "é", "a b", "-a", "_a", strings.Repeat("a", 65),
		"con", "prn", "aux", "nul", "com1", "lpt9", "a\x00", "a\n", "c:", "~"} {
		if validHost(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

// The child's copy: the vectors are gone from the bytes, the rest is there.
func TestWithoutEmbeddings(t *testing.T) {
	ctx := context.Background()
	p := newTestPair(t, nil, nil)
	fillSynthetic(t, p.es, p.ss, 40, 20)
	ep, sem := p.images(t)
	// What the first episode's vector looks like on disk.
	var vector []byte
	must(t, p.ep.QueryRow(`SELECT embedding FROM episodes WHERE id = 1`).Scan(&vector))
	if len(vector) != synthDim*4 || !bytes.Contains(ep, vector[:64]) {
		t.Fatal("the test's vector is not in the image to begin with")
	}
	cep, err := withoutEmbeddings(ctx, ep)
	must(t, err)
	csem, err := withoutEmbeddings(ctx, sem)
	must(t, err)
	if bytes.Contains(cep, vector[:16]) {
		t.Error("the copy still holds vector bytes")
	}
	if len(cep) > len(ep)*3/4 || len(csem) > len(sem)/4 {
		t.Errorf("the copy is not much smaller: episodic %d of %d, semantic %d of %d", len(cep), len(ep), len(csem), len(sem))
	}
	c := newTestPair(t, cep, csem)
	if c.content(t) != p.content(t) {
		t.Error("the copy's content differs")
	}
	var n, withVec int
	must(t, c.sem.QueryRow(`SELECT count(*), sum(length(embedding) > 0) FROM facts`).Scan(&n, &withVec))
	if n != 40 || withVec != 0 {
		t.Errorf("%d facts, %d with a vector; want 40 and 0", n, withVec)
	}
	// The copy is a working store: keyword search finds what was there, and a
	// re-embed fills the vectors in again.
	hits, err := c.es.Search(ctx, episodicQuery("nginx"))
	must(t, err)
	if len(hits) == 0 {
		t.Error("keyword search finds nothing in the copy")
	}
	must(t, c.es.UpdateEmbedding(ctx, 1, testVec(1), "embedder"))
	// The original is untouched.
	var still int
	must(t, p.ep.QueryRow(`SELECT count(*) FROM episodes WHERE length(embedding) > 0`).Scan(&still))
	if still != 20 {
		t.Errorf("the shadow itself lost vectors: %d of 20 left", still)
	}
	if _, err := withoutEmbeddings(ctx, []byte("not a database")); err == nil {
		t.Error("withoutEmbeddings of garbage: accepted")
	}
}

func FuzzPeek(f *testing.F) {
	id := strings.Repeat("0f", keyIDBytes)
	f.Add([]byte(snapMagic + " " + id + " 1\nage-encryption.org/v1\n"))
	f.Add([]byte(snapMagic + " " + id + " 9223372036854775807\n"))
	f.Add([]byte(snapMagic + " " + id + " 18446744073709551616\n"))
	f.Add([]byte(snapMagic + " " + id + " 007\n"))
	f.Add([]byte(snapMagic + "\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, blob []byte) {
		keyID, gen, err := Peek(blob)
		if err != nil {
			if keyID != "" || gen != 0 {
				t.Fatalf("an error with a value: %q %d", keyID, gen)
			}
			return
		}
		// Accepted means: exactly the one spelling that is written.
		if want := appendHeader(nil, keyID, gen); !bytes.HasPrefix(blob, want) {
			t.Fatalf("%q was read as %q", blob[:min(len(blob), 100)], want)
		}
		if !validKeyID(keyID) || gen > maxGen {
			t.Fatalf("accepted key id %q, generation %d", keyID, gen)
		}
	})
}

func FuzzParseMeta(f *testing.F) {
	good := meta{Host: testHost, KeyID: strings.Repeat("0f", keyIDBytes), Gen: 3, Content: strings.Repeat("ab", 32), Episodic: 4096, Semantic: 8192}
	line, _ := json.Marshal(good)
	f.Add(line)
	f.Add([]byte(`{"host":"../../etc","key":"x","gen":18446744073709551615,"episodic":-1,"semantic":1099511627776}`))
	f.Add([]byte(`{"host":"a","key":"0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f","gen":1e3,"episodic":1,"semantic":1}`))
	f.Add([]byte(`{"host":"a"}{"host":"b"}`))
	f.Add([]byte(`[]`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, line []byte) {
		m, err := parseMeta(line)
		if err != nil {
			return
		}
		if err := m.validate(); err != nil {
			t.Fatalf("parseMeta accepted what validate refuses: %v", err)
		}
		if !validHost(m.Host) || !validKeyID(m.KeyID) || m.Gen > maxGen || m.Episodic <= 0 || m.Semantic <= 0 ||
			m.Episodic > MaxSnapshotBytes || m.Semantic > MaxSnapshotBytes {
			t.Fatalf("accepted %+v", m)
		}
		again, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if m2, err := parseMeta(again); err != nil || m2 != m {
			t.Fatalf("%+v does not survive being written again: %+v, %v", m, m2, err)
		}
	})
}

// FuzzOpenSnapshot feeds openSnapshot arbitrary bytes under one fixed key.
// Whatever opens must be exactly the memory that was sealed.
func FuzzOpenSnapshot(f *testing.F) {
	fx := newSealed(f, false)
	header := bytes.IndexByte(fx.blob, '\n') + 1
	f.Add(fx.blob)
	f.Add(fx.blob[:header])
	f.Add(fx.blob[:len(fx.blob)-1])
	f.Add(append(bytes.Clone(fx.blob), 0))
	f.Add(craft(f, nil, string(appendHeader(nil, otherKeyID, 7)), fx.to, gzip.DefaultCompression, metaLine(f, fx.meta, fx.ep, fx.sem), fx.ep, fx.sem))
	f.Add(craft(f, fx.key, string(fx.blob[:header]), fx.to, gzip.DefaultCompression, []byte("{}\n")))
	f.Add([]byte{})
	// Cut short, the authenticator moved to the front, a wrong authenticator,
	// the first version's spelling, and a body signed by another key.
	f.Add(fx.blob[:len(fx.blob)-macSize])
	f.Add(append(bytes.Clone(fx.blob[len(fx.blob)-macSize:]), fx.blob[:len(fx.blob)-macSize]...))
	f.Add(append(bytes.Clone(fx.blob[:len(fx.blob)-macSize]), make([]byte, macSize)...))
	f.Add(append([]byte("tenant-fleet-snapshot/1"), fx.blob[len(snapMagic):]...))
	f.Add(authenticate(f, testKey(f), fx.blob[:len(fx.blob)-macSize]))
	f.Add(fx.blob[:macSize])
	f.Fuzz(func(t *testing.T, blob []byte) {
		s, err := openSnapshot(context.Background(), bytes.NewReader(blob), fx.key, testHost)
		if err != nil {
			if !errors.Is(err, ErrDamaged) && !errors.Is(err, ErrForeignKey) {
				t.Fatalf("an error of neither kind: %v", err)
			}
			return
		}
		defer s.close()
		got, err := contentHash(context.Background(), s.episodic.DB, s.semantic.DB)
		if err != nil || got != fx.content || s.Gen != 7 || s.KeyID != fx.key.ID() || !s.authentic {
			t.Fatalf("something else opened: generation %d, key %s, content %s (%v)", s.Gen, s.KeyID, got, err)
		}
	})
}
