package main

import (
	"bufio"
	"path/filepath"
	"strings"
	"testing"
)

// TEN-477: Modify → Skills → sql edits only that skill and saves; the
// provider, embeddings and gateway steps never run.
func TestModifySections_EditsOneSkill(t *testing.T) {
	picks := []string{"skills", "sql", "save"}
	old := selectOne
	selectOne = func(title string, opts []pickOption, cur string) string {
		v := picks[0]
		picks = picks[1:]
		return v
	}
	defer func() { selectOne = old }()

	db := filepath.Join(t.TempDir(), "tenant.db")
	in := bufio.NewReader(strings.NewReader("y\n" + db + "\n"))
	lc := &launchConfig{Provider: "vllm", Skills: map[string]*skillConfig{}}
	creds := &credentials{Secrets: map[string]string{}}
	fail := func() { t.Fatal("a section that wasn't picked ran") }
	err := modifySections(in, lc, creds, t.TempDir(),
		func() error { fail(); return nil }, fail, fail)
	if err != nil {
		t.Fatal(err)
	}
	if sc := lc.Skills["sql"]; sc == nil || !sc.Enabled || sc.Settings["db"] != db {
		t.Fatalf("sql skill = %+v", lc.Skills["sql"])
	}
	if len(lc.Skills) != 1 || lc.Provider != "vllm" {
		t.Errorf("other config changed: skills %v provider %q", lc.Skills, lc.Provider)
	}
}
