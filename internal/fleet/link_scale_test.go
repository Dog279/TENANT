package fleet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/mcp/transport"
	"tenant/internal/model"
)

// What an idle link costs. The design is: on the master one goroutine per
// child, blocked on a read; on the child that one and the goroutine that
// called Run and waits for the link to end; no timer on either; and per ping
// interval one ping from the child and one pong from the master. These tests
// hold the code to that with a hundred children, and print the numbers.

const scaleChildren = 100

// stacks counts the goroutines whose stack has each of the given functions.
func stacks(fns ...string) []int {
	buf := make([]byte, 8<<20)
	buf = buf[:runtime.Stack(buf, true)]
	out := make([]int, len(fns))
	for _, g := range bytes.Split(buf, []byte("\n\n")) {
		for i, fn := range fns {
			if bytes.Contains(g, []byte(fn)) {
				out[i]++
			}
		}
	}
	return out
}

// settled waits until the number of goroutines has stopped changing.
func settled() int {
	last, same := runtime.NumGoroutine(), 0
	for deadline := time.Now().Add(waitLong); same < 20 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if n := runtime.NumGoroutine(); n == last {
			same++
		} else {
			last, same = n, 0
		}
	}
	return last
}

// goroutines waits until this process has exactly want goroutines, and
// returns how many it has: want, unless the wait ran out. A count that has
// merely stopped changing for a moment proves nothing on a busy machine,
// where a handler that is about to finish can sit still for longer than
// that; the count the design names is either reached or it is not.
func goroutines(want int) int {
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(waitLong); n != want && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(10 * time.Millisecond)
	}
	return n
}

// heapNow is the live heap and the goroutine stacks, in bytes.
func heapNow() int {
	var m runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	return int(m.HeapAlloc + m.StackInuse)
}

