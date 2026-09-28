package privacy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/memory/archive"
)

// ExportOptions says what Export writes and where.
type ExportOptions struct {
	// Out is the directory to write. It must not exist yet, or be empty.
	Out string
	// NoArchive leaves out the raw session events, which can be large.
	NoArchive bool
	// Agent limits the export to one agent's data; empty means every agent.
	Agent string
	// Connectors name what each connected service has stored (names of
	// things, never their values), for the README.
	Connectors []Connector
	// TenantVersion stamps the README.
	TenantVersion string
}

// Connector is a connected service and what Tenant keeps for it.
type Connector struct {
	Name   string
	Stored []string
}

// ExportResult says what Export wrote.
type ExportResult struct {
	Dir   string
	Files int
	Bytes int64
	// Rows counts the rows written per table file.
	Rows map[string]int
}

// exportTable is one store table written as JSON Lines.
type exportTable struct {
	db, table, file, about string
}

var exportTables = []exportTable{
	{episodesDB, "episodes", "episodes.jsonl", "every conversation turn Tenant remembers (tombstoned = removed from recall but not yet deleted)"},
	{factsDB, "facts", "facts.jsonl", "facts distilled from conversations, with their sources and confidence"},
	{factsDB, "fact_signals", "fact_signals.jsonl", "per-fact importance, pins and use counts"},
	{factsDB, "fact_source_tombstones", "fact_removed_sources.jsonl", "sources of facts that were deleted, and why"},
	{factsDB, "sme_docs", "sme_docs.jsonl", "subject-matter-expert sections synthesized from facts, every version"},
	{skillsDB, "skills", "skills.jsonl", "learned skill recipes"},
	{skillsDB, "skill_history", "skill_history.jsonl", "earlier versions of each skill"},
	{runsDB, "runs", "runs.jsonl", "operational records of each turn: model, tokens, tool calls, approvals"},
	{usageDB, "token_usage", "token_usage.jsonl", "tokens used per model call"},
}

// Export writes what Tenant knows, readable without Tenant: each store table
// as JSON Lines (embedding vectors left out), the synthesized user profiles,
// the souls, research runs and the raw archive, with a README that says what
// each file is. The directory is written owner-only.
func Export(ctx context.Context, d Data, opts ExportOptions) (*ExportResult, error) {
	if opts.Out == "" {
		return nil, errors.New("privacy: export needs an output directory")
	}
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return nil, err
	}
	for _, root := range []string{d.DataDir, d.ConfigDir} {
		if root != "" && within(out, root) {
			return nil, fmt.Errorf("privacy: write the export outside Tenant's directories (%s is inside %s)", out, root)
		}
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("privacy: %s is not empty", out)
	}
	if err := fsperm.MkdirAll(out); err != nil {
		return nil, err
	}
	res := &ExportResult{Dir: out, Rows: map[string]int{}}
	var readme []string
	for _, t := range exportTables {
		n, err := exportTableRows(ctx, d.path(t.db), t.table, filepath.Join(out, t.file), opts.Agent)
		if missing(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("privacy: export %s: %w", t.table, err)
		}
		res.Rows[t.file] = n
		readme = append(readme, fmt.Sprintf("- `%s` (%d): %s", t.file, n, t.about))
	}
	// The log book, sensitive fields included.
	if n, err := exportLogbook(ctx, d, opts.Agent, filepath.Join(out, "logbook.jsonl")); err == nil {
		res.Rows["logbook.jsonl"] = n
		readme = append(readme, fmt.Sprintf("- `logbook.jsonl` (%d): the log book's events (with --only-agent, those of the agent's sessions and runs), "+
			"fields marked sensitive included and unmasked; Level runs from 1 critical to 5 verbose", n))
	} else if !missing(err) {
		return nil, fmt.Errorf("privacy: export the log book: %w", err)
	}
	// Plain files.
	profiles, _ := filepath.Glob(d.path("profile.*.md"))
	for _, p := range profiles {
		if opts.Agent != "" && filepath.Base(p) != "profile."+opts.Agent+".md" {
			continue
		}
		if err := copyTree(p, filepath.Join(out, "profile", filepath.Base(p)), nil); err != nil {
			return nil, err
		}
	}
	if len(profiles) > 0 {
		readme = append(readme, "- `profile/`: the user profile Tenant synthesizes from memory, per agent")
	}
	if d.ConfigDir != "" {
		if _, err := os.Stat(filepath.Join(d.ConfigDir, "soul")); err == nil {
			if err := copyTree(filepath.Join(d.ConfigDir, "soul"), filepath.Join(out, "soul"), nil); err != nil {
				return nil, err
			}
			readme = append(readme, "- `soul/`: each agent's identity and standing instructions, and proposed edits")
		}
	}
	if opts.Agent == "" {
		if _, err := os.Stat(d.path(researchDir)); err == nil {
			if err := copyTree(d.path(researchDir), filepath.Join(out, "research"), nil); err != nil {
				return nil, err
			}
			readme = append(readme, "- `research/`: research runs, their findings and reports")
		}
	}
	if !opts.NoArchive {
		var filter func([]byte) bool
		if opts.Agent != "" {
			filter = func(line []byte) bool {
				var ev archiveEvent
				return json.Unmarshal(line, &ev) == nil && ev.AgentID == opts.Agent
			}
		}
		if _, err := os.Stat(archive.ArchiveDir(d.DataDir)); err == nil {
			if err := copyTree(archive.ArchiveDir(d.DataDir), filepath.Join(out, "archive"), filter); err != nil {
				return nil, err
			}
			readme = append(readme, "- `archive/YYYY-MM/<session>.jsonl`: every raw event of every session: messages, tool calls, tool results")
		}
	}
	if len(opts.Connectors) > 0 {
		b, _ := json.MarshalIndent(opts.Connectors, "", "  ")
		if err := fsperm.WriteFile(filepath.Join(out, "connectors.json"), append(b, '\n')); err != nil {
			return nil, err
		}
		readme = append(readme, "- `connectors.json`: the services Tenant holds credentials or data for (names only; no secret values)")
	}
	if err := writeReadme(out, d, opts, readme); err != nil {
		return nil, err
	}
	err = filepath.WalkDir(out, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		res.Files++
		res.Bytes += info.Size()
		return nil
	})
	return res, err
}

