package imessage

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"
)

// contacts.go puts the names from the Mac's Contacts on iMessage handles. The
// names live in the Contacts app's AddressBook SQLite stores, read-only under
// the same Full Disk Access as chat.db. Best effort: a store that can't be
// read just means no names, never a failed read of the messages.

// contactsTTL is how long names are cached before the stores are read again,
// so a contact added while Tenant runs shows up without a restart.
const contactsTTL = 10 * time.Minute

// contacts maps handles to contact names. A nil *contacts has no names.
type contacts struct {
	load func() map[string]string // contactKey → name

	mu    sync.Mutex
	names map[string]string
	at    time.Time
}

// name returns handle's contact name, or "" when there is none.
func (c *contacts) name(handle string) string {
	k := contactKey(handle)
	if c == nil || c.load == nil || k == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil || time.Since(c.at) > contactsTTL {
		c.names, c.at = c.load(), time.Now()
	}
	return c.names[k]
}

// label is "Name (handle)", or the bare handle when it has no contact.
func (c *contacts) label(handle string) string {
	if n := c.name(handle); n != "" {
		return n + " (" + handle + ")"
	}
	return handle
}

// contactKey is the form a handle and an address-book entry are matched on.
func contactKey(h string) string {
	k := normalizeHandle(h)
	// ponytail: phones match on their last 10 digits, so chat.db's
	// +15551230000 meets the address book's (555) 123-0000. Two countries'
	// numbers that share those digits would collide; parse country codes if
	// that ever bites.
	if !strings.Contains(k, "@") && len(k) > 10 {
		k = k[len(k)-10:]
	}
	return k
}

// contactName is a record's display name: first + last, else nickname, else
// organization.
const contactName = `COALESCE(NULLIF(TRIM(COALESCE(r.ZFIRSTNAME, '') || ' ' || COALESCE(r.ZLASTNAME, '')), ''), NULLIF(r.ZNICKNAME, ''), r.ZORGANIZATION)`

// readContacts adds one AddressBook store's phone and email names to into.
// The first name seen for a handle wins.
func readContacts(ctx context.Context, db *sql.DB, into map[string]string) error {
	rows, err := db.QueryContext(ctx, `SELECT `+contactName+`, p.ZFULLNUMBER FROM ZABCDPHONENUMBER p JOIN ZABCDRECORD r ON r.Z_PK = p.ZOWNER
UNION ALL
SELECT `+contactName+`, e.ZADDRESS FROM ZABCDEMAILADDRESS e JOIN ZABCDRECORD r ON r.Z_PK = e.ZOWNER`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, addr sql.NullString
		if err := rows.Scan(&name, &addr); err != nil {
			return err
		}
		k, n := contactKey(addr.String), strings.TrimSpace(name.String)
		if _, seen := into[k]; k != "" && n != "" && !seen {
			into[k] = n
		}
	}
	return rows.Err()
}
