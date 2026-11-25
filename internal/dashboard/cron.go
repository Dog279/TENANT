package dashboard

// cron.go is the server-rendered Cron admin section: list / add / enable /
// disable / run / delete recurring jobs. It mirrors the Memory curator's
// shape — a CronControl interface (satisfied in cmd/tenant), unconditional
// route mounting with nil-guarded handlers, and form mutations answered by
// cronDone: in place when Datastar posts them (the page keeps its scroll and
// a toast says what happened), else a 303 so a refresh can't double-submit.
// All job fields render through html/template's auto-escaping (no
// template.HTML), so a hostile job name/prompt cannot inject markup.
//
// Every route here is registered on the server mux, which Run() wraps in
// secure() (auth + same-origin + fail-closed bind policy) — so the whole Cron
// surface rides the dashboard's existing auth. Mutations are POST only.

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CronControl is the runtime cron surface the dashboard drives. cmd/tenant
// adapts its cron manager to it (see dashCron there). A nil control renders an
// "isn't configured" state rather than 404ing the nav link.
type CronControl interface {
	Jobs() []CronJobView
	Add(spec CronAddSpec) error
	Remove(id string) error
	SetEnabled(id string, on bool) error
	RunNow(id string) error
	// ExecEnabled reports the GLOBAL cron-exec kill-switch. Distinct from a job's
	// own Exec opt-in: a job marked exec still runs read/comms-safe while this is
	// off, which silently strips gated tools (e.g. discord_send_message) from it.
	ExecEnabled() bool
	// SetExec flips that kill-switch and persists it. Takes effect live.
	SetExec(on bool) error
}

// CronAddSpec is the input to Add. Kind is "" (prompt) / "prompt" / "shell";
// Exec opts a prompt job into the dangerous tool surface; TZ is optional.
type CronAddSpec struct {
	Name   string
	Spec   string
	Prompt string
	Kind   string
	Exec   bool
	TZ     string
}

// CronJobView is one job's render-ready state. Time fields are pre-formatted
// strings ("" when unset).
type CronJobView struct {
	ID         string
	Name       string
	Spec       string
	Prompt     string
	Enabled    bool
	Kind       string
	Exec       bool
	TZ         string
	NextRun    string
	LastRun    string
	LastStatus string
	LastError  string // the last run's error, plain, when it failed (TEN-427)
}

// SetCron installs the cron control after construction (mirrors the optional
// MemoryControl). Safe to call before the server starts serving; handlers
// nil-guard until it's set.
func (s *Server) SetCron(c CronControl) { s.cron = c }

// mountCronSSR registers the Cron page + its form actions. Mounted
// unconditionally; handlers nil-guard on s.cron. A job's own actions take its
// id in a form field, job, so each form posts to one fixed URL (F1). Not
// "id": an input named id hides the form's own id from Datastar's morph,
// which then replaces the form and resets its throttle.
func (s *Server) mountCronSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /cron", s.handleCronPage)
	mux.HandleFunc("POST /cron/add", s.handleCronAddForm)
	mux.HandleFunc("POST /cron/enable", s.handleCronEnableForm)
	// Deleting a job and letting jobs make changes confirm (TEN-211);
	// turning that off doesn't.
	mux.HandleFunc("POST /cron/delete", needConfirm(nil, s.handleCronDeleteForm))
	mux.HandleFunc("POST /cron/run", s.handleCronRunForm)
	mux.HandleFunc("POST /cron/exec", needConfirm(confirmUnless("enabled", "", "false"), s.handleCronExecForm))
}

type cronPageData struct {
	layoutData
	Configured bool
	ExecOn     bool
	Jobs       []CronJobView
	AddKey     string   // ends the add form's id; a new one empties its boxes (cronDone)
	Days       []string // the week grid's columns, Monday first (customSpec)
	Err        string
	Msg        string
}

func (s *Server) cronData() cronPageData {
	d := cronPageData{layoutData: layoutData{Title: "Scheduled jobs", Page: "cron"}, Days: weekDays}
	if s.cron != nil {
		d.Configured = true
		d.Jobs = s.cron.Jobs()
		d.ExecOn = s.cron.ExecEnabled()
	}
	return d
}

func (s *Server) handleCronPage(w http.ResponseWriter, r *http.Request) {
	d := s.cronData()
	d.Err = r.URL.Query().Get("err")
	d.Msg = r.URL.Query().Get("msg")
	s.render(w, s.tmpl.cron, d)
}

