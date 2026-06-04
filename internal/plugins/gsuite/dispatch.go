package gsuite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tenant/internal/model"
)

// Policy gates Workspace actions by blast radius — same shape as the
// sql/web plugins. Read (search/read/list) is always allowed. Send
// (gmail_send, calendar_create) LEAVES THE BUILDING (mails people /
// notifies attendees) so it is denied unless the operator set
// AllowSend, or a per-action Confirm explicitly approves it. The model
// cannot change the policy.
type Policy struct {
	AllowSend bool
	Confirm   func(ctx context.Context, action, detail string) bool
}

type actionClass int

const (
	classRead actionClass = iota
	classSend
)

func (p Policy) gate(ctx context.Context, c actionClass, detail string) error {
	if c == classRead {
		return nil
	}
	if p.AllowSend {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, "gsuite_send", detail) {
		return nil
	}
	return fmt.Errorf("blocked: this would send/create externally (mail a recipient / notify attendees) " +
		"and was not approved — enable the send flag or confirm. This is a blast-radius boundary, not a bug")
}

// Dispatcher implements agent.ToolDispatcher for Workspace.
type Dispatcher struct {
	svc    *Service
	policy Policy
}

func NewDispatcher(svc *Service, policy Policy) *Dispatcher {
	return &Dispatcher{svc: svc, policy: policy}
}

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		{Name: "gmail_search", Description: "Search the user's Gmail (Gmail query syntax, e.g. `from:alice is:unread newer_than:7d`). Returns id/from/subject/date/snippet. Use gmail_read for the body.",
			Parameters: obj(`"query":{"type":"string"},"max":{"type":"integer","description":"max results (default 10, cap 25)"}`, "query")},
		{Name: "gmail_read", Description: "Read one email's full plain-text body + headers by message id (from gmail_search).",
			Parameters: obj(`"id":{"type":"string"}`, "id")},
		{Name: "gmail_send", Description: "Send a plain-text email. GATED: requires operator approval (it leaves the building).",
			Parameters: obj(`"to":{"type":"string"},"subject":{"type":"string"},"body":{"type":"string"}`, "to", "subject", "body"), Gated: true},
		{Name: "calendar_list", Description: "List upcoming events on the primary calendar.",
			Parameters: obj(`"days":{"type":"integer","description":"look-ahead window in days (default 7)"}`)},
		{Name: "calendar_create", Description: "Create a calendar event. GATED: requires operator approval (it notifies attendees). Times are RFC3339, e.g. 2026-05-20T15:00:00Z.",
			Parameters: obj(`"summary":{"type":"string"},"start":{"type":"string"},"end":{"type":"string"},"attendees":{"type":"array","items":{"type":"string"}},"description":{"type":"string"},"location":{"type":"string"}`, "summary", "start", "end"), Gated: true},
		// TEN-72: Drive tools. Read-only (classRead) — no policy gate.
		{Name: "drive_search", Description: "Search the user's Google Drive using Drive's q= query syntax. " +
			"Examples: `name contains 'auth spec'`; `fullText contains 'rate limiting' and modifiedTime > '2025-01-01'`; " +
			"`mimeType = 'application/vnd.google-apps.document' and 'me' in owners`. " +
			"Operators: contains, =, !=, <, >. Combine with `and`/`or`. String literals use single quotes. " +
			"Common mimeTypes: application/vnd.google-apps.document (Docs), .spreadsheet (Sheets), " +
			".presentation (Slides), .folder (folders). Returns id/name/mimeType/modifiedTime/owner/webViewLink. " +
			"Use drive_read on a returned id to get content.",
			Parameters: obj(`"query":{"type":"string"},"max":{"type":"integer","description":"max results (default 10, cap 25)"}`, "query")},
		{Name: "drive_list", Description: "List files directly inside a Drive folder (or My Drive root if folder_id omitted). " +
			"Flat, non-recursive, newest first. To go deeper, call drive_list again with the child folder's id.",
			Parameters: obj(`"folder_id":{"type":"string","description":"Drive folder id; omit for My Drive root"},"max":{"type":"integer","description":"max results (default 10, cap 25)"}`)},
		{Name: "drive_read", Description: "Read a Drive file by id. Google Docs/Slides export to plain text, " +
			"Google Sheets exports to CSV, text/code files are returned raw. Binaries (PDFs, images, " +
			"Office docs) return metadata + a tombstone with the webViewLink. Body capped at 64KB.",
			Parameters: obj(`"id":{"type":"string"}`, "id")},

		// --- Gmail write/extras ---
		{Name: "gmail_draft", Description: "Save a plain-text email as a DRAFT (does not send). Ungated — drafts stay in the user's mailbox. Use this to prepare an email for the user to review/send.",
			Parameters: obj(`"to":{"type":"string"},"subject":{"type":"string"},"body":{"type":"string"}`, "subject", "body")},
		{Name: "gmail_labels", Description: "List the mailbox's labels (id + name + type). Use to resolve a label name to the id gmail_modify needs. System labels (INBOX, UNREAD, STARRED, IMPORTANT, SPAM, TRASH) use their name as their id.",
			Parameters: obj(``)},
		{Name: "gmail_modify", Description: "Add/remove labels on a message by id. GATED. Common ops: mark read = remove UNREAD; archive = remove INBOX; star = add STARRED. Pass label IDs (from gmail_labels for user labels; system-label names are their own ids).",
			Parameters: obj(`"id":{"type":"string"},"add":{"type":"array","items":{"type":"string"},"description":"label ids to add"},"remove":{"type":"array","items":{"type":"string"},"description":"label ids to remove"}`, "id"), Gated: true},
		{Name: "gmail_trash", Description: "Move a message to Trash by id (reversible; recoverable ~30 days). GATED. Permanent delete is not exposed.",
			Parameters: obj(`"id":{"type":"string"}`, "id"), Gated: true},

		// --- Calendar write/extras ---
		{Name: "calendar_update", Description: "Patch an existing event by id on the primary calendar. GATED (may notify attendees). Only provided fields change. Times are RFC3339, e.g. 2026-05-20T15:00:00Z.",
			Parameters: obj(`"id":{"type":"string"},"summary":{"type":"string"},"start":{"type":"string"},"end":{"type":"string"},"attendees":{"type":"array","items":{"type":"string"}},"description":{"type":"string"},"location":{"type":"string"}`, "id"), Gated: true},
		{Name: "calendar_delete", Description: "Delete an event by id from the primary calendar. GATED (cancels the event / notifies attendees).",
			Parameters: obj(`"id":{"type":"string"}`, "id"), Gated: true},
		{Name: "calendar_calendars", Description: "List the calendars the user can access (id, summary, access role). Use a returned id with calendar_freebusy.",
			Parameters: obj(``)},
		{Name: "calendar_freebusy", Description: "Query busy time blocks over a look-ahead window. Use to find when the user is free before scheduling.",
			Parameters: obj(`"days":{"type":"integer","description":"look-ahead window in days (default 7)"},"calendars":{"type":"array","items":{"type":"string"},"description":"calendar ids; omit for primary"}`)},

		// --- Drive write/extras ---
		{Name: "drive_create", Description: "Create a new text file in Drive. GATED. Provide name + content; optional mime (default text/plain) and folder_id (default My Drive root).",
			Parameters: obj(`"name":{"type":"string"},"content":{"type":"string"},"mime":{"type":"string","description":"MIME type (default text/plain)"},"folder_id":{"type":"string","description":"parent folder id; omit for root"}`, "name", "content"), Gated: true},
		{Name: "drive_folder", Description: "Create a new Drive folder. GATED. Provide name; optional parent_id (default My Drive root).",
			Parameters: obj(`"name":{"type":"string"},"parent_id":{"type":"string","description":"parent folder id; omit for root"}`, "name"), Gated: true},
		{Name: "drive_update", Description: "Rename a Drive file and/or replace its text content by id. GATED. Provide a new name, new content, or both.",
			Parameters: obj(`"id":{"type":"string"},"name":{"type":"string"},"content":{"type":"string"}`, "id"), Gated: true},
		{Name: "drive_trash", Description: "Move a Drive file to Trash by id (reversible). GATED. Permanent delete is not exposed.",
			Parameters: obj(`"id":{"type":"string"}`, "id"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "gmail_search":
		return d.gmailSearch(ctx, call.Arguments)
	case "gmail_read":
		return d.gmailRead(ctx, call.Arguments)
	case "gmail_send":
		return d.gmailSend(ctx, call.Arguments)
	case "calendar_list":
		return d.calendarList(ctx, call.Arguments)
	case "calendar_create":
		return d.calendarCreate(ctx, call.Arguments)
	case "drive_search":
		return d.driveSearch(ctx, call.Arguments)
	case "drive_list":
		return d.driveList(ctx, call.Arguments)
	case "drive_read":
		return d.driveRead(ctx, call.Arguments)
	case "gmail_draft":
		return d.gmailDraft(ctx, call.Arguments)
	case "gmail_labels":
		return d.gmailLabels(ctx, call.Arguments)
	case "gmail_modify":
		return d.gmailModify(ctx, call.Arguments)
	case "gmail_trash":
		return d.gmailTrash(ctx, call.Arguments)
	case "calendar_update":
		return d.calendarUpdate(ctx, call.Arguments)
	case "calendar_delete":
		return d.calendarDelete(ctx, call.Arguments)
	case "calendar_calendars":
		return d.calendarCalendars(ctx, call.Arguments)
	case "calendar_freebusy":
		return d.calendarFreeBusy(ctx, call.Arguments)
	case "drive_create":
		return d.driveCreate(ctx, call.Arguments)
	case "drive_folder":
		return d.driveFolder(ctx, call.Arguments)
	case "drive_update":
		return d.driveUpdate(ctx, call.Arguments)
	case "drive_trash":
		return d.driveTrash(ctx, call.Arguments)
	default:
		return "unknown gsuite tool: " + call.Name, true, nil
	}
}

