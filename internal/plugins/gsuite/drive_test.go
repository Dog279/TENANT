package gsuite

// TEN-72: Drive plugin tests. Mirrors api_test.go's pattern — httptest
// server with hand-written endpoint stubs, fake token source via the
// `runner` seam exposed by gcloud auth, full request/response wired
// through api.do.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// driveStubMux returns an httptest mux that fakes the Drive v3 endpoints
// the plugin hits. Tests register expectations via the returned setters.
type driveStub struct {
	mux *http.ServeMux

	// Per-test overrides.
	listFunc   func(q string, w http.ResponseWriter)
	getFunc    func(id string, w http.ResponseWriter)
	mediaFunc  func(id string, w http.ResponseWriter)
	exportFunc func(id, mime string, w http.ResponseWriter)
}

func newDriveStub() *driveStub {
	s := &driveStub{mux: http.NewServeMux()}
	s.mux.HandleFunc("/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		// files.list: ?q=...&fields=...
		if r.URL.Path == "/drive/v3/files" && s.listFunc != nil {
			s.listFunc(r.URL.Query().Get("q"), w)
			return
		}
		http.NotFound(w, r)
	})
	s.mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		// /drive/v3/files/{id}                — get metadata
		// /drive/v3/files/{id}?alt=media      — raw content
		// /drive/v3/files/{id}/export?mimeType=… — Google native export
		path := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(path, "/export"):
			id := strings.TrimSuffix(path, "/export")
			if s.exportFunc != nil {
				s.exportFunc(id, q.Get("mimeType"), w)
				return
			}
		case q.Get("alt") == "media":
			if s.mediaFunc != nil {
				s.mediaFunc(path, w)
				return
			}
		default:
			if s.getFunc != nil {
				s.getFunc(path, w)
				return
			}
		}
		http.NotFound(w, r)
	})
	return s
}

