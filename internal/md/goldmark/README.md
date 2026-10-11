# goldmark, Tenant's copy

This directory is a copy of [yuin/goldmark](https://github.com/yuin/goldmark) v2 at commit
`cbf81e9` (2026-10-01), MIT licensed (see `LICENSE`). It is kept in the tree rather than in
`go.mod` so Tenant stays a single in-house module; it imports nothing outside the standard
library. To port an upstream fix, diff upstream against that commit and apply the hunk here.

Import paths are rewritten from `github.com/yuin/goldmark/v2` to `tenant/internal/md/goldmark`,
and the files are run through `gofmt` (upstream's generated tables were not).

## What is here

`ast`, `text`, `util`, `parser`, `renderer`, `renderer/html`, `testutil`, and from `extension`
the GFM table (`table.go`, `ast/table.go`) and task-list (`tasklist.go`) extensions, with their
tests and test data. The CommonMark spec cases (`testdata/spec.json`) and goldmark's extra cases
(`testdata/extra.txt`) run in `commonmark_test.go` and `extra_test.go`.

## What was removed, and why

- The raw-HTML parsers (`parser/html_block.go`, `parser/raw_html.go`), the `HTMLBlock` and
  `RawHTML` AST nodes and their HTML renderers. Tenant shows a model's answer; HTML inside it is
  text, by construction, on every surface. With the parsers gone there is nothing for an
  "unsafe" mode to pass through, so `html.WithUnsafe()` is gone too, and link destinations
  always go through `IsDangerousURL`.
- The footnote, definition-list, typographer, linkify and strikethrough extensions and the
  GFM bundle: not needed.
- `doc_test.go` (compiles the upstream README's examples), `cjk_test.go` (needs linkify), the
  fuzz corpus, benchmarks and tooling.

Because HTML is text here, the two spec sections that specify it ("HTML blocks", "Raw HTML")
are not run, and the fifteen examples in other sections whose Markdown carries a tag or comment
(`rawHTMLCases` in `commonmark_test.go`) are skipped; everything else in the spec runs and
passes. `TestRawText` and `TestHasBlankPreviousLines` lost their HTML cases for the same reason.

`html.WithXHTML()` stays: it is a few branches, and the spec's expected output uses it.
