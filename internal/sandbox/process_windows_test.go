//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A server started in a Job Object serves over its pipes, and its memory is
// capped: a gigabyte allocation under a 128 MB cap does not succeed.
func TestJobObjectStartServesAndCaps(t *testing.T) {
	jb, err := For("jobobject")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(context.Background(), jb, helperArgv(), helperPolicy("echo"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(p.Stdin(), "in-job")
	if got := readLine(t, p); got != "echo:in-job" {
		t.Errorf("got %q", got)
	}
	_ = p.Close(5 * time.Second)

	pol := helperPolicy("alloc")
	pol.Limits.MemoryBytes = 128 << 20
	p, err = Start(context.Background(), jb, helperArgv(), pol)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(60 * time.Second):
		_ = p.Close(time.Second)
		t.Fatal("the allocating process neither finished nor died")
	}
	if p.Wait() == nil {
		t.Errorf("a 1 GiB allocation succeeded under a 128 MB job cap; stderr: %s", p.Stderr())
	}
}
