package logs

// Notify puts line on the operator's status feed, the one OnAlert was
// given, which the terminal, the TUI and the system log show: an alert
// rule's banner (TEN-399). Before OnAlert it does nothing.
func (h *Hub) Notify(line string) {
	h.lb.mu.Lock()
	fn := h.lb.alert
	h.lb.mu.Unlock()
	if fn != nil {
		fn(line)
	}
}
