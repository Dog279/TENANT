package privacy

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, m)
	}
	return out
}

func TestExportWritesWhatTenantKnows(t *testing.T) {
	fx := newFixture(t)
	out := filepath.Join(t.TempDir(), "export")
	res, err := Export(context.Background(), fx.d, ExportOptions{Out: out,
		Connectors: []Connector{{Name: "gsuite", Stored: []string{"OAuth token"}}}})
	if err != nil {
		t.Fatal(err)
	}
	eps := readJSONL(t, filepath.Join(out, "episodes.jsonl"))
	if len(eps) != 4 {
		t.Fatalf("exported %d episodes, want 4", len(eps))
	}
	for _, e := range eps {
		for k := range e {
			if strings.Contains(k, "embedding") {
				t.Fatalf("an embedding column was exported: %s", k)
			}
		}
	}
	if facts := readJSONL(t, filepath.Join(out, "facts.jsonl")); len(facts) != 4 {
		t.Fatalf("exported %d facts, want 4", len(facts))
	}
	for _, f := range []string{"skills.jsonl", "sme_docs.jsonl", "runs.jsonl", "token_usage.jsonl", "README.md",
		"connectors.json", "profile/profile.main.md", "soul/main.toml", "research/20260501-120000-old/report.md"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(f))); err != nil {
			t.Errorf("export lacks %s: %v", f, err)
		}
	}
	archived, _ := filepath.Glob(filepath.Join(out, "archive", "*", "*.jsonl"))
	if len(archived) != 3 {
		t.Fatalf("exported archive files = %v", archived)
	}
	readme, _ := os.ReadFile(filepath.Join(out, "README.md"))
	if !strings.Contains(string(readme), "episodes.jsonl` (4)") {
		t.Fatalf("README does not describe the files:\n%s", readme)
	}
	if res.Rows["episodes.jsonl"] != 4 || res.Files == 0 {
		t.Fatalf("result: %+v", res)
	}
}

func TestExportOneAgentAndGuards(t *testing.T) {
	fx := newFixture(t)
	out := filepath.Join(t.TempDir(), "export")
	if _, err := Export(context.Background(), fx.d, ExportOptions{Out: out, Agent: "researcher", NoArchive: false}); err != nil {
		t.Fatal(err)
	}
	if eps := readJSONL(t, filepath.Join(out, "episodes.jsonl")); len(eps) != 1 || eps[0]["agent_id"] != "researcher" {
		t.Fatalf("agent filter: %v", eps)
	}
	archived, _ := filepath.Glob(filepath.Join(out, "archive", "*", "*.jsonl"))
	for _, a := range archived {
		b, _ := os.ReadFile(a)
		if strings.Contains(string(b), `"agent_id":"main"`) {
			t.Fatalf("%s holds another agent's events", a)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "research")); !os.IsNotExist(err) {
		t.Fatal("an agent-scoped export included research, which belongs to no agent")
	}
	if _, err := Export(context.Background(), fx.d, ExportOptions{Out: out}); err == nil {
		t.Fatal("exported into a non-empty directory")
	}
	if _, err := Export(context.Background(), fx.d, ExportOptions{Out: filepath.Join(fx.d.DataDir, "x")}); err == nil {
		t.Fatal("exported into the data dir")
	}
	noArch := filepath.Join(t.TempDir(), "e2")
	if _, err := Export(context.Background(), fx.d, ExportOptions{Out: noArch, NoArchive: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(noArch, "archive")); !os.IsNotExist(err) {
		t.Fatal("--no-archive still exported the archive")
	}
}

func TestSessionsAndCounts(t *testing.T) {
	fx := newFixture(t)
	ss, err := Sessions(context.Background(), fx.d, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 3 || ss[0].ID != "sess_c" || ss[2].ID != "sess_a" || ss[2].Turns != 2 || !strings.Contains(ss[2].Opening, "221B") {
		t.Fatalf("sessions: %+v", ss)
	}
	c := Counts(context.Background(), fx.d)
	if c["episodes"] != 4 || c["facts"] != 4 || c["skills"] != 2 || c["runs"] != 4 || c["usage"] != 1 {
		t.Fatalf("counts: %v", c)
	}
}
