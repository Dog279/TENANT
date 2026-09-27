package main

// extapprove.go: operator-gated approve/deny buttons for EXTERNAL automations.
//
// Any script on the box can post a #channel message through the bot carrying an
// action row whose buttons use custom_ids "ext-approve:<ns>:<id>" /
// "ext-deny:<ns>:<id>". When the operator clicks one, the relay ACKs within
// Discord's 3-second window by editing the message (buttons removed) and
// records the verdict as a marker file:
//
//	<dataDir>/approvals/<ns>/<id>.approve   (or .deny)
//
// The posting automation polls for its markers and acts on them — tenant.exe is
// a thin recorder, holds no queue, and never runs the external action itself.
// This mirrors the <dataDir>/relay-inbox pattern (operator attachments) and
// composes with, but stays structurally separate from, the TEN-117 nonce
// approver: the "ext-" namespace is routed before it in handleInteraction, and
// clicks are operator-gated there like everything else.
//
// Namespace and id are validated to strict shapes before touching the
// filesystem, so a crafted custom_id can never name a path outside the
// namespace dir. First consumer: the Hermes-Sentinel quality loop (ns "qi").

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tenant/internal/plugins/discord"
)

var (
	extNSRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,15}$`)
	extIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// handleExtApprove records the operator's button verdict as a marker file and
// ACKs the click. Failures ACK with a hint rather than staying silent — an
// un-ACKed click shows "interaction failed" on the operator's phone.
func (r *relay) handleExtApprove(in discord.Interaction) {
	verdict, ns, id, ok := parseExtButtonID(in.CustomID)
	if !ok {
		return
	}
	ack := func(msg string) {
		if r.approver == nil || r.approver.io == nil {
			return
		}
		if err := r.approver.io.RespondInteraction(context.Background(), in.ID, in.Token, msg); err != nil {
			discord.AckFailed.Log(context.Background(), "click", "external approval "+ns+"/"+id, "err", err)
		}
	}
	if r.dataDir == "" {
		ack("⚠️ " + id + ": no data dir configured — verdict not recorded.")
		return
	}
	dir := filepath.Join(r.dataDir, "approvals", ns)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		ack("⚠️ " + id + ": couldn't record the verdict (" + ns + " dir unavailable).")
		return
	}
	marker := filepath.Join(dir, id+"."+verdict)
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+" via button\n"), 0o644); err != nil {
		ack("⚠️ " + id + ": couldn't record the verdict.")
		return
	}
	if verdict == "approve" {
		ack("✅ **" + id + "** approved — recorded; the requesting automation will act and report.")
	} else {
		ack("⛔ **" + id + "** dismissed.")
	}
}

// parseExtButtonID splits "ext-approve:<ns>:<id>" / "ext-deny:<ns>:<id>" and
// validates both parts to filesystem-safe shapes.
func parseExtButtonID(custom string) (verdict, ns, id string, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(custom, "ext-approve:"):
		verdict, rest = "approve", strings.TrimPrefix(custom, "ext-approve:")
	case strings.HasPrefix(custom, "ext-deny:"):
		verdict, rest = "deny", strings.TrimPrefix(custom, "ext-deny:")
	default:
		return "", "", "", false
	}
	ns, id, found := strings.Cut(rest, ":")
	if !found || !extNSRe.MatchString(ns) || !extIDRe.MatchString(id) {
		return "", "", "", false
	}
	return verdict, ns, id, true
}
