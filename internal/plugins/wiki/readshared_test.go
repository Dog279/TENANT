package wiki_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReadSharedNote_EgressSafety pins the trust-boundary guarantees of the
// peer-facing read (peer_wiki_read → ReadSharedNote): only indexed notes,
// no traversal, and — critically — no following an in-root symlink OUT of the
// wiki root. (Adversarial review TEN-243: the lenient ReadFile follows symlinks.)
func TestReadSharedNote_EgressSafety(t *testing.T) {
	ix, root, _ := mkIndex(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "milk.md"), []byte("# Milk\nfull body text"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-note file that merely sits in the wiki dir — must NOT be readable.
	if err := os.WriteFile(filepath.Join(root, "secret.env"), []byte("API_KEY=topsecret"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A secret OUTSIDE the root, and an in-root symlink that points at it.
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinks := runtime.GOOS != "windows"
	if symlinks {
		if err := os.Symlink(secret, filepath.Join(root, "leak.md")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	if _, _, err := ix.Reindex(ctx); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	// 1. A legitimate indexed note reads fine.
	body, err := ix.ReadSharedNote("milk.md")
	if err != nil || !strings.Contains(body, "full body text") {
		t.Errorf("indexed note should read: body=%q err=%v", body, err)
	}

	// 2. A non-note file in the root is refused (indexed-notes-only).
	if _, err := ix.ReadSharedNote("secret.env"); err == nil {
		t.Error("non-note file in the wiki dir must be refused")
	}

	// 3. Lexical traversal is refused (no absolute path leaked in the error).
	if _, err := ix.ReadSharedNote("../../etc/passwd"); err == nil {
		t.Error("path traversal must be refused")
	} else if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "/etc/") {
		t.Errorf("error must not leak filesystem paths: %v", err)
	}

	// 4. THE egress fix: an in-root symlink pointing OUTSIDE the root is refused.
	//    Defense in depth: the indexer drops it (not in Files → membership fails)
	//    AND ReadSharedNote's EvalSymlinks containment would catch it anyway. The
	//    lenient ReadFile, by contrast, WOULD have leaked the key material.
	if symlinks {
		out, err := ix.ReadSharedNote("leak.md")
		if err == nil || strings.Contains(out, "PRIVATE KEY") {
			t.Errorf("symlink escaping the wiki root must be refused; got out=%q err=%v", out, err)
		}
	}
}

// TestReindex_DropsEscapingSymlink verifies the indexer no longer follows an
// in-root symlink that points OUTSIDE the wiki root (so its target can't leak via
// peer_wiki_search snippets), while an in-root symlink stays indexed (local
// symlinked-notes workflows are unaffected — additive).
func TestReindex_DropsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require admin on Windows")
	}
	ix, root, _ := mkIndex(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("# Real\nreal note"), 0o644); err != nil {
		t.Fatal(err)
	}
	// In-root symlink → another in-root note: MUST stay indexed.
	if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "inlink.md")); err != nil {
		t.Fatal(err)
	}
	// Escaping symlink → a secret outside the root: MUST be dropped.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# Secret\nLEAKED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "leak.md")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ix.Reindex(ctx); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	got := map[string]bool{}
	for _, f := range ix.List() {
		got[f] = true
	}
	if !got["real.md"] {
		t.Error("real note should be indexed")
	}
	if !got["inlink.md"] {
		t.Error("in-root symlink should stay indexed (additive)")
	}
	if got["leak.md"] {
		t.Error("escaping symlink must NOT be indexed (egress at the indexer)")
	}
	// And its secret content must not be searchable.
	hits, _ := ix.Search(ctx, "LEAKED", 5)
	for _, h := range hits {
		if strings.Contains(h.Snippet, "LEAKED") {
			t.Errorf("escaping symlink's content leaked via search: %q", h.Snippet)
		}
	}
}