// newDriveClientForTest wires a Drive client over an httptest server.
// The official client carries auth on its transport in prod; tests skip
// auth and inject a transport that rewrites googleapis.com → the test
// server (option.WithHTTPClient short-circuits credential resolution).
func newDriveClientForTest(t *testing.T, stub *driveStub) (*Drive, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(stub.mux)
	hc := &http.Client{Transport: rewriteToServer(srv)}
	ds, err := drive.NewService(context.Background(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	return &Drive{svc: ds}, srv
}

// rewriteToServer is a tiny RoundTripper that swaps googleapis.com
// requests to the httptest server's base. Keeps tests deterministic
// without changing production code.
type rewriteRT struct {
	server *httptest.Server
	inner  http.RoundTripper
}

func (r *rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "googleapis.com") {
		u := *req.URL
		// Replace scheme+host with the test server's.
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(r.server.URL, "http://")
		req2 := req.Clone(req.Context())
		req2.URL = &u
		req2.Host = u.Host
		return r.inner.RoundTrip(req2)
	}
	return r.inner.RoundTrip(req)
}

func rewriteToServer(srv *httptest.Server) http.RoundTripper {
	return &rewriteRT{server: srv, inner: http.DefaultTransport}
}

// --- tests ---

func TestDriveSearch_HappyPath(t *testing.T) {
	stub := newDriveStub()
	stub.listFunc = func(q string, w http.ResponseWriter) {
		if !strings.Contains(q, "name contains 'spec'") {
			http.Error(w, "unexpected q: "+q, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []map[string]any{
				{
					"id":           "file1",
					"name":         "auth spec.gdoc",
					"mimeType":     "application/vnd.google-apps.document",
					"modifiedTime": "2025-01-15T10:30:00Z",
					"size":         "0",
					"webViewLink":  "https://docs.google.com/file1",
					"owners":       []map[string]string{{"emailAddress": "alice@example.com", "displayName": "Alice"}},
				},
			},
		})
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()

	files, err := d.Search(context.Background(), "name contains 'spec'", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file; got %d", len(files))
	}
	f := files[0]
	if f.ID != "file1" || f.Name != "auth spec.gdoc" {
		t.Errorf("file fields wrong: %+v", f)
	}
	if f.Owner != "alice@example.com" {
		t.Errorf("owner email not extracted: %q", f.Owner)
	}
}

func TestDriveSearch_EmptyQueryRejected(t *testing.T) {
	stub := newDriveStub()
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	_, err := d.Search(context.Background(), "  ", 0)
	if err == nil || !strings.Contains(err.Error(), "query required") {
		t.Errorf("expected query-required error; got %v", err)
	}
}

func TestDriveSearch_OverlongQueryRejected(t *testing.T) {
	stub := newDriveStub()
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	_, err := d.Search(context.Background(), strings.Repeat("a", 1001), 0)
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("expected too-long error; got %v", err)
	}
}

func TestDriveList_RootDefault(t *testing.T) {
	stub := newDriveStub()
	stub.listFunc = func(q string, w http.ResponseWriter) {
		if !strings.Contains(q, "'root' in parents") {
			http.Error(w, "expected root parents q; got "+q, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	_, err := d.List(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
}

// Position A amendment: folder IDs with single-quotes must be escaped.
func TestDriveList_EscapesSingleQuotes(t *testing.T) {
	stub := newDriveStub()
	var observedQ string
	stub.listFunc = func(q string, w http.ResponseWriter) {
		observedQ = q
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	_, err := d.List(context.Background(), "weird'id", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(observedQ, `weird\'id`) {
		t.Errorf("single-quote not escaped in q=%s", observedQ)
	}
}

func TestDriveRead_GoogleDocExports(t *testing.T) {
	stub := newDriveStub()
	stub.getFunc = func(id string, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           id,
			"name":         "Q3 plan.gdoc",
			"mimeType":     "application/vnd.google-apps.document",
			"modifiedTime": "2025-03-01T12:00:00Z",
			"webViewLink":  "https://docs.google.com/" + id,
		})
	}
	stub.exportFunc = func(id, mime string, w http.ResponseWriter) {
		if mime != "text/plain" {
			http.Error(w, "expected text/plain export; got "+mime, http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Q3 plan body content")
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()

	fc, err := d.Read(context.Background(), "doc123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.Body, "Q3 plan body content") {
		t.Errorf("Doc body not exported: %q", fc.Body)
	}
}

func TestDriveRead_SheetsExportsAsCSV(t *testing.T) {
	stub := newDriveStub()
	stub.getFunc = func(id string, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "OKRs.gsheet",
			"mimeType": "application/vnd.google-apps.spreadsheet",
		})
	}
	var exportedMime string
	stub.exportFunc = func(id, mime string, w http.ResponseWriter) {
		exportedMime = mime
		fmt.Fprintln(w, "header1,header2\nv1,v2")
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	fc, err := d.Read(context.Background(), "sheet1")
	if err != nil {
		t.Fatal(err)
	}
	if exportedMime != "text/csv" {
		t.Errorf("expected text/csv export; got %q", exportedMime)
	}
	if !strings.Contains(fc.Body, "header1,header2") {
		t.Errorf("CSV body not returned: %q", fc.Body)
	}
}

func TestDriveRead_TextFileRawDownload(t *testing.T) {
	stub := newDriveStub()
	stub.getFunc = func(id string, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "notes.md", "mimeType": "text/markdown",
		})
	}
	stub.mediaFunc = func(id string, w http.ResponseWriter) {
		fmt.Fprintln(w, "# Notes\n\nplain markdown content")
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	fc, err := d.Read(context.Background(), "md1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.Body, "plain markdown content") {
		t.Errorf("text/markdown not downloaded: %q", fc.Body)
	}
}

func TestDriveRead_BinaryReturnsTombstone(t *testing.T) {
	stub := newDriveStub()
	stub.getFunc = func(id string, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "contract.pdf",
			"mimeType":    "application/pdf",
			"size":        "1048576",
			"webViewLink": "https://drive.google.com/" + id,
		})
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	fc, err := d.Read(context.Background(), "pdf1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.Body, "binary file") {
		t.Errorf("PDF should return tombstone; got %q", fc.Body)
	}
	if !strings.Contains(fc.Body, "drive.google.com") {
		t.Errorf("tombstone should include webViewLink; got %q", fc.Body)
	}
}

func TestDriveRead_TruncatesLargeBody(t *testing.T) {
	stub := newDriveStub()
	stub.getFunc = func(id string, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "big.txt", "mimeType": "text/plain",
		})
	}
	stub.mediaFunc = func(id string, w http.ResponseWriter) {
		// Write 80KB — driveReadCap is 64KB so we expect truncation.
		_, _ = io.WriteString(w, strings.Repeat("x", 80*1024))
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	fc, err := d.Read(context.Background(), "big1")
	if err != nil {
		t.Fatal(err)
	}
	if !fc.Truncated {
		t.Error("expected Truncated=true on 80KB body")
	}
	if len(fc.Body) != driveReadCap {
		t.Errorf("body should be exactly %d bytes; got %d", driveReadCap, len(fc.Body))
	}
}

func TestDriveRead_EmptyIDRejected(t *testing.T) {
	stub := newDriveStub()
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	_, err := d.Read(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "id required") {
		t.Errorf("expected id-required error; got %v", err)
	}
}

func TestDriveRead_PropagatesGoogleErrors(t *testing.T) {
	stub := newDriveStub()
	stub.getFunc = func(id string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code": 403, "message": "Insufficient Permission",
				"status": "PERMISSION_DENIED",
			},
		})
	}
	d, srv := newDriveClientForTest(t, stub)
	defer srv.Close()
	_, err := d.Read(context.Background(), "forbidden")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		t.Errorf("expected *googleapi.Error; got %T (%v)", err, err)
	}
	if !strings.Contains(err.Error(), "Insufficient Permission") {
		t.Errorf("error should surface Google's message; got %v", err)
	}
}

