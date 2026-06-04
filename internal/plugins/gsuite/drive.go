package gsuite

// TEN-72: Drive support. Wraps the official google.golang.org/api/drive/v3
// client (the package moved off hand-rolled REST to get full Workspace API
// coverage — see gsuite.go). Reads are MIME-aware:
//   - drive_search: Drive q= syntax pass-through (length-capped)
//   - drive_list:   flat folder listing, non-recursive
//   - drive_read:   Google native → export; text/code → raw; binaries →
//                   metadata + tombstone with webViewLink
//
// Writes (create/folder/update/trash) are gated by the dispatcher. We
// expose trash (reversible) but never permanent delete.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// Drive wraps the official Google Drive v3 client.
type Drive struct{ svc *drive.Service }

const driveFileFields = "id,name,mimeType,modifiedTime,size,webViewLink,owners(emailAddress,displayName)"

// driveFolderMime is the magic mimeType that marks a Drive file as a folder.
const driveFolderMime = "application/vnd.google-apps.folder"

// driveReadCap bounds the body returned by drive_read so a 5 MB Doc
// can't blow the LLM's context budget. 64 KB matches the spirit of
// gmail's 12 KB dispatch-layer cap (Drive files trend larger so we
// allow a bit more; dispatcher caps again before prompt assembly).
const driveReadCap = 64 * 1024

// File is one Drive entry (no body — Read fetches that).
type File struct {
	ID, Name, MimeType, WebViewLink string
	Modified                        time.Time
	Size                            int64
	Owner                           string
}

// FileContent is File + extracted body (or a tombstone).
type FileContent struct {
	File
	Body      string
	Truncated bool
}

// Search runs a Drive q= query (e.g. `name contains 'auth spec'`,
// `fullText contains 'rate limiting' and modifiedTime > '2025-01-01'`,
// `mimeType = 'application/vnd.google-apps.document'`). We pass the
// query through after trimming + a length cap; Drive's parser owns the
// syntax. Errors flow back from the official client so the LLM gets
// actionable feedback ("Invalid Value: name LIKE …" → it rewrites).
func (d *Drive) Search(ctx context.Context, query string, max int) ([]File, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("gsuite: drive search query required")
	}
	if len(q) > 1000 {
		return nil, fmt.Errorf("gsuite: drive search query too long (%d > 1000 chars)", len(q))
	}
	return d.list(ctx, q, max)
}

// List returns files directly in folderID (or My Drive root if empty).
// Flat, non-recursive — the model can drill down by calling List again
// with a child folder's id.
//
// Defense: sanitize single-quotes in folder ID. Drive's q= grammar uses
// single-quoted string literals with backslash escapes; a hostile or
// typo'd id containing `'` could otherwise break out of the literal.
func (d *Drive) List(ctx context.Context, folderID string, max int) ([]File, error) {
	parent := "root"
	if id := strings.TrimSpace(folderID); id != "" {
		parent = strings.ReplaceAll(id, "'", `\'`)
	}
	q := fmt.Sprintf("'%s' in parents and trashed = false", parent)
	return d.list(ctx, q, max)
}

func (d *Drive) list(ctx context.Context, q string, max int) ([]File, error) {
	if max <= 0 {
		max = 10
	}
	if max > 25 {
		max = 25
	}
	resp, err := d.svc.Files.List().Q(q).PageSize(int64(max)).
		OrderBy("modifiedTime desc").
		Fields(googleapi.Field(fmt.Sprintf("files(%s)", driveFileFields))).
		Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(resp.Files))
	for _, f := range resp.Files {
		out = append(out, normalizeFile(f))
	}
	return out, nil
}

// Read fetches a file by id. Behavior by mimeType:
//   - Google Docs / Slides → export text/plain
//   - Google Sheets        → export text/csv
//   - text/* + small allow-list of code-ish JSON/XML/YAML/SQL → raw media
//   - anything else        → metadata only, Body is a "[binary file]"
//     tombstone with the webViewLink so the model can route the user to Drive
func (d *Drive) Read(ctx context.Context, id string) (*FileContent, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gsuite: drive file id required")
	}
	df, err := d.svc.Files.Get(id).Fields(googleapi.Field(driveFileFields)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	fc := &FileContent{File: normalizeFile(df)}

	if exportMime, ok := googleExportMime(df.MimeType); ok {
		resp, err := d.svc.Files.Export(id, exportMime).Context(ctx).Download()
		if err != nil {
			return nil, err
		}
		fc.Body, fc.Truncated, err = readCapped(resp)
		if err != nil {
			return nil, err
		}
		return fc, nil
	}
	if isReadableTextMIME(df.MimeType) {
		resp, err := d.svc.Files.Get(id).Context(ctx).Download()
		if err != nil {
			return nil, err
		}
		fc.Body, fc.Truncated, err = readCapped(resp)
		if err != nil {
			return nil, err
		}
		return fc, nil
	}
	link := df.WebViewLink
	if link == "" {
		link = "(no link)"
	}
	fc.Body = fmt.Sprintf("[binary file: mimeType=%s, %d bytes — content not extracted in this MVP. "+
		"Open via %s, or ask the user to paste the relevant text.]",
		df.MimeType, fc.Size, link)
	return fc, nil
}

