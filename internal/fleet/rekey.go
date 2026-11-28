package fleet

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"tenant/internal/cnsa"
	"tenant/internal/mcp"
	"tenant/internal/secrets"
)

// Moving a machine off its legacy identity.
//
// A machine enrolled before the cnsa profile has an ECDSA certificate that
// its master pins by SHA-256, and pins its master's the same way. Its master
// keeps the identity it had then (LoadMasterIdentity) and serves it to such
// machines only, under a handshake rule of its own (MasterTLS). Over a link
// made that way nothing is asked of the machine but a hello, a ping, an
// update, and this:
//
//	fleet/hello   master to child   {"link":N,"rekey":"<the master's new fingerprint>"}
//	fleet/rekey   child to master   {"certificate":"<DER, base64>"} -> {}
//
// The move, step by step:
//
//  1. In the hello of a legacy link the master says which certificate it has
//     now.
//  2. A child that knows the profile, and is not in the middle of an update
//     that could still put an older Tenant back, makes itself a new
//     identity, keeps it beside the legacy one, and sends its certificate.
//  3. The master pins that as the machine's next certificate, beside the
//     legacy one, and answers.
//  4. On the answer the child writes the master's new fingerprint into
//     child.json, puts the new identity in the legacy one's place, in that
//     order, and drops the link.
//  5. It dials again as any machine does. The master sees the next
//     certificate, makes it the machine's only one, and the legacy pin is
//     gone.
//
// Both ends trust the other's new certificate on the strength of the link it
// came over, which each authenticated with the certificate it has pinned.
// Either can stop at any step: until 4 the child is as it was, and at the
// next hello sends the same new certificate again; a child stopped inside 4
// finishes it when it starts (NewClient).
const (
	methodRekey = "fleet/rekey"
	// An ML-DSA-87 certificate is some 7.5 KiB.
	maxCertificate = 16 << 10
)

// ErrLegacy: the machine is still on the certificate it had before the cnsa
// profile. It is also an ErrIncompatible: only the hello, ping and
// Manager.Update pass.
var ErrLegacy = errors.New("fleet: the machine is still on its certificate from before the ML-DSA-87 profile: update its Tenant, and it moves to a new one by itself")

type rekeyParams struct {
	Certificate []byte `json:"certificate"`
}

// setNext pins der, the certificate a legacy machine said over its link it
// is moving to, as the machine's next one, and returns its fingerprint.
func (s *Store) setNext(id string, der []byte) (string, error) {
	leaf, err := x509.ParseCertificate(der)
	if err == nil {
		err = cnsa.CheckLeaf(leaf)
	}
	if err == nil && s.now().After(leaf.NotAfter) {
		err = errors.New("it has expired")
	}
	if err != nil {
		return "", fmt.Errorf("fleet: not a certificate a machine can move to: %w", err)
	}
	fp := fingerprintOf(der)
	return fp, s.change(id, func(d *fileData, m *Machine) error {
		if !m.Legacy() {
			return errors.New("fleet: the machine is not on a legacy certificate")
		}
		if m.Next == fp {
			return errUnchanged
		}
		for other, o := range d.Machines {
			if o.Fingerprint == fp || (other != id && o.Next == fp) {
				return errors.New("fleet: that certificate is another machine's") // one certificate, one machine
			}
		}
		m.Next = fp
		return nil
	})
}

// promote makes the next certificate of a machine that has just connected
// with it the machine's only one.
func (s *Store) promote(id, fp string) error {
	return s.change(id, func(d *fileData, m *Machine) error {
		if m.Next == "" || m.Next != fp {
			return errUnchanged
		}
		for other, o := range d.Machines {
			if other != id && (o.Fingerprint == fp || o.Next == fp) {
				return errors.New("fleet: that certificate is another machine's") // one certificate, one machine
			}
		}
		m.Fingerprint, m.Next = fp, ""
		return nil
	})
}

