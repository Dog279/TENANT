package gsuite

import (
	"context"
	"fmt"
	"strings"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// Calendar wraps the official Google Calendar v3 client, translating its
// types into tenant's normalized shapes.
type Calendar struct{ svc *calendar.Service }

// calPrimary is the special calendarId for the user's main calendar.
const calPrimary = "primary"

// Event is a normalized calendar entry.
type Event struct {
	ID, Summary, Location, Description string
	Start, End                         time.Time
	AllDay                             bool
	Attendees                          []string
	HTMLLink                           string
}

// EventInput is a new or patched event. For updates, zero-valued fields
// are left untouched.
type EventInput struct {
	Summary, Description, Location string
	Start, End                     time.Time
	Attendees                      []string
}

// ListEvents returns events in [timeMin,timeMax) on the primary calendar,
// expanding recurring ones, sorted by start.
func (c *Calendar) ListEvents(ctx context.Context, timeMin, timeMax time.Time, max int) ([]Event, error) {
	if max <= 0 {
		max = 25
	}
	resp, err := c.svc.Events.List(calPrimary).
		TimeMin(timeMin.UTC().Format(time.RFC3339)).
		TimeMax(timeMax.UTC().Format(time.RFC3339)).
		SingleEvents(true).OrderBy("startTime").MaxResults(int64(max)).
		Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(resp.Items))
	for _, it := range resp.Items {
		out = append(out, normalizeEvent(it))
	}
	return out, nil
}

// CreateEvent adds an event to the primary calendar. Gated by the
// dispatcher (blast-radius: this notifies attendees).
func (c *Calendar) CreateEvent(ctx context.Context, in EventInput) (*Event, error) {
	if strings.TrimSpace(in.Summary) == "" {
		return nil, fmt.Errorf("gsuite: event needs a summary")
	}
	if !in.End.After(in.Start) {
		return nil, fmt.Errorf("gsuite: event end must be after start")
	}
	ev := &calendar.Event{
		Summary:     in.Summary,
		Description: in.Description,
		Location:    in.Location,
		Start:       &calendar.EventDateTime{DateTime: in.Start.UTC().Format(time.RFC3339)},
		End:         &calendar.EventDateTime{DateTime: in.End.UTC().Format(time.RFC3339)},
		Attendees:   attendees(in.Attendees),
	}
	created, err := c.svc.Events.Insert(calPrimary, ev).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := normalizeEvent(created)
	return &out, nil
}

// UpdateEvent patches an existing event on the primary calendar. Only the
// non-zero fields of in are changed (PATCH semantics). Gated.
func (c *Calendar) UpdateEvent(ctx context.Context, id string, in EventInput) (*Event, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gsuite: update needs an event id")
	}
	patch := &calendar.Event{}
	if in.Summary != "" {
		patch.Summary = in.Summary
	}
	if in.Description != "" {
		patch.Description = in.Description
	}
	if in.Location != "" {
		patch.Location = in.Location
	}
	if !in.Start.IsZero() {
		patch.Start = &calendar.EventDateTime{DateTime: in.Start.UTC().Format(time.RFC3339)}
	}
	if !in.End.IsZero() {
		patch.End = &calendar.EventDateTime{DateTime: in.End.UTC().Format(time.RFC3339)}
	}
	if len(in.Attendees) > 0 {
		patch.Attendees = attendees(in.Attendees)
	}
	updated, err := c.svc.Events.Patch(calPrimary, id, patch).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := normalizeEvent(updated)
	return &out, nil
}

// DeleteEvent removes an event from the primary calendar. Gated
// (blast-radius: cancels the event / notifies attendees).
func (c *Calendar) DeleteEvent(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("gsuite: delete needs an event id")
	}
	return c.svc.Events.Delete(calPrimary, id).Context(ctx).Do()
}

// CalendarInfo is one entry from the user's calendar list.
type CalendarInfo struct {
	ID, Summary, AccessRole string
	Primary                 bool
}

// Calendars lists the calendars the user can see (so the model can pick a
// calendarId for free/busy queries).
func (c *Calendar) Calendars(ctx context.Context) ([]CalendarInfo, error) {
	resp, err := c.svc.CalendarList.List().Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]CalendarInfo, 0, len(resp.Items))
	for _, it := range resp.Items {
		out = append(out, CalendarInfo{
			ID: it.Id, Summary: it.Summary, AccessRole: it.AccessRole, Primary: it.Primary,
		})
	}
	return out, nil
}

// BusyInterval is one busy block on a calendar (free/busy query result).
type BusyInterval struct {
	Calendar   string
	Start, End time.Time
}

// FreeBusy returns the busy intervals for the given calendars in
// [timeMin,timeMax). Empty ids defaults to the primary calendar.
func (c *Calendar) FreeBusy(ctx context.Context, timeMin, timeMax time.Time, ids []string) ([]BusyInterval, error) {
	if len(ids) == 0 {
		ids = []string{calPrimary}
	}
	items := make([]*calendar.FreeBusyRequestItem, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			items = append(items, &calendar.FreeBusyRequestItem{Id: id})
		}
	}
	req := &calendar.FreeBusyRequest{
		TimeMin: timeMin.UTC().Format(time.RFC3339),
		TimeMax: timeMax.UTC().Format(time.RFC3339),
		Items:   items,
	}
	resp, err := c.svc.Freebusy.Query(req).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []BusyInterval
	for calID, cal := range resp.Calendars {
		for _, b := range cal.Busy {
			start, _ := time.Parse(time.RFC3339, b.Start)
			end, _ := time.Parse(time.RFC3339, b.End)
			out = append(out, BusyInterval{Calendar: calID, Start: start, End: end})
		}
	}
	return out, nil
}

// --- helpers ---

func attendees(emails []string) []*calendar.EventAttendee {
	if len(emails) == 0 {
		return nil
	}
	at := make([]*calendar.EventAttendee, 0, len(emails))
	for _, e := range emails {
		if e = strings.TrimSpace(e); e != "" {
			at = append(at, &calendar.EventAttendee{Email: e})
		}
	}
	return at
}

func normalizeEvent(c *calendar.Event) Event {
	if c == nil {
		return Event{}
	}
	e := Event{
		ID: c.Id, Summary: c.Summary, Location: c.Location,
		Description: c.Description, HTMLLink: c.HtmlLink,
	}
	for _, a := range c.Attendees {
		if a.Email != "" {
			e.Attendees = append(e.Attendees, a.Email)
		}
	}
	e.Start, e.AllDay = parseEventTime(c.Start)
	e.End, _ = parseEventTime(c.End)
	return e
}

func parseEventTime(t *calendar.EventDateTime) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	if t.DateTime != "" {
		if v, err := time.Parse(time.RFC3339, t.DateTime); err == nil {
			return v, false
		}
	}
	if t.Date != "" {
		if v, err := time.Parse("2006-01-02", t.Date); err == nil {
			return v, true
		}
	}
	return time.Time{}, false
}
