package server

import (
	"strings"
	"testing"
)

// TestServerNamesTheRightHalfOfThePair is a companion to
// TestAnAccountNameCannotBeAPath, which this task does not touch. That test
// only checks errors.Is(err, ErrName) — true for both positions, no matter
// which word ends up attached to which value — so it would stay green even
// if database()'s two fmt.Errorf calls (the ones wrapping account and
// database) were swapped with each other. This checks the actual wording:
// an account failure names the account and does not also claim to be about
// a database, and vice versa.
//
// Per house-rules.md's "Contains with a short string" warning, the checks
// below use the value together with its label ("account \"a/b\"", not just
// "account"), plus the negative half — a bare positive check on "account"
// would not distinguish this from its sibling, since both error strings
// begin "sapedb/server:" and contain plenty of short common substrings.
func TestServerNamesTheRightHalfOfThePair(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	// Both calls below are expected to fail before ever opening a file, so
	// under today's behavior `release` is always nil. It is still captured
	// and called when non-nil: a mutation that removes database()'s checks
	// (P12) makes one of these succeed for real, taking a lock on that
	// database's own mutex — and Server.Close (t.Cleanup above) locks every
	// open database in turn to shut it down. A release left uncalled here
	// would deadlock that Close on a lock this test is still holding,
	// exactly what QA measured happening before this line existed.
	_, release, err := server.Store("a/b", "main")
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("account \"a/b\" was accepted")
	}
	if !strings.Contains(err.Error(), `account "a/b"`) {
		t.Errorf("account error does not name the account: %v", err)
	}
	if strings.Contains(err.Error(), `database "`) {
		t.Errorf("account error also names a database: %v", err)
	}

	_, release, err = server.Store("acme", "a/b")
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("database \"a/b\" was accepted")
	}
	if !strings.Contains(err.Error(), `database "a/b"`) {
		t.Errorf("database error does not name the database: %v", err)
	}
	if strings.Contains(err.Error(), `account "`) {
		t.Errorf("database error also names an account: %v", err)
	}
}