// One manager, a hundred children in this process, idle; then one call to
// each. An idle link is three goroutines, one on the master and two on the
// child, and they are the ones the design names.
func TestLinkScaleSmoke(t *testing.T) {
	const interval = 400 * time.Millisecond
	r := newRig(t, func(m *Manager) { m.ws = transport.WSConfig{PingInterval: interval} })
	kids := make([]*kid, scaleChildren)
	for i := range kids {
		kids[i] = r.kid(t, fmt.Sprintf("box-%03d", i), StateConfirmed)
		kids[i].c.ws = transport.WSConfig{PingInterval: interval}
	}
	before, heapBefore := settled(), heapNow()
	for _, k := range kids {
		k.start()
	}
	for _, k := range kids {
		k.await(LinkConnected)
		r.awaitOnline(t, k.mach.ID, true)
	}
	// Every link is up; what is left over is each link's handler finishing
	// its hello and its address list. When the count is down to three a link,
	// those are done and the links are idle.
	idle := goroutines(before + 3*scaleChildren)
	heapIdle := heapNow()
	count := stacks("fleet.(*guard).Recv", "fleet.noticing.Recv", "fleet.(*Client).Run")
	t.Logf("%d children idle: %d goroutines more than before (%.2f per link): %d master read loops, %d child read loops, %d child Run callers",
		scaleChildren, idle-before, float64(idle-before)/scaleChildren, count[0], count[1], count[2])
	t.Logf("heap and stacks, both ends together: %d bytes per link", (heapIdle-heapBefore)/scaleChildren)
	if idle-before != 3*scaleChildren || count[0] != scaleChildren || count[1] != scaleChildren || count[2] != scaleChildren {
		t.Errorf("an idle link is meant to be 3 goroutines: the master's read loop, the child's, and the caller of Run")
	}

	// Idle traffic. All an idle link carries is the transport's own ping and
	// the pong that answers it, each an empty control frame in a TLS record:
	// 28 bytes from the child, whose frames are masked, and 24 from the
	// master. The link adds nothing of its own. That holds however busy the
	// machine is, and is what is asserted: every byte is such a record, every
	// ping has its pong, and no end sends more than one of each per interval.
	// Who pings is the transport's rule and depends on timing: the child,
	// unless its ping is a quarter of an interval late, and then the master
	// too. On a quiet machine that is one ping and one pong per interval,
	// 104 bytes a minute at the default interval; what it was here is logged.
	const intervals = 6
	sentMaster, sentKids := r.masterSent.Load(), r.kidsSent.Load()
	time.Sleep(intervals * interval)
	byMaster, byKids := r.masterSent.Load()-sentMaster, r.kidsSent.Load()-sentKids
	perMaster, perKid := float64(byMaster)/intervals/scaleChildren, float64(byKids)/intervals/scaleChildren
	t.Logf("idle traffic per link and ping interval, as TLS records on the wire: child %.1f bytes, master %.1f bytes; at the default interval of %v that is %.0f bytes a minute",
		perKid, perMaster, transport.DefaultWSPingInterval, (perKid+perMaster)*float64(time.Minute/transport.DefaultWSPingInterval))
	if byKids == 0 || byMaster == 0 || byKids%28 != 0 || byMaster%24 != 0 {
		t.Errorf("an idle link carried something other than pings and pongs: %d bytes from the children, %d from the master", byKids, byMaster)
	}
	fromKids, fromMaster := byKids/28, byMaster/24
	// A ping sent just inside the window has its pong just outside it, at
	// either edge: one frame a link.
	if d := fromKids - fromMaster; d > scaleChildren || d < -scaleChildren {
		t.Errorf("%d frames from the children and %d from the master: a ping went unanswered", fromKids, fromMaster)
	}
	if most := int64(2 * (intervals + 1) * scaleChildren); fromKids > most || fromMaster > most {
		t.Errorf("%d frames from the children and %d from the master in %d intervals: more than a ping and a pong per end and interval", fromKids, fromMaster, intervals)
	}

	// One call to each, all at once.
	var wg sync.WaitGroup
	for i, k := range kids {
		wg.Go(func() {
			arg := strconv.Itoa(i)
			if res, err := r.call(t, k.mach.ID, "echo", arg); err != nil || res.Text != arg {
				t.Errorf("call to %s = %+v, %v", k.mach.Name, res, err)
			}
		})
	}
	wg.Wait()
	if after := goroutines(idle); after != idle {
		t.Errorf("%d goroutines when idle, %d after a call to each", idle, after)
	}
}

