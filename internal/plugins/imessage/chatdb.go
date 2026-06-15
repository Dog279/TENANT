package imessage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// This file is the SQL layer for the native transport. It is
// deliberately build-tag-free and operates on an injected *sql.DB so the
// query logic, the Chat/Message mapping, the Mac-time conversion, and the
// attributedBody fallback are all unit-testable on any OS against a
// synthetic Apple-schema database. Only the read-only open of the real
// ~/Library/Messages/chat.db is macOS-specific (native_darwin.go).
//
// Apple's schema (the subset we touch):
//
//	message(ROWID, guid, text, attributedBody, handle_id, is_from_me, date)
//	chat(ROWID, guid, chat_identifier, display_name)
//	handle(ROWID, id)                       -- id is the phone/email
//	chat_message_join(chat_id, message_id)
//	chat_handle_join(chat_id, handle_id)
//
// message.date is Mac absolute time (see macTime). text is often NULL on
// modern macOS; the real text is in attributedBody (see decodeMessageText).

// chatReader implements the read half of transport over an Apple
// chat.db handle. It carries no platform assumptions.
type chatReader struct {
	db *sql.DB
}

// InboundMessage is a Message tagged with the data the anti-loop Watcher
// needs: its monotonic ROWID (the cursor) and the chat it belongs to.
type InboundMessage struct {
	Message
	RowID    int64
	ChatGUID string
}

// macEpochOffset is the seconds between the Unix epoch (1970-01-01) and
// the Mac absolute-time epoch (2001-01-01), both UTC.
const macEpochOffset = 978307200

