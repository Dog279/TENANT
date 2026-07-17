package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAppendAcknowledgesOnlyAfterSyncAndClose(t *testing.T) {
	f := &fakeArchiveFile{}
	installTestFileFactory(t, func(string, int) (archiveFile, error) {
		return f, nil
	})

	w := NewWriter(t.TempDir())
	err := w.Append(Event{
		Timestamp: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		SessionID: "durable",
		Role:      "user",
		Content:   "acknowledged",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got, want := strings.Join(f.operations, ","), "write,sync,close"; got != want {
		t.Fatalf("operations = %q, want %q", got, want)
	}
	if len(f.durable) == 0 {
		t.Fatal("Append returned success before the event was synced")
	}
	if f.durable[len(f.durable)-1] != '\n' {
		t.Fatal("synced record is not newline-terminated")
	}
	var got Event
	if err := json.Unmarshal(bytes.TrimSuffix(f.durable, []byte{'\n'}), &got); err != nil {
		t.Fatalf("synced record is invalid JSON: %v", err)
	}
	if got.Content != "acknowledged" {
		t.Fatalf("synced Content = %q, want acknowledged", got.Content)
	}
}

func TestAppendSurfacesWriteSyncAndCloseErrors(t *testing.T) {
	tests := []struct {
		name          string
		file          *fakeArchiveFile
		wantError     string
		wantSyncCalls int
	}{
		{
			name:      "write",
			file:      &fakeArchiveFile{writeErr: errors.New("write failed")},
			wantError: "write failed",
		},
		{
			name:      "short write",
			file:      &fakeArchiveFile{shortWrite: true},
			wantError: "short write",
		},
		{
			name:          "sync",
			file:          &fakeArchiveFile{syncErr: errors.New("sync failed")},
			wantError:     "sync failed",
			wantSyncCalls: 1,
		},
		{
			name:          "close",
			file:          &fakeArchiveFile{closeErr: errors.New("close failed")},
			wantError:     "close failed",
			wantSyncCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installTestFileFactory(t, func(string, int) (archiveFile, error) {
				return tt.file, nil
			})
			w := NewWriter(t.TempDir())
			err := w.Append(Event{
				Timestamp: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
				SessionID: "errors",
				Role:      "user",
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Append error = %v, want error containing %q", err, tt.wantError)
			}
			if tt.file.syncCalls != tt.wantSyncCalls {
				t.Fatalf("Sync calls = %d, want %d", tt.file.syncCalls, tt.wantSyncCalls)
			}
			if tt.file.closeCalls != 1 {
				t.Fatalf("Close calls = %d, want 1", tt.file.closeCalls)
			}
			if tt.name == "sync" && len(tt.file.durable) != 0 {
				t.Fatal("sync failure incorrectly marked the event durable")
			}
		})
	}
}

func TestStreamAndReopenedWriterRecoverTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	w := NewWriter(dir)
	for i, content := range []string{"preserved", "torn"} {
		if err := w.Append(Event{
			Timestamp: ts.Add(time.Duration(i) * time.Second),
			SessionID: "recovery",
			Role:      "user",
			Content:   content,
		}); err != nil {
			t.Fatalf("Append %q: %v", content, err)
		}
	}

	path := PathFor(dir, ts, "recovery")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	firstEnd := bytes.IndexByte(data, '\n') + 1
	if firstEnd <= 0 || firstEnd >= len(data) {
		t.Fatalf("unexpected archive fixture: %q", data)
	}
	tornSize := firstEnd + (len(data)-firstEnd)/2
	if err := os.Truncate(path, int64(tornSize)); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	assertStreamContents(t, NewReader(dir), []string{"preserved"})

	reopened := NewWriter(dir)
	if err := reopened.Append(Event{
		Timestamp: ts.Add(2 * time.Second),
		SessionID: "recovery",
		Role:      "assistant",
		Content:   "after-recovery",
	}); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	assertStreamContents(t, NewReader(dir), []string{"preserved", "after-recovery"})

	recovered, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile recovered archive: %v", err)
	}
	for i, line := range bytes.Split(bytes.TrimSuffix(recovered, []byte{'\n'}), []byte{'\n'}) {
		if !json.Valid(line) {
			t.Fatalf("recovered line %d is invalid JSON: %q", i, line)
		}
	}
}

func TestBatchedSyncPolicyAndFlush(t *testing.T) {
	var files []*fakeArchiveFile
	installTestFileFactory(t, func(string, int) (archiveFile, error) {
		f := &fakeArchiveFile{}
		files = append(files, f)
		return f, nil
	})

	w := NewWriterWithOptions(t.TempDir(), WriterOptions{SyncInterval: 2})
	e := Event{
		Timestamp: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		SessionID: "batch",
		Role:      "user",
	}
	if err := w.Append(e); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if files[0].syncCalls != 0 {
		t.Fatalf("first Append Sync calls = %d, want 0", files[0].syncCalls)
	}
	if err := w.Append(e); err != nil {
		t.Fatalf("second Append: %v", err)
	}
	if files[1].syncCalls != 1 {
		t.Fatalf("second Append Sync calls = %d, want 1", files[1].syncCalls)
	}
	if err := w.Append(e); err != nil {
		t.Fatalf("third Append: %v", err)
	}
	if files[2].syncCalls != 0 {
		t.Fatalf("third Append Sync calls = %d, want 0", files[2].syncCalls)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(files) != 4 || files[3].syncCalls != 1 || files[3].closeCalls != 1 {
		t.Fatalf("Flush files = %+v, want a synced and closed flush handle", files)
	}
}

func BenchmarkArchiveAppendSyncPolicy(b *testing.B) {
	for _, tc := range []struct {
		name     string
		interval int
	}{
		{name: "per-event", interval: 1},
		{name: "batch-64", interval: 64},
	} {
		b.Run(tc.name, func(b *testing.B) {
			w := NewWriterWithOptions(b.TempDir(), WriterOptions{SyncInterval: tc.interval})
			e := Event{
				Timestamp: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
				SessionID: "benchmark",
				Role:      "user",
				Content:   "durability benchmark",
			}
			b.ResetTimer()
			for b.Loop() {
				if err := w.Append(e); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := w.Flush(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

func assertStreamContents(t *testing.T, r *Reader, want []string) {
	t.Helper()
	var got []string
	for e, err := range r.Stream(Filter{}) {
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		got = append(got, e.Content)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Stream contents = %v, want %v", got, want)
	}
}

func installTestFileFactory(t *testing.T, factory func(string, int) (archiveFile, error)) {
	t.Helper()
	if testFileFactory != nil {
		t.Fatal("testFileFactory already installed")
	}
	testFileFactory = factory
	t.Cleanup(func() { testFileFactory = nil })
}

type fakeArchiveFile struct {
	written    []byte
	durable    []byte
	operations []string
	writeErr   error
	shortWrite bool
	syncErr    error
	closeErr   error
	syncCalls  int
	closeCalls int
}

func (f *fakeArchiveFile) Write(p []byte) (int, error) {
	f.operations = append(f.operations, "write")
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.shortWrite {
		n := len(p) / 2
		f.written = append(f.written, p[:n]...)
		return n, nil
	}
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeArchiveFile) Sync() error {
	f.operations = append(f.operations, "sync")
	f.syncCalls++
	if f.syncErr != nil {
		return f.syncErr
	}
	f.durable = append(f.durable[:0], f.written...)
	return nil
}

func (f *fakeArchiveFile) Close() error {
	f.operations = append(f.operations, "close")
	f.closeCalls++
	return f.closeErr
}
