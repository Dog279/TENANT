package episodic

import "testing"

// isCorruptionError must match SQLite's disk-corruption messages so the
// graceful-degradation guard in vec/fts search kicks in instead of killing
// the whole assemble step. Live trigger: user's episodes.db got corrupted
// (WAL didn't checkpoint before a kill); every /goal call against the
// main agent died at "vec scan: database disk image is malformed (11)".
func TestIsCorruptionError(t *testing.T) {
	// Match: every variant the live error wrapping produces.
	for _, in := range []string{
		"database disk image is malformed",
		"episodic: vec scan: database disk image is malformed (11)",
		"file is not a database",
		"SQLITE_CORRUPT",
		"some wrapper: SQLITE_NOTADB: file unrecognized",
	} {
		if !isCorruptionError(stringErr(in)) {
			t.Errorf("isCorruptionError(%q) = false, want true", in)
		}
	}
	// Don't match: every OTHER error must NOT trigger graceful degradation
	// (we DO want context-cancel, syntax errors, etc. to propagate).
	if isCorruptionError(nil) {
		t.Error("isCorruptionError(nil) = true, want false")
	}
	for _, in := range []string{
		"context canceled",
		"sql: no rows in result set",
		"connection refused",
		"syntax error near \"WHERE\"",
	} {
		if isCorruptionError(stringErr(in)) {
			t.Errorf("isCorruptionError(%q) = true, want false", in)
		}
	}
}

type stringErr string

func (s stringErr) Error() string { return string(s) }