// cronDone is every Scheduled jobs form's answer: in place for a Datastar
// post, a 303 with the flash otherwise. In place it empties #jobs-flash and
// re-renders #jobs-body. After a job is added it also sends #jobs-add with
// its form under a new id, which the morph can't match to the old form, so
// the boxes come back empty rather than ready to add the same job again.
// Other answers leave #jobs-add, and what is typed in it, alone.
func (s *Server) cronDone(w http.ResponseWriter, r *http.Request, msg, errMsg string, added bool) {
	s.formDone(w, r, "/cron", msg, errMsg, func() (string, error) {
		d := s.cronData()
		parts := []string{"jobs-flash", "jobs-body"}
		if added && d.Configured {
			d.AddKey = "-" + rand.Text()
			parts = append(parts, "jobs-add")
		}
		var buf bytes.Buffer
		for _, p := range parts {
			if err := s.tmpl.cron.ExecuteTemplate(&buf, p, d); err != nil {
				return "", err
			}
		}
		return buf.String(), nil
	})
}

func (s *Server) handleCronAddForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		s.cronDone(w, r, "", "", false)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	spec := strings.TrimSpace(r.FormValue("spec"))
	if spec == "" { // a preset from the list, when nothing was typed (TEN-420)
		spec = strings.TrimSpace(r.FormValue("preset"))
	}
	if spec == "custom" { // the Custom card's boxes (TEN-547)
		var problem string
		if spec, problem = customSpec(r.Form); problem != "" {
			s.cronDone(w, r, "", problem, false)
			return
		}
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	kind := strings.TrimSpace(r.FormValue("kind"))
	tz := strings.TrimSpace(r.FormValue("tz"))
	exec := r.FormValue("exec") == "true" || r.FormValue("exec") == "on"
	if spec == "" || prompt == "" {
		s.cronDone(w, r, "", "A job needs a schedule and something to do.", false)
		return
	}
	if err := s.cron.Add(CronAddSpec{Name: name, Spec: spec, Prompt: prompt, Kind: kind, Exec: exec, TZ: tz}); err != nil {
		s.cronDone(w, r, "", s.flashFor(r.Context(), "cron add", err,
			"Couldn't schedule that job. Check the schedule (for example 0 9 * * 1-5, or @every 30m) and the time zone."), false)
		return
	}
	s.cronDone(w, r, "Job scheduled.", "", true)
}

// handleCronExecForm flips the GLOBAL cron-exec kill-switch. Previously this was
// reachable only from the TUI (/cron exec on|off), which forced an operator onto
// the console for a setting that gates every unattended job.
func (s *Server) handleCronExecForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		s.cronDone(w, r, "", "", false)
		return
	}
	on := r.FormValue("enabled") == "true"
	if err := s.cron.SetExec(on); err != nil {
		s.cronDone(w, r, "", s.flashFor(r.Context(), "cron exec", err, "Couldn't change whether scheduled jobs may make changes."), false)
		return
	}
	if on {
		s.cronDone(w, r, "Scheduled jobs may make changes now: jobs that run a command, or that you let make changes, get the tools that change things.", "", false)
		return
	}
	s.cronDone(w, r, "Scheduled jobs can't make changes now.", "", false)
}

// cronJob is how a job's toast names it, as its card does, so a tap that
// went to another job, or was dropped, shows. ok is false when the page was
// stale and the job has gone (the engine would quietly do nothing).
func (s *Server) cronJob(id string) (name string, ok bool) {
	for _, j := range s.cron.Jobs() {
		if j.ID == id {
			if j.Name != "" {
				return visibleText(j.Name), true
			}
			return j.ID, true
		}
	}
	return "", false
}

const cronGone = "That job isn't there any more. Reload the page to see your jobs as they are now."

func (s *Server) handleCronEnableForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		s.cronDone(w, r, "", "", false)
		return
	}
	id := r.FormValue("job")
	name, ok := s.cronJob(id)
	if !ok {
		s.cronDone(w, r, "", cronGone, false)
		return
	}
	on := r.FormValue("enabled") == "true"
	if err := s.cron.SetEnabled(id, on); err != nil {
		s.cronDone(w, r, "", s.flashFor(r.Context(), "cron set-enabled", err, "Couldn't update that job."), false)
		return
	}
	if on {
		s.cronDone(w, r, name+" is on. It runs on its schedule.", "", false)
		return
	}
	s.cronDone(w, r, name+" is off. It won't run until you turn it on again.", "", false)
}

