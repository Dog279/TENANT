package config

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateKeys = flag.Bool("update", false, "rewrite testdata/keys.golden")

// jsonKeys lists every key path a type marshals to: "a.b" for nested
// objects, "a.*" for map values, "a[]" for slice elements.
func jsonKeys(t reflect.Type, prefix string, out map[string]bool, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Map:
		jsonKeys(t.Elem(), prefix+".*", out, seen)
		return
	case reflect.Slice, reflect.Array:
		jsonKeys(t.Elem(), prefix+"[]", out, seen)
		return
	case reflect.Struct:
	default:
		return
	}
	if t == reflect.TypeOf(time.Time{}) || seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		key := strings.TrimPrefix(prefix+"."+name, ".")
		out[key] = true
		jsonKeys(f.Type, key, out, seen)
	}
}

// config.json, credentials.json and settings.<agent>.json are operator
// interfaces (ADR 0001): a key may be added, never renamed or dropped. This
// pins every key path; after adding a key on purpose, go test
// ./internal/config -run TestTheFileFormatsAreStable -update rewrites the
// list.
func TestTheFileFormatsAreStable(t *testing.T) {
	keys := map[string]bool{}
	for file, v := range map[string]any{"config.json": Launch{}, "credentials.json": Credentials{}, "settings.json": Settings{}} {
		sub := map[string]bool{}
		jsonKeys(reflect.TypeOf(v), "", sub, map[reflect.Type]bool{})
		for k := range sub {
			keys[file+": "+k] = true
		}
	}
	lines := make([]string, 0, len(keys))
	for k := range keys {
		lines = append(lines, k)
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "keys.golden")
	if *updateKeys {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got == want {
		return
	}
	wantSet := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(want), "\n") {
		wantSet[l] = true
	}
	for _, l := range strings.Split(strings.TrimSpace(want), "\n") {
		if !keys[l] {
			t.Errorf("key gone: %s (operators' files still use it)", l)
		}
	}
	for _, l := range lines {
		if !wantSet[l] {
			t.Errorf("key added: %s (if on purpose, rerun with -update)", l)
		}
	}
}
