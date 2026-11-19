package fleet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/model"
)

// The link is one held WebSocket per child, dialed by the child, carrying
// JSON-RPC 2.0 on an mcp.Session in both directions. This file is the
// protocol: the methods, what they carry, and the limits. manager.go is the
// master's end and client.go the child's.
//
// Master to child:
//
//	fleet/hello     {"link":N}                         -> {"link":N,"os","user","version","sandbox","ceiling"}
//	ping            (no params)                        -> {}
//	fleet/update    {"version","sha256","size","offset","data"} -> {"received":N,"applying":bool}
//	tools/list      (no params)                        -> {"tools":[{"name","description","parameters"}]}
//	tools/call      {"call_id","name","arguments","timeout_ms"} -> {"text","is_error","clipped","audit"}
//	fleet/result    {"call_id"}                        -> {"state":"done|running|unknown","text","is_error","clipped","audit"}
//	fleet/addresses {"addresses":["host:port"]}        -> {}
//	fleet/memory/list  (no params)                     -> [{"key","gen","hash","ok"}]
//	fleet/memory/put   {"key","gen","hash","size","offset","data"} -> {"received":N}
//	fleet/memory/get   {"key","gen","hash","offset"}   -> {"size":N,"data"}
//	fleet/memory/purge {"key","gen","hash"}            -> {}
//	fleet/revoked   notification, no params
//	notifications/cancelled, sent by the session when a caller gives up
//
// Child to master:
//
//	fleet/confirm   {"call_id","action","detail"}      -> {"allow":bool}
//	ping            (no params)                        -> {}
//	notifications/cancelled
//
// The hello, ping and fleet/update never change shape, whatever LinkVersion
// becomes, so that a child of any age can still be told who it is talking to
// and be updated. tools/list, tools/call and fleet/result are refused, by
// both ends, between two partners whose link versions do not go together
// (speaks).
const (
	// LinkVersion is the version of the link protocol this build speaks.
	LinkVersion = 1
	// LinkPath is where the master serves the link.
	LinkPath = "/fleet/link"

	methodHello     = "fleet/hello"
	methodPing      = mcp.MethodPing
	methodUpdate    = "fleet/update"
	methodToolsList = "tools/list"
	methodToolsCall = "tools/call"
	methodResult    = "fleet/result"
	methodAddresses = "fleet/addresses"
	methodConfirm   = "fleet/confirm"
	noticeRevoked   = "fleet/revoked"
)

// JSON-RPC error codes of the link, beside the standard ones.
const (
	codeIncompatible = -32001 // the two link versions do not go together
	codeBusy         = -32002 // as many calls in flight as the machine takes
	codeNoCall       = -32003 // fleet/confirm for a call that is not in flight to this machine
	codeTooMany      = -32004 // fleet/confirm: one is already being decided, or the call has asked too often
	codeUnavailable  = -32005 // not possible here right now: no updater, child.json cannot be saved, fleet.json cannot be read
	codeDuplicate    = -32006 // tools/call with a call id this machine has seen
)

// What the master answers a connection it does not upgrade. The status is
// the answer: a child reads nothing else.
const (
	statusWaiting     = http.StatusTooEarly           // pending: keep trying
	statusPaused      = http.StatusLocked             // paused by the operator: keep trying
	statusRevoked     = http.StatusGone               // revoked: stop for good
	statusUnknown     = http.StatusForbidden          // not a machine of this fleet
	statusUnavailable = http.StatusServiceUnavailable // fleet.json cannot be read
)

// Limits. Every size and every wait on the link has one.
const (
	// MaxUpdateChunk is the most archive bytes one fleet/update carries: it
	// fits a frame once it is base64.
	MaxUpdateChunk = 2 << 20
	maxUpdateSize  = 1 << 30

	defaultInFlight = 8                // tools/call in flight per child, on both ends
	defaultCallTime = 15 * time.Minute // a call whose caller set no deadline: longer than any tool lets itself run
	maxCallTime     = 30 * time.Minute // the longest a child lets one call run
	callGrace       = time.Second      // how long after a call's deadline a child ends it by itself
	requestTime     = 30 * time.Second // every other request
	helloTime       = 10 * time.Second
	maxLinkVersion  = 1 << 20

	maxResultText = 1 << 20 // a tool result's text, before it is put in a frame
	frameReserve  = 1 << 10 // room left in a frame for the JSON-RPC envelope
	maxCallIDLen  = 64
	maxActionLen  = 64
	maxDetailLen  = 8 << 10
	maxConfirms   = 16 // fleet/confirm per call
	maxTools      = 2048
	maxToolDesc   = 8 << 10

	// What a child keeps of finished calls, for fleet/result.
	resultsKept     = 32
	resultBytesKept = 2 << 20
	resultKeep      = 10 * time.Minute

	// What the master takes from one child. Requests and cancellations are
	// paid for from a bucket; a result is paid for by the request it answers.
	// Anything else is a strike, and so is anything unpaid.
	requestBurst   = 256
	requestsPerSec = 32
	maxStrikes     = 16
	maxPreUpgrade  = 128 // connections that are not a link yet, at once
)

