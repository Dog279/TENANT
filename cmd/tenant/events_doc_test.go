package main

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

var update = flag.Bool("update", false, "rewrite docs/EVENTS.md from the event catalog")

const eventsDocHead = `# Event reference

<!-- Generated from the event catalog: go test ./cmd/tenant -run TestEventsDoc -update. Do not edit by hand. -->

Every catalogued event Tenant writes: what it means, why it happens and what to
do about it. The definitions live in the code, in ` + "`internal/logs/catalog`" + ` and in
each package that declares a provider. This file, the dashboard's Help tab and
` + "`tenant logs catalog`" + ` are all rendered from them. To declare an event, see
[LOGGING.md](LOGGING.md#event-ids-the-reference-and-coverage).

IDs are unique within their provider and never reused: a retired ID stays
here, marked retired. They are assigned in blocks: 1–999 lifecycle, 1000s
operations, 2000s warnings, 3000s errors, 4000s security and audit, 5000s
configuration, 9000s diagnostics.

`

// docs/EVENTS.md is rendered from every provider linked into the binary,
// which is why this test lives in cmd/tenant. It also holds the whole catalog
// to its conventions, and refuses to drop an ID the committed file lists.
func TestEventsDoc(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "EVENTS.md")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	have := strings.ReplaceAll(string(raw), "\r\n", "\n")
	specs := catalog.Specs("")
	if err := errors.Join(event.Validate(), catalog.Check(specs, have)); err != nil {
		t.Fatal(err)
	}
	want := eventsDocHead + catalog.Markdown(specs)
	if have == want {
		return
	}
	if !*update {
		t.Fatal("docs/EVENTS.md is out of date with the event catalog; run go test ./cmd/tenant -run TestEventsDoc -update")
	}
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
}
