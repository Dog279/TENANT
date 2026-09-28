package main

// newcategories.go: an update can add a safety category the operator never
// chose (TEN-343). It asks, even where every other category allows, so the
// operator is told: the terminal UI once per category, serve on every start
// while any is still new.

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"tenant/internal/approval"
)

// newCategoryNotice names the new categories and where to review them (where
// is a sentence); "" when there are none.
func newCategoryNotice(cats []string, where string) string {
	if len(cats) == 0 {
		return ""
	}
	names := make([]string, len(cats))
	for i, c := range cats {
		names[i] = approval.CategoryPlain[c].Title + " (" + c + ")"
	}
	head := fmt.Sprintf("%d new safety checks ask", len(cats))
	if len(cats) == 1 {
		head = "1 new safety check asks"
	}
	return head + " you first until you choose: " + strings.Join(names, ", ") + ". " + where
}

// unannounced is cats without the ones already announced.
func unannounced(cats, seen []string) []string {
	return slices.DeleteFunc(slices.Clone(cats), func(c string) bool { return slices.Contains(seen, c) })
}

// announceNewCategories tells the terminal operator about each new category
// once. The seen-marker goes into the shared settings under mu before the
// save, so a later broker persist (which saves the same object) keeps it.
func announceNewCategories(broker *approvalBroker, stg *settings, mu *sync.Mutex, save func(), tell func(string)) {
	cats := broker.NewCategories()
	mu.Lock()
	cats = unannounced(cats, stg.AnnouncedCategories)
	stg.AnnouncedCategories = append(stg.AnnouncedCategories, cats...)
	mu.Unlock()
	if len(cats) == 0 {
		return
	}
	tell("permissions: " + newCategoryNotice(cats, "Review with /permissions."))
	save()
}
