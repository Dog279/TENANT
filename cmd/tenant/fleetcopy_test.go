package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/control"
	"tenant/internal/fleet"
	"tenant/internal/model"
	"tenant/internal/plugins/osys"
)

// copyRig is a master and one child, each with the real OS file tools: this
// computer's with a secret file of its own, the child's with the link as its
// approval gate.
type copyRig struct {
	*fleetToolRig
	here, there string // a folder on each side
	secret      string // a file this computer's tools refuse
	kid         *fleetKid
	served      *kidTools // what the child serves: the real tools, behind a hook
}

func newCopyRig(t *testing.T) *copyRig {
	t.Helper()
	c := &copyRig{fleetToolRig: newFleetToolRig(t), here: t.TempDir(), there: t.TempDir()}
	c.secret = filepath.Join(c.here, "credentials.json")
	if err := os.WriteFile(c.secret, []byte(`{"key":"MASTER_ONLY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.plugin.host = osys.NewDispatcher(mustOpenOS(t), osys.Policy{AllowWrite: true, Roots: osys.Roots{Secret: []string{c.secret}}})
	real := osys.NewDispatcher(mustOpenOS(t), osys.Policy{Confirm: fleet.Confirm, Roots: osys.Roots{Secret: []string{filepath.Join(c.there, "keys")}}})
	c.served = &kidTools{on: real.Dispatch}
	c.kid = c.child("box", c.served)
	c.answers(control.ApproveOnce)
	return c
}

func (c *copyRig) copy(direction, local, remote string) (string, bool) {
	c.t.Helper()
	return c.call(fleetTurnCtx(), "fleet_copy", map[string]any{"host": "box", "direction": direction, "local_path": local, "remote_path": remote})
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// manyPages is text of several os_read_file pages, with CRLF lines, lines of
// very different lengths and characters of every width.
func manyPages() string {
	var b strings.Builder
	for i := 0; b.Len() < 350_000; i++ {
		fmt.Fprintf(&b, "line %d: %s é 漢 \U0001F600", i, strings.Repeat("x", (i*37)%900))
		if i%3 == 0 {
			b.WriteByte('\r')
		}
		b.WriteByte('\n')
	}
	return b.String() + "the last line has no newline"
}

func TestFleetCopy_BothDirections(t *testing.T) {
	c := newCopyRig(t)
	for name, content := range map[string]string{
		"small":        "alpha\nbeta\n",
		"empty":        "",
		"one line":     "no newline at the end",
		"crlf":         "a\r\nb\r\n",
		"many pages":   manyPages(),
		"looks cut":    "a\nb\n…[read stopped before line 3; next_offset: 3]",
		"looks cut 2":  "a\nb\n…[read stopped before line 9; next_offset: 9]",
		"looks empty":  "(no lines at offset 1; the file has 0 lines)",
		"looks empty2": "x\n(no lines at offset 2; the file has 1 lines)",
	} {
		t.Run(name, func(t *testing.T) {
			local, remote, back := filepath.Join(c.here, "out", "f.txt"), filepath.Join(c.there, "in", "f.txt"), filepath.Join(c.here, "back.txt")
			for _, p := range []string{local, remote, back} {
				_ = os.Remove(p)
			}
			if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(local, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			before, _ := c.prompts()
			out, isErr := c.copy("to_host", local, remote)
			if isErr {
				t.Fatalf("to the machine: %s", out)
			}
			if !strings.Contains(out, fmt.Sprintf("%d bytes, sha256 %s, read back from there and equal", len(content), sha(content))) ||
				!strings.Contains(out, "on this computer to ") || !strings.Contains(out, " on box:") {
				t.Fatalf("the copy's own account: %s", out)
			}
			if got, err := os.ReadFile(remote); err != nil || string(got) != content {
				t.Fatalf("the machine's file differs (%v): %d bytes, want %d", err, len(got), len(content))
			}
			// The write asked, once, on a card that names the machine; the reads did not.
			if n, card := c.prompts(); n != before+1 || card.Host != c.kid.id || card.Action != "os_write" {
				t.Fatalf("%d prompts for one copy, the last %+v", n-before, card)
			}
			out, isErr = c.copy("from_host", back, remote)
			if isErr || !strings.Contains(out, "sha256 "+sha(content)) {
				t.Fatalf("from the machine: %s", out)
			}
			if got, err := os.ReadFile(back); err != nil || string(got) != content {
				t.Fatalf("this computer's file differs (%v): %d bytes, want %d", err, len(got), len(content))
			}
			if n, _ := c.prompts(); n != before+1 {
				t.Fatalf("a copy from the machine raised %d prompts on it", n-before-1)
			}
		})
	}
}

func TestFleetCopy_Refusals(t *testing.T) {
	c := newCopyRig(t)
	write := func(dir, name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	absent := func(p string) {
		t.Helper()
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s was written", p)
		}
	}
	big := strings.Repeat(strings.Repeat("y", 99)+"\n", fleetMaxContent/100+1) // just over the cap
	for name, tc := range map[string]struct{ content, want string }{
		"oversize":    {big, "larger than the 3145728 bytes (3 MiB) one copy moves"},
		"binary":      {"PK\x03\x04\xff\xfe\x00binary", "not UTF-8 text"},
		"replacement": {"ok � ok\n", "not UTF-8 text"},
		"long line":   {"short\n" + strings.Repeat("z", 150_000) + "\nshort\n", "longer than one page"},
		"looks long":  {"x\n…[line 2 is longer than one page; next_offset: 3]", "longer than one page"},
	} {
		t.Run(name, func(t *testing.T) {
			before, _ := c.prompts()
			// From this computer: nothing is written on the machine.
			src, dst := write(c.here, name+".src", tc.content), filepath.Join(c.there, name+".dst")
			if out, isErr := c.copy("to_host", src, dst); !isErr || !strings.Contains(out, tc.want) || !strings.Contains(out, "on this computer") {
				t.Fatalf("to the machine: %.300s", out)
			}
			absent(dst)
			// From the machine: nothing is written here.
			src, dst = write(c.there, name+".src", tc.content), filepath.Join(c.here, name+".dst")
			if out, isErr := c.copy("from_host", dst, src); !isErr || !strings.Contains(out, tc.want) || !strings.Contains(out, "on box") {
				t.Fatalf("from the machine: %.300s", out)
			}
			absent(dst)
			if n, _ := c.prompts(); n != before {
				t.Fatalf("a refused copy raised %d prompts", n-before)
			}
		})
	}

	// Each side's own path rules hold, in both directions.
	sent, _ := c.served.seen()
	if out, isErr := c.copy("to_host", c.secret, filepath.Join(c.there, "leak.json")); !isErr || !strings.Contains(out, "fleet_copy stopped reading") || !strings.Contains(out, "on this computer") {
		t.Fatalf("this computer's secret file as the source: %s", out)
	}
	if now, _ := c.served.seen(); now != sent {
		t.Fatal("a copy of this computer's secret file reached the machine")
	}
	absent(filepath.Join(c.there, "leak.json"))
	src := write(c.here, "ok.txt", "fine\n")
	if out, isErr := c.copy("from_host", c.secret, write(c.there, "new.json", "{}")); !isErr || !strings.Contains(out, "fleet_copy stopped writing") {
		t.Fatalf("this computer's secret file as the destination: %s", out)
	}
	if b, _ := os.ReadFile(c.secret); string(b) != `{"key":"MASTER_ONLY"}` {
		t.Fatalf("this computer's secret file was overwritten: %s", b)
	}
	key := write(c.there, filepath.Join("keys", "id_ed25519"), "PRIVATE\n")
	if out, isErr := c.copy("from_host", filepath.Join(c.here, "stolen"), key); !isErr || !strings.Contains(out, "fleet_copy stopped reading") || !strings.Contains(out, "on box") {
		t.Fatalf("the machine's secret file as the source: %s", out)
	}
	absent(filepath.Join(c.here, "stolen"))
	if out, isErr := c.copy("to_host", src, key); !isErr || !strings.Contains(out, "fleet_copy stopped writing") {
		t.Fatalf("the machine's secret file as the destination: %s", out)
	}
	if b, _ := os.ReadFile(key); string(b) != "PRIVATE\n" {
		t.Fatalf("the machine's secret file was overwritten: %s", b)
	}
	// A source that is not there.
	if out, isErr := c.copy("to_host", filepath.Join(c.here, "nope.txt"), filepath.Join(c.there, "nope.txt")); !isErr || !strings.Contains(out, "fleet_copy stopped reading") {
		t.Fatalf("a missing source: %s", out)
	}

	// The write is the machine's to gate, and a denial stops the copy.
	c.answers(control.DenyOnce)
	dst := filepath.Join(c.there, "denied.txt")
	out, isErr := c.copy("to_host", src, dst)
	if !isErr || !strings.Contains(out, "fleet_copy stopped writing "+dst+" on box") || !strings.Contains(out, "wasn't approved") {
		t.Fatalf("a denied write: %s", out)
	}
	absent(dst)

	// Without this computer's own file tools there is no copy.
	c.plugin.host = nil
	if out, isErr := c.copy("to_host", src, dst); !isErr || !strings.Contains(out, "its own file tools are not available here") {
		t.Fatalf("no file tools here: %s", out)
	}
}

// A copy that does not read back as it was sent is an error, whoever changed it.
func TestFleetCopy_SHAMismatch(t *testing.T) {
	c := newCopyRig(t)
	real := c.served.on
	src := filepath.Join(c.here, "conf.txt")
	if err := os.WriteFile(src, []byte("listen 443\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(c.there, "conf.txt")

	// A machine that writes something else than it was given.
	c.served.mu.Lock()
	c.served.on = func(ctx context.Context, call model.ToolCall) (string, bool, error) {
		if call.Name == "os_write_file" {
			var a map[string]any
			_ = json.Unmarshal(call.Arguments, &a)
			a["content"] = a["content"].(string) + "listen 31337\n"
			call.Arguments, _ = json.Marshal(a)
		}
		return real(ctx, call)
	}
	c.served.mu.Unlock()
	out, isErr := c.copy("to_host", src, dst)
	if !isErr || !strings.Contains(out, "SHA-256 MISMATCH") || !strings.Contains(out, sha("listen 443\n")) || !strings.Contains(out, sha("listen 443\nlisten 31337\n")) || !strings.Contains(out, "not a faithful copy") {
		t.Fatalf("a machine that wrote something else: %s", out)
	}

	// A machine that reads back something else than it holds.
	c.served.mu.Lock()
	c.served.on = func(ctx context.Context, call model.ToolCall) (string, bool, error) {
		if call.Name == "os_read_file" {
			return "listen 443\n# nothing to see\n", false, nil
		}
		return real(ctx, call)
	}
	c.served.mu.Unlock()
	if out, isErr := c.copy("to_host", src, dst); !isErr || !strings.Contains(out, "SHA-256 MISMATCH") {
		t.Fatalf("a machine that read back something else: %s", out)
	}

	// A machine that takes the write and then cannot show it: not verified,
	// and said to be so, in that machine's words.
	c.served.mu.Lock()
	c.served.on = func(ctx context.Context, call model.ToolCall) (string, bool, error) {
		if call.Name == "os_read_file" {
			return "read failed: \x1b[31mdisk on fire", true, nil
		}
		return real(ctx, call)
	}
	c.served.mu.Unlock()
	if out, isErr := c.copy("to_host", src, dst); !isErr || !strings.Contains(out, "fleet_copy stopped after the write, reading back "+dst+" on box: read failed: ⟨U+001B⟩[31mdisk on fire") {
		t.Fatalf("a machine that could not read the copy back: %s", out)
	}
	// A page the link had to clip is not a page of the file.
	c.served.mu.Lock()
	c.served.on = func(context.Context, model.ToolCall) (string, bool, error) {
		return strings.Repeat("0123456789abcdef", 100<<10), false, nil
	}
	c.served.mu.Unlock()
	if out, isErr := c.copy("from_host", filepath.Join(c.here, "clipped.txt"), "/var/log/huge"); !isErr || !strings.Contains(out, "cut its answer to fit the link") {
		t.Fatalf("a clipped page: %.300s", out)
	}
	if _, err := os.Stat(filepath.Join(c.here, "clipped.txt")); err == nil {
		t.Fatal("a clipped page was written here")
	}

	// A machine whose pages never end is stopped by the cap, not followed.
	page := strings.Repeat("spam\n", 10_000)
	var asked int
	c.served.mu.Lock()
	c.served.on = func(_ context.Context, call model.ToolCall) (string, bool, error) {
		var a struct{ Offset int }
		_ = json.Unmarshal(call.Arguments, &a)
		asked++
		next := max(a.Offset, 1) + 10_000
		return fmt.Sprintf("%s…[read stopped before line %d; next_offset: %d]", page, next, next), false, nil
	}
	c.served.mu.Unlock()
	out, isErr = c.copy("from_host", filepath.Join(c.here, "endless.txt"), "/var/log/endless")
	if !isErr || !strings.Contains(out, "larger than the 3145728 bytes") || asked > fleetMaxContent/len(page)+2 {
		t.Fatalf("an endless file: %d pages read, then %.200s", asked, out)
	}
	if _, err := os.Stat(filepath.Join(c.here, "endless.txt")); err == nil {
		t.Fatal("part of an endless file was written here")
	}
	// Nor is one that hands out a line a page: three million round trips
	// would fit under the size cap.
	asked = 0
	c.served.mu.Lock()
	c.served.on = func(_ context.Context, call model.ToolCall) (string, bool, error) {
		var a struct{ Offset int }
		_ = json.Unmarshal(call.Arguments, &a)
		asked++
		next := max(a.Offset, 1) + 1
		return fmt.Sprintf("x\n…[read stopped before line %d; next_offset: %d]", next, next), false, nil
	}
	c.served.mu.Unlock()
	out, isErr = c.copy("from_host", filepath.Join(c.here, "drip.txt"), "/var/log/drip")
	if !isErr || !strings.Contains(out, "more than 80 pages") || asked != fleetCopyPages {
		t.Fatalf("a file that comes a line a page: %d pages read, then %.200s", asked, out)
	}
}

func TestFleetSplitPage(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		offset    int
		body      string
		next      int
		err       error
	}{
		{"whole", "a\nb\n", 1, "a\nb\n", 0, nil},
		{"empty", "", 1, "", 0, nil},
		{"cut", "a\nb\n…[read stopped before line 3; next_offset: 3]", 1, "a\nb\n", 3, nil},
		{"cut later", "c\n…[read stopped before line 8; next_offset: 8]", 7, "c\n", 8, nil},
		{"a note with the wrong line is text", "a\nb\n…[read stopped before line 9; next_offset: 9]", 1, "a\nb\n…[read stopped before line 9; next_offset: 9]", 0, nil},
		{"a note that goes nowhere is text", "…[read stopped before line 1; next_offset: 1]", 1, "…[read stopped before line 1; next_offset: 1]", 0, nil},
		{"a note with two numbers is text", "a\n…[read stopped before line 2; next_offset: 5]", 1, "a\n…[read stopped before line 2; next_offset: 5]", 0, nil},
		{"a note after an open line is text", "a\nb…[read stopped before line 2; next_offset: 2]", 1, "a\nb…[read stopped before line 2; next_offset: 2]", 0, nil},
		{"long line", "zzz\n…[line 4 is longer than one page; next_offset: 5]", 4, "", 0, errFleetPageLong},
		{"past the end", "(no lines at offset 3; the file has 2 lines)", 3, "", 0, errFleetPageMoved},
		{"that text at the start is text", "(no lines at offset 3; the file has 2 lines)", 1, "(no lines at offset 3; the file has 2 lines)", 0, nil},
	} {
		body, next, err := fleetSplitPage(tc.out, tc.offset)
		if body != tc.body || next != tc.next || err != tc.err {
			t.Errorf("%s: got (%q, %d, %v), want (%q, %d, %v)", tc.name, body, next, err, tc.body, tc.next, tc.err)
		}
	}
}