// Errors of the master's end. Test with errors.Is.
var (
	// ErrOffline: no link to the machine is open. Nothing is queued.
	ErrOffline = errors.New("fleet: the machine is offline")
	// ErrBusy: as many calls are in flight to the machine as it takes.
	ErrBusy = errors.New("fleet: too many calls in flight to the machine")
	// ErrOutcomeUnknown: the link dropped while a call was in flight. The call
	// may have run, may still be running, or may never have arrived; it is
	// not sent again. Ask with Manager.Result once the machine is back.
	ErrOutcomeUnknown = errors.New("fleet: the link dropped during the call: outcome unknown")
	// ErrIncompatible: the machine's link version and this one do not go
	// together. Only the hello, ping and Manager.Update pass.
	ErrIncompatible = errors.New("fleet: the link versions do not go together")
	// ErrCallRunning: Manager.Result for a call the machine is still running.
	ErrCallRunning = errors.New("fleet: the call is still running")
	// ErrCallUnknown: Manager.Result for a call the machine has no record of.
	ErrCallUnknown = errors.New("fleet: the machine has no record of the call")
)

// OfflineError is ErrOffline with when the machine was last connected.
type OfflineError struct {
	Machine string
	// Since is when its link ended. Zero: not seen since Started.
	Since time.Time
	// Started is when this master started holding the fleet.
	Started time.Time
}

func (e *OfflineError) Error() string {
	if e.Since.IsZero() {
		return fmt.Sprintf("fleet: machine %s is offline: not seen since this master started at %s", e.Machine, e.Started.Format(time.DateTime))
	}
	return fmt.Sprintf("fleet: machine %s is offline since %s", e.Machine, e.Since.Format(time.DateTime))
}

func (e *OfflineError) Is(target error) bool { return target == ErrOffline }

// CallError is a call that was given an id and did not come back with a
// result. Err says why: ErrOutcomeUnknown, the caller's context error, or
// the machine's refusal.
type CallError struct {
	Machine string
	CallID  string
	Err     error
}

func (e *CallError) Error() string {
	return fmt.Sprintf("fleet: call %s to machine %s: %v", e.CallID, e.Machine, e.Err)
}

func (e *CallError) Unwrap() error { return e.Err }

// Tools is what a child serves over the link: its tool mux.
type Tools interface {
	All() []model.ToolSpec
	Dispatch(ctx context.Context, call model.ToolCall) (result string, isError bool, err error)
}

// Call is one tool call the master makes on a child.
type Call struct {
	Tool      string
	Arguments json.RawMessage
	// Confirm decides each approval the child asks for while this call runs,
	// and only then: one at a time, and no more than sixteen. Its context is
	// the call's, with the caller's values, and ends when the call ends, the
	// link drops or the child stops asking; a yes given after that counts for
	// nothing, and Confirm may still be returning when Manager.Call already
	// has. A nil Confirm denies. action is a short identifier (os_exec,
	// web_transact); detail is the child's words: untrusted text, cut to
	// eight kilobytes and made valid UTF-8, and nothing more.
	Confirm func(ctx context.Context, action, detail string) bool
}

// Result is what a tool call on a child came back with. Text is untrusted.
type Result struct {
	CallID  string
	Text    string
	IsError bool // the tool reported a failure
	Clipped bool // Text was cut to fit
	// Audit is the SHA-256, in lowercase hex, of the record the child wrote in
	// its own audit log for this call (gate.go): the one of how the call
	// ended, or of its arrival when the end could not be written. Empty when
	// the child sent none or sent something that is not such a hash.
	Audit string
}

