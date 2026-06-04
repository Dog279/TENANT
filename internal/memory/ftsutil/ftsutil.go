// Package ftsutil turns free-text queries into safe, useful SQLite
// FTS5 MATCH expressions. It is shared by the MCP server and the CLI
// so they sanitize identically.
//
// Two jobs:
//
//   - Safety: strip punctuation so user text never injects FTS5 syntax
//     (bare ':' '*' '"' '(' ')' '-' are FTS5 operators).
//
//   - Signal: drop English stop words. Without this, a natural query
//     ("what does the user enjoy") matches almost every stored row
//     because facts/episodes all contain "the", "user", "is", etc.
//     That noise dominates reciprocal-rank fusion and drowns the
//     vector signal — exactly the bug real embeddings exposed. Keeping
//     only content words makes the keyword channel meaningful again.
package ftsutil

import "strings"

// stopWords are high-frequency English tokens that carry ~no
// retrieval signal and appear in nearly every stored fact/episode.
// Intentionally conservative — only words that are almost always
// noise in this corpus.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"do": true, "does": true, "did": true, "doing": true, "done": true,
	"what": true, "which": true, "who": true, "whom": true, "whose": true,
	"when": true, "where": true, "why": true, "how": true,
	"i": true, "you": true, "he": true, "she": true, "it": true,
	"we": true, "they": true, "me": true, "him": true, "her": true,
	"us": true, "them": true, "my": true, "your": true, "his": true,
	"its": true, "our": true, "their": true,
	"this": true, "that": true, "these": true, "those": true,
	"to": true, "of": true, "in": true, "on": true, "at": true,
	"by": true, "for": true, "with": true, "about": true, "as": true,
	"into": true, "from": true, "and": true, "or": true, "but": true,
	"not": true, "no": true, "yes": true, "so": true, "if": true,
	"then": true, "than": true, "can": true, "could": true, "would": true,
	"should": true, "will": true, "shall": true, "may": true, "might": true,
	"must": true, "have": true, "has": true, "had": true,
	"user": true, "users": true, // corpus-specific: every fact says "the user ..."
}

// Sanitize lowercases the query, splits on non-alphanumerics, drops
// stop words and 1-char tokens, and OR-joins the rest into an FTS5
// MATCH expression. Returns "" when nothing meaningful remains — the
// store layer treats an empty Keywords as "no keyword signal" and
// falls back to vector-only retrieval (the right behavior for a query
// that is all stop words).
func Sanitize(query string) string {
	var toks []string
	for _, f := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(f) <= 1 || stopWords[f] {
			continue
		}
		toks = append(toks, f)
	}
	return strings.Join(toks, " OR ")
}