// macTime converts a chat.db message.date into a readable timestamp.
// High Sierra+ stores nanoseconds since 2001-01-01; legacy rows store
// seconds. We disambiguate by magnitude: a seconds value is ~1e9 today,
// a nanoseconds value ~1e18, so anything below 1e11 is treated as
// seconds. Returns "" for absent/zero dates.
func macTime(date int64) string {
	if date <= 0 {
		return ""
	}
	var sec int64
	if date < 1e11 {
		sec = date + macEpochOffset // legacy: already seconds
	} else {
		sec = date/1e9 + macEpochOffset // nanoseconds since 2001
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04")
}

// decodeMessageText prefers the plain text column and falls back to the
// attributedBody typedstream when text is NULL/empty (the common case on
// modern macOS).
func decodeMessageText(text sql.NullString, attributedBody []byte) string {
	if text.Valid {
		if s := strings.TrimRight(text.String, "\x00"); s != "" {
			return s
		}
	}
	if len(attributedBody) > 0 {
		return decodeAttributedBody(attributedBody)
	}
	return ""
}

// rowToMessage builds a normalized Message from the columns every read
// query selects. handleID is the sender's handle.id (phone/email);
// for is_from_me rows the handle is unreliable so From is "me".
func rowToMessage(guid string, text sql.NullString, attributedBody []byte, isFromMe bool, date int64, handleID sql.NullString) Message {
	from := ""
	if isFromMe {
		from = "me"
	} else if handleID.Valid {
		from = handleID.String
	}
	return Message{
		GUID:     guid,
		Text:     decodeMessageText(text, attributedBody),
		From:     from,
		IsFromMe: isFromMe,
		Date:     macTime(date),
	}
}

// ListChats returns recent conversations, most-recent-first, with
// participants and a last-message preview.
func (r *chatReader) ListChats(ctx context.Context, limit int) ([]Chat, error) {
	if limit <= 0 {
		limit = 25
	}
	// One row per chat with its last-message id and the latest date,
	// ordered by recency. Subqueries (not GROUP BY) keep the last-message
	// id unambiguous.
	const q = `
SELECT c.ROWID, c.guid, c.chat_identifier, COALESCE(c.display_name, ''),
       (SELECT cmj.message_id FROM chat_message_join cmj
          JOIN message m ON m.ROWID = cmj.message_id
         WHERE cmj.chat_id = c.ROWID
         ORDER BY m.date DESC LIMIT 1) AS last_msg_id,
       (SELECT MAX(m2.date) FROM chat_message_join cmj2
          JOIN message m2 ON m2.ROWID = cmj2.message_id
         WHERE cmj2.chat_id = c.ROWID) AS last_date
  FROM chat c
 ORDER BY last_date DESC NULLS LAST
 LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("imessage: list chats: %w", err)
	}
	defer rows.Close()

	type chatRow struct {
		rowID     int64
		guid      string
		ident     string
		display   string
		lastMsgID sql.NullInt64
	}
	var crs []chatRow
	var chatIDs []int64
	var msgIDs []int64
	for rows.Next() {
		var cr chatRow
		var lastDate sql.NullInt64
		if err := rows.Scan(&cr.rowID, &cr.guid, &cr.ident, &cr.display, &cr.lastMsgID, &lastDate); err != nil {
			return nil, fmt.Errorf("imessage: scan chat: %w", err)
		}
		crs = append(crs, cr)
		chatIDs = append(chatIDs, cr.rowID)
		if cr.lastMsgID.Valid {
			msgIDs = append(msgIDs, cr.lastMsgID.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("imessage: list chats: %w", err)
	}

	parts, err := r.participants(ctx, chatIDs)
	if err != nil {
		return nil, err
	}
	previews, err := r.messageTexts(ctx, msgIDs)
	if err != nil {
		return nil, err
	}

	out := make([]Chat, 0, len(crs))
	for _, cr := range crs {
		c := Chat{GUID: cr.guid, Name: strings.TrimSpace(cr.display), Participants: parts[cr.rowID]}
		if c.Name == "" {
			if len(c.Participants) > 0 {
				c.Name = strings.Join(c.Participants, ", ")
			} else {
				c.Name = cr.ident
			}
		}
		if cr.lastMsgID.Valid {
			c.LastMessage = previews[cr.lastMsgID.Int64]
		}
		out = append(out, c)
	}
	return out, nil
}

// participants returns chat ROWID → handle ids (phone/email).
func (r *chatReader) participants(ctx context.Context, chatIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(chatIDs) == 0 {
		return out, nil
	}
	ph, args := inClause(chatIDs)
	q := `SELECT chj.chat_id, h.id
            FROM chat_handle_join chj
            JOIN handle h ON h.ROWID = chj.handle_id
           WHERE chj.chat_id IN (` + ph + `)`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("imessage: participants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var chatID int64
		var id string
		if err := rows.Scan(&chatID, &id); err != nil {
			return nil, fmt.Errorf("imessage: scan participant: %w", err)
		}
		if id != "" {
			out[chatID] = append(out[chatID], id)
		}
	}
	return out, rows.Err()
}

// messageTexts returns message ROWID → decoded text for the given ids
// (used for last-message previews), applying the attributedBody fallback.
func (r *chatReader) messageTexts(ctx context.Context, msgIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(msgIDs) == 0 {
		return out, nil
	}
	ph, args := inClause(msgIDs)
	q := `SELECT ROWID, text, attributedBody FROM message WHERE ROWID IN (` + ph + `)`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("imessage: message texts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var text sql.NullString
		var attr []byte
		if err := rows.Scan(&id, &text, &attr); err != nil {
			return nil, fmt.Errorf("imessage: scan message text: %w", err)
		}
		out[id] = decodeMessageText(text, attr)
	}
	return out, rows.Err()
}

// ChatMessages returns the most recent messages in a chat (newest first).
func (r *chatReader) ChatMessages(ctx context.Context, chatGUID string, limit int) ([]Message, error) {
	if strings.TrimSpace(chatGUID) == "" {
		return nil, fmt.Errorf("imessage: chat guid required")
	}
	if limit <= 0 {
		limit = 25
	}
	const q = `
SELECT m.guid, m.text, m.attributedBody, m.is_from_me, m.date, h.id
  FROM message m
  JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
  JOIN chat c ON c.ROWID = cmj.chat_id
  LEFT JOIN handle h ON h.ROWID = m.handle_id
 WHERE c.guid = ?
 ORDER BY m.date DESC
 LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, chatGUID, limit)
	if err != nil {
		return nil, fmt.Errorf("imessage: chat messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// SearchMessages finds messages whose decoded text contains the term
// (case-insensitive substring), newest first. Because text is often NULL
// and the real content is in attributedBody (un-LIKE-able), we scan a
// bounded recent window and substring-match in Go after decoding.
func (r *chatReader) SearchMessages(ctx context.Context, text string, limit int) ([]Message, error) {
	term := strings.TrimSpace(text)
	if term == "" {
		return nil, fmt.Errorf("imessage: search text required")
	}
	if limit <= 0 {
		limit = 25
	}
	// Scan cap: bound the work on large histories while covering recent
	// conversation. Generous relative to limit.
	scan := limit * 50
	if scan < 2000 {
		scan = 2000
	}
	const q = `
SELECT m.guid, m.text, m.attributedBody, m.is_from_me, m.date, h.id
  FROM message m
  LEFT JOIN handle h ON h.ROWID = m.handle_id
 ORDER BY m.date DESC
 LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, scan)
	if err != nil {
		return nil, fmt.Errorf("imessage: search: %w", err)
	}
	defer rows.Close()
	all, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(term)
	out := make([]Message, 0, limit)
	for _, m := range all {
		if strings.Contains(strings.ToLower(m.Text), needle) {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// MessagesSince returns messages with ROWID strictly greater than
// afterRowID, oldest-first (so the cursor advances monotonically). It is
// the read primitive the anti-loop Watcher polls. Each row carries its
// ROWID and chat guid.
func (r *chatReader) MessagesSince(ctx context.Context, afterRowID int64, limit int) ([]InboundMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
SELECT m.ROWID, c.guid, m.guid, m.text, m.attributedBody, m.is_from_me, m.date, h.id
  FROM message m
  JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
  JOIN chat c ON c.ROWID = cmj.chat_id
  LEFT JOIN handle h ON h.ROWID = m.handle_id
 WHERE m.ROWID > ?
 ORDER BY m.ROWID ASC
 LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, afterRowID, limit)
	if err != nil {
		return nil, fmt.Errorf("imessage: messages since: %w", err)
	}
	defer rows.Close()
	var out []InboundMessage
	for rows.Next() {
		var (
			rowID    int64
			chatGUID string
			guid     string
			text     sql.NullString
			attr     []byte
			isFromMe bool
			date     int64
			handleID sql.NullString
		)
		if err := rows.Scan(&rowID, &chatGUID, &guid, &text, &attr, &isFromMe, &date, &handleID); err != nil {
			return nil, fmt.Errorf("imessage: scan since: %w", err)
		}
		out = append(out, InboundMessage{
			Message:  rowToMessage(guid, text, attr, isFromMe, date, handleID),
			RowID:    rowID,
			ChatGUID: chatGUID,
		})
	}
	return out, rows.Err()
}

// LatestRowID returns the current maximum message ROWID (0 when the table is
// empty). The Watcher seeds its cursor here so a fresh or fallen-behind
// responder watches NEW messages instead of replaying the whole chat.db history
// oldest-first (TEN-230). Cheap: an indexed MAX over the primary key.
func (r *chatReader) LatestRowID(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	if err := r.db.QueryRowContext(ctx, `SELECT MAX(ROWID) FROM message`).Scan(&max); err != nil {
		return 0, fmt.Errorf("imessage: latest rowid: %w", err)
	}
	return max.Int64, nil // NULL (empty table) → 0
}

// scanMessages reads rows shaped (guid, text, attributedBody, is_from_me,
// date, handle.id) into normalized Messages.
func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var (
			guid     string
			text     sql.NullString
			attr     []byte
			isFromMe bool
			date     int64
			handleID sql.NullString
		)
		if err := rows.Scan(&guid, &text, &attr, &isFromMe, &date, &handleID); err != nil {
			return nil, fmt.Errorf("imessage: scan message: %w", err)
		}
		out = append(out, rowToMessage(guid, text, attr, isFromMe, date, handleID))
	}
	return out, rows.Err()
}

// inClause builds "?,?,?" and the matching args for an IN (...) over
// int64 ids.
func inClause(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return ph, args
}

// normalizeHandle canonicalizes a phone/email handle for allowlist and
// echo-dedup matching. Emails are lowercased; phone numbers keep a
// leading '+' and digits only (toward E.164), dropping spaces, dashes,
// and parens.
func normalizeHandle(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if strings.Contains(h, "@") {
		return strings.ToLower(h)
	}
	var b strings.Builder
	for i, ch := range h {
		switch {
		case ch >= '0' && ch <= '9':
			b.WriteRune(ch)
		case ch == '+' && i == 0:
			b.WriteRune(ch)
		}
	}
	return b.String()
}
