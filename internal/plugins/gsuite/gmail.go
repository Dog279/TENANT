package gsuite

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	gmail "google.golang.org/api/gmail/v1"
)

// Gmail wraps the official Gmail v1 client, translating its types into
// tenant's normalized shapes. The official client carries auth on its
// transport (see Open), so methods just build requests and translate.
type Gmail struct{ svc *gmail.Service }

// gmailUser is the special userId meaning "the authenticated user"
// (the impersonated subject under DWD, or the OAuth/ADC principal).
const gmailUser = "me"

// MsgHdr is one search result (no body — read fetches that).
type MsgHdr struct {
	ID, From, Subject, Date, Snippet string
}

// Search runs a Gmail query (same syntax as the Gmail search box,
// e.g. `from:alice is:unread newer_than:7d`) and returns header rows.
func (g *Gmail) Search(ctx context.Context, query string, max int) ([]MsgHdr, error) {
	if max <= 0 {
		max = 10
	}
	if max > 25 {
		max = 25 // keep the per-result metadata fan-out bounded
	}
	list, err := g.svc.Users.Messages.List(gmailUser).Q(query).MaxResults(int64(max)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]MsgHdr, 0, len(list.Messages))
	for _, m := range list.Messages {
		msg, err := g.svc.Users.Messages.Get(gmailUser, m.Id).
			Format("metadata").MetadataHeaders("From", "Subject", "Date").Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		h := headerMap(msg)
		out = append(out, MsgHdr{
			ID: m.Id, From: h["from"], Subject: h["subject"],
			Date: h["date"], Snippet: msg.Snippet,
		})
	}
	return out, nil
}

// Message is a fully-read email.
type Message struct {
	ID, From, To, Subject, Date, Body string
}

// Read fetches one message and extracts its plain-text body.
func (g *Gmail) Read(ctx context.Context, id string) (*Message, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gsuite: message id required")
	}
	msg, err := g.svc.Users.Messages.Get(gmailUser, id).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	h := headerMap(msg)
	return &Message{
		ID: id, From: h["from"], To: h["to"], Subject: h["subject"],
		Date: h["date"], Body: bodyText(msg.Payload),
	}, nil
}

// Send composes a minimal RFC 5322 message and sends it. Gated by the
// dispatcher (blast-radius: this leaves the building).
func (g *Gmail) Send(ctx context.Context, to, subject, body string) (string, error) {
	if strings.TrimSpace(to) == "" || strings.TrimSpace(subject) == "" {
		return "", fmt.Errorf("gsuite: send needs both `to` and `subject`")
	}
	msg := &gmail.Message{Raw: encodeRFC822(to, subject, body)}
	res, err := g.svc.Users.Messages.Send(gmailUser, msg).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return res.Id, nil
}

// Draft saves a plain-text email as a draft (does NOT send). Ungated —
// a draft stays in the user's own mailbox, it doesn't leave the building.
// Needs the gmail.modify scope (so it only works in read/write posture).
func (g *Gmail) Draft(ctx context.Context, to, subject, body string) (string, error) {
	if strings.TrimSpace(subject) == "" && strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("gsuite: draft needs at least a subject or body")
	}
	d := &gmail.Draft{Message: &gmail.Message{Raw: encodeRFC822(to, subject, body)}}
	res, err := g.svc.Users.Drafts.Create(gmailUser, d).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return res.Id, nil
}

// Label is one Gmail label (system like INBOX/UNREAD/STARRED, or user-made).
type Label struct {
	ID, Name, Type string
}

// Labels lists the mailbox's labels so the model can resolve a name to the
// id that Modify needs. System labels (INBOX, UNREAD, STARRED, IMPORTANT,
// SPAM, TRASH, SENT, DRAFT) use their name as their id.
func (g *Gmail) Labels(ctx context.Context) ([]Label, error) {
	res, err := g.svc.Users.Labels.List(gmailUser).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]Label, 0, len(res.Labels))
	for _, l := range res.Labels {
		out = append(out, Label{ID: l.Id, Name: l.Name, Type: l.Type})
	}
	return out, nil
}

// Modify adds/removes label ids on a message — the mechanism behind
// "mark as read" (remove UNREAD), "archive" (remove INBOX), "star" (add
// STARRED). Gated (it mutates mailbox state). Returns the resulting label
// id set. Pass label *ids*; system-label names are their own ids, user
// labels come from Labels().
func (g *Gmail) Modify(ctx context.Context, id string, add, remove []string) ([]string, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gsuite: modify needs a message id")
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil, fmt.Errorf("gsuite: modify needs at least one label to add or remove")
	}
	req := &gmail.ModifyMessageRequest{AddLabelIds: add, RemoveLabelIds: remove}
	msg, err := g.svc.Users.Messages.Modify(gmailUser, id, req).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return msg.LabelIds, nil
}

// Trash moves a message to Trash (reversible; recoverable for 30 days).
// Gated. We deliberately do NOT expose permanent delete.
func (g *Gmail) Trash(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("gsuite: trash needs a message id")
	}
	_, err := g.svc.Users.Messages.Trash(gmailUser, id).Context(ctx).Do()
	return err
}

// --- helpers ---

// encodeRFC822 builds a minimal text/plain message and base64url-encodes
// it for the Gmail `raw` field.
func encodeRFC822(to, subject, body string) string {
	var msg strings.Builder
	if strings.TrimSpace(to) != "" {
		fmt.Fprintf(&msg, "To: %s\r\n", to)
	}
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n\r\n")
	msg.WriteString(body)
	return base64.RawURLEncoding.EncodeToString([]byte(msg.String()))
}

// headerMap lowercases header names for case-insensitive lookup.
func headerMap(m *gmail.Message) map[string]string {
	h := map[string]string{}
	if m == nil || m.Payload == nil {
		return h
	}
	for _, x := range m.Payload.Headers {
		h[strings.ToLower(x.Name)] = x.Value
	}
	return h
}

// bodyText walks the MIME tree preferring text/plain, falling back to a
// crude HTML strip. Gmail base64url-encodes part bodies.
func bodyText(p *gmail.MessagePart) string {
	if p == nil {
		return ""
	}
	if t := walkParts(p, "text/plain"); t != "" {
		return t
	}
	if t := walkParts(p, "text/html"); t != "" {
		return stripHTML(t)
	}
	if p.Body != nil {
		return decodeB64URL(p.Body.Data)
	}
	return ""
}

func walkParts(p *gmail.MessagePart, want string) string {
	if p == nil {
		return ""
	}
	if strings.HasPrefix(p.MimeType, want) && p.Body != nil && p.Body.Data != "" {
		return decodeB64URL(p.Body.Data)
	}
	for _, c := range p.Parts {
		if t := walkParts(c, want); t != "" {
			return t
		}
	}
	return ""
}

func decodeB64URL(s string) string {
	if s == "" {
		return ""
	}
	if b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return string(b)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return ""
}

// stripHTML is deliberately minimal (Karpathy: the simple thing that
// works) — drop tags, collapse whitespace. Good enough for an LLM.
func stripHTML(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
