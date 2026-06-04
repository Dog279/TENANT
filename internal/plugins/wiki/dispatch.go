package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// Dispatcher exposes the wiki to the agent. Read-only by nature, so —
// unlike web/sql — there is no Policy gate. search, read, list,
// reindex + links (walk the Obsidian graph for multi-hop).
type Dispatcher struct {
	ix *Index
}

func NewDispatcher(ix *Index) *Dispatcher { return &Dispatcher{ix: ix} }

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		{Name: "wiki_search", Description: "Semantic + link-graph search over the user's notes. Returns snippets with file › heading; results tagged '(via X)' were pulled in because note X links to them. Use this first.",
			Parameters: obj(`"query":{"type":"string"},"k":{"type":"integer","description":"max results (default 6)"}`, "query")},
		{Name: "wiki_read", Description: "Read a full note by its file path (as returned by wiki_search). Use after search to get complete context.",
			Parameters: obj(`"file":{"type":"string","description":"note path relative to the wiki root"}`, "file")},
		{Name: "wiki_links", Description: "Show a note's Obsidian graph: outgoing [[links]], backlinks (notes linking TO it), and #tags. Use to follow connections for multi-hop questions.",
			Parameters: obj(`"file":{"type":"string","description":"note path (as returned by wiki_search/wiki_list)"}`, "file")},
		{Name: "wiki_list", Description: "List all indexed note file paths.",
			Parameters: obj(``)},
		{Name: "wiki_reindex", Description: "Rebuild the index after the user edited their notes. Usually unnecessary (search auto-indexes when empty).",
			Parameters: obj(``)},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "wiki_search":
		return d.search(ctx, call.Arguments)
	case "wiki_read":
		return d.read(call.Arguments)
	case "wiki_links":
		return d.links(call.Arguments)
	case "wiki_list":
		return d.list()
	case "wiki_reindex":
		return d.reindex(ctx)
	default:
		return "unknown wiki tool: " + call.Name, true, nil
	}
}

func (d *Dispatcher) search(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return "query is required", true, nil
	}
	hits, err := d.ix.Search(ctx, a.Query, a.K)
	if err != nil {
		return "search failed: " + err.Error(), true, nil
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no notes matched %q (the wiki may be empty)", a.Query), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) for %q:\n", len(hits), a.Query)
	for i, h := range hits {
		loc := h.File
		if h.Heading != "" {
			loc += " › " + h.Heading
		}
		extra := ""
		if h.Via != "" {
			extra += " (via " + h.Via + ")"
		}
		if len(h.Tags) > 0 {
			extra += " #" + strings.Join(h.Tags, " #")
		}
		fmt.Fprintf(&b, "%d. [%s] (score %.3f)%s\n   %s\n", i+1, loc, h.Score, extra, h.Snippet)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) read(args json.RawMessage) (string, bool, error) {
	var a struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.File) == "" {
		return "file is required", true, nil
	}
	content, err := d.ix.ReadFile(a.File)
	if err != nil {
		return err.Error(), true, nil // includes the path-traversal refusal
	}
	const cap = 12000
	if len(content) > cap {
		content = content[:cap] + "\n…[note truncated; use wiki_search for specific parts]"
	}
	return content, false, nil
}

func (d *Dispatcher) links(args json.RawMessage) (string, bool, error) {
	var a struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.File) == "" {
		return "file is required", true, nil
	}
	fwd, back, tags, _, err := d.ix.Links(a.File)
	if err != nil {
		return err.Error(), true, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "graph for %s\n", a.File)
	if len(fwd) > 0 {
		fmt.Fprintf(&b, "links to: %s\n", strings.Join(fwd, ", "))
	}
	if len(back) > 0 {
		fmt.Fprintf(&b, "linked from: %s\n", strings.Join(back, ", "))
	}
	if len(tags) > 0 {
		fmt.Fprintf(&b, "tags: #%s\n", strings.Join(tags, " #"))
	}
	if len(fwd) == 0 && len(back) == 0 && len(tags) == 0 {
		b.WriteString("(no links or tags — a leaf note)\n")
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) list() (string, bool, error) {
	files := d.ix.List()
	if len(files) == 0 {
		return "(no notes indexed yet — run wiki_reindex or wiki_search)", false, nil
	}
	return fmt.Sprintf("%d note(s):\n%s", len(files), strings.Join(files, "\n")), false, nil
}

func (d *Dispatcher) reindex(ctx context.Context) (string, bool, error) {
	files, chunks, err := d.ix.Reindex(ctx)
	if err != nil {
		return "reindex failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("indexed %d file(s); %d chunk(s) (re)embedded", files, chunks), false, nil
}
