package main

import (
	"bufio"
	"strings"
	"testing"
)

// TEN-70: the setup wizard now reads the shared catalog (skillKinds +
// wizardLocalKinds) via wizardCatalog. These tests drive configureSkills with
// canned stdin and assert the behaviors the ticket calls out: skip a skill,
// enable + valid input, enable + invalid input (validator re-asks), and the
// "(keep saved)" secret affordance.

// feed builds a bufio.Reader over newline-joined canned answers.
func feed(lines ...string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

func TestConfigureSkills_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	creds, _ := loadCredentials(dir)

	// Wizard order is wiki, sql, gsuite, x, imessage, discord, web, os.
	// Enable wiki (local, no probe), skip sql/gsuite, enable x (framework:
	// invalid bearer → re-ask → valid; decline the probe), enable imessage
	// (local secret), skip discord/web, enable os (no fields).
	validBearer := strings.Repeat("A", 90)
	in := feed(
		"y", "/tmp/wiki", // wiki: enable + dir
		"n",                          // sql: skip
		"n",                          // gsuite: skip
		"y", "bad", validBearer, "n", // x: enable, invalid bearer, valid bearer, decline verify
		"y", "http://localhost:1234", "imsg-pass", // imessage: enable + url + password
		"n", // discord: skip
		"n", // web: skip
		"y", // os: enable (no fields)
	)

	out := configureSkills(in, nil, creds, dir)

	// wiki enabled, dir captured.
	if out["wiki"] == nil || !out["wiki"].Enabled {
		t.Fatalf("wiki should be enabled, got %+v", out["wiki"])
	}
	if got := out["wiki"].Settings["dir"]; got != "/tmp/wiki" {
		t.Errorf("wiki dir = %q, want /tmp/wiki", got)
	}
	// sql + gsuite skipped → not enabled.
	if out["sql"] != nil && out["sql"].Enabled {
		t.Error("sql should be skipped")
	}
	if out["gsuite"] != nil && out["gsuite"].Enabled {
		t.Error("gsuite should be skipped")
	}
	// x enabled; the invalid bearer was rejected and the valid one stored.
	if out["x"] == nil || !out["x"].Enabled {
		t.Fatalf("x should be enabled, got %+v", out["x"])
	}
	if got := creds.get(skillSecretID("x", "bearer")); got != validBearer {
		t.Errorf("x bearer = %q, want the valid 90-char token (re-ask should have replaced 'bad')", got)
	}
	// imessage enabled with url setting + secret in creds (not settings).
	if out["imessage"] == nil || !out["imessage"].Enabled {
		t.Fatalf("imessage should be enabled, got %+v", out["imessage"])
	}
	if got := out["imessage"].Settings["url"]; got != "http://localhost:1234" {
		t.Errorf("imessage url = %q", got)
	}
	if got := creds.get(skillSecretID("imessage", "password")); got != "imsg-pass" {
		t.Errorf("imessage password should be in creds, got %q", got)
	}
	if _, leaked := out["imessage"].Settings["password"]; leaked {
		t.Error("secret password must NOT be written to non-secret Settings")
	}
	// discord + web skipped; os enabled with no fields.
	if out["discord"] != nil && out["discord"].Enabled {
		t.Error("discord should be skipped")
	}
	if out["os"] == nil || !out["os"].Enabled {
		t.Error("os should be enabled")
	}
}

func TestConfigureSkills_KeepSavedSecret(t *testing.T) {
	dir := t.TempDir()
	creds, _ := loadCredentials(dir)
	creds.set(skillSecretID("imessage", "password"), "previously-saved")

	// Enable imessage, set a new url, press Enter at the password prompt to
	// keep the saved secret. Skip everything else.
	in := feed(
		"n",                            // wiki
		"n",                            // sql
		"n",                            // gsuite
		"n",                            // x
		"y", "http://new-url:9999", "", // imessage: enable, url, KEEP password
		"n", // discord
		"n", // web
		"n", // os
	)

	out := configureSkills(in, nil, creds, dir)

	if out["imessage"] == nil || !out["imessage"].Enabled {
		t.Fatalf("imessage should be enabled, got %+v", out["imessage"])
	}
	if got := out["imessage"].Settings["url"]; got != "http://new-url:9999" {
		t.Errorf("imessage url = %q, want the new url", got)
	}
	if got := creds.get(skillSecretID("imessage", "password")); got != "previously-saved" {
		t.Errorf("blank password entry should KEEP the saved secret, got %q", got)
	}
}

// TestConfigureSkills_AbortDoesNotLeakSecret guards the secret-safe-by-
// construction invariant: a secret entered before a field whose NoteAfter
// aborts must NOT be flushed to credentials.json. The shipped catalog can't
// reach this (gsuite is the only aborting skill and has no secret fields), so
// we inject a synthetic skill with a secret field ordered before an always-
// abort NoteAfter and assert the secret never lands in creds.
func TestConfigureSkills_AbortDoesNotLeakSecret(t *testing.T) {
	const id = "leaktest"
	skillKinds[id] = skillKind{
		ID: id, Label: "Leak Test", Wired: true,
		Fields: []skillKindField{
			{Key: "token", Prompt: "secret token", Secret: true},
			{Key: "trigger", Prompt: "trigger", NoteAfter: func(string) (string, bool) {
				return "aborting on purpose", true
			}},
		},
	}
	origOrder := wizardOrder
	wizardOrder = append(append([]string{}, origOrder...), id)
	defer func() { delete(skillKinds, id); wizardOrder = origOrder }()

	dir := t.TempDir()
	creds, _ := loadCredentials(dir)
	// Skip every real skill (8 "n"), then enable leaktest, enter the secret,
	// then hit the aborting trigger field (empty value).
	lines := []string{"n", "n", "n", "n", "n", "n", "n", "n", "y", "super-secret-token", ""}
	out := configureSkills(feed(lines...), nil, creds, dir)

	if out[id] != nil && out[id].Enabled {
		t.Error("aborted skill must not be enabled")
	}
	if got := creds.get(skillSecretID(id, "token")); got != "" {
		t.Errorf("secret entered before an aborting NoteAfter leaked into creds: %q", got)
	}
}

func TestConfigureSkills_DisableTogglesOff(t *testing.T) {
	dir := t.TempDir()
	creds, _ := loadCredentials(dir)
	cur := map[string]*skillConfig{
		"os": {Enabled: true, Settings: map[string]string{}},
	}
	// Answer "n" to every prompt — an already-enabled skill should flip off.
	in := feed("n", "n", "n", "n", "n", "n", "n", "n")
	out := configureSkills(in, cur, creds, dir)
	if out["os"].Enabled {
		t.Error("answering n to an already-enabled skill should disable it")
	}
}
