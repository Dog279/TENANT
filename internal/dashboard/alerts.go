package dashboard

import (
	"context"

	"tenant/internal/logs/logapi"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
)

// SetAlertRules lets the log book API manage the alert rules (TEN-399),
// which live in config.json: the composition root holds them.
func (s *Server) SetAlertRules(ac logapi.AlertConfig) { s.logbook.Alerts = ac }

// recentAlerts is the Overview's health card: how many alerts fired in the
// last day (alerts 1001, whichever process evaluated them) and the newest
// one's message.
func (s *Server) recentAlerts(ctx context.Context) (int, string) {
	var st *store.Store
	if s.logs != nil {
		st = s.logs.Store()
	}
	if st == nil {
		return 0, ""
	}
	f, _ := query.Parse("provider:alerts id:1001 since:24h")
	evs, _, _, err := st.Query(ctx, f, store.Cursor{}, 1, true)
	if err != nil || len(evs) == 0 {
		return 0, ""
	}
	n, err := st.Count(ctx, f)
	if err != nil {
		n = 1
	}
	return int(n), evs[0].Msg
}
