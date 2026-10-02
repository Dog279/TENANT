package imessage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Contacts names reach handles in either form: a chat.db E.164 phone meets
// the address book's local format, emails match case-insensitively, and a
// record without a first/last name falls back to its organization.
func TestContacts_NamesHandles(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ab.abcddb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZNICKNAME TEXT, ZORGANIZATION TEXT)`,
		`CREATE TABLE ZABCDPHONENUMBER (ZOWNER INTEGER, ZFULLNUMBER TEXT)`,
		`CREATE TABLE ZABCDEMAILADDRESS (ZOWNER INTEGER, ZADDRESS TEXT)`,
		`INSERT INTO ZABCDRECORD VALUES (1, 'Pat', 'Lee', NULL, NULL), (2, NULL, NULL, NULL, 'FindTime LLC')`,
		`INSERT INTO ZABCDPHONENUMBER VALUES (1, '(555) 000-1111'), (2, '+1 555 222 3333')`,
		`INSERT INTO ZABCDEMAILADDRESS VALUES (1, 'Pat@Example.com')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	names := map[string]string{}
	if err := readContacts(context.Background(), db, names); err != nil {
		t.Fatal(err)
	}
	c := &contacts{load: func() map[string]string { return names }}
	for h, want := range map[string]string{
		"+15550001111":    "Pat Lee",
		"pat@example.com": "Pat Lee",
		"5552223333":      "FindTime LLC",
		"+15559999999":    "",
		"me":              "",
	} {
		if got := c.name(h); got != want {
			t.Errorf("name(%q) = %q, want %q", h, got, want)
		}
	}
	if got := c.label("+15550001111"); got != "Pat Lee (+15550001111)" {
		t.Errorf("label = %q", got)
	}
	if got := (*contacts)(nil).label("+15550001111"); got != "+15550001111" {
		t.Errorf("nil contacts label = %q", got)
	}
}
