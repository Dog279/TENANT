package lifecycle

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// What is running now (logging v2 §8.7): the component registry the
// Components view reads. A component is tracked where it starts: Group.Start
// for what the group also stops, Track for what it doesn't (the iMessage
// responder, the watchers, the peer heartbeat, the turn gate), and
// internal/logs/capture for each child process while it runs. Every state
// change is a system event under the run's activity, so "when did Discord
// last reconnect, and why?" is one query: activity:<id>.
//
// The registry lives here, beside the stops, because lifecycle already hears
// of every subsystem a long-running command starts; it imports only the
// catalog and the event model, so capture can import it too.

// Kind is what sort of component it is.
type Kind string

const (
	Subsystem Kind = "subsystem" // a part of Tenant: cron, the turn gate, a watcher
	Connector Kind = "connector" // a link to a service outside: Discord, iMessage
	Server    Kind = "server"    // a listener: the dashboard, the peer listener
	Child     Kind = "child"     // a child process, while it runs
	Job       Kind = "job"       // a unit of scheduled work
)

// State is where a component stands.
type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Degraded State = "degraded" // running, but not as it should: Error says why
	Stopped  State = "stopped"
	Failed   State = "failed"
)

// Component is a component as the registry lists it.
type Component struct {
	Name     string    `json:"name"`
	Kind     Kind      `json:"kind"`
	Log      string    `json:"log"` // the log it writes, which the view links to
	State    State     `json:"state"`
	Since    time.Time `json:"since"`              // when it entered State
	Started  time.Time `json:"started,omitzero"`   // when this run began: its uptime; zero once it stopped
	Error    string    `json:"error,omitempty"`    // the last error it failed or degraded with
	Activity string    `json:"activity,omitempty"` // this run's activity: activity:<id> is its history
}

// Comp is a tracked component. Its methods are safe for concurrent use.
type Comp struct {
	key  string
	base context.Context // what a new run's activity is related to
	ctx  context.Context // this run's activity, which its events carry
	c    Component
}

var reg = struct {
	sync.Mutex
	m   map[string]*Comp
	seq uint64
}{m: map[string]*Comp{}}

// Track tracks component name in state st, with err as the reason it failed
// or degraded, and returns it. A name tracked before is the same component;
// one that starts again after it stopped or failed begins a new run, a new
// activity, related to ctx's. A child is always a new component, and it is
// dropped once it stops or fails.
func Track(ctx context.Context, name string, kind Kind, log string, st State, err error) *Comp {
	if ctx == nil {
		ctx = context.Background()
	}
	reg.Lock()
	key := name
	if kind == Child {
		reg.seq++
		key += "#" + strconv.FormatUint(reg.seq, 10)
	}
	c := reg.m[key]
	if c == nil {
		c = &Comp{key: key}
		reg.m[key] = c
	}
	c.c.Name, c.c.Kind, c.c.Log = name, kind, log
	if !c.live() {
		c.base, c.ctx = ctx, ctx
	}
	reg.Unlock()
	c.Set(st, err)
	return c
}

// live reports whether a run is under way. The caller holds reg.
func (c *Comp) live() bool {
	return c.c.State == Starting || c.c.State == Running || c.c.State == Degraded
}

// Set moves the component to st, with err as the reason it failed or
// degraded, and files the change. Setting the state it is in, with no new
// error, does nothing. The event is filed under the lock, so a component's
// events are in the order of its changes; filing never blocks.
func (c *Comp) Set(st State, err error) {
	var msg string
	if err != nil {
		msg = err.Error()
	}
	reg.Lock()
	defer reg.Unlock()
	if st == c.c.State && (err == nil || msg == c.c.Error) {
		return
	}
	now := time.Now().UTC()
	begin := !c.live() && (st == Starting || st == Running || st == Degraded)
	var uptime time.Duration
	if !c.c.Started.IsZero() {
		uptime = now.Sub(c.c.Started).Round(time.Second)
	}
	c.c.State, c.c.Since = st, now
	if err != nil {
		c.c.Error = msg
	}

	kv := []any{"component", c.c.Name, "kind", string(c.c.Kind), "log", c.c.Log}
	var d *event.Def
	switch st {
	case Starting:
		d = catalog.CompStarting
	case Running:
		d = catalog.CompRunning
	case Degraded:
		d, kv = catalog.CompDegraded, append(kv, "err", msg)
	case Stopped:
		d, kv = catalog.CompStopped, append(kv, "uptime", uptime)
	default:
		d, kv = catalog.CompFailed, append(kv, "err", msg, "uptime", uptime)
		if c.c.Kind == Child {
			d = catalog.ChildFailed // often a command's answer, not a fault: a warning, as capture files its exit
		}
	}
	switch {
	case begin:
		c.c.Started = now
		c.ctx, _ = event.Start(c.base, d, kv...)
		c.c.Activity = event.ActivityFromContext(c.ctx)
	case st == Stopped || st == Failed:
		c.c.Started = time.Time{}
		d.Log(c.ctx, kv...)
		if c.c.Kind == Child {
			delete(reg.m, c.key)
		}
	default:
		d.Log(c.ctx, kv...)
	}
}

// Start is Add for a component that is running now: it is tracked, so the
// Components view lists it, and its stop marks it stopped, or failed with
// the stop's error. It returns the component, to report a failed start or a
// degraded state.
func (g *Group) Start(stage Stage, name string, kind Kind, log string, stop func() error) *Comp {
	c := Track(context.Background(), name, kind, log, Running, nil)
	g.Add(stage, name, func() error {
		err := stop()
		if err != nil {
			c.Set(Failed, err)
		} else {
			c.Set(Stopped, nil)
		}
		return err
	})
	return c
}

// StartFunc is Start for a stop that cannot fail.
func (g *Group) StartFunc(stage Stage, name string, kind Kind, log string, stop func()) *Comp {
	return g.Start(stage, name, kind, log, func() error { stop(); return nil })
}

// Components lists what is tracked, by kind and then name: the Components
// view, and GET /api/logbook/components.
func Components() []Component {
	reg.Lock()
	out := make([]Component, 0, len(reg.m))
	for _, c := range reg.m {
		out = append(out, c.c)
	}
	reg.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind > b.Kind // subsystems, servers, jobs, connectors, then children
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Started.Before(b.Started)
	})
	return out
}