// Create makes a new file with text content in folderID (My Drive root if
// empty). Gated. Returns the created file's metadata.
func (d *Drive) Create(ctx context.Context, name, mimeType, content, folderID string) (*File, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("gsuite: drive create needs a name")
	}
	meta := &drive.File{Name: name}
	if mimeType != "" {
		meta.MimeType = mimeType
	}
	if p := strings.TrimSpace(folderID); p != "" {
		meta.Parents = []string{p}
	}
	created, err := d.svc.Files.Create(meta).
		Media(strings.NewReader(content)).
		Fields(googleapi.Field(driveFileFields)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := normalizeFile(created)
	return &out, nil
}

// Folder creates a new folder named name inside parentID (root if empty).
// Gated. Returns the folder's metadata.
func (d *Drive) Folder(ctx context.Context, name, parentID string) (*File, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("gsuite: drive folder needs a name")
	}
	meta := &drive.File{Name: name, MimeType: driveFolderMime}
	if p := strings.TrimSpace(parentID); p != "" {
		meta.Parents = []string{p}
	}
	created, err := d.svc.Files.Create(meta).
		Fields(googleapi.Field(driveFileFields)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := normalizeFile(created)
	return &out, nil
}

// Update renames a file and/or replaces its text content. An empty
// newName leaves the name unchanged; a nil content pointer leaves the
// body unchanged. Gated.
func (d *Drive) Update(ctx context.Context, id, newName string, content *string) (*File, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gsuite: drive update needs a file id")
	}
	if newName == "" && content == nil {
		return nil, fmt.Errorf("gsuite: drive update needs a new name or new content")
	}
	meta := &drive.File{}
	if newName != "" {
		meta.Name = newName
	}
	call := d.svc.Files.Update(id, meta).Fields(googleapi.Field(driveFileFields))
	if content != nil {
		call = call.Media(strings.NewReader(*content))
	}
	updated, err := call.Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := normalizeFile(updated)
	return &out, nil
}

// Trash moves a file to Trash (reversible). Gated. We deliberately do
// NOT expose permanent delete.
func (d *Drive) Trash(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("gsuite: drive trash needs a file id")
	}
	_, err := d.svc.Files.Update(id, &drive.File{
		Trashed:         true,
		ForceSendFields: []string{"Trashed"},
	}).Context(ctx).Do()
	return err
}

// readCapped reads at most driveReadCap+1 bytes from resp.Body so we can
// flag truncation, then closes the body.
func readCapped(resp *http.Response) (string, bool, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(driveReadCap)+1))
	if err != nil {
		return "", false, err
	}
	truncated := len(b) > driveReadCap
	if truncated {
		b = b[:driveReadCap]
	}
	return string(b), truncated, nil
}

// googleExportMime maps Google-native types to the text format we export
// to. Slides goes to text/plain (LLMs read slide text better than PDFs).
func googleExportMime(m string) (string, bool) {
	switch m {
	case "application/vnd.google-apps.document":
		return "text/plain", true
	case "application/vnd.google-apps.spreadsheet":
		return "text/csv", true
	case "application/vnd.google-apps.presentation":
		return "text/plain", true
	}
	return "", false
}

// isReadableTextMIME is a STRICT allow-list. Prefix-globbing
// application/x-* would let weird binaries through; enumerate the few we
// actually want to read raw via media download.
func isReadableTextMIME(m string) bool {
	if strings.HasPrefix(m, "text/") {
		return true
	}
	switch m {
	case "application/json",
		"application/xml",
		"application/x-yaml",
		"application/yaml",
		"application/javascript",
		"application/typescript",
		"application/sql",
		"application/x-sh",
		"application/x-shellscript":
		return true
	}
	return false
}

func normalizeFile(f *drive.File) File {
	if f == nil {
		return File{}
	}
	out := File{
		ID: f.Id, Name: f.Name, MimeType: f.MimeType, WebViewLink: f.WebViewLink,
		Size: f.Size,
	}
	if t, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
		out.Modified = t
	}
	if len(f.Owners) > 0 {
		out.Owner = f.Owners[0].EmailAddress
		if out.Owner == "" {
			out.Owner = f.Owners[0].DisplayName
		}
	}
	return out
}
