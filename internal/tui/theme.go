package tui

// The terminal app's palette, in one place (TEN-514). Every style the panes
// draw with is a field of palette, picked for a dark or a light terminal at
// start; the package-level style names the rest of the package uses are set
// from it, so a colour meaning changes here and nowhere else. The meanings
// follow the dashboard's: green is on or fine, amber needs you, red is an
// error or can't be undone, cyan is structure and the primary action.

import "github.com/charmbracelet/lipgloss"

type palette struct {
	User, Agent, Dim, Tool, Err, OK, Sys lipgloss.Style
	StatusBar, FeedTitle, Border         lipgloss.Style
	Heading, Key, Name, OnMark, OffMark  lipgloss.Style
	Glyph, Rule, Time                    lipgloss.Style // turn blocks: the gutter dot, the rule between turns, time stamps
	markdown                             mdTheme        // the chat pane's Markdown
}

func darkPalette() palette {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	cyan := lipgloss.Color("14")
	p := palette{
		User:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		Agent:     lipgloss.NewStyle().Foreground(lipgloss.Color("15")),
		Dim:       dim,
		Tool:      lipgloss.NewStyle().Foreground(lipgloss.Color("13")),
		Err:       lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		OK:        lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		Sys:       lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		StatusBar: lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15")),
		FeedTitle: lipgloss.NewStyle().Bold(true).Foreground(cyan),
		Border:    lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, true, false, false).BorderForeground(lipgloss.Color("238")),
		Heading:   lipgloss.NewStyle().Bold(true).Foreground(cyan),
		Key:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		Name:      lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		OnMark:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")),
		OffMark:   lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
		Glyph:     lipgloss.NewStyle().Foreground(cyan),
		Rule:      lipgloss.NewStyle().Foreground(lipgloss.Color("238")),
		Time:      dim,
	}
	p.markdown = defaultMDTheme()
	return p
}

// lightPalette is the same set read against a light background: darker inks,
// the same meanings.
func lightPalette() palette {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	teal := lipgloss.Color("6")
	p := palette{
		User:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4")),
		Agent:     lipgloss.NewStyle().Foreground(lipgloss.Color("0")),
		Dim:       dim,
		Tool:      lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		Err:       lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		OK:        lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Sys:       lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		StatusBar: lipgloss.NewStyle().Background(lipgloss.Color("254")).Foreground(lipgloss.Color("0")),
		FeedTitle: lipgloss.NewStyle().Bold(true).Foreground(teal),
		Border:    lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, true, false, false).BorderForeground(lipgloss.Color("250")),
		Heading:   lipgloss.NewStyle().Bold(true).Foreground(teal),
		Key:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4")),
		Name:      lipgloss.NewStyle().Foreground(lipgloss.Color("235")),
		OnMark:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")),
		OffMark:   lipgloss.NewStyle().Foreground(lipgloss.Color("248")),
		Glyph:     lipgloss.NewStyle().Foreground(teal),
		Rule:      lipgloss.NewStyle().Foreground(lipgloss.Color("250")),
		Time:      dim,
	}
	p.markdown = mdTheme{
		Heading:   p.Heading,
		Strong:    lipgloss.NewStyle().Bold(true),
		Emphasis:  lipgloss.NewStyle().Italic(true),
		Code:      lipgloss.NewStyle().Foreground(teal),
		CodeBlock: lipgloss.NewStyle().Foreground(lipgloss.Color("235")),
		CodeRule:  dim,
		Quote:     dim,
		Bullet:    p.Glyph,
		Rule:      p.Rule,
		Link:      lipgloss.NewStyle().Foreground(teal).Underline(true),
		URL:       dim,
		TableHead: lipgloss.NewStyle().Bold(true),
		TableLine: p.Rule,
	}
	return p
}

// pal is the palette in use. lipgloss asks the terminal for its background;
// with no terminal (tests, a pipe) it reports dark, the default look.
var pal = func() palette {
	if lipgloss.HasDarkBackground() {
		return darkPalette()
	}
	return lightPalette()
}()

// The style names the panes and the slash commands draw with.
var (
	cUser          = pal.User
	cAgent         = pal.Agent
	cDim           = pal.Dim
	cTool          = pal.Tool
	cErr           = pal.Err
	cOK            = pal.OK
	cSys           = pal.Sys
	statusBarStyle = pal.StatusBar
	feedTitleStyle = pal.FeedTitle
	paneBorder     = pal.Border

	// List / command formatting: high-contrast so /tools, /help, /skills
	// are scannable rather than a wall of dim grey.
	cHeading = pal.Heading // section titles
	cKey     = pal.Key     // commands, plugin names
	cName    = pal.Name    // active item name
	cOnMark  = pal.OnMark  // ● enabled
	cOffMark = pal.OffMark // ○ disabled

	// chatGutter is the left breathing room for the chat pane so the first
	// characters don't sit flush against the terminal edge.
	chatGutter = 2
	chatPad    = lipgloss.NewStyle().MarginLeft(chatGutter)
)