func (s *Server) handleCronDeleteForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		s.cronDone(w, r, "", "", false)
		return
	}
	id := r.FormValue("job")
	name, ok := s.cronJob(id)
	if !ok {
		s.cronDone(w, r, "", cronGone, false)
		return
	}
	if err := s.cron.Remove(id); err != nil {
		s.cronDone(w, r, "", s.flashFor(r.Context(), "cron remove", err, "Couldn't delete that job."), false)
		return
	}
	s.cronDone(w, r, name+" is deleted.", "", false)
}

func (s *Server) handleCronRunForm(w http.ResponseWriter, r *http.Request) {
	if s.cron == nil {
		s.cronDone(w, r, "", "", false)
		return
	}
	id := r.FormValue("job")
	name, ok := s.cronJob(id)
	if !ok {
		s.cronDone(w, r, "", cronGone, false)
		return
	}
	if err := s.cron.RunNow(id); err != nil {
		s.cronDone(w, r, "", s.flashFor(r.Context(), "cron run", err, "Couldn't run that job."), false)
		return
	}
	s.cronDone(w, r, name+" is running now. The result appears on the Logs page.", "", false)
}

// weekDays are the week grid's columns. A box's value is "<column>-<hour>".
var weekDays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// customSpec turns the Custom card's boxes into a schedule (TEN-547): the
// hours ticked in the week grid, at minute past the hour, on the days they
// are ticked under (weekly), every day (daily), or on the date's day of the
// month (monthly) or its day and month (yearly). A schedule is one crontab
// line, so in a weekly job every ticked day runs at the same hours. problem
// is what to tell the operator when the boxes don't make a schedule.
func customSpec(f url.Values) (spec, problem string) {
	minute, err := strconv.Atoi(cmp.Or(f.Get("minute"), "0"))
	if err != nil || minute < 0 || minute > 59 {
		return "", "Pick the minutes past the hour, from 0 to 59."
	}
	byDay := map[int][]int{} // column (0 is Monday) to its hours
	var hours []int
	for _, slot := range f["slot"] {
		d, h, _ := strings.Cut(slot, "-")
		day, e1 := strconv.Atoi(d)
		hour, e2 := strconv.Atoi(h)
		if e1 != nil || e2 != nil || day < 0 || day > 6 || hour < 0 || hour > 23 {
			return "", "That schedule didn't come through. Reload the page and try again."
		}
		byDay[day] = append(byDay[day], hour)
		hours = append(hours, hour)
	}
	if len(hours) == 0 {
		return "", "Tick at least one hour in the week."
	}
	slices.Sort(hours)
	hours = slices.Compact(hours)
	dom, mon, dow := "*", "*", "*"
	switch repeat := f.Get("repeat"); repeat {
	case "daily":
	case "monthly", "yearly":
		on, err := time.Parse(time.DateOnly, f.Get("date"))
		if err != nil {
			return "", "Pick the date it runs on."
		}
		dom = strconv.Itoa(on.Day())
		if repeat == "yearly" {
			mon = strconv.Itoa(int(on.Month()))
		}
	default: // weekly
		var days []int
		for day, hs := range byDay {
			slices.Sort(hs)
			if !slices.Equal(slices.Compact(hs), hours) {
				return "", "In one job every day runs at the same hours. Make the days match, or add a second job for the other hours."
			}
			days = append(days, (day+1)%7) // crontab's 0 is Sunday
		}
		if len(days) < 7 {
			slices.Sort(days)
			dow = cronList(days)
		}
	}
	return fmt.Sprintf("%d %s %s %s %s", minute, cronList(hours), dom, mon, dow), ""
}

// cronList writes sorted numbers as a crontab list, a run of three or more
// as a range: 9-11,14.
func cronList(n []int) string {
	var b strings.Builder
	for i := 0; i < len(n); {
		j := i
		for j+1 < len(n) && n[j+1] == n[j]+1 {
			j++
		}
		if j-i < 2 {
			j = i
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(n[i]))
		if j > i {
			fmt.Fprintf(&b, "-%d", n[j])
		}
		i = j + 1
	}
	return b.String()
}
