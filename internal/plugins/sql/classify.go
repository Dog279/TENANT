// Package sql is Tenant's SQL plugin: the agent queries databases via
// tools, behind a blast-radius gate analogous to the web plugin's.
//
// A local model generating SQL against a real DB is the data-loss
// analog of autonomous web purchases: a hallucinated `DELETE` with no
// WHERE, or `DROP TABLE`, is irreversible. So statements are CLASSIFIED
// and gated, never trusted by tool name alone.
package sql

import "strings"

// Class is the blast-radius tier of a SQL statement.
type Class int

const (
	ClassRead  Class = iota // SELECT / WITH…SELECT / EXPLAIN — always allowed
	ClassWrite              // INSERT / UPDATE / DELETE — gated (AllowWrite)
	ClassDDL                // DROP/TRUNCATE/ALTER/CREATE/PRAGMA/… — confirm-gated, deny by default
)

func (c Class) String() string {
	switch c {
	case ClassRead:
		return "read"
	case ClassWrite:
		return "write"
	default:
		return "ddl/destructive"
	}
}

// readKeywords / writeKeywords lead a statement of that class. Anything
// not read or write is treated as DDL/destructive — unknown verbs get
// the MOST restrictive class on purpose (fail closed).
var (
	writeKeywords = map[string]bool{
		"INSERT": true, "UPDATE": true, "DELETE": true,
		"UPSERT": true, "REPLACE": true, "MERGE": true,
	}
	readKeywords = map[string]bool{
		"SELECT": true, "EXPLAIN": true, "VALUES": true,
	}
)

// Classify strips comments, rejects multi-statement injection, and
// returns the statement's class + its leading keyword.
//
//	ok, msg := ""  →  (class, keyword, "")
//	rejected       →  (_, _, reason)  — caller turns reason into a tool error
//
// Comment stripping defeats "-- benign\nDROP TABLE x" and
// "SELECT 1 /* */; DROP …" smuggling. Multi-statement rejection
// defeats "SELECT 1; DROP TABLE x" (the classic).
func Classify(raw string) (Class, string, string) {
	s := stripComments(raw)
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "; \t\r\n")
	if s == "" {
		return ClassDDL, "", "empty statement"
	}
	// After trimming a single trailing ';', no ';' may remain — that
	// would be a second statement (injection).
	if strings.Contains(s, ";") {
		return ClassDDL, "", "multiple statements are not allowed (one statement per call)"
	}

	kw := strings.ToUpper(firstToken(s))
	switch {
	case readKeywords[kw]:
		return ClassRead, kw, ""
	case kw == "WITH":
		// CTE: a WITH can lead to SELECT or to INSERT/UPDATE/DELETE.
		// Heuristic — if a DML verb appears as a token, treat as Write;
		// else Read. Conservative: ambiguous WITH+DML → Write (gated),
		// never silently Read.
		up := " " + strings.ToUpper(spaceTokens(s)) + " "
		for k := range writeKeywords {
			if strings.Contains(up, " "+k+" ") {
				return ClassWrite, "WITH/" + k, ""
			}
		}
		return ClassRead, "WITH", ""
	case writeKeywords[kw]:
		return ClassWrite, kw, ""
	default:
		// DROP, TRUNCATE, ALTER, CREATE, PRAGMA, ATTACH, GRANT,
		// VACUUM, REINDEX, and anything unrecognized.
		return ClassDDL, kw, ""
	}
}

// stripComments removes -- line comments and /* */ block comments
// WITHOUT removing them inside string/identifier literals (a string
// containing "--" must survive). Single-quote and double-quote aware.
func stripComments(s string) string {
	var b strings.Builder
	inS, inD := false, false // ' string, " ident
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inS {
			b.WriteByte(c)
			if c == '\'' {
				inS = false
			}
			continue
		}
		if inD {
			b.WriteByte(c)
			if c == '"' {
				inD = false
			}
			continue
		}
		switch {
		case c == '\'':
			inS = true
			b.WriteByte(c)
		case c == '"':
			inD = true
			b.WriteByte(c)
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++ // land on '/', loop ++ moves past
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '(' {
			return s[:i]
		}
	}
	return s
}

// spaceTokens normalizes whitespace+parens to single spaces so the
// WITH/DML scan can match " INSERT " etc. without false positives
// inside identifiers like "inserted_at".
func spaceTokens(s string) string {
	repl := strings.NewReplacer("\n", " ", "\t", " ", "\r", " ", "(", " ", ")", " ", ",", " ")
	out := repl.Replace(s)
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return out
}
