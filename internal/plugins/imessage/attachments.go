package imessage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Attachment is one file sent with an inbound message (TEN-493). chat.db
// rows carry a local Path; BlueBubbles rows carry a GUID to download.
type Attachment struct {
	GUID string
	Path string // local file (chat.db); "" over BlueBubbles
	MIME string // often empty in chat.db: sniff the bytes
	Name string // the file's original name
}

// IsImage reports whether the attachment looks like a picture by its type or
// name (the bytes are sniffed again before use).
func (a Attachment) IsImage() bool {
	if strings.HasPrefix(strings.ToLower(a.MIME), "image/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(firstNonEmptyStr(a.Name, a.Path))) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".heif":
		return true
	}
	return false
}

// ErrAttachmentMissing means the file isn't on this host yet: a photo still in
// iCloud, not downloaded to this Mac.
var ErrAttachmentMissing = errors.New("not downloaded to this host yet")

// maxAttachmentRead caps one attachment read (the attachment store refuses
// larger sources anyway).
const maxAttachmentRead = 20 << 20

// attachmentsFor reads the attachments of the given message ROWIDs from
// chat.db, keyed by ROWID. Best-effort: a chat.db without the attachment
// tables (an old fixture, a schema change) yields none.
func (r *chatReader) attachmentsFor(ctx context.Context, rowIDs []int64) map[int64][]Attachment {
	if len(rowIDs) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rowIDs)), ",")
	args := make([]any, len(rowIDs))
	for i, id := range rowIDs {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT maj.message_id, a.guid, a.filename, a.mime_type, a.transfer_name
  FROM message_attachment_join maj
  JOIN attachment a ON a.ROWID = maj.attachment_id
 WHERE maj.message_id IN (`+ph+`)
 ORDER BY maj.message_id, a.ROWID`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64][]Attachment{}
	home, _ := os.UserHomeDir()
	for rows.Next() {
		var (
			id                       int64
			guid                     string
			filename, mime, transfer sql.NullString
		)
		if rows.Scan(&id, &guid, &filename, &mime, &transfer) != nil {
			continue
		}
		p := filename.String
		if strings.HasPrefix(p, "~/") && home != "" {
			p = filepath.Join(home, p[2:])
		}
		out[id] = append(out[id], Attachment{GUID: guid, Path: p, MIME: mime.String, Name: firstNonEmptyStr(transfer.String, filepath.Base(p))})
	}
	return out
}

// AttachmentBytes reads a chat.db attachment's file: a regular file only (a
// symlink is refused, so a swapped link can't point it elsewhere), at most
// 20 MB.
func (r *chatReader) AttachmentBytes(_ context.Context, a Attachment) ([]byte, error) {
	if a.Path == "" {
		return nil, ErrAttachmentMissing
	}
	fi, err := os.Lstat(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAttachmentMissing
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if fi.Size() > maxAttachmentRead {
		return nil, fmt.Errorf("over %d MB", maxAttachmentRead>>20)
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxAttachmentRead+1))
}

// AttachmentBytes downloads a BlueBubbles attachment by GUID, at most 20 MB.
func (s *Service) AttachmentBytes(ctx context.Context, a Attachment) ([]byte, error) {
	if a.GUID == "" {
		return nil, ErrAttachmentMissing
	}
	q := url.Values{}
	q.Set("password", s.a.password)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.a.base+"/api/v1/attachment/"+url.PathEscape(a.GUID)+"/download?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imessage: attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, &apiError{Status: resp.StatusCode, Msg: "attachment download failed"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentRead+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAttachmentRead {
		return nil, fmt.Errorf("over %d MB", maxAttachmentRead>>20)
	}
	return data, nil
}

// AttachmentBytes reads an attachment through the watcher's source.
func (w *Watcher) AttachmentBytes(ctx context.Context, a Attachment) ([]byte, error) {
	if ar, ok := w.src.(interface {
		AttachmentBytes(context.Context, Attachment) ([]byte, error)
	}); ok {
		return ar.AttachmentBytes(ctx, a)
	}
	return nil, errors.New("this message source can't read attachments")
}

func firstNonEmptyStr(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
