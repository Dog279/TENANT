package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TEN-332: CreateExclusive keeps the first writer's data, so racing processes
// agree on one value instead of the last rename winning.

func TestCreateExclusiveKeepsFirstWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "token")
	created, err := CreateExclusive(path, []byte("first"))
	if err != nil || !created {
		t.Fatalf("first CreateExclusive = %v, %v; want created", created, err)
	}
	created, err = CreateExclusive(path, []byte("second"))
	if err != nil || created {
		t.Fatalf("second CreateExclusive = %v, %v; want existing file kept", created, err)
	}
	assertExclusiveResult(t, path, "first")
}

func TestCreateExclusiveConcurrentWritersAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "token")
	const writers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		errs    []error
	)
	start := make(chan struct{})
	for i := range writers {
		data := fmt.Sprintf("token-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			created, err := CreateExclusive(path, []byte(data))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else if created {
				winners = append(winners, data)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("CreateExclusive errors: %v", errs)
	}
	if len(winners) != 1 {
		t.Fatalf("created by %v, want exactly one writer", winners)
	}
	assertExclusiveResult(t, path, winners[0])
}

func TestCreateExclusiveWithoutHardLinks(t *testing.T) {
	orig := linkFile
	linkFile = func(string, string) error {
		return &os.LinkError{Op: "link", Err: errors.New("hard links unsupported")}
	}
	t.Cleanup(func() { linkFile = orig })

	path := filepath.Join(t.TempDir(), "private", "token")
	created, err := CreateExclusive(path, []byte("first"))
	if err != nil || !created {
		t.Fatalf("first CreateExclusive = %v, %v; want created through the O_EXCL fallback", created, err)
	}
	created, err = CreateExclusive(path, []byte("second"))
	if err != nil || created {
		t.Fatalf("second CreateExclusive = %v, %v; want existing file kept", created, err)
	}
	assertExclusiveResult(t, path, "first")
}

// assertExclusiveResult checks path holds want, is protected, and that no
// temporary file was left beside it.
func assertExclusiveResult(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
	if report := Probe(path); !report.Protected {
		t.Fatalf("file is not protected: %+v", report)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only %s", names, filepath.Base(path))
	}
}
