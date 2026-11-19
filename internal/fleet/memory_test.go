package fleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet/shadow"
	"tenant/internal/mcp"
	"tenant/internal/memory/semantic"
)

// The memory methods of the link (memory.go), both ends for real over
// loopback TLS. What the master does with what comes back (who wins) is
// cmd/tenant's; here it is only what crosses the link and what is refused.

// snapshots makes n real generations of one machine's memory with a fleet
// key of their own, each with one more fact than the last, and returns their
// names and bytes, oldest first.
func snapshots(t *testing.T, host string, n int) ([]shadow.Copy, [][]byte) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	key := must(shadow.CreateKey(filepath.Join(dir, "fleet-key")))(t)
	t.Cleanup(func() { _ = key.Close() })
	identity, recipient, err := shadow.NewRecovery()
	if err != nil {
		t.Fatal(err)
	}
	_ = identity.Release()
	s := must(shadow.OpenStore(filepath.Join(dir, "fleet"), key, recipient, shadow.Options{}))(t)
	defer s.Close()
	var copies []shadow.Copy
	var blobs [][]byte
	for i := range n {
		p := must(s.Acquire(ctx, host))(t)
		long := time.Unix(1_600_000_000, 0)
		if _, err := p.Semantic.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "fact " + strconv.Itoa(i), FirstSeen: long, LastConfirmed: long, EmbedderID: "embedder", Embedding: []float32{1, 2, 3}}); err != nil {
			t.Fatal(err)
		}
		p.Release()
		if _, _, err := s.Mint(ctx, host); err != nil {
			t.Fatal(err)
		}
		c, blob := must3(s.Newest(host))(t)
		copies, blobs = append(copies, c), append(blobs, blob)
	}
	return copies, blobs
}

func must3[A, B any](a A, b B, err error) func(testing.TB) (A, B) {
	return func(t testing.TB) (A, B) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a, b
	}
}

// memoryKid is a confirmed machine, connected, that keeps copies of its
// memory in its own directory.
func (r *rig) memoryKid(t *testing.T, name string) *kid {
	t.Helper()
	k := r.kid(t, name, StateConfirmed)
	k.memory = must(shadow.OpenChildStore(filepath.Join(k.dir, "memory")))(t)
	k.client()
	k.start()
	k.await(LinkConnected)
	r.awaitOnline(t, k.mach.ID, true)
	return k
}

func memCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// A child from before the memory methods says "no such method": the master
// hears that its machine keeps no copy yet, and the link is not harmed.
func TestMemoryAChildThatDoesNotKnowTheMethods(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "old") // no memory store: answers as an older child does
	copies, blobs := snapshots(t, k.mach.ID, 1)
	ctx := memCtx(t, waitLong)
	if _, err := r.mgr.MemoryList(ctx, k.mach.ID); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("list on an old child: %v, want ErrNoMemory", err)
	}
	if err := r.mgr.MemoryPut(ctx, k.mach.ID, copies[0], blobs[0]); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("put on an old child: %v, want ErrNoMemory", err)
	}
	if _, err := r.mgr.MemoryGet(ctx, k.mach.ID, copies[0]); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("get on an old child: %v, want ErrNoMemory", err)
	}
	if err := r.mgr.MemoryPurge(ctx, k.mach.ID, copies[0]); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("purge on an old child: %v, want ErrNoMemory", err)
	}
	r.echo(t, k.mach.ID)
	if st := r.status(t, k.mach.ID); !st.Online {
		t.Fatal("the link ended")
	}
}