func (d *Dispatcher) driveSearch(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Query string `json:"query"`
		Max   int    `json:"max"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	files, err := d.svc.Drive.Search(ctx, a.Query, a.Max)
	if err != nil {
		return "drive search failed: " + err.Error(), true, nil
	}
	if len(files) == 0 {
		return fmt.Sprintf("no files matched %q", a.Query), false, nil
	}
	return renderDriveList(files), false, nil
}

func (d *Dispatcher) driveList(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		FolderID string `json:"folder_id"`
		Max      int    `json:"max"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	files, err := d.svc.Drive.List(ctx, a.FolderID, a.Max)
	if err != nil {
		return "drive list failed: " + err.Error(), true, nil
	}
	if len(files) == 0 {
		where := "root"
		if a.FolderID != "" {
			where = "folder " + a.FolderID
		}
		return fmt.Sprintf("no files in %s", where), false, nil
	}
	return renderDriveList(files), false, nil
}

func (d *Dispatcher) driveRead(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	fc, err := d.svc.Drive.Read(ctx, a.ID)
	if err != nil {
		return "drive read failed: " + err.Error(), true, nil
	}
	body := fc.Body
	// Dispatcher-layer cap (matches gmail_read's 12 KB) to keep prompt
	// budget predictable even when the body was already pre-capped at
	// 64 KB in the plugin layer.
	const cap = 12000
	if len(body) > cap {
		body = body[:cap] + "\n…[truncated at dispatcher]"
	}
	link := fc.WebViewLink
	if link == "" {
		link = "(no link)"
	}
	owner := fc.Owner
	if owner == "" {
		owner = "(unknown)"
	}
	header := fmt.Sprintf("Name: %s\nID: %s\nMime: %s\nModified: %s\nOwner: %s\nLink: %s",
		fc.Name, fc.ID, fc.MimeType, fc.Modified.Format(time.RFC3339), owner, link)
	if fc.Truncated {
		header += "\nNote: body truncated at 64KB"
	}
	return header + "\n\n" + body, false, nil
}

