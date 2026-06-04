package soul_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"tenant/internal/memory/soul"
)

// Clone must deep-copy the slice fields: mutating the clone (append/edit)
// must not be visible through the original's backing array.
func TestSoul_CloneIsolation(t *testing.T) {
	orig := soul.NewDefault("main")
	orig.User.Facts = []string{"a", "b"}
	orig.Instructions.Items = []string{"x"}

	c := orig.Clone()
	c.User.Facts[0] = "MUTATED"
	c.User.Facts = append(c.User.Facts, "c")
	c.Instructions.Items = append(c.Instructions.Items, "y")

	if orig.User.Facts[0] != "a" {
		t.Fatalf("clone aliased the original's slice: %q", orig.User.Facts[0])
	}
	if len(orig.User.Facts) != 2 || len(orig.Instructions.Items) != 1 {
		t.Fatalf("clone append leaked into original: facts=%v instr=%v", orig.User.Facts, orig.Instructions.Items)
	}
}

// THE RACE TEST. Many readers Load + Render the live soul while writers
// swap in fresh souls via Edit. -race is unavailable here (no cgo), so we
// use a high iteration count and assert every read is internally consistent
// — a torn read would surface as a Render whose body doesn't match the
// snapshot's own fact count, or a panic. With the old `*soulPtr = *sl`
// in-place struct copy this raced; with the atomic pointer swap it can't.
func TestLive_ConcurrentReadDuringEdits(t *testing.T) {
	live := soul.NewLive(soul.NewDefault("main"))

	const (
		readers   = 16
		writers   = 4
		perReader = 4000
		perWriter = 2000
	)
	var wg sync.WaitGroup

	// Readers: Load a snapshot, then verify the snapshot is self-consistent.
	// The snapshot pointer is immutable, so its fact count and its rendered
	// body must agree no matter how many writers swap concurrently.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perReader; i++ {
				s := live.Load()
				if s == nil {
					t.Errorf("Load returned nil")
					return
				}
				n := len(s.User.Facts)
				body := s.Render() // reads the same slices — must not tear
				// Every fact in the snapshot must appear in its own render.
				for _, f := range s.User.Facts {
					if f != "" && !strings.Contains(body, f) {
						t.Errorf("torn read: fact %q (of %d) missing from its own render", f, n)
						return
					}
				}
			}
		}()
	}

	// Writers: each appends a unique fact via Edit (clone+mutate+store),
	// occasionally removing one, so the live soul churns under the readers.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				live.Edit(func(s *soul.Soul) {
					s.User.Facts = append(s.User.Facts, fmt.Sprintf("w%d-fact-%d", id, i))
					if len(s.User.Facts) > 32 {
						s.User.Facts = s.User.Facts[1:]
					}
				})
			}
		}(w)
	}

	wg.Wait()

	// Final state is whatever the last writer stored — just assert it's a
	// live, readable soul (the point of the test is the absence of a torn
	// read / data race during the run, asserted in the reader loop).
	if live.Load() == nil {
		t.Fatal("live soul nil after stress")
	}
}
