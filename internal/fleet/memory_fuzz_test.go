package fleet

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	"tenant/internal/fleet/shadow"
)

// FuzzMemoryPiece: every answer a child can give to fleet/memory/get, read
// as the master reads it, after nothing, or after part of a copy.
func FuzzMemoryPiece(f *testing.F) {
	data := base64.StdEncoding.EncodeToString([]byte("0123456789"))
	for _, seed := range []string{
		`{"size":10,"data":"` + data + `"}`,
		`{"size":20,"data":"` + data + `"}`,
		`{"size":0,"data":""}`,
		`{"size":-1,"data":"` + data + `"}`,
		`{"size":1073741825,"data":"` + data + `"}`,
		`{"size":5,"data":"` + data + `"}`,
		`{"size":10,"data":"` + data + `","more":1}`,
		`{"size":10,"size":10,"data":"` + data + `"}`,
		`{"size":10,"data":null}`,
		`{"size":"10","data":"` + data + `"}`,
		`{"size":10,"data":"not base64!"}`,
		`[]`, `null`, ``, `{`, `{"size":10,"data":"` + data + `"}{}`,
	} {
		f.Add([]byte(seed), int64(-1), int64(0))
		f.Add([]byte(seed), int64(20), int64(10))
	}
	f.Fuzz(func(t *testing.T, raw []byte, size, have int64) {
		if size < -1 || have < 0 || (size >= 0 && have > size) {
			return // states the master is never in
		}
		p, err := parseMemoryPiece(raw, size, have)
		if err != nil {
			if p.Size != 0 || p.Data != nil {
				t.Fatal("refused and returned a piece")
			}
			return
		}
		if p.Size < 1 || p.Size > maxMemoryBytes || (size >= 0 && p.Size != size) ||
			len(p.Data) == 0 || len(p.Data) > memoryChunk || have+int64(len(p.Data)) > p.Size {
			t.Fatalf("accepted a piece of %d bytes of a copy of %d, after %d of %d", len(p.Data), p.Size, have, size)
		}
	})
}

// FuzzChildMemoryRequest: every memory request a master's frame can carry,
// answered by a child with a real store that holds one copy. Nothing is kept
// that does not hash to its name and say so in its first line, and the copy
// it holds is never harmed but by a purge that keeps a newer one.
func FuzzChildMemoryRequest(f *testing.F) {
	c, blob := fuzzCopy()
	w := wireCopy(c)
	put := func(p memoryPut) []byte { b, _ := json.Marshal(p); return b }
	get := func(p memoryGet) []byte { b, _ := json.Marshal(p); return b }
	next := append(bytes.Clone(blob[:bytes.IndexByte(blob, '\n')-1]), "2\nmore"...)
	nc := memoryCopy{Key: c.KeyID, Gen: 2, Hash: shadow.Hash(next)}
	for _, seed := range []struct {
		method int
		params []byte
	}{
		{0, nil},
		{1, put(memoryPut{memoryCopy: nc, Size: int64(len(next)), Data: next})},
		{1, put(memoryPut{memoryCopy: nc, Size: int64(len(next)), Data: next[:3]})},
		{1, put(memoryPut{memoryCopy: nc, Size: int64(len(next)), Offset: 3, Data: next[3:]})},
		{1, put(memoryPut{memoryCopy: w, Size: int64(len(blob)), Data: blob})},
		{1, []byte(`{"key":"x","gen":1,"hash":"y","size":1,"offset":0,"data":"AA=="}`)},
		{2, get(memoryGet{memoryCopy: w})},
		{2, get(memoryGet{memoryCopy: w, Offset: 3})},
		{2, get(memoryGet{memoryCopy: w, Offset: 1 << 40})},
		{3, []byte(`{"key":"` + w.Key + `","gen":1,"hash":"` + w.Hash + `"}`)},
		{3, []byte(`{"key":"` + nc.Key + `","gen":2,"hash":"` + nc.Hash + `"}`)},
		{3, []byte(`null`)},
	} {
		f.Add(seed.method, seed.params)
	}
	methods := []string{methodMemoryList, methodMemoryPut, methodMemoryGet, methodMemoryPurge}
	f.Fuzz(func(t *testing.T, method int, params []byte) {
		if method < 0 || method >= len(methods) {
			return
		}
		store, err := shadow.OpenChildStore(filepath.Join(t.TempDir(), "memory"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(c, blob); err != nil {
			t.Fatal(err)
		}
		cl := &Client{cfg: ClientConfig{Memory: store}}
		_, _ = cl.memory(methods[method], params)
		_, _ = cl.memory(methods[method], params) // a second time: the continuation of a transfer
		copies, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, have := range copies {
			got, err := store.Get(have)
			if err != nil || shadow.Hash(got) != have.Hash {
				t.Fatalf("the child keeps a copy that is not what its name says: %+v", have)
			}
		}
		if _, err := store.Get(c); err != nil {
			// Only a purge keeping a newer copy may drop it, and nothing newer
			// is held after a single request but one that came whole.
			kept := false
			for _, have := range copies {
				kept = kept || (have.KeyID == c.KeyID && have.Gen > c.Gen)
			}
			if !kept {
				t.Fatalf("the copy the child held is gone: %v (keeps %+v)", err, copies)
			}
		}
		cl.mem.mu.Lock()
		cl.mem.drop(&cl.mem.in)
		cl.mem.drop(&cl.mem.out)
		cl.mem.mu.Unlock()
	})
}

// fuzzCopy is a copy a child can keep: a first line naming generation 1 of
// a key, and bytes after it. The child cannot read inside a copy, so this is
// all it ever checks.
func fuzzCopy() (shadow.Copy, []byte) {
	key := "0123456789abcdef0123456789abcdef"
	blob := []byte("tenant-fleet-snapshot/2 " + key + " " + strconv.Itoa(1) + "\nsealed bytes")
	return shadow.Copy{KeyID: key, Gen: 1, Hash: shadow.Hash(blob)}, blob
}
