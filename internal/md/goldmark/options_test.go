//go:build !goldmark_v1_attribute

package goldmark_test

import (
	"testing"

	"tenant/internal/md/goldmark/parser"
	"tenant/internal/md/goldmark/renderer/html"
	"tenant/internal/md/goldmark/testutil"
)

func TestAttributeAndAutoHeadingID(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(parser.WithAttribute(), parser.WithAutoHeadingID()),
		html.New(),
	)
	testutil.DoTestCaseFile(markdown, "testdata/options-auto-heading-id.txt", t, testutil.ParseCliCaseArg()...)
}

// rawHTMLDelimiterCases are the emphasis cases whose Markdown is an HTML tag;
// text in this copy (see README.md), so not run.
var rawHTMLDelimiterCases = []int{126, 127, 128}

func TestParseDelimiterSimple(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(parser.WithParseDelimiterFunc(parser.ParseDelimiterSimple)),
		html.New(),
	)
	nos := testutil.ParseCliCaseArg()
	if len(nos) == 0 {
		nos = caseNumbersExcept(t, "testdata/options-parse-delimiter-simple.txt", rawHTMLDelimiterCases)
	}
	testutil.DoTestCaseFile(markdown, "testdata/options-parse-delimiter-simple.txt", t, nos...)
}
