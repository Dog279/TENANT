package main

// peertools.go (TEN-186): the SERVER-SIDE federation knowledge tools that fill
// the TEN-184 listener's ToolRegistrar. A paired peer can query THIS instance's
// shared/public wiki + memory — retrieval-only, with provenance, snippet-capped,
// and gated at CALL TIME on the live share policy (pc.CurrentShare(), per the
// TEN-184 contract, so a `tenant peer share … off` downgrade lands without a
// reconnect). Private-tier memory is NEVER exposed.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/ftsutil"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
	"tenant/internal/peering"
	"tenant/internal/plugins/wiki"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildPeerToolDeps assembles the knowledge-tool deps for a HEADLESS peer
// server (`tenant peer serve`): opens the memory stores, resolves an optional
// embedder, and builds the wiki index when wikiDir is set. The interactive TUI
// passes its already-live stores directly instead of calling this. Returns the
// deps + a cleanup that closes the stores.
func buildPeerToolDeps(ctx context.Context, c *commonFlags, wikiDir string) (peerToolDeps, func(), error) {
	st, closeStores, err := openStores(c)
	if err != nil {
		return peerToolDeps{}, nil, err
	}
	name, _ := os.Hostname()
	if name == "" {
		name = "this tenant"
	}
	deps := peerToolDeps{selfName: name, semantic: st.semantic, episodic: st.episodic}

	// Embedder is best-effort: a failure leaves memory search keyword-only.
	// Wiki reuses buildToolMux's index builder (same sidecar/embedID derivation).
	if router, rerr := buildRouter(c, newLogger()); rerr == nil {
		if emb, _, eerr := router.EmbedderForRole(ctx, model.RoleEmbedder); eerr == nil {
			deps.embedder = emb
		}
		if wikiDir != "" {
			if ix, werr := buildWikiIndex(ctx, c, router, wikiDir, newLogger()); werr == nil {
				deps.wiki = ix
			}
		}
	}
	return deps, closeStores, nil
}

// peerToolDeps are the live stores the knowledge tools read from (the host
// process holds them). Embedder + wiki are optional (nil ⇒ keyword-only search
// / no wiki tool).
type peerToolDeps struct {
	selfName string
	semantic *semantic.Store
	episodic *episodic.Store
	embedder model.Embedder
	wiki     *wiki.Index
}

// peerSearchArgs is the shared input for both knowledge tools.
type peerSearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

// peerSharedVisibility is the ONLY memory a peer may ever see — never private.
var peerSharedVisibility = []string{semantic.VisibilityShared, semantic.VisibilityPublic}

// maxPeerResultBytes bounds the WHOLE response of a knowledge tool (not just
// each snippet) so a peer can't assemble a giant reply from many capped
// snippets that still blows a small model's context. ~32KB is generous for
// retrieval snippets while staying well inside small-context budgets.
const maxPeerResultBytes = 32 * 1024

// peerKnowledgeRegistrar returns the ToolRegistrar injected into the listener.
// It registers both knowledge tools unconditionally (so tools/list shows the
// full enumerated surface); each handler enforces the live share gate at call
// time and returns a clear denial when the capability is off.
func peerKnowledgeRegistrar(deps peerToolDeps) peering.ToolRegistrar {
	return func(s *mcp.Server, pc peering.PeerContext) {
		registerPeerWikiSearch(s, pc, deps)
		registerPeerMemorySearch(s, pc, deps)
	}
}

func registerPeerWikiSearch(s *mcp.Server, pc peering.PeerContext, deps peerToolDeps) {
	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "peer_wiki_search",
			Description: "Search the peer's shared wiki/knowledge base. Returns snippets with file provenance. Read-only.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, a peerSearchArgs) (*mcp.CallToolResult, any, error) {
			if !pc.CurrentShare().Wiki {
				return peerDenied("wiki"), nil, nil
			}
			if deps.wiki == nil {
				return peerText("(this peer has no wiki configured)"), nil, nil
			}
			q := strings.TrimSpace(a.Query)
			if q == "" {
				return peerText("query is required"), nil, nil
			}
			hits, err := deps.wiki.Search(ctx, q, clampK(a.K))
			if err != nil {
				return peerText("wiki search failed: " + err.Error()), nil, nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "## Wiki results from %s (%d)\n", deps.selfName, len(hits))
			for i, h := range hits {
				fmt.Fprintf(&b, "%d. [%s] (score %.3f)\n   %s\n", i+1, h.File, h.Score, capSnippet(h.Snippet))
			}
			return peerText(strings.TrimRight(b.String(), "\n")), nil, nil
		},
	)
}

