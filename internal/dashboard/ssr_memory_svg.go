package dashboard

// ssr_memory_svg.go is the server-rendered knowledge-time timeline for the
// Memory · Knowledge time page (TEN-111). The old SPA drew this SVG in the
// browser and re-decayed confidence against an interactive "as of" slider
// (TEN-91); the SSR version renders a static snapshot AS OF the request using
// the store's own server-computed effective confidence, with ZERO client JS.
//
// HONESTY (carried from TEN-91): the store records only transaction time —
// first_seen / last_confirmed — never when a fact stopped being true. So every
// lifeline ENDS at last_confirmed and the end marker's SHAPE (not its position)
// encodes status. We do not draw a dated end-of-life.

import (
	"html"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SVG geometry. A fixed viewBox keeps the render deterministic (and testable);
// the element scales to its container width via width="100%".
const (
	svgW       = 1400 // viewBox width (wide; the SVG scales to 100% of its container)
	svgML      = 230  // left gutter for fact labels
	svgMR      = 28  // right margin
	svgMT      = 26  // top margin (tick labels)
	svgRow     = 22  // px per fact lifeline
	svgBottom  = 30  // bottom margin (axis ticks)
	svgMaxText = 34  // label truncation
)

// statusStroke maps a lifecycle status to its line/marker color. These mirror
// the dashboard palette (--green / --amber / --red) so the timeline reads the
// same as the rest of the UI.
func statusStroke(status string) string {
	switch status {
	case FactStatusTombstoned:
		return "#fb7185"
	case FactStatusSuperseded:
		return "#fbbf24"
	default:
		return "#34d399"
	}
}

// fmtUnixDate renders a unix-seconds timestamp as YYYY-MM-DD, or "—" when
// absent (zero/negative).
func fmtUnixDate(unix int64) string {
	if unix <= 0 {
		return "—"
	}
	return time.Unix(unix, 0).Format("2006-01-02")
}

// renderTemporalSVG builds the knowledge-time SVG for facts. Returns "" for an
// empty set (the page shows its own empty state instead). Every text node is
// html-escaped; the result is trusted markup (template.HTML) the page embeds.
func renderTemporalSVG(facts []TemporalFactView) template.HTML {
	if len(facts) == 0 {
		return ""
	}

	// Sort a copy oldest-first so lifelines read top-to-bottom chronologically.
	rows := make([]TemporalFactView, len(facts))
	copy(rows, facts)
	sort.SliceStable(rows, func(i, j int) bool {
		ai, aj := axisStart(rows[i]), axisStart(rows[j])
		if ai != aj {
			return ai < aj
		}
		return rows[i].ID < rows[j].ID
	})

	// Time domain: earliest start → latest last-confirmed. Degenerate spans
	// (single instant) collapse to the left edge via the x() guard.
	minT, maxT := axisStart(rows[0]), int64(0)
	for _, f := range rows {
		if s := axisStart(f); s < minT {
			minT = s
		}
		if f.LastConfirmed > maxT {
			maxT = f.LastConfirmed
		}
		if f.FirstSeen > maxT {
			maxT = f.FirstSeen
		}
	}
	if maxT <= minT {
		maxT = minT + 1
	}
	plotW := float64(svgW - svgML - svgMR)
	x := func(t int64) float64 {
		if t <= 0 {
			t = minT
		}
		return float64(svgML) + float64(t-minT)/float64(maxT-minT)*plotW
	}

	height := svgMT + len(rows)*svgRow + svgBottom
	axisY := float64(height - svgBottom + 8)

	// width="100%" + CSS height:auto (no fixed height attr) makes the SVG fill
	// its container width and derive height from the viewBox aspect ratio — no
	// preserveAspectRatio letterboxing, so it spans the full width.
	var b strings.Builder
	b.WriteString(`<svg viewBox="0 0 ` + itoa(svgW) + ` ` + itoa(height) +
		`" width="100%" class="ov-svg" role="img" aria-label="Knowledge timeline of ` +
		itoa(len(rows)) + ` facts from ` + fmtUnixDate(minT) + ` to ` + fmtUnixDate(maxT) + `">`)

	// Axis + date ticks (start / mid / end).
	b.WriteString(line(float64(svgML), axisY, float64(svgW-svgMR), axisY, "#27365a", 1, 1))
	for _, frac := range []float64{0, 0.5, 1} {
		t := minT + int64(frac*float64(maxT-minT))
		tx := x(t)
		b.WriteString(line(tx, float64(svgMT-4), tx, axisY, "#1e2a44", 1, 1))
		b.WriteString(text(tx, axisY+14, "#5e6f90", 10, "middle", fmtUnixDate(t)))
	}

	for i, f := range rows {
		y := float64(svgMT + i*svgRow + svgRow/2)
		stroke := statusStroke(f.Status)
		// Opacity tracks effective (decayed) confidence, floored so even a
		// faded fact stays legible.
		op := 0.35 + 0.65*clamp01(f.EffectiveConfidence)

		// Label (truncated, full text in <title>).
		b.WriteString(`<text x="8" y="` + ftoa(y+4) + `" fill="#cdd8ea" font-size="11">` +
			html.EscapeString(truncRunes(f.Text, svgMaxText)) +
			`<title>` + html.EscapeString(f.Text) + `</title></text>`)

		x1, x2 := x(axisStart(f)), x(endPoint(f))
		if x2 < x1 {
			x2 = x1
		}
		b.WriteString(line(x1, y, x2, y, stroke, 2, op))    // lifeline
		b.WriteString(dot(x1, y, 2.6, stroke, op))          // first-seen
		b.WriteString(endMarker(x2, y, f.Status, stroke, op)) // last-confirmed, shaped by status
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// axisStart is the left end of a lifeline: first_seen, falling back to
// last_confirmed when first_seen is unknown (0).
func axisStart(f TemporalFactView) int64 {
	if f.FirstSeen > 0 {
		return f.FirstSeen
	}
	return f.LastConfirmed
}

// endPoint is the right end of a lifeline: last_confirmed, falling back to
// first_seen.
func endPoint(f TemporalFactView) int64 {
	if f.LastConfirmed > 0 {
		return f.LastConfirmed
	}
	return f.FirstSeen
}

// endMarker shapes the lifeline's end by status: live = filled dot, superseded
// = hollow dot, removed = ✕. The shape (not a dated position) encodes status.
func endMarker(x, y float64, status, stroke string, op float64) string {
	switch status {
	case FactStatusTombstoned:
		o := ftoa(op)
		return `<g stroke="` + stroke + `" stroke-width="1.6" stroke-opacity="` + o + `">` +
			`<line x1="` + ftoa(x-3) + `" y1="` + ftoa(y-3) + `" x2="` + ftoa(x+3) + `" y2="` + ftoa(y+3) + `"/>` +
			`<line x1="` + ftoa(x-3) + `" y1="` + ftoa(y+3) + `" x2="` + ftoa(x+3) + `" y2="` + ftoa(y-3) + `"/></g>`
	case FactStatusSuperseded:
		return `<circle cx="` + ftoa(x) + `" cy="` + ftoa(y) + `" r="3.4" fill="none" stroke="` + stroke +
			`" stroke-width="1.4" stroke-opacity="` + ftoa(op) + `"/>`
	default:
		return dot(x, y, 3.4, stroke, op)
	}
}

// --- tiny SVG element writers (all numeric, no escaping needed) -------------

func line(x1, y1, x2, y2 float64, stroke string, wdt int, op float64) string {
	return `<line x1="` + ftoa(x1) + `" y1="` + ftoa(y1) + `" x2="` + ftoa(x2) + `" y2="` + ftoa(y2) +
		`" stroke="` + stroke + `" stroke-width="` + itoa(wdt) + `" stroke-opacity="` + ftoa(op) + `"/>`
}

func dot(x, y, r float64, fill string, op float64) string {
	return `<circle cx="` + ftoa(x) + `" cy="` + ftoa(y) + `" r="` + ftoa(r) + `" fill="` + fill +
		`" fill-opacity="` + ftoa(op) + `"/>`
}

func text(x, y float64, fill string, size int, anchor, s string) string {
	return `<text x="` + ftoa(x) + `" y="` + ftoa(y) + `" fill="` + fill + `" font-size="` + itoa(size) +
		`" text-anchor="` + anchor + `">` + html.EscapeString(s) + `</text>`
}

func itoa(n int) string { return strconv.Itoa(n) }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// truncRunes shortens s to n runes with an ellipsis, counting by rune so
// multibyte text isn't cut mid-character.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
