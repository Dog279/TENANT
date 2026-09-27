package config

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"tenant/internal/logs/catalog"
)

// saveAudited writes data to path through write, then files the Security
// record of the change (docs/design/logging-v2.md §10.1): each setting that
// changed, old → new, with every secret value replaced by its name. It is
// the one place config.json and settings.<agent>.json are written, so every
// caller is covered. A save that changes nothing files nothing.
func saveAudited(path string, data []byte, write func(string, []byte) error) error {
	old, _ := os.ReadFile(path) // missing: every setting is new
	if err := write(path, data); err != nil {
		return err
	}
	if diff := configDiff(old, data); len(diff) > 0 {
		// A record that fails to commit is the log book's to report (§10.3);
		// the change itself is made.
		_ = catalog.ConfigChanged.Audit(context.Background(), "file", filepath.Base(path), "changes", len(diff), "diff", strings.Join(diff, "\n"))
	}
	return nil
}

// configDiff lists the leaves that differ between two JSON documents, one
// line each: path: old → new, (none) for a side that lacks it. An
// unreadable document counts as empty.
func configDiff(old, cur []byte) []string {
	var a, b any
	_ = json.Unmarshal(old, &a)
	_ = json.Unmarshal(cur, &b)
	var out []string
	diffValue(&out, "", "", "", a, b)
	return out
}

// diffValue walks a and b together. Objects are compared key by key and
// arrays index by index (ponytail: an entry removed from the middle of a
// list shows as every later entry changing; match by id if that gets noisy).
func diffValue(out *[]string, path, parent, key string, a, b any) {
	if reflect.DeepEqual(a, b) {
		return
	}
	am, aObj := a.(map[string]any)
	bm, bObj := b.(map[string]any)
	if (aObj || a == nil) && (bObj || b == nil) {
		keys := slices.Collect(maps.Keys(am))
		for k := range bm {
			if _, ok := am[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			diffValue(out, p, key, k, am[k], bm[k])
		}
		return
	}
	as, aArr := a.([]any)
	bs, bArr := b.([]any)
	if (aArr || a == nil) && (bArr || b == nil) {
		for i := range max(len(as), len(bs)) {
			var x, y any
			if i < len(as) {
				x = as[i]
			}
			if i < len(bs) {
				y = bs[i]
			}
			diffValue(out, path+"["+strconv.Itoa(i)+"]", parent, key, x, y)
		}
		return
	}
	secret := parent == "env_set" || secretSetting(key)
	*out = append(*out, path+": "+showSetting(path, a, secret)+" → "+showSetting(path, b, secret))
}

// secretSetting reports whether a setting named key holds a secret value:
// dashboard.auth, and any key ending as the log scrubber's credential keys
// do. The values of env_set, the variables handed to an MCP server, are
// secrets too, whatever their names.
func secretSetting(key string) bool {
	k := strings.ToLower(key)
	if k == "auth" {
		return true
	}
	for _, w := range []string{"password", "passwd", "secret", "token", "apikey", "api_key", "api-key"} {
		if strings.HasSuffix(k, w) {
			return true
		}
	}
	return false
}

func showSetting(path string, v any, secret bool) string {
	switch {
	case v == nil:
		return "(none)"
	case secret:
		return "«secret:" + path + "»"
	}
	b, _ := json.Marshal(v)
	return string(b)
}
