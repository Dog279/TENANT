// Package md parses the Markdown a model writes, from one in-house copy of
// goldmark (see goldmark/README.md), and renders it as HTML for the dashboard.
// The terminal app renders the same tree itself (internal/tui, which alone may
// draw with Charm libraries) through Parse. HTML inside the Markdown is text on
// both surfaces: the copy has no raw-HTML parser.
package md

import (
	"bytes"
	"html/template"
	"strings"
	"sync"

	"tenant/internal/md/goldmark/ast"
	"tenant/internal/md/goldmark/extension"
	"tenant/internal/md/goldmark/parser"
	"tenant/internal/md/goldmark/renderer/html"
	"tenant/internal/md/goldmark/text"
)

var (
	setup    sync.Once
	mdParser parser.Parser
	htmlOut  html.Renderer
)

func init() {
	setup.Do(func() {
		mdParser = parser.New(parser.WithExtensions(extension.TableParser, extension.TaskListItemParser))
		htmlOut = html.New(html.WithExtensions(extension.TableHTMLRenderer, extension.TaskListItemHTMLRenderer))
	})
}

// HTML renders src for the dashboard. Every text node is escaped and link
// destinations with a script scheme are dropped, so the result is safe to
// place in a page as it is. An image is rendered as a link to it
// (imagesToLinks): the page never fetches an address the text names.
func HTML(src string) template.HTML {
	source := []byte(src)
	var buf bytes.Buffer
	doc := mdParser.Parse(source)
	imagesToLinks(doc, source)
	if err := htmlOut.Render(&buf, source, doc); err != nil {
		return template.HTML(template.HTMLEscapeString(src)) //nolint:gosec // escaped above
	}
	return template.HTML(buf.String()) //nolint:gosec // rendered from escaped text nodes
}

// imagesToLinks replaces every image in doc with a link to the same address
// whose text is the image's alt text. An <img> is fetched by the browser the
// moment the page shows it, with no click, and the text comes from a model
// that has read mail, web pages and tool results: where that request goes,
// and what its address carries, would be for that text to decide. A link
// goes nowhere until the operator follows it. The dashboard's
// Content-Security-Policy refuses off-origin images as well (img-src).
func imagesToLinks(doc ast.Node, source []byte) {
	var images []*ast.Image
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if img, ok := n.(*ast.Image); ok && entering {
			images = append(images, img)
		}
		return ast.WalkContinue, nil
	})
	for _, img := range images {
		parent := img.Parent()
		if parent == nil {
			continue
		}
		if insideLink(parent) { // an image that is itself a link: keep the outer link, show the alt text
			for c := img.FirstChild(); c != nil; c = img.FirstChild() {
				parent.InsertBefore(img, c)
			}
			parent.RemoveChild(img)
			continue
		}
		link := ast.NewLink(img.Destination)
		link.Title = img.Title
		for c := img.FirstChild(); c != nil; c = img.FirstChild() {
			link.AppendChild(c) // moves it: a node has one parent
		}
		if !showsText(link, source) { // no alt text: a link needs something to show
			link.AppendChild(ast.NewText(text.NewSingleLineValue("image", nil)))
		}
		parent.ReplaceChild(img, link)
	}
}

// insideLink reports whether n is a link or sits inside one. HTML has no
// link inside a link.
func insideLink(n ast.Node) bool {
	for ; n != nil; n = n.Parent() {
		if _, ok := n.(*ast.Link); ok {
			return true
		}
	}
	return false
}

// showsText reports whether n renders any visible text. An image without alt
// text still has a child: an empty text node.
func showsText(n ast.Node, source []byte) bool {
	shows := false
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		switch v := c.(type) {
		case *ast.Text:
			shows = shows || strings.TrimSpace(v.Value.Value(source)) != ""
		case *ast.CodeSpan:
			shows = true
		}
		if shows {
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return shows
}

// Parse returns the document tree of source, with GFM tables and task boxes.
// Node values refer into source, so a renderer takes both.
func Parse(source []byte) ast.Node { return mdParser.Parse(source) }