// What the master refuses to send, before it sends anything.
func TestMemoryPutRefusals(t *testing.T) {
	r := newRig(t)
	k := r.memoryKid(t, "web1")
	copies, blobs := snapshots(t, k.mach.ID, 1)
	c, blob := copies[0], blobs[0]
	ctx := memCtx(t, waitLong)
	for name, tc := range map[string]struct {
		c    shadow.Copy
		blob []byte
	}{
		"the bytes are not the stated hash's": {c, append(bytes.Clone(blob), 'x')},
		"no bytes":                            {shadow.Copy{KeyID: c.KeyID, Gen: 1, Hash: shadow.Hash(nil)}, nil},
		"not a key id":                        {shadow.Copy{KeyID: "../x", Gen: c.Gen, Hash: c.Hash}, blob},
		"generation 0":                        {shadow.Copy{KeyID: c.KeyID, Gen: 0, Hash: c.Hash}, blob},
		"a hash in capitals":                  {shadow.Copy{KeyID: c.KeyID, Gen: c.Gen, Hash: strings.ToUpper(c.Hash)}, blob},
	} {
		if err := r.mgr.MemoryPut(ctx, k.mach.ID, tc.c, tc.blob); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	// The child checks too: a snapshot that is not the generation it is
	// stated to be is refused there, and nothing is kept.
	forged := shadow.Copy{KeyID: c.KeyID, Gen: c.Gen + 1, Hash: c.Hash}
	if err := r.mgr.MemoryPut(ctx, k.mach.ID, forged, blob); err == nil {
		t.Error("a copy stated as the wrong generation was kept")
	}
	if got := must(r.mgr.MemoryList(ctx, k.mach.ID))(t); len(got) != 0 {
		t.Fatalf("the child keeps %+v", got)
	}
	r.echo(t, k.mach.ID)
}

// Copies go to the child and come back byte for byte, larger than a frame
// included; the child keeps three, refuses an older one as stale and the same
// generation with other content as a conflict, and purges on order.
func TestMemoryRoundTrip(t *testing.T) {
	r := newRig(t)
	k := r.memoryKid(t, "web1")
	ctx := memCtx(t, 2*waitLong)
	copies, blobs := snapshots(t, k.mach.ID, 4)
	for i := range copies {
		if err := r.mgr.MemoryPut(ctx, k.mach.ID, copies[i], blobs[i]); err != nil {
			t.Fatalf("put generation %d: %v", copies[i].Gen, err)
		}
	}
	got := must(r.mgr.MemoryList(ctx, k.mach.ID))(t)
	if len(got) != 3 || got[0].Gen != 4 || got[2].Gen != 2 || !got[0].OK {
		t.Fatalf("the child lists %+v", got)
	}
	back := must(r.mgr.MemoryGet(ctx, k.mach.ID, copies[3]))(t)
	if !bytes.Equal(back, blobs[3]) {
		t.Fatal("the copy came back different")
	}
	if err := r.mgr.MemoryPut(ctx, k.mach.ID, copies[0], blobs[0]); !errors.Is(err, shadow.ErrStale) {
		t.Fatalf("an older generation: %v, want ErrStale", err)
	}
	// The same generation under the same key with other content, as a master
	// restored from an older backup makes: the child's stands.
	other := append(bytes.Clone(blobs[3][:bytes.IndexByte(blobs[3], '\n')+1]), "other content"...)
	if err := r.mgr.MemoryPut(ctx, k.mach.ID, shadow.Copy{KeyID: copies[3].KeyID, Gen: 4, Hash: shadow.Hash(other)}, other); !errors.Is(err, shadow.ErrConflict) {
		t.Fatalf("the same generation with other content: %v, want ErrConflict", err)
	}
	if _, err := r.mgr.MemoryGet(ctx, k.mach.ID, copies[0]); !errors.Is(err, shadow.ErrNoCopy) {
		t.Fatalf("a copy the child dropped: %v, want ErrNoCopy", err)
	}
	if err := r.mgr.MemoryPurge(ctx, k.mach.ID, copies[3]); err != nil {
		t.Fatal(err)
	}
	if got := must(r.mgr.MemoryList(ctx, k.mach.ID))(t); len(got) != 1 || got[0].Gen != 4 {
		t.Fatalf("after the purge the child lists %+v", got)
	}
	if err := r.mgr.MemoryPurge(ctx, k.mach.ID, copies[0]); !errors.Is(err, shadow.ErrNoCopy) {
		t.Fatalf("a purge keeping a copy the child does not have: %v", err)
	}
	// What the child keeps is the bytes it was sent, and nothing readable.
	entries := must(os.ReadDir(filepath.Join(k.dir, "memory")))(t)
	for _, e := range entries {
		raw := must(os.ReadFile(filepath.Join(k.dir, "memory", e.Name())))(t)
		if bytes.Contains(raw, []byte("fact 3")) || bytes.Contains(raw, []byte("SQLite format")) {
			t.Fatalf("%s holds readable memory", e.Name())
		}
	}

	// A copy larger than one piece: the pieces go in order and the whole is
	// checked. Generation 5, under the same key, padded with a header that
	// says so: the child checks the hash and the first line, not the inside.
	head := blobs[3][:bytes.IndexByte(blobs[3], '\n')]
	big := append(bytes.Clone(head[:bytes.LastIndexByte(head, ' ')+1]), "5\n"...)
	big = append(big, bytes.Repeat([]byte{0x5a}, 2*memoryChunk+1234)...)
	if key, gen, err := shadow.Peek(big); err != nil || key != copies[3].KeyID || gen != 5 {
		t.Fatalf("the padded copy's first line: %v %v %v", key, gen, err)
	}
	bc := shadow.Copy{KeyID: copies[3].KeyID, Gen: 5, Hash: shadow.Hash(big)}
	if err := r.mgr.MemoryPut(ctx, k.mach.ID, bc, big); err != nil {
		t.Fatal(err)
	}
	if back := must(r.mgr.MemoryGet(ctx, k.mach.ID, bc))(t); !bytes.Equal(back, big) {
		t.Fatal("the large copy came back different")
	}
	r.echo(t, k.mach.ID)
}

// --- hostile children ---

// liar is a confirmed machine whose child answers the memory methods with
// serve, and hello honestly.
func (r *rig) liar(t *testing.T, name string, serve func(c *rawChild, msg mcp.Message) bool) string {
	t.Helper()
	mach, id := r.add(t, name, StateConfirmed)
	if _, err := r.dialRaw(t, id, func(c *rawChild, msg mcp.Message) bool {
		if msg.Method == methodHello {
			c.reply(msg, `{"link":1,"os":"linux","ceiling":"full"}`)
			return true
		}
		return serve(c, msg)
	}); err != nil {
		t.Fatal(err)
	}
	r.awaitOnline(t, mach.ID, true)
	return mach.ID
}

func (c *rawChild) refuseWith(to mcp.Message, code int) {
	_ = c.send(`{"jsonrpc":"2.0","id":` + string(to.ID.Raw()) + `,"error":{"code":` + strconv.Itoa(code) + `,"message":"no"}}`)
}

// Everything a hostile child can say about its copies, and fetch answers
// that do not add up: each is refused, nothing it sent is returned, and the
// link to an honest machine is not touched.
func TestMemoryHostileChild(t *testing.T) {
	h := newHostile(t)
	host := "m00000000000000aa"
	copies, blobs := snapshots(t, host, 2)
	c, blob := copies[1], blobs[1]
	other, otherBlobs := snapshots(t, host, 1) // another fleet key's
	piece := func(size int64, data []byte) string {
		return `{"size":` + strconv.FormatInt(size, 10) + `,"data":"` + base64.StdEncoding.EncodeToString(data) + `"}`
	}
	type answer func(c *rawChild, msg mcp.Message, n int) bool
	pieces := func(sizes []int64, datas [][]byte) answer {
		return func(c *rawChild, msg mcp.Message, n int) bool {
			if n >= len(datas) {
				return false // stalls
			}
			c.reply(msg, piece(sizes[n], datas[n]))
			return true
		}
	}
	half := len(blob) / 2
	liars := 0
	for name, tc := range map[string]struct {
		list string // the answer to fleet/memory/list; "" fetches c instead
		get  answer
	}{
		"the list is not JSON":                  {list: `[{`},
		"the list is an object":                 {list: `{"copies":[]}`},
		"a copy with a field no master writes":  {list: `[{"key":"` + c.KeyID + `","gen":1,"hash":"` + c.Hash + `","ok":true,"path":"/etc/shadow"}]`},
		"a key id that is a path":               {list: `[{"key":"../../../../etc","gen":1,"hash":"` + c.Hash + `","ok":true}]`},
		"generation zero":                       {list: `[{"key":"` + c.KeyID + `","gen":0,"hash":"` + c.Hash + `","ok":true}]`},
		"a generation past the last one":        {list: `[{"key":"` + c.KeyID + `","gen":18446744073709551615,"hash":"` + c.Hash + `","ok":true}]`},
		"a generation that is negative":         {list: `[{"key":"` + c.KeyID + `","gen":-1,"hash":"` + c.Hash + `","ok":true}]`},
		"a list too long":                       {list: "[" + strings.Repeat(`{"key":"`+c.KeyID+`","gen":1,"hash":"`+c.Hash+`","ok":true},`, 200) + `{"key":"` + c.KeyID + `","gen":1,"hash":"` + c.Hash + `","ok":true}]`},
		"bytes that do not hash to the copy":    {get: pieces([]int64{int64(len(blob))}, [][]byte{append(bytes.Clone(blob[:len(blob)-1]), 0)})},
		"another key's snapshot under its name": {get: pieces([]int64{int64(len(otherBlobs[0]))}, [][]byte{otherBlobs[0]})},
		"a size larger than any snapshot":       {get: pieces([]int64{maxMemoryBytes + 1}, [][]byte{blob[:10]})},
		"a size of zero":                        {get: pieces([]int64{0}, [][]byte{blob[:10]})},
		"a size that changes":                   {get: pieces([]int64{int64(len(blob)), int64(len(blob)) + 1}, [][]byte{blob[:half], blob[half:]})},
		"a piece that runs past the end":        {get: pieces([]int64{10}, [][]byte{blob[:11]})},
		"an empty piece":                        {get: pieces([]int64{int64(len(blob))}, [][]byte{nil})},
		"stalls after the first piece":          {get: pieces([]int64{int64(len(blob))}, [][]byte{blob[:half]})},
		"a field the piece does not have": {get: func(c *rawChild, msg mcp.Message, _ int) bool {
			c.reply(msg, `{"size":10,"data":"AAAAAAAAAAAAAA==","more":true}`)
			return true
		}},
		"a refusal with the stale code to a get": {get: func(c *rawChild, msg mcp.Message, _ int) bool {
			c.refuseWith(msg, codeStale)
			return true
		}},
	} {
		t.Run(name, func(t *testing.T) {
			n := 0
			liars++
			id := h.liar(t, "liar"+strconv.Itoa(liars), func(rc *rawChild, msg mcp.Message) bool {
				switch msg.Method {
				case methodMemoryList:
					rc.reply(msg, tc.list)
					return true
				case methodMemoryGet:
					handled := tc.get(rc, msg, n)
					n++
					return handled
				}
				return false
			})
			ctx := memCtx(t, 3*time.Second)
			if tc.list != "" {
				if got, err := h.mgr.MemoryList(ctx, id); err == nil {
					t.Fatalf("taken: %+v", got)
				}
				return
			}
			want := c
			if name == "another key's snapshot under its name" {
				want = shadow.Copy{KeyID: c.KeyID, Gen: other[0].Gen, Hash: other[0].Hash}
			}
			start := time.Now()
			if got, err := h.mgr.MemoryGet(ctx, id, want); err == nil {
				t.Fatalf("taken: %d bytes", len(got))
			}
			if time.Since(start) > 10*time.Second {
				t.Fatal("the fetch was not bounded by its deadline")
			}
		})
	}
	h.intact(t)
}

// What a child refuses of a put that is not in order.
func TestMemoryChildRefusesPiecesOutOfOrder(t *testing.T) {
	r := newRig(t)
	k := r.memoryKid(t, "web1")
	copies, blobs := snapshots(t, k.mach.ID, 1)
	c, blob := copies[0], blobs[0]
	wc := wireCopy(c)
	call := func(p memoryPut) error {
		raw, _ := json.Marshal(p)
		_, err := k.c.memory(methodMemoryPut, raw)
		return err
	}
	size := int64(len(blob))
	half := size / 2
	for name, p := range map[string]memoryPut{
		"a piece not at the start, with nothing before it": {memoryCopy: wc, Size: size, Offset: half, Data: blob[half:]},
		"a size of zero":            {memoryCopy: wc, Size: 0, Offset: 0, Data: blob[:1]},
		"a size past the limit":     {memoryCopy: wc, Size: maxMemoryBytes + 1, Offset: 0, Data: blob[:1]},
		"a piece past the size":     {memoryCopy: wc, Size: 1, Offset: 0, Data: blob[:2]},
		"no data":                   {memoryCopy: wc, Size: size, Offset: 0},
		"a negative offset":         {memoryCopy: wc, Size: size, Offset: -1, Data: blob[:1]},
		"a piece larger than limit": {memoryCopy: wc, Size: maxMemoryBytes, Offset: 0, Data: make([]byte, memoryChunk+1)},
		"not a copy's name":         {memoryCopy: memoryCopy{Key: "x", Gen: 1, Hash: c.Hash}, Size: size, Data: blob},
	} {
		if err := call(p); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	// The second piece of a copy, then the second piece of another one.
	if err := call(memoryPut{memoryCopy: wc, Size: size, Offset: 0, Data: blob[:half]}); err != nil {
		t.Fatal(err)
	}
	if err := call(memoryPut{memoryCopy: memoryCopy{Key: c.KeyID, Gen: 9, Hash: c.Hash}, Size: size, Offset: half, Data: blob[half:]}); err == nil {
		t.Error("a piece of another copy was taken as the rest of this one")
	}
	// That dropped the copy being received: its rest is refused too.
	if err := call(memoryPut{memoryCopy: wc, Size: size, Offset: half, Data: blob[half:]}); err == nil {
		t.Error("the rest of a dropped copy was taken")
	}
	if got := must(k.memory.List())(t); len(got) != 0 {
		t.Fatalf("the child keeps %+v", got)
	}
	// Bytes that do not hash to the name: refused by the store when whole.
	flipped := bytes.Clone(blob)
	flipped[len(flipped)-1] ^= 1
	if err := call(memoryPut{memoryCopy: wc, Size: size, Offset: 0, Data: flipped}); err == nil {
		t.Error("bytes that are not the copy's were kept")
	}
	if got := must(k.memory.List())(t); len(got) != 0 {
		t.Fatalf("the child keeps %+v", got)
	}
}
