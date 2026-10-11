package extension

import (
	"testing"

	"tenant/internal/md/goldmark/parser"
	"tenant/internal/md/goldmark/renderer/html"
	"tenant/internal/md/goldmark/testutil"
)

func TestTaskList(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTaskListItemParser()),
		),
		html.New(
			html.WithExtensions(NewTaskListItemHTMLRenderer()),
		),
	)
	testutil.DoTestCaseFile(markdown, "testdata/tasklist.txt", t, testutil.ParseCliCaseArg()...)
}
