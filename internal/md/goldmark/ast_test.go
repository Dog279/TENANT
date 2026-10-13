package goldmark_test

import (
	"testing"

	"tenant/internal/md/goldmark/ast"
	"tenant/internal/md/goldmark/parser"
)

func TestHasBlankPreviousLines(t *testing.T) {
	var cases = []struct {
		Name     string
		Source   string
		Node     func(n ast.Node) ast.Node
		Expected bool
	}{
		{
			Name: "nesting paragraphs in blockquotes",
			Source: `
> a
> 
> b
`,
			Node: func(n ast.Node) ast.Node {
				return n.FirstChild().FirstChild().NextSibling()
			},
			Expected: true,
		},
		{
			Name: "nesting loose lists in blockquotes",
			Source: `
> - a
> 
> - b
`,
			Node: func(n ast.Node) ast.Node {
				return n.FirstChild().FirstChild().FirstChild().NextSibling()
			},
			Expected: true,
		},
		{
			Name: "nesting tight lists in blockquotes",
			Source: `
> - a
> - b
`,
			Node: func(n ast.Node) ast.Node {
				return n.FirstChild().FirstChild().FirstChild().NextSibling()
			},
			Expected: false,
		},
		{
			Name: "nesting paragraphs in lists",
			Source: `
- a

  b
`,
			Node: func(n ast.Node) ast.Node {
				return n.FirstChild().FirstChild().FirstChild().NextSibling()
			},
			Expected: true,
		},
	}
	for _, cs := range cases {
		t.Run(cs.Name, func(t *testing.T) {
			n := parser.New().Parse([]byte(cs.Source))
			if cs.Node(n).(ast.BlockNode).HasBlankPreviousLines() != cs.Expected {
				t.Errorf("expected %v, got %v", cs.Expected, !cs.Expected)
			}
		})
	}
}

func TestInlinePos(t *testing.T) {
	source := []byte(`[bar][]

[foo][bar]

[bar]

[foo](http://example.com)

aaaa **b** 

![aaa](http://example.com/foo.png "title")

[bar]: 
  /url "ti
  tle"
`)
	n := parser.New().Parse(source)
	if n.FirstChild().FirstChild().Pos() != 0 {
		t.Error("unexpected position for 1st link reference")
	}
	if n.FirstChild().NextSibling().FirstChild().Pos() != 9 {
		t.Error("unexpected position for 2nd link reference")
	}
	if n.FirstChild().NextSibling().NextSibling().FirstChild().Pos() != 21 {
		t.Error("unexpected position for 3rd link reference")
	}
	if n.FirstChild().NextSibling().NextSibling().NextSibling().FirstChild().Pos() != 28 {
		t.Error("unexpected position for 1st inline link ")
	}
	if n.FirstChild().NextSibling().NextSibling().NextSibling().NextSibling().FirstChild().NextSibling().Pos() != 60 {
		t.Error("unexpected position for 1st emphasis")
	}
	if n.FirstChild().NextSibling().NextSibling().NextSibling().NextSibling().NextSibling().FirstChild().Pos() != 68 {
		t.Error("unexpected position for 1st image")
	}
}

func TestBlockPos(t *testing.T) {
	source := []byte(`paragraph text

## heading

---

> blockquote

- list item

1. ordered

    indented code

` + "```go\nfenced code\n```" + `

<!-- html block -->

[bar]: /url`)
	n := parser.New().Parse(source)
	paragraph := n.FirstChild()
	heading := paragraph.NextSibling()
	thematicBreak := heading.NextSibling()
	blockquote := thematicBreak.NextSibling()
	unorderedList := blockquote.NextSibling()
	orderedList := unorderedList.NextSibling()
	codeBlock := orderedList.NextSibling()
	htmlBlock := codeBlock.NextSibling()
	linkRefDef := htmlBlock.NextSibling()
	if paragraph.Pos() != 0 {
		t.Error("unexpected position for paragraph")
	}
	if heading.Pos() != 16 {
		t.Error("unexpected position for heading")
	}
	if thematicBreak.Pos() != 28 {
		t.Error("unexpected position for thematic break")
	}
	if blockquote.Pos() != 33 {
		t.Error("unexpected position for blockquote")
	}
	if unorderedList.Pos() != 47 {
		t.Error("unexpected position for unordered list")
	}
	if orderedList.Pos() != 60 {
		t.Error("unexpected position for ordered list")
	}
	if codeBlock.Pos() != 91 {
		t.Error("unexpected position for fenced code block")
	}
	if htmlBlock.Pos() != 114 {
		t.Error("unexpected position for html block")
	}
	if linkRefDef.Pos() != 135 {
		t.Error("unexpected position for link reference definition")
	}
}

func TestBlockPosWithTabs(t *testing.T) {
	source := []byte(">\t# x\n")
	n := parser.New().Parse(source)
	blockquote := n.FirstChild()
	if blockquote.Pos() != 0 {
		t.Error("unexpected position for blockquote")
	}
	heading := blockquote.FirstChild()
	if heading.Pos() != 2 {
		t.Error("unexpected position for heading")
	}
}

func TestRawText(t *testing.T) {
	// The raw-HTML inline parser is not part of this copy (see README.md), so
	// the upstream case's <code> tags are left out: here HTML is plain text.
	source := []byte("`inline &amp; value` inline &amp; value <http://www.example.com/&AElig;>[aaa](http://www.example.com/&AElig;)")
	ctx := parser.NewContext()
	n := parser.New().Parse(source, parser.WithContext(ctx))
	code1 := n.FirstChild().FirstChild().(*ast.CodeSpan)
	// inline code value is a raw text
	if code1.Value.Value(source) != "inline &amp; value" {
		t.Error("unexpected value for 1st code span")
	}
	// inline text is a normal text
	text := code1.NextSibling().(*ast.Text)
	if text.Value.Value(source) != " inline & value " {
		t.Error("unexpected value for text")
	}
	// autolink text, label, destination are raw texts
	autolink := text.NextSibling().(*ast.AutoLink)
	if autolink.Destination.Value(source) != "http://www.example.com/&AElig;" {
		t.Error("unexpected value for autolink")
	}
	if autolink.Label.Value(source) != "http://www.example.com/&AElig;" {
		t.Error("unexpected value for autolink")
	}
	if autolink.Text.Value(source) != "<http://www.example.com/&AElig;>" {
		t.Error("unexpected value for autolink")
	}
	// link destination is a text
	link := autolink.NextSibling().(*ast.Link)
	if link.Destination.Value(source) != "http://www.example.com/Æ" {
		t.Error("unexpected value for link destination")
	}
}

func TestPP(_ *testing.T) {
	source := []byte("`inline &amp; value` inline &amp; value <code class=\"&AElig;\">inline &AElig; value</code><http://www.example.com/&AElig;>[aaa](http://www.example.com/&AElig; \"\\.&AElig;\")")
	_ = parser.New().Parse(source, parser.WithPrettyPrint(ast.WithSource(false)))
}
