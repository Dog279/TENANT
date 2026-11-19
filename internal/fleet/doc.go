// Package fleet is the identity, enrollment and state of a Tenant fleet
// (TEN-531), and the link between its machines: one master that owns child
// machines. It is a library; the `tenant fleet` / `tenant child` commands,
// the child's tools and the master's fleet tools live elsewhere.
//
// # Identity and TLS
//
// Every machine has one key pair and one self-signed certificate
// (MintIdentity, LoadIdentity), valid for server and client use for ten
// years. Nothing is signed by an authority: each side pins the other's
// certificate by its SHA-256 fingerprint. MasterTLS is the master's listener
// configuration and ChildTLS the child's; both demand TLS 1.3 and a
// certificate from the other side, and neither can be built without one, so
// there is no plain-HTTP fleet connection to construct.
//
// On the master a connection is checked against fleet.json during the
// handshake. The handler then asks Store.Identify for the machine behind the
// connection: the answer comes from the certificate the TLS stack verified,
// never from anything the machine sent.
//
// # fleet.json
//
// Store is the master's record of its machines, keyed by an id the master
// assigns and never changes. A machine's pinned certificate is its only
// credential; there is no token. Every change takes a lock shared by all
// Tenant processes and re-reads the file before it writes, so the command
// line and the running master never lose each other's changes. While the file
// cannot be read every lookup fails: a fleet does not carry on with what it
// had in memory.
//
// # Enrollment
//
// CreateEnrollment prints a one-time code; Enroll, on the new machine, spends
// it at the master's enroll endpoint (NewEnrollHandler), the only place a
// certificate that is not pinned yet is accepted. The machine is then pending
// until the operator types the six digits both sides derive from the two
// fingerprints (ConfirmCode, Store.Confirm).
//
// # child.json
//
// Child is a child's own configuration: where its master is, which
// certificate the master must present, and the ceiling the machine sets on
// what it will do. ChildFile re-reads it when it changes on disk.
//
// # The link
//
// A child dials its master and holds one WebSocket open (link.go has the
// protocol). Manager is the master's end: it serves the enroll endpoint and
// the link endpoint on the one fleet port, holds a session per connected
// child, and makes the calls: Tools, Call, Result, Update, Ping. Client is
// the child's end: it serves a tool mux to the master, keeps a call running
// through a dropped link, and dials again. Approvals are decided on the
// master: a tool on the child calls Confirm, which asks over the link, and
// the answer comes from the Confirm of the Call that is running.
//
// # On the child
//
// What a child lets its master do is decided on the child. Gate stands
// between the link and the child's tools: it reads the machine's ceiling and
// its list of served tools from child.json as it is on the disk at each
// check, and the pause mark beside it, and refuses what they rule out
// without asking the master. It writes every call to the child's audit log
// (AuditLog) before the call runs and again when it has ended; a call that
// cannot be recorded is not run, and each result carries the hash of its
// record. The log is a chain of hashes, checked by VerifyAudit.
//
// One process holds a fleet's links at a time; a second is told which
// (HeldError). A call to a machine that is offline, paused, pending or
// revoked fails at once; nothing is queued and nothing is sent twice. What a
// child sends is untrusted, down to the frame: see guard in manager.go.
package fleet