func registerPeerMemorySearch(s *mcp.Server, pc peering.PeerContext, deps peerToolDeps) {
	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "peer_memory_search",
			Description: "Search the peer's SHARED/PUBLIC memory (facts + episodes). Returns snippets with provenance. Never returns private memory. Read-only.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, a peerSearchArgs) (*mcp.CallToolResult, any, error) {
			if !pc.CurrentShare().Memory {
				return peerDenied("memory"), nil, nil
			}
			q := strings.TrimSpace(a.Query)
			if q == "" {
				return peerText("query is required"), nil, nil
			}
			k := clampK(a.K)
			// Embed for hybrid search if an embedder is available; degrade to
			// keyword-only on failure (never fatal).
			var embedding []float32
			if deps.embedder != nil {
				if vecs, err := deps.embedder.Embed(ctx, []string{q}); err == nil && len(vecs) == 1 {
					embedding = vecs[0]
				}
			}
			kw := ftsutil.Sanitize(q)

			var b strings.Builder
			fmt.Fprintf(&b, "## Shared memory from %s\n", deps.selfName)

			if deps.semantic != nil {
				hits, err := deps.semantic.Search(ctx, semantic.Query{
					Embedding:  embedding,
					Keywords:   kw,
					K:          k,
					Visibility: peerSharedVisibility, // shared/public ONLY — never private
				})
				if err == nil {
					fmt.Fprintf(&b, "### Facts (%d)\n", len(hits))
					for _, h := range hits {
						fmt.Fprintf(&b, "- %s (%s, confidence %.2f)\n", capSnippet(h.Fact.Fact), h.Fact.Visibility, h.Fact.Confidence)
					}
				}
			}
			if deps.episodic != nil {
				hits, err := deps.episodic.Search(ctx, episodic.Query{
					Embedding:  embedding,
					Keywords:   kw,
					K:          k,
					Visibility: peerSharedVisibility,
				})
				if err == nil {
					fmt.Fprintf(&b, "### Episodes (%d)\n", len(hits))
					for _, h := range hits {
						e := h.Episode
						fmt.Fprintf(&b, "[%s, %s] %s -> %s\n", e.Timestamp.Format("2006-01-02"), e.Visibility,
							capSnippet(e.Prompt), capSnippet(e.Response))
					}
				}
			}
			out := strings.TrimRight(b.String(), "\n")
			return peerText(out), nil, nil
		},
	)
}

// --- helpers --------------------------------------------------------------

func clampK(k int) int {
	if k <= 0 {
		return 8
	}
	if k > 25 {
		return 25
	}
	return k
}

// capSnippet bounds a single snippet so a peer can't pull huge chunks into a
// small model's context (TEN-184 MaxSnippetBytes).
func capSnippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= peering.MaxSnippetBytes {
		return s
	}
	return s[:peering.MaxSnippetBytes] + "…"
}

func peerText(s string) *mcp.CallToolResult {
	if s == "" {
		s = "(no results)"
	}
	if len(s) > maxPeerResultBytes { // aggregate ceiling, not just per-snippet
		s = s[:maxPeerResultBytes] + "\n… (truncated — refine your query for fewer/tighter results)"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// peerDenied is the call-time share-gate refusal — a normal tool result (not a
// transport error) so the calling agent sees a clear message.
func peerDenied(cap string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("denied: this peer has not shared its %s (set `tenant peer share … %s=on` on the serving side)", cap, cap)}},
	}
}
