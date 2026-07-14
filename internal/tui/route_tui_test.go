package tui

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/agent"
)

type fakeRouteControl struct {
	status      string
	statusCalls int
	whyCalls    []int
	statsCalls  int
	modes       []string
	forces      []string
}

func (f *fakeRouteControl) Status(context.Context) string {
	f.statusCalls++
	return f.status
}

func (f *fakeRouteControl) Why(n int) string {
	f.whyCalls = append(f.whyCalls, n)
	return "route decision " + strconv.Itoa(n)
}

func (f *fakeRouteControl) Stats() string {
	f.statsCalls++
	return "route stats"
}

func (f *fakeRouteControl) SetMode(mode string) (string, error) {
	f.modes = append(f.modes, mode)
	return "route mode " + mode, nil
}

func (f *fakeRouteControl) Force(profileID string) (string, error) {
	f.forces = append(f.forces, profileID)
	return "route force " + profileID, nil
}

func newRouteModel(c RouteControl) *model {
	m := newModel(context.Background(), Config{Route: c})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func TestSlash_RouteStatusRunsAsync(t *testing.T) {
	for _, command := range []string{"/route", "/route status", "/route STATUS"} {
		t.Run(command, func(t *testing.T) {
			f := &fakeRouteControl{status: "prompt router\nenabled: true\nmode: shadow\nclassifier: healthy"}
			m := newRouteModel(f)

			cmd := m.handleSlash(command)
			if cmd == nil {
				t.Fatal("route status should return a tea.Cmd")
			}
			if f.statusCalls != 0 {
				t.Fatal("route status probe ran on the UI goroutine")
			}
			rawMsg := cmd()
			msg, ok := rawMsg.(sysChatMsg)
			if !ok {
				t.Fatalf("route status returned %T, want sysChatMsg", rawMsg)
			}
			if f.statusCalls != 1 {
				t.Fatalf("Status calls = %d, want 1", f.statusCalls)
			}
			if !strings.Contains(msg.text, "classifier: healthy") {
				t.Errorf("status card = %q", msg.text)
			}
		})
	}
}

func TestSlash_RouteWhyDispatch(t *testing.T) {
	f := &fakeRouteControl{}
	m := newRouteModel(f)

	if cmd := m.handleSlash("/route why"); cmd != nil {
		t.Fatal("route why should be a local synchronous read")
	}
	if cmd := m.handleSlash("/route why 3"); cmd != nil {
		t.Fatal("route why should be a local synchronous read")
	}
	if want := []int{1, 3}; !reflect.DeepEqual(f.whyCalls, want) {
		t.Fatalf("Why calls = %v, want %v", f.whyCalls, want)
	}
	if got := m.msgs[len(m.msgs)-1].content; got != "route decision 3" {
		t.Errorf("why output = %q", got)
	}
}

func TestSlash_RouteControls(t *testing.T) {
	f := &fakeRouteControl{}
	m := newRouteModel(f)
	for _, command := range []string{"/route on", "/route assisted", "/route everywhere", "/route shadow", "/route off"} {
		if cmd := m.handleSlash(command); cmd != nil {
			t.Fatalf("%s unexpectedly asynchronous", command)
		}
	}
	if want := []string{"on", "assisted", "everywhere", "shadow", "off"}; !reflect.DeepEqual(f.modes, want) {
		t.Fatalf("modes = %v, want %v", f.modes, want)
	}
	if cmd := m.handleSlash("/route force cloud/vllm-planner"); cmd != nil {
		t.Fatal("force unexpectedly asynchronous")
	}
	if !reflect.DeepEqual(f.forces, []string{"cloud/vllm-planner"}) {
		t.Fatalf("forces = %v", f.forces)
	}
	cmd := m.handleSlash("/route stats")
	if cmd == nil {
		t.Fatal("stats should run asynchronously")
	}
	if f.statsCalls != 0 {
		t.Fatal("stats ran on UI goroutine")
	}
	if msg := cmd().(sysChatMsg); msg.text != "route stats" || f.statsCalls != 1 {
		t.Fatalf("stats = %+v calls=%d", msg, f.statsCalls)
	}
}

func TestSlash_RouteRejectsBadArguments(t *testing.T) {
	for _, command := range []string{
		"/route why 0",
		"/route why -1",
		"/route why nope",
		"/route why 1 extra",
		"/route status extra",
		"/route force",
		"/route force one two",
		"/route stats extra",
		"/route frobnicate",
	} {
		t.Run(command, func(t *testing.T) {
			f := &fakeRouteControl{}
			m := newRouteModel(f)

			if cmd := m.handleSlash(command); cmd != nil {
				t.Fatal("invalid route command should not return a tea.Cmd")
			}
			if len(f.whyCalls) != 0 || f.statusCalls != 0 {
				t.Fatalf("invalid command called control: status=%d why=%v", f.statusCalls, f.whyCalls)
			}
			if got := m.msgs[len(m.msgs)-1].content; !strings.Contains(got, "usage: /route") {
				t.Errorf("invalid command output = %q", got)
			}
		})
	}
}

func TestSlash_RouteUnavailable(t *testing.T) {
	m := newRouteModel(nil)
	m.handleSlash("/route")
	if got := m.msgs[len(m.msgs)-1].content; !strings.Contains(got, "not available") {
		t.Errorf("missing control output = %q", got)
	}
}

func TestApplyEvent_RouteIsDimFeedLine(t *testing.T) {
	m := newRouteModel(nil)
	base := len(m.feedLines)
	text := "label=big complexity=4 mode=shadow"

	m.applyEvent(agent.Event{Kind: agent.EventRoute, Text: text})

	if len(m.feedLines) != base+1 {
		t.Fatalf("route event appended %d lines, want 1", len(m.feedLines)-base)
	}
	if got, want := m.feedLines[len(m.feedLines)-1], cDim.Render("route: "+text); got != want {
		t.Errorf("route feed line = %q, want dim line %q", got, want)
	}
}

func TestHelp_ModelIncludesRouteCommands(t *testing.T) {
	sec := helpSectionByID("model")
	if sec == nil {
		t.Fatal("model help section missing")
	}
	plain, _ := renderHelpSection(sec)
	for _, want := range []string{"/route", "/route on|off|shadow|assisted|everywhere", "/route force", "/route stats", "/route why [n]"} {
		if !strings.Contains(plain, want) {
			t.Errorf("model help missing %q", want)
		}
	}
}