func writeReadme(out string, d Data, opts ExportOptions, lines []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# What Tenant knows\n\nExported %s", time.Now().Format("2006-01-02 15:04 MST"))
	if opts.TenantVersion != "" {
		fmt.Fprintf(&b, " by tenant %s", opts.TenantVersion)
	}
	fmt.Fprintf(&b, " from %s.\n", d.DataDir)
	if opts.Agent != "" {
		fmt.Fprintf(&b, "Only agent %q's data is included.\n", opts.Agent)
	}
	b.WriteString("\n## Files\n\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString("\nThe `.jsonl` files hold one JSON object per line, one per database row, with\n" +
		"the store's own column names. Times named `ts`, `first_seen`, `last_confirmed`,\n" +
		"`removed_at`, `updated_at`, `created_at` and `changed_at` are Unix seconds;\n" +
		"text times are RFC 3339. Embedding vectors are left out.\n\n" +
		"Not included: the log book's text files and archives (`logs/`: the same events as\n" +
		"logbook.jsonl, or older ones kept for their retention), the route decision log\n" +
		"(prompt hashes, not text), credentials and keys, and your wiki vault, which is\n" +
		"your own folder.\n")
	return fsperm.WriteFile(filepath.Join(out, "README.md"), []byte(b.String()))
}

// exportTableRows writes every row of table as a JSON object per line,
// leaving out embedding columns, and returns the row count. A missing table
// (an older store) writes nothing.
func exportTableRows(ctx context.Context, dbPath, table, dst, agent string) (int, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if ok, err := hasTable(ctx, db, table); err != nil {
		return 0, err
	} else if !ok {
		return 0, fs.ErrNotExist
	}
	query := `SELECT * FROM ` + table
	var args []any
	if agent != "" && hasColumn(ctx, db, table, "agent_id") {
		query += ` WHERE agent_id = ?`
		args = append(args, agent)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	f, err := fsperm.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(f)
	n := 0
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			f.Close()
			return n, err
		}
		obj := make(map[string]any, len(cols))
		for i, c := range cols {
			if strings.Contains(c, "embedding") {
				continue
			}
			switch v := vals[i].(type) {
			case []byte:
				obj[c] = string(v)
			default:
				obj[c] = v
			}
		}
		line, err := json.Marshal(obj)
		if err != nil {
			f.Close()
			return n, err
		}
		w.Write(line)
		w.WriteByte('\n')
		n++
	}
	if err := rows.Err(); err != nil {
		f.Close()
		return n, err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return n, err
	}
	return n, f.Close()
}

func hasColumn(ctx context.Context, q queryer, table, column string) bool {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && name == column {
			return true
		}
	}
	return false
}

// copyTree copies a file or a directory tree owner-only. For .jsonl files a
// filter, when set, keeps only the lines it accepts.
func copyTree(src, dst string, filter func([]byte) bool) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyOne(src, dst, filter)
	}
	return filepath.WalkDir(src, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if e.IsDir() {
			return fsperm.MkdirAll(target)
		}
		if !e.Type().IsRegular() {
			return nil
		}
		return copyOne(path, target, filter)
	})
}

func copyOne(src, dst string, filter func([]byte) bool) error {
	if err := fsperm.MkdirAll(filepath.Dir(dst)); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := fsperm.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	if filter == nil || !strings.HasSuffix(src, ".jsonl") {
		_, err = io.Copy(out, in)
	} else {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		w := bufio.NewWriter(out)
		for sc.Scan() {
			if filter(sc.Bytes()) {
				w.Write(sc.Bytes())
				w.WriteByte('\n')
			}
		}
		err = sc.Err()
		if ferr := w.Flush(); err == nil {
			err = ferr
		}
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// within reports whether p is root or inside it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// keysSorted is used by callers that print row counts in a stable order.
func keysSorted(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RowFiles returns the table files an export wrote, sorted.
func (r *ExportResult) RowFiles() []string { return keysSorted(r.Rows) }
