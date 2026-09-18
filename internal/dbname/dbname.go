// Package dbname is the one place in this tree that decides whether an
// account name or a database name is usable.
//
// Both names become a path component, and nothing else checks that they can
// be: internal/cli and internal/server each join an account and a database
// name onto a directory to get the database file, and internal/dbkey joins
// them with a "/" to derive the file's encryption key. A name that is not
// exactly one path component — one that holds a separator, normalizes away
// under filepath.Join (".", "..", a trailing slash, an embedded "x/.."), or
// is simply too long or empty — lets two different (account, db) pairs
// collide on one file, or lets a name walk out of the directory it was told
// to stay inside. Before this package existed, internal/server checked all
// of that in usableComponent and internal/cli checked none of it at all;
// this is what task 0053 closed.
//
// This is the only place in the tree that is allowed to answer that
// question. A caller that turns an account or a database name into a path
// or a key must call Check or CheckPair here, not write its own
// filepath.Join or its own character table — that is exactly the "sixth
// copy" task 0053's own notes warn against, and internal/naming's patrol
// (which watches for the OLD product name) would not see it, because a new
// copy of this rule does not have to mention that name at all. The
// structural test in this package's own test file — the one that greps
// every non-test .go file in the tree for the file extension a database
// path always ends in — is what would catch a new caller writing its own
// copy instead of calling this package; keep it in sync with the caller
// list above when a new one is added. (That test's own source has to spell
// the extension out literally to know what it is looking for — this
// comment does not, on purpose, so that this file does not trip its own
// patrol.)
package dbname

import (
	"errors"
	"fmt"
	"strings"
)

// Err marks a name that cannot be used as an account or a database name.
// Callers that want to say which half of a pair failed, and why, should use
// CheckPair rather than building that sentence themselves.
var Err = errors.New("sapedb/dbname: that is not a usable account or database name")

// Check keeps a name from being a path component.
//
// An account or database name becomes a directory and a file name, so a name
// holding a separator or a parent reference would put a database somewhere
// nobody meant it to be — and a signed name is only as safe as what it is
// allowed to mean.
//
// This is copied byte for byte from internal/server's usableComponent as it
// stood before this package existed (task 0053 moved the body here without
// widening or narrowing it — see that task's Result section for the
// before/after measurement). One consequence of the copy is worth writing
// down where the next person will actually read it: the character table
// below already refuses every one of '/', '\', ':' and NUL on its own — none
// of them are letters, digits, dot, dash or underscore — so the explicit
// "it holds a path separator" clause is provably redundant FOR ACCEPTANCE.
// It earns its place anyway, and the reason is the error message, not
// safety: "it holds a path separator" tells an operator what they actually
// typed, where the character table's message ("it holds 'x', and names
// are...") is technically correct but buries a slash inside a sentence about
// letters and digits. Task 0053 measured this directly (mutation P5):
// deleting this clause and keeping only the table below leaves every
// existing test green, because the table alone still refuses everything
// this clause refuses — the two clauses are not independent conditions, they
// are two different sentences for an overlapping set of inputs. Do not
// delete this clause as "dead code" without deciding, on purpose, that the
// worse message is an acceptable trade — it is not a coverage gap today.
func Check(name string) error {
	switch {
	case name == "":
		return errors.New("it is empty")
	case len(name) > 64:
		return fmt.Errorf("%d characters is more than 64", len(name))
	case name == "." || name == "..":
		return errors.New("it names a directory")
	case strings.ContainsAny(name, `/\:`+"\x00"):
		return errors.New("it holds a path separator")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("it holds %q, and names are letters, digits, dot, dash and underscore", r)
		}
	}
	return nil
}

// CheckPair checks an account name and a database name together, and names
// which one is unusable when either fails. The error wraps Err, so a caller
// that only cares "is this pair usable at all" can use errors.Is.
//
// Account is checked first, so a pair that fails both checks is reported as
// an account failure — the same order internal/server's database() already
// checked in before this package existed, kept so a caller that has both
// wrong sees the same first complaint it always did.
func CheckPair(account, db string) error {
	if err := Check(account); err != nil {
		return fmt.Errorf("%w: account %q: %v", Err, account, err)
	}
	if err := Check(db); err != nil {
		return fmt.Errorf("%w: database %q: %v", Err, db, err)
	}
	return nil
}