func TestIsReadableTextMIME(t *testing.T) {
	yes := []string{
		"text/plain", "text/markdown", "text/csv", "text/html",
		"text/x-go", "text/x-python",
		"application/json", "application/xml", "application/x-yaml",
		"application/javascript", "application/typescript",
		"application/sql", "application/x-sh", "application/x-shellscript",
	}
	no := []string{
		"application/pdf", "image/png", "image/jpeg",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/octet-stream", "video/mp4", "audio/mpeg",
		"application/zip", "application/x-rar-compressed",
	}
	for _, m := range yes {
		if !isReadableTextMIME(m) {
			t.Errorf("expected readable: %s", m)
		}
	}
	for _, m := range no {
		if isReadableTextMIME(m) {
			t.Errorf("expected NOT readable: %s", m)
		}
	}
}

func TestGoogleExportMime(t *testing.T) {
	cases := []struct {
		mime, out string
		ok        bool
	}{
		{"application/vnd.google-apps.document", "text/plain", true},
		{"application/vnd.google-apps.spreadsheet", "text/csv", true},
		{"application/vnd.google-apps.presentation", "text/plain", true},
		{"application/vnd.google-apps.form", "", false},
		{"text/plain", "", false},
		{"application/pdf", "", false},
	}
	for _, c := range cases {
		out, ok := googleExportMime(c.mime)
		if out != c.out || ok != c.ok {
			t.Errorf("googleExportMime(%q) = (%q, %v); want (%q, %v)", c.mime, out, ok, c.out, c.ok)
		}
	}
}

func TestScopesFor(t *testing.T) {
	has := func(scopes []string, want string) bool {
		for _, s := range scopes {
			if s == want {
				return true
			}
		}
		return false
	}

	// Read-only posture: least-privilege readonly scopes, no write/send.
	ro := scopesFor(false)
	for _, want := range []string{scopeGmailRead, scopeCalRead, scopeDriveRead} {
		if !has(ro, want) {
			t.Errorf("scopesFor(false) missing %s: %v", want, ro)
		}
	}
	for _, deny := range []string{scopeGmailSend, scopeGmailMod, scopeDriveFull, scopeCalFull} {
		if has(ro, deny) {
			t.Errorf("scopesFor(false) must NOT grant write scope %s: %v", deny, ro)
		}
	}

	// Read/write posture: modify+send Gmail, full Calendar + Drive.
	rw := scopesFor(true)
	for _, want := range []string{scopeGmailMod, scopeGmailSend, scopeCalFull, scopeDriveFull} {
		if !has(rw, want) {
			t.Errorf("scopesFor(true) missing %s: %v", want, rw)
		}
	}
	// Read/write deliberately does not also request the readonly scopes
	// (modify/full subsume them) — and never the permanent-delete scope.
	if has(rw, "https://mail.google.com/") {
		t.Errorf("scopesFor(true) must never grant full-mailbox (permanent delete): %v", rw)
	}
}
