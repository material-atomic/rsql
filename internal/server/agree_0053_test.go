// This file lives in package server_test, alongside handoff_test.go, for
// the same reason that one gives for its own placement: it is where a test
// can import both internal/dbname and internal/server and check that they
// actually agree, rather than trusting "they call the same function" as an
// argument on its own. Task 0053's mục 2 says a shared function only one
// caller actually reaches is not a shared rule — this is what proves
// internal/server's caller is one of the ones actually reaching it.
package server_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/dbname"
	"github.com/sapedb/sapedb/internal/server"
)

// TestDbnameAndServerStoreAgreeOnEveryName runs one list of names through
// two doors — dbname.Check directly, and server.Store, which task 0053
// made call dbname.Check at internal/server/server.go's database() — and
// requires the same verdict from both for every name, in both the account
// position and the database position.
//
// The list mixes three kinds of case on purpose: two the path-separator
// clause alone would refuse (a/b, and NUL), several that clause does NOT
// refuse but the character table does (a;b, m|n, "a b" — task 0053 mục 3's
// "not every half of the rule is a separator" finding), and two controls at
// the 64-character boundary that must be accepted by both doors. A table
// that only exercised the separator clause would not catch server.database
// or dbname.Check narrowing to just that clause.
func TestDbnameAndServerStoreAgreeOnEveryName(t *testing.T) {
	srv, err := server.New(server.Options{Dir: t.TempDir(), Secret: "the secret only this table's server has"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	names := []string{
		"a/b", `a\b`, "a:b", "a\x00b", ".", "..", "../escape",
		"a;b", "m|n", "a b",
		strings.Repeat("x", 65),
		strings.Repeat("x", 64), // control: must be usable on both doors
		"acme",                  // control: must be usable on both doors
	}

	for i, name := range names {
		wantUsable := dbname.Check(name) == nil

		// Position 1: name is the account, paired with a db that is always
		// valid and unique to this row so successive rows never collide on
		// an already-open database. Store's second return value is a
		// release func that unlocks that database's own mutex — Server.Close
		// (in t.Cleanup above) locks every open database's mutex in turn to
		// shut it down, so a release left uncalled here deadlocks Close
		// forever on a lock this test is still holding.
		_, release, err := srv.Store(name, "control-db-"+strconv.Itoa(i))
		if release != nil {
			release()
		}
		gotUsable := err == nil || !errors.Is(err, server.ErrName)
		if gotUsable != wantUsable {
			t.Errorf("account %q: dbname.Check usable=%v, server.Store usable=%v (err=%v)", name, wantUsable, gotUsable, err)
		}

		// Position 2: name is the database, paired with a distinct valid
		// account.
		_, release, err = srv.Store("control-acct-"+strconv.Itoa(i), name)
		if release != nil {
			release()
		}
		gotUsable = err == nil || !errors.Is(err, server.ErrName)
		if gotUsable != wantUsable {
			t.Errorf("database %q: dbname.Check usable=%v, server.Store usable=%v (err=%v)", name, wantUsable, gotUsable, err)
		}
	}
}