// The same hundred children in a process of their own, so that each end's
// cost can be told from the other's, at the real ping interval.
func TestLinkScaleTwoProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a second process with a hundred children")
	}
	r := newRig(t, func(m *Manager) { m.ws = transport.WSConfig{} })
	base := t.TempDir()
	ids := make([]string, scaleChildren)
	for i := range ids {
		dir := filepath.Join(base, fmt.Sprintf("c%03d", i))
		id := must(MintIdentity(ChildIdentityPath(dir)))(t)
		mach := r.addIdentity(t, fmt.Sprintf("box-%03d", i), StateConfirmed, id)
		ids[i] = mach.ID
		// What the machine reports is what its record says already, so that a
		// hundred hellos are not a hundred writes of fleet.json.
		if err := r.store.SetReported(mach.ID, Reported{OS: runtime.GOOS, Sandbox: "host", Ceiling: "full"}); err != nil {
			t.Fatal(err)
		}
		if err := SaveChild(ChildPath(dir), Child{ID: mach.ID, Name: mach.Name, MasterAddresses: []string{r.addr},
			MasterFingerprint: r.id.Fingerprint, Sandbox: "host", Ceiling: CeilingFull}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+helperChildren, helperDir+"="+base, helperCount+"="+strconv.Itoa(scaleChildren))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	noWindow(cmd)
	stdin := must(cmd.StdinPipe())(t)
	stdout := must(cmd.StdoutPipe())(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	t.Cleanup(func() {
		_ = stdin.Close() // the helper ends when its stdin does
		select {
		case <-exited:
		case <-time.After(waitLong):
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	// The helper says one line when all its children are connected and idle.
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- text
		_, _ = io.Copy(io.Discard, stdout)
		exited <- cmd.Wait()
	}()
	// It starts them when it is told to, so that what this end has before
	// them can be measured first.
	before, heapBefore := settled(), heapNow()
	if _, err := io.WriteString(stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
	var child struct{ Goroutines, Bytes int }
	select {
	case text := <-line:
		if err := json.Unmarshal([]byte(text), &child); err != nil {
			t.Fatalf("the helper said %q (%v); stderr: %s", text, err, stderr.String())
		}
	case <-ctx.Done():
		t.Fatalf("the helper never reported; stderr: %s", stderr.String())
	}
	for _, id := range ids {
		r.awaitOnline(t, id, true)
	}
	idle := goroutines(before + scaleChildren)
	heapIdle := heapNow()
	count := stacks("fleet.(*guard).Recv")
	masterBytes := (heapIdle - heapBefore) / scaleChildren
	t.Logf("master, per idle child: %.2f goroutines (%d read loops for %d children), %d bytes of heap and stacks",
		float64(idle-before)/scaleChildren, count[0], scaleChildren, masterBytes)
	t.Logf("child, per idle link: %.2f goroutines, %d bytes of heap and stacks",
		float64(child.Goroutines)/scaleChildren, child.Bytes/scaleChildren)
	if count[0] != scaleChildren || idle-before != scaleChildren {
		t.Errorf("the master is meant to hold one goroutine per idle child")
	}
	if child.Goroutines != 2*scaleChildren {
		t.Errorf("a child is meant to hold two goroutines for an idle link: its read loop and the caller of Run")
	}
	// A first budget, with room: what is measured is in the log above.
	if masterBytes > 256<<10 || child.Bytes/scaleChildren > 256<<10 {
		t.Errorf("an idle link costs more than 256 KiB on one end")
	}

	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			arg := strconv.Itoa(i)
			if res, err := r.call(t, id, "echo", arg); err != nil || res.Text != arg {
				t.Errorf("call to child %d = %+v, %v", i, res, err)
			}
		})
	}
	wg.Wait()
}

const helperChildren = "children"

// echoTools is the mux of a helper's children.
type echoTools struct{}

func (echoTools) All() []model.ToolSpec { return []model.ToolSpec{{Name: "echo"}} }
func (echoTools) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	return string(call.Arguments), false, nil
}

// childrenProcess is a process holding the child's end of N links: the
// machines whose files are in the directories c000, c001, ... of helperDir.
// When all are connected and idle it prints what they cost it, as one line of
// JSON, and then holds the links until its stdin closes.
func childrenProcess() error {
	time.AfterFunc(2*time.Minute, func() { os.Exit(4) })
	// The test says "go", and holds the other end of stdin for as long as the
	// children are to live.
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(io.Discard, in)
		os.Exit(0)
	}()

	n, _ := strconv.Atoi(os.Getenv(helperCount))
	clients := make([]*Client, n)
	for i := range clients {
		c, err := NewClient(ClientConfig{Dir: filepath.Join(os.Getenv(helperDir), fmt.Sprintf("c%03d", i)), Tools: echoTools{}, Log: quiet})
		if err != nil {
			return err
		}
		clients[i] = c
	}
	before, heapBefore := settled(), heapNow()
	for _, c := range clients {
		go func() { _ = c.Run(context.Background()) }()
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		connected := 0
		var why []string
		for _, c := range clients {
			if st := c.Status(); st.State == LinkConnected {
				connected++
			} else if len(why) < 3 {
				why = append(why, string(st.State)+": "+st.Detail)
			}
		}
		if connected == n {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d children connected: %s", connected, n, strings.Join(why, "; "))
		}
	}
	idle := goroutines(before + 2*n) // two a link, once each has answered its master's hello
	out, _ := json.Marshal(map[string]int{"Goroutines": idle - before, "Bytes": heapNow() - heapBefore})
	fmt.Println(string(out))
	select {}
}
