package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// Every plugin but the channels brings its event catalog (TEN-393), and the
// host writes its lifecycle through it: whether it started, and the
// operator's enable and disable.
func TestPluginsLogThroughTheirOwnCatalog(t *testing.T) {
	for _, d := range plugins().All() {
		if channel := d.ID == "discord" || d.ID == "imessage"; (d.Events == nil) != channel {
			t.Errorf("plugin %q: Events = %v; every plugin but the channels declares its catalog", d.ID, d.Events)
		}
	}

	c := captureChannelEvents(t)
	ctx := context.Background()
	gh, _ := plugins().Get("github")
	gh.logStart(ctx, fmt.Errorf("github plugin: %w yet", errNotConfigured)) // a state, not a failure
	gh.logStart(ctx, errors.New("bad credentials"))
	gh.logStart(ctx, nil)

	mux := newToolMux()
	mux.Add("github", stubPlugin{Specs: gh.Tools()})
	mux.SetEnabled("github", false) // the launch's own
	mux.SetOnChange(func(map[string]bool) {})
	mux.SetEnabled("github", true)
	mux.SetEnabled("github_create_pr", false)

	var got []string
	for _, e := range c.events {
		if e.Provider == "plugin.github" {
			got = append(got, fmt.Sprintf("%d %s", e.ID, e.Msg))
		}
	}
	want := []string{
		"3001 GitHub could not start: bad credentials",
		"1 GitHub started",
		"2 github enabled",
		"3 github_create_pr disabled",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("plugin.github logged %q, want %q", got, want)
	}
}
