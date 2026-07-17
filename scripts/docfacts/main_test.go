package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileLineCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		want    int
	}{
		{name: "empty", content: "", want: 0},
		{name: "newline terminated", content: "one\ntwo\n", want: 2},
		{name: "unterminated final line", content: "one\ntwo", want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.go")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := fileLineCount(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("fileLineCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLibraryVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "version.go")
	if err := os.WriteFile(path, []byte("package fixture\nconst LibraryVersion = \"1.2.3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := libraryVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.2.3" {
		t.Fatalf("libraryVersion() = %q, want 1.2.3", got)
	}
}

func TestRenderMarkdown(t *testing.T) {
	t.Parallel()
	got := renderMarkdown(facts{GoPackages: 47, ProductionLOC: 76001, Version: "0.0.1"})
	for _, want := range []string{"| Go packages | 47 |", "| Production Go LOC | 76,001 |", "| Version | `0.0.1` |"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderMarkdown() missing %q:\n%s", want, got)
		}
	}
}

func TestGeneratedBlockWriteAndCheck(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ARCHITECTURE.md")
	initial := "before\n<!-- docfacts:generated:start; date=old -->\nstale\n<!-- docfacts:generated:end -->\nafter\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	markdown := "| Measure | Result |\n|---|---:|\n| Go packages | 47 |\n"
	if err := writeGeneratedBlock(path, markdown); err != nil {
		t.Fatal(err)
	}
	if err := checkGeneratedBlock(path, markdown); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "date=") || !strings.Contains(got, "before\n") || !strings.Contains(got, "after\n") {
		t.Fatalf("writeGeneratedBlock() damaged surrounding content:\n%s", got)
	}
	if err := checkGeneratedBlock(path, markdown+"changed\n"); err == nil {
		t.Fatal("checkGeneratedBlock() accepted stale content")
	}
}
