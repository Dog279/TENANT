package extension

import "tenant/internal/md/goldmark/renderer/html"

type withXHTML struct {
	value bool
}

func (o *withXHTML) applyTableHTMLRendererOption(c *tableHTMLRendererConfig) {
	c.XHTML = o.value
}

func (o *withXHTML) applyTaskListItemHTMLRendererOption(c *taskListItemHTMLRendererConfig) {
	c.XHTML = o.value
}

// WithXHTML is a functional option that indicates that table and task list
// elements should be rendered as XHTML.
func WithXHTML() interface {
	TableHTMLRendererOption
	TaskListItemHTMLRendererOption
} {
	return &withXHTML{value: true}
}

type withIsInTightBlockFunc struct {
	IsInTightBlockFunc html.IsInTightBlockFunc
}

func (o *withIsInTightBlockFunc) applyTaskListItemHTMLRendererOption(c *taskListItemHTMLRendererConfig) {
	c.IsInTightBlockFunc = o.IsInTightBlockFunc
}

// WithIsInTightBlockFunc is a functional option that sets the function to determine
// whether the list item is in a tight block.
func WithIsInTightBlockFunc(f html.IsInTightBlockFunc) interface {
	TaskListItemHTMLRendererOption
} {
	return &withIsInTightBlockFunc{IsInTightBlockFunc: f}
}
