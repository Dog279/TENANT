package main

// doctorfleet.go (TEN-533; docs/plans/gap6-fleet-plan.md, section 7): on a
// fleet master, one doctor check per machine. Doctor is a process of its own
// and cannot see which machines are online: only the process that holds the
// fleet can. It asks the running Tenant's dashboard (GET /api/fleet), as
// `tenant fleet update` does, and says so when there is none to ask.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/selfupdate"
	"tenant/internal/version"
)

// apiFleetMachine is one machine of GET /api/fleet, as doctor reads it.
type apiFleetMachine struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Online      bool   `json:"online"`
	Link        string `json:"link"`
	LinkVersion int    `json:"link_version"`
	Reported    struct {
		Version string `json:"version"` // the machine's words, in the quotes the page shows them in
	} `json:"reported_by_the_machine"`
	Memory       string     `json:"memory"`
	MemoryDetail string     `json:"memory_detail"`
	Update       *hubUpdate `json:"update"`
}

type apiFleet struct {
	HeldHere bool              `json:"held_here"`
	Holder   string            `json:"holder"`
	Machines []apiFleetMachine `json:"machines"`
}

// doctorFleetView is GET /api/fleet, fetched once for all the machines' checks.
type doctorFleetView struct {
	once sync.Once
	view *apiFleet
	why  string // why there is none
}

func (d *doctorFleetView) get(ctx context.Context, e *doctorEnv) (*apiFleet, string) {
	d.once.Do(func() {
		base := attachBaseURL("", e.lc)
		tok, _ := dashboardClientToken(e.c.cfgDir, e.lc, e.c.auditOnly)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/fleet", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		httpc := e.httpc
		if httpc == nil {
			httpc = http.DefaultClient
		}
		resp, err := httpc.Do(req)
		if err != nil {
			d.why = "no running Tenant answers at " + base + ", and only the one that holds the fleet knows which machines are online"
			return
		}
		defer resp.Body.Close()
		var v apiFleet
		if resp.StatusCode != http.StatusOK {
			d.why = fmt.Sprintf("the running Tenant at %s answered %s for its fleet", base, resp.Status)
			return
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&v); err != nil {
			d.why = "the running Tenant's answer about its fleet cannot be read: " + err.Error()
			return
		}
		if !v.HeldHere {
			d.why = "the running Tenant at " + base + " does not hold the fleet (" + firstNonEmpty(v.Holder, "nothing does") + "), so it cannot tell which machines are online"
			return
		}
		d.view = &v
	})
	return d.view, d.why
}

// fleetDoctorChecks is one check per machine of this master's fleet that is
// not revoked; none on a Tenant that is not a fleet master.
func fleetDoctorChecks(cfgDir string) []doctorCheck {
	if _, err := os.Lstat(fleet.MasterIdentityPath(cfgDir)); err != nil {
		return nil
	}
	machines, err := fleet.Open(cfgDir).Machines()
	if err != nil {
		return []doctorCheck{{"fleet", func(context.Context, *doctorEnv) checkResult {
			return checkResult{Status: statusFail, Detail: err.Error() + ": every call to the fleet is refused until it can be read"}
		}}}
	}
	view := &doctorFleetView{}
	var out []doctorCheck
	for _, m := range machines {
		if m.State == fleet.StateRevoked {
			continue
		}
		out = append(out, doctorCheck{"fleet machine " + fleetName(m.Name), func(ctx context.Context, e *doctorEnv) checkResult {
			return checkFleetMachine(ctx, e, m, view)
		}})
	}
	return out
}

// checkFleetMachine is one machine: reachable; its version, against this
// master's; its link version, against the ones this master serves; how its
// last update went; and its memory.
func checkFleetMachine(ctx context.Context, e *doctorEnv, m fleet.Machine, view *doctorFleetView) checkResult {
	switch m.State {
	case fleet.StatePending:
		return checkResult{Status: statusWarn, Detail: "enrolled " + fleetTime(m.EnrolledAt) + " and waiting for confirmation: nothing is sent to it until then",
			Fix: "tenant fleet confirm " + m.Name + " (or tenant fleet revoke " + m.Name + " if you do not know it)"}
	case fleet.StatePaused:
		return checkResult{Status: statusSkip, Detail: "paused by the operator"}
	}
	v, why := view.get(ctx, e)
	if v == nil {
		return checkResult{Status: statusSkip, Detail: "reachable: not known (" + why + ")"}
	}
	var am *apiFleetMachine
	for i := range v.Machines {
		if v.Machines[i].ID == m.ID {
			am = &v.Machines[i]
		}
	}
	if am == nil {
		return checkResult{Status: statusSkip, Detail: "the running Tenant does not list it yet"}
	}
	if !am.Online {
		return checkResult{Status: statusWarn, Detail: "not reachable: " + firstNonEmpty(am.Link, "offline"),
			Fix: "on the machine: tenant child status"}
	}
	r := checkResult{Status: statusOK}
	worse := func(s checkStatus, fix string) {
		if s > r.Status {
			r.Status = s
		}
		if fix != "" && r.Fix == "" {
			r.Fix = fix
		}
	}
	reported, err := strconv.Unquote(am.Reported.Version)
	if err != nil {
		reported = am.Reported.Version
	}
	notes := []string{"online (" + am.Link + ")"}
	switch c, ok := selfupdate.Compare(reported, version.Version); {
	case !ok:
		notes = append(notes, fmt.Sprintf("runs %s, which is not a version this master can compare with its own %s", fleetQuote(reported), version.Version))
		worse(statusWarn, "")
	case c < 0:
		notes = append(notes, fmt.Sprintf("runs %s, behind this master's %s", reported, version.Version))
		worse(statusWarn, "tenant fleet update "+m.Name)
	case c > 0:
		notes = append(notes, fmt.Sprintf("runs %s, newer than this master's %s", reported, version.Version))
	default:
		notes = append(notes, "runs "+reported+", as this master does")
	}
	if !fleet.Served(am.LinkVersion) {
		notes = append(notes, fmt.Sprintf("speaks link version %d, which this master (link version %d) no longer serves: only the hello, ping and an update pass", am.LinkVersion, fleet.LinkVersion))
		worse(statusFail, "tenant fleet update "+m.Name)
	}
	if u := am.Update; u != nil && !u.Running && u.Stage != fleetUpdDone && len(u.Lines) > 0 {
		notes = append(notes, "its last update did not go through: "+u.Lines[len(u.Lines)-1])
		worse(statusWarn, "")
	}
	// Memory in step: the API's memory field is the holder's last comparison
	// with the machine (memoryStateIn). "Recovery key proven" is the fleet's,
	// not a machine's: one recovery key seals every machine's copies, and
	// checkFleetKeys reports the proof `tenant fleet init` recorded.
	detail := strings.TrimSpace(am.MemoryDetail)
	switch am.Memory {
	case memoryInStep:
		notes = append(notes, "memory in step")
	case memoryBehind:
		notes = append(notes, "memory behind: "+detail)
		worse(statusWarn, "")
	case memoryNeedsKey:
		notes = append(notes, "memory needs the recovery key: "+detail)
		worse(statusFail, "tenant fleet recover "+m.Name+" (with the recovery identity of the fleet that made its copies)")
	default:
		notes = append(notes, "memory not known: "+firstNonEmpty(detail, "the running Tenant does not say"))
		worse(statusWarn, "")
	}
	r.Detail = strings.Join(notes, "; ")
	return r
}
