package goldmark_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"tenant/internal/md/goldmark/parser"
	"tenant/internal/md/goldmark/renderer/html"
	"tenant/internal/md/goldmark/testutil"
)

type commonmarkSpecTestCase struct {
	Markdown  string `json:"markdown"`
	HTML      string `json:"html"`
	Example   int    `json:"example"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Section   string `json:"section"`
}

// rawHTMLCases are the spec examples outside those two sections whose
// Markdown carries an HTML tag or comment that the spec expects to pass
// through; in this copy the tag is text. Everything else in their sections
// runs.
var rawHTMLCases = []int{21, 31, 201, 308, 309, 344, 475, 476, 477, 491, 494, 524, 536, 642, 643}

func TestSpec(t *testing.T) {
	bs, err := os.ReadFile("testdata/spec.json")
	if err != nil {
		panic(err)
	}
	var testCases []commonmarkSpecTestCase
	if err := json.Unmarshal(bs, &testCases); err != nil {
		panic(err)
	}
	cases := []testutil.MarkdownTestCase{}
	nos := testutil.ParseCliCaseArg()
	for _, c := range testCases {
		// The raw-HTML parsers are not part of this copy (see README.md):
		// HTML in Markdown is text here, so the two sections that specify
		// it are not run.
		if c.Section == "HTML blocks" || c.Section == "Raw HTML" || slices.Contains(rawHTMLCases, c.Example) {
			continue
		}
		shouldAdd := len(nos) == 0
		if !shouldAdd {
			shouldAdd = slices.Contains(nos, c.Example)
		}

		if shouldAdd {
			cases = append(cases, testutil.MarkdownTestCase{
				No:       c.Example,
				Markdown: c.Markdown,
				Expected: c.HTML,
			})
		}
	}
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(),
		html.New(html.WithXHTML()),
	)
	testutil.DoTestCases(markdown, cases, t)
}