// Update is one piece of a new Tenant for a child (fleet/update). The
// archive is sent in order in pieces of at most MaxUpdateChunk; a piece with
// no Data asks how far the child has got. Its wire shape is final.
type Update struct {
	Version string `json:"version"` // what the archive installs
	SHA256  string `json:"sha256"`  // of the whole archive, lowercase hex
	Size    int64  `json:"size"`    // of the whole archive, in bytes
	Offset  int64  `json:"offset"`  // where Data belongs in it
	Data    []byte `json:"data"`
}

// UpdateAck answers an Update.
type UpdateAck struct {
	Received int64 `json:"received"` // how much of the archive the child holds: the next Offset
	Applying bool  `json:"applying"` // the archive is whole and checked, and the child is restarting into it
}

func (u Update) validate() error {
	if _, err := parseFingerprint(u.SHA256); err != nil || len(u.Version) == 0 || len(u.Version) > maxReportedLen || !printable(u.Version) ||
		u.Size < 1 || u.Size > maxUpdateSize || u.Offset < 0 || len(u.Data) > MaxUpdateChunk || u.Offset+int64(len(u.Data)) > u.Size {
		return errors.New("fleet: not a valid piece of an update")
	}
	return nil
}

// --- wire shapes ---

type helloParams struct {
	Link int `json:"link"`
}

type helloResult struct {
	Link int `json:"link"`
	Reported
}

type toolsResult struct {
	Tools []model.ToolSpec `json:"tools"`
}

type callParams struct {
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	TimeoutMS int64           `json:"timeout_ms"`
}

type callResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
	Clipped bool   `json:"clipped,omitempty"`
	Audit   string `json:"audit,omitempty"` // the hash of the child's audit record of the call
}

type resultParams struct {
	CallID string `json:"call_id"`
}

const (
	callDone    = "done"
	callRunning = "running"
	callUnknown = "unknown"
)

type resultResult struct {
	State string `json:"state"`
	callResult
}

type addressParams struct {
	Addresses []string `json:"addresses"`
}

type confirmParams struct {
	CallID string `json:"call_id"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

type confirmResult struct {
	Allow bool `json:"allow"`
}

var emptyObject = json.RawMessage(`{}`)

// encode is json.Marshal without the escaping of <, > and &, which would
// make shell output up to six times its size on the wire.
func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// speaks reports whether a side at link version mine serves a partner at
// peer. A partner one version behind is still spoken to, in its version; an
// older one is only said hello to, pinged and updated. A newer partner is
// left to judge for itself.
func speaks(mine, peer int) bool { return peer >= 1 && peer >= mine-1 }

func incompatible(side string, mine, peer int) string {
	if peer == 0 {
		return "fleet: no hello yet: " + side + " does not know which link version its partner speaks"
	}
	return fmt.Sprintf("fleet: %s speaks link version %d and its partner %d: only the hello, ping and fleet/update pass until the older side is updated", side, mine, peer)
}

func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand fills b or stops the program
	return hex.EncodeToString(b[:])
}

// validCallID accepts what newCallID makes and anything of that kind.
func validCallID(s string) bool {
	if len(s) == 0 || len(s) > maxCallIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// validAction accepts an approval category as the tools name them
// (os_exec_dangerous, web_transact): a short identifier, never free text.
func validAction(s string) bool {
	if len(s) == 0 || len(s) > maxActionLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func validToolName(s string) bool {
	return len(s) > 0 && len(s) <= maxToolLen && printable(s) && !strings.ContainsFunc(s, unicode.IsSpace)
}

// cut shortens s to at most n bytes of valid UTF-8.
func cut(s string, n int) (string, bool) {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// fit makes a tool result that goes into one frame of at most frameMax
// bytes, cutting its text until it does, and returns it with its encoding.
// A result that did not fit would be dropped by the session without a word
// and its caller left to wait out the deadline.
func fit(res callResult, frameMax int) (callResult, json.RawMessage) {
	var clipped bool
	if res.Text, clipped = cut(res.Text, maxResultText); clipped {
		res.Clipped = true
	}
	for {
		raw, err := encode(res)
		if err == nil && len(raw) <= frameMax-frameReserve {
			return res, raw
		}
		if res.Text == "" { // a frame too small for an empty result: nothing fits
			return res, emptyObject
		}
		res.Text, _ = cut(res.Text, len(res.Text)/2)
		res.Clipped = true
	}
}

func frameMax(cfg transport.WSConfig) int {
	if cfg.MaxFrameSize > 0 {
		return cfg.MaxFrameSize
	}
	return transport.DefaultMaxFrameSize
}

// withDeadline gives ctx a deadline of d from now unless it has one.
func withDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
