package catalog

import (
	"testing"

	"tenant/internal/logs/event"
)

// Every built-in definition passes the registration checks again, and says
// what it means, why it happens and what to do.
func TestTheCatalogIsValidAndDocumented(t *testing.T) {
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range event.Providers() {
		for _, d := range p.Defs() {
			n++
			if s := d.Spec(); s.Meaning == "" || s.Cause == "" || s.Action == "" {
				t.Errorf("%s/%d is not documented: Doc(meaning, cause, action)", s.Provider, s.ID)
			}
		}
	}
	if n == 0 {
		t.Fatal("no definitions registered")
	}
}