// rekey is the master's end of fleet/rekey.
func (l *link) rekey(params json.RawMessage) (json.RawMessage, error) {
	var p rekeyParams
	if err := json.Unmarshal(params, &p); err != nil || len(p.Certificate) == 0 || len(p.Certificate) > maxCertificate {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: fleet/rekey needs a certificate")
	}
	if !l.legacy {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: this machine is not on a legacy certificate")
	}
	fp, err := l.m.store.setNext(l.id, p.Certificate)
	if err != nil {
		RekeyRefused.Log(l.m.ctx, "name", l.m.label(l.id), "machine", l.id, "err", err)
		return nil, mcp.NewError(codeUnavailable, "fleet: the master did not take the certificate")
	}
	Rekeying.Log(l.m.ctx, "name", l.m.label(l.id), "machine", l.id, "certificate", fp)
	return emptyObject, nil
}

// rekey is the child's end: steps 2 to 4. masterFingerprint is what the
// master said in its hello on l, a legacy link.
func (c *Client) rekey(l *childLink, masterFingerprint string) {
	defer c.calls.Done()
	fail := func(step string, err error) {
		c.log.Warn("fleet: this machine could not move to a new certificate; it stays on the one it has and tries again when it next connects", "step", step, "err", err)
	}
	if _, err := parseFingerprint(masterFingerprint); err != nil {
		fail("the master's new fingerprint", err)
		return
	}
	next, err := nextIdentity(c.cfg.Dir)
	if err != nil {
		fail("make the new identity", err)
		return
	}
	params, _ := encode(rekeyParams{Certificate: next.Certificate.Certificate[0]}) // bytes: cannot fail
	ctx, cancel := context.WithTimeout(c.runCtx, requestTime)
	_, err = l.sess.Call(ctx, methodRekey, params)
	cancel()
	if err != nil {
		fail("send the new certificate", err)
		return
	}
	// The master pins the new certificate from here on, beside the old one.
	// Written down before it is done, as every call is: which master a
	// machine trusts does not change without a record on the machine.
	if c.cfg.Rekeyed != nil {
		was, err := c.file.Current()
		if err == nil {
			err = c.cfg.Rekeyed(was.MasterFingerprint, masterFingerprint)
		}
		if err != nil {
			fail("record the move in the audit log", err)
			return
		}
	}
	err = UpdateChild(ChildPath(c.cfg.Dir), func(child *Child) error {
		child.MasterFingerprint = masterFingerprint
		return nil
	})
	if err != nil {
		fail("save the master's new fingerprint", err)
		return
	}
	id, err := finishRekey(c.cfg.Dir)
	if err != nil {
		fail("put the new identity in place", err) // NewClient finishes it
		return
	}
	c.mu.Lock()
	c.id = id
	c.mu.Unlock()
	c.log.Info("fleet: this machine moved to an ML-DSA-87 certificate; connecting again with it", "certificate", id.Fingerprint)
	_ = l.sess.Close()
}

// nextIdentity is the identity a child is moving to: the one it made at an
// earlier try, or a new one.
func nextIdentity(dir string) (Identity, error) {
	path := ChildNextIdentityPath(dir)
	if id, err := LoadIdentity(path); err == nil && !id.Legacy {
		return id, nil
	}
	// None yet, or one that cannot be used: the master has pinned nothing
	// this machine has moved to, so a new one loses nothing.
	return MintIdentity(path)
}

// finishRekey is the second half of step 4: the identity a child is moving
// to goes in the place of its legacy one. It is only called once child.json
// pins the master's new certificate.
func finishRekey(dir string) (Identity, error) {
	path := ChildNextIdentityPath(dir)
	b, err := secrets.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("fleet: read the new identity: %w", err)
	}
	id, err := parseIdentity(b)
	if err == nil && id.Legacy {
		err = errors.New("fleet: the new identity is a legacy one")
	}
	if err != nil {
		return Identity{}, err
	}
	if err := writePrivate(ChildIdentityPath(dir), b); err != nil {
		return Identity{}, fmt.Errorf("fleet: save identity: %w", err)
	}
	_ = os.Remove(path)
	return id, nil
}

// loadChildIdentity is LoadIdentity for a child, finishing a move that
// stopped between its two writes: child.json already pins the master's new
// certificate and the identity is still the legacy one.
func loadChildIdentity(dir string, child Child) (Identity, error) {
	id, err := LoadIdentity(ChildIdentityPath(dir))
	if err != nil {
		return Identity{}, err
	}
	if id.Legacy && len(child.MasterFingerprint) == cnsa.FingerprintLen {
		return finishRekey(dir)
	}
	return id, nil
}