// renderDriveList formats a slice of File rows as the compact text
// table the agent consumes. Mirrors gmailSearch's output style.
func renderDriveList(files []File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s):\n", len(files))
	for _, f := range files {
		modified := f.Modified.Format("2006-01-02 15:04")
		owner := f.Owner
		if owner == "" {
			owner = "(unknown)"
		}
		fmt.Fprintf(&b, "- id=%s | %s | %s | %s | %s\n", f.ID, f.Name, f.MimeType, modified, owner)
	}
	return strings.TrimRight(b.String(), "\n")
}

func unmarshal(args json.RawMessage, v any) (string, bool) {
	if err := json.Unmarshal(args, v); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	return "", false
}

func (d *Dispatcher) gmailSearch(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Query string `json:"query"`
		Max   int    `json:"max"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return "query is required", true, nil
	}
	hits, err := d.svc.Gmail.Search(ctx, a.Query, a.Max)
	if err != nil {
		return "gmail search failed: " + err.Error(), true, nil
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no messages matched %q", a.Query), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d message(s):\n", len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, "- id=%s | %s | from %s | %s\n  %s\n", h.ID, h.Subject, h.From, h.Date, h.Snippet)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) gmailRead(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	m, err := d.svc.Gmail.Read(ctx, a.ID)
	if err != nil {
		return "gmail read failed: " + err.Error(), true, nil
	}
	body := m.Body
	const cap = 12000
	if len(body) > cap {
		body = body[:cap] + "\n…[truncated]"
	}
	return fmt.Sprintf("From: %s\nTo: %s\nDate: %s\nSubject: %s\n\n%s",
		m.From, m.To, m.Date, m.Subject, body), false, nil
}

func (d *Dispatcher) gmailSend(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		To, Subject, Body string
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	detail := fmt.Sprintf("email to %s — subject %q (%d chars)", a.To, a.Subject, len(a.Body))
	if err := d.policy.gate(ctx, classSend, detail); err != nil {
		return err.Error(), true, nil
	}
	id, err := d.svc.Gmail.Send(ctx, a.To, a.Subject, a.Body)
	if err != nil {
		return "gmail send failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("sent: message id %s to %s", id, a.To), false, nil
}

func (d *Dispatcher) calendarList(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Days int `json:"days"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if a.Days <= 0 {
		a.Days = 7
	}
	now := time.Now()
	evs, err := d.svc.Calendar.ListEvents(ctx, now, now.AddDate(0, 0, a.Days), 25)
	if err != nil {
		return "calendar list failed: " + err.Error(), true, nil
	}
	if len(evs) == 0 {
		return fmt.Sprintf("no events in the next %d day(s)", a.Days), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d event(s) in the next %d day(s):\n", len(evs), a.Days)
	for _, e := range evs {
		when := e.Start.Format("Mon 2006-01-02 15:04")
		if e.AllDay {
			when = e.Start.Format("Mon 2006-01-02") + " (all day)"
		}
		loc := ""
		if e.Location != "" {
			loc = " @ " + e.Location
		}
		fmt.Fprintf(&b, "- %s — %s%s\n", when, e.Summary, loc)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) calendarCreate(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Summary, Description, Location, Start, End string
		Attendees                                  []string
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	start, err := time.Parse(time.RFC3339, a.Start)
	if err != nil {
		return "invalid start (use RFC3339, e.g. 2026-05-20T15:00:00Z): " + err.Error(), true, nil
	}
	end, err := time.Parse(time.RFC3339, a.End)
	if err != nil {
		return "invalid end (use RFC3339): " + err.Error(), true, nil
	}
	detail := fmt.Sprintf("event %q %s→%s, %d attendee(s)", a.Summary,
		start.Format(time.RFC3339), end.Format(time.RFC3339), len(a.Attendees))
	if err := d.policy.gate(ctx, classSend, detail); err != nil {
		return err.Error(), true, nil
	}
	ev, err := d.svc.Calendar.CreateEvent(ctx, EventInput{
		Summary: a.Summary, Description: a.Description, Location: a.Location,
		Start: start, End: end, Attendees: a.Attendees,
	})
	if err != nil {
		return "calendar create failed: " + err.Error(), true, nil
	}
	link := ev.HTMLLink
	if link == "" {
		link = "(no link)"
	}
	return fmt.Sprintf("created: %q id=%s %s", ev.Summary, ev.ID, link), false, nil
}

// --- Gmail write/extras handlers ---

func (d *Dispatcher) gmailDraft(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		To, Subject, Body string
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	// Ungated: a draft stays in the user's mailbox (does not leave the building).
	id, err := d.svc.Gmail.Draft(ctx, a.To, a.Subject, a.Body)
	if err != nil {
		return "gmail draft failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("draft saved: id=%s", id), false, nil
}

func (d *Dispatcher) gmailLabels(ctx context.Context, _ json.RawMessage) (string, bool, error) {
	labels, err := d.svc.Gmail.Labels(ctx)
	if err != nil {
		return "gmail labels failed: " + err.Error(), true, nil
	}
	if len(labels) == 0 {
		return "no labels", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d label(s):\n", len(labels))
	for _, l := range labels {
		fmt.Fprintf(&b, "- %s | id=%s | %s\n", l.Name, l.ID, l.Type)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) gmailModify(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID     string   `json:"id"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	detail := fmt.Sprintf("modify message %s (add %v, remove %v)", a.ID, a.Add, a.Remove)
	if err := d.policy.gate(ctx, classSend, detail); err != nil {
		return err.Error(), true, nil
	}
	labels, err := d.svc.Gmail.Modify(ctx, a.ID, a.Add, a.Remove)
	if err != nil {
		return "gmail modify failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("modified %s; labels now: %s", a.ID, strings.Join(labels, ", ")), false, nil
}

func (d *Dispatcher) gmailTrash(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if err := d.policy.gate(ctx, classSend, "trash message "+a.ID); err != nil {
		return err.Error(), true, nil
	}
	if err := d.svc.Gmail.Trash(ctx, a.ID); err != nil {
		return "gmail trash failed: " + err.Error(), true, nil
	}
	return "trashed message " + a.ID, false, nil
}

// --- Calendar write/extras handlers ---

func (d *Dispatcher) calendarUpdate(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID, Summary, Description, Location, Start, End string
		Attendees                                      []string
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.ID) == "" {
		return "event id is required", true, nil
	}
	in := EventInput{
		Summary: a.Summary, Description: a.Description, Location: a.Location,
		Attendees: a.Attendees,
	}
	if a.Start != "" {
		start, err := time.Parse(time.RFC3339, a.Start)
		if err != nil {
			return "invalid start (use RFC3339, e.g. 2026-05-20T15:00:00Z): " + err.Error(), true, nil
		}
		in.Start = start
	}
	if a.End != "" {
		end, err := time.Parse(time.RFC3339, a.End)
		if err != nil {
			return "invalid end (use RFC3339): " + err.Error(), true, nil
		}
		in.End = end
	}
	if err := d.policy.gate(ctx, classSend, "update event "+a.ID); err != nil {
		return err.Error(), true, nil
	}
	ev, err := d.svc.Calendar.UpdateEvent(ctx, a.ID, in)
	if err != nil {
		return "calendar update failed: " + err.Error(), true, nil
	}
	link := ev.HTMLLink
	if link == "" {
		link = "(no link)"
	}
	return fmt.Sprintf("updated: %q id=%s %s", ev.Summary, ev.ID, link), false, nil
}

func (d *Dispatcher) calendarDelete(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if err := d.policy.gate(ctx, classSend, "delete event "+a.ID); err != nil {
		return err.Error(), true, nil
	}
	if err := d.svc.Calendar.DeleteEvent(ctx, a.ID); err != nil {
		return "calendar delete failed: " + err.Error(), true, nil
	}
	return "deleted event " + a.ID, false, nil
}

func (d *Dispatcher) calendarCalendars(ctx context.Context, _ json.RawMessage) (string, bool, error) {
	cals, err := d.svc.Calendar.Calendars(ctx)
	if err != nil {
		return "calendar list-calendars failed: " + err.Error(), true, nil
	}
	if len(cals) == 0 {
		return "no calendars", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d calendar(s):\n", len(cals))
	for _, c := range cals {
		primary := ""
		if c.Primary {
			primary = " (primary)"
		}
		fmt.Fprintf(&b, "- %s | id=%s | %s%s\n", c.Summary, c.ID, c.AccessRole, primary)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) calendarFreeBusy(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Days      int      `json:"days"`
		Calendars []string `json:"calendars"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if a.Days <= 0 {
		a.Days = 7
	}
	now := time.Now()
	busy, err := d.svc.Calendar.FreeBusy(ctx, now, now.AddDate(0, 0, a.Days), a.Calendars)
	if err != nil {
		return "calendar freebusy failed: " + err.Error(), true, nil
	}
	if len(busy) == 0 {
		return fmt.Sprintf("no busy blocks in the next %d day(s)", a.Days), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d busy block(s) in the next %d day(s):\n", len(busy), a.Days)
	for _, iv := range busy {
		fmt.Fprintf(&b, "- %s → %s | %s\n",
			iv.Start.Format("Mon 2006-01-02 15:04"), iv.End.Format("15:04"), iv.Calendar)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// --- Drive write/extras handlers ---

func (d *Dispatcher) driveCreate(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Name, Content, Mime, FolderID string
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	detail := fmt.Sprintf("create drive file %q (%d chars)", a.Name, len(a.Content))
	if err := d.policy.gate(ctx, classSend, detail); err != nil {
		return err.Error(), true, nil
	}
	f, err := d.svc.Drive.Create(ctx, a.Name, a.Mime, a.Content, a.FolderID)
	if err != nil {
		return "drive create failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("created: %s id=%s %s", f.Name, f.ID, driveLink(f)), false, nil
}

func (d *Dispatcher) driveFolder(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if err := d.policy.gate(ctx, classSend, fmt.Sprintf("create drive folder %q", a.Name)); err != nil {
		return err.Error(), true, nil
	}
	f, err := d.svc.Drive.Folder(ctx, a.Name, a.ParentID)
	if err != nil {
		return "drive folder failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("folder created: %s id=%s %s", f.Name, f.ID, driveLink(f)), false, nil
}

func (d *Dispatcher) driveUpdate(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID, Name string
		Content  *string `json:"content"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	detail := fmt.Sprintf("update drive file %s", a.ID)
	if err := d.policy.gate(ctx, classSend, detail); err != nil {
		return err.Error(), true, nil
	}
	f, err := d.svc.Drive.Update(ctx, a.ID, a.Name, a.Content)
	if err != nil {
		return "drive update failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("updated: %s id=%s %s", f.Name, f.ID, driveLink(f)), false, nil
}

func (d *Dispatcher) driveTrash(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if msg, bad := unmarshal(args, &a); bad {
		return msg, true, nil
	}
	if err := d.policy.gate(ctx, classSend, "trash drive file "+a.ID); err != nil {
		return err.Error(), true, nil
	}
	if err := d.svc.Drive.Trash(ctx, a.ID); err != nil {
		return "drive trash failed: " + err.Error(), true, nil
	}
	return "trashed file " + a.ID, false, nil
}

func driveLink(f *File) string {
	if f.WebViewLink == "" {
		return "(no link)"
	}
	return f.WebViewLink
}
