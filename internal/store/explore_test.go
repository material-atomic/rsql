package store

import (
	"errors"
	"testing"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/vfs"
)

// shelf is a small collection to look around in.
func shelf(t *testing.T, store *Store) {
	t.Helper()

	if _, err := store.Declare(Spec{
		Name: "books",
		Key:  Key{Path: "id", Type: TypeString},
		Indexes: []Index{{
			Name: "by_shelf",
			Fields: []Field{
				{Path: "shelf", Type: TypeString, Missing: MissingSkip},
				{Path: "at", Type: TypeNumber, Missing: MissingLast},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	books, err := store.Collection("books")
	if err != nil {
		t.Fatal(err)
	}
	for i, book := range []map[string]any{
		{"id": "b1", "shelf": "history", "at": 1.0, "title": "the first"},
		{"id": "b2", "shelf": "history", "at": 2.0, "title": "the second"},
		{"id": "b3", "shelf": "history", "at": 3.0, "title": "the third"},
		{"id": "b4", "shelf": "poetry", "at": 1.0, "title": "the fourth"},
	} {
		if _, err := books.Put(book); err != nil {
			t.Fatalf("book %d: %v", i, err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestWhatWasTypedComesBackAsSomethingToDeclare: the shell's answer is two
// things — the rows, and the operation that would fetch them again.
//
// Checked by declaring what came back and running it: an operation that only
// looks like the access that ran would send somebody away with a declaration
// that returns something else, and they would find out in production.
func TestWhatWasTypedComesBackAsSomethingToDeclare(t *testing.T) {
	_, store := fresh(t, 100)
	shelf(t, store)

	drafted, explored, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "scan", Collection: "books", Index: "by_shelf",
		From: &Bound{Values: []any{"history", 2.0}},
		To:   &Bound{Values: []any{"history"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if explored.Count != 2 {
		t.Fatalf("the scan found %d rows, want 2", explored.Count)
	}

	// Now declare the draft and run it. Same rows, or the shell lied.
	drafted.Name = "books.on_a_shelf_since"
	if _, err := store.DeclareOperation(drafted); err != nil {
		t.Fatalf("the shell handed back something that will not declare: %v", err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	declared, err := store.Invoke(Caller{}, "books.on_a_shelf_since", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if declared.Count != explored.Count {
		t.Fatalf("declared it and got %d rows, the shell got %d", declared.Count, explored.Count)
	}
	for i := range declared.Rows {
		if declared.Rows[i]["id"] != explored.Rows[i]["id"] {
			t.Errorf("row %d: declared %v, explored %v", i, declared.Rows[i]["id"], explored.Rows[i]["id"])
		}
	}
}

// TestLookingAtOneDocument is the other half: a key, and what is under it.
func TestLookingAtOneDocument(t *testing.T) {
	_, store := fresh(t, 101)
	shelf(t, store)

	drafted, found, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "get", Collection: "books", Key: "b3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if found.Count != 1 || found.Rows[0]["title"] != "the third" {
		t.Fatalf("looked at b3 and got %+v", found.Rows)
	}
	if drafted.Action != ActionGet || drafted.Key == nil || drafted.Key.Value != "b3" {
		t.Errorf("the draft does not fetch b3: %+v", drafted)
	}

	// Nothing under a key is an empty answer, not a failure: "it is not there"
	// is the thing the operator came to find out.
	if _, missing, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "get", Collection: "books", Key: "b9",
	}); err != nil {
		t.Fatal(err)
	} else if missing.Count != 0 {
		t.Errorf("a key with nothing under it returned %d rows", missing.Count)
	}
}

// TestAShellScanIsAlwaysBounded: exploring is exactly where an unbounded scan
// would be reached for, and there is not one to reach for. An operator who
// does not name a limit gets a page and is told there was more.
func TestAShellScanIsAlwaysBounded(t *testing.T) {
	_, store := fresh(t, 102)
	shelf(t, store)

	drafted, _, err := store.Explore(Caller{Actor: "ops@acme"}, Access{Kind: "scan", Collection: "books"})
	if err != nil {
		t.Fatal(err)
	}
	if drafted.Limit != MostRows {
		t.Errorf("a scan with no limit was declared as %d, want %d", drafted.Limit, MostRows)
	}

	asked, _, err := store.Explore(Caller{Actor: "ops@acme"}, Access{Kind: "scan", Collection: "books", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Limit != 2 {
		t.Errorf("a scan asking for 2 was declared as %d", asked.Limit)
	}
	// Asking for more than the cap does not get it. An operator who types a
	// large number has not decided the store can afford it.
	greedy, capped, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "scan", Collection: "books", Limit: MostRows * 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if greedy.Limit != MostRows {
		t.Errorf("asked for %d and the draft says %d, want %d", MostRows*10, greedy.Limit, MostRows)
	}
	if capped.Truncated {
		t.Error("four books did not fit in a page")
	}

	// And the limit is real, not a number written into the draft.
	_, page, err := store.Explore(Caller{Actor: "ops@acme"}, Access{Kind: "scan", Collection: "books", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 2 || !page.Truncated {
		t.Errorf("asked for 2 of 4 and got %d rows, truncated %v", page.Count, page.Truncated)
	}
}

// TestLookingIsInTheLogToo: a read that leaves no trace is a read nobody can
// be asked about afterwards, and the people who can reach the shell are
// exactly the people who most need to be asked.
func TestLookingIsInTheLogToo(t *testing.T) {
	_, store := fresh(t, 103)
	shelf(t, store)

	before, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "get", Collection: "books", Key: "b1",
	}); err != nil {
		t.Fatal(err)
	}

	var read []Change
	if err := store.Changes(before+1, func(change Change) bool {
		read = append(read, change)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(read) != 1 {
		t.Fatalf("looking at one document wrote %d log entries, want 1", len(read))
	}

	entry := read[0]
	if entry.Kind != ChangeRead {
		t.Errorf("the entry is a %q", entry.Kind)
	}
	if entry.By.Actor != "ops@acme" {
		t.Errorf("the entry says %q looked", entry.By.Actor)
	}
	if entry.Operation == nil || entry.Operation.Action != ActionGet {
		t.Error("the entry does not say what was looked at")
	}
	// What was read is not in the log. An audit trail holding every document
	// anybody read is a second copy of the database, and it would carry those
	// documents into every replica and every change feed.
	if entry.Document != nil {
		t.Errorf("the log kept what was read: %v", entry.Document)
	}
}

// TestAReplicaReplaysALookAsNothing: the entry travels, because an audit trail
// that stops at the primary cannot be read anywhere the primary is not. What
// does not travel is any effect, because there was none.
func TestAReplicaReplaysALookAsNothing(t *testing.T) {
	_, primary := fresh(t, 104)
	shelf(t, primary)
	if _, _, err := primary.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "get", Collection: "books", Key: "b1",
	}); err != nil {
		t.Fatal(err)
	}

	_, replica := fresh(t, 105)
	var entries []Change
	if err := primary.Changes(1, func(change Change) bool {
		entries = append(entries, change)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := replica.Apply(entry); err != nil {
			t.Fatalf("entry %d (%s): %v", entry.LSN, entry.Kind, err)
		}
	}
	if err := replica.Commit(); err != nil {
		t.Fatal(err)
	}

	books, err := replica.Collection("books")
	if err != nil {
		t.Fatal(err)
	}
	counted := 0
	if err := books.Walk(func(any, map[string]any) bool { counted++; return true }); err != nil {
		t.Fatal(err)
	}
	if counted != 4 {
		t.Errorf("the replica holds %d books, want 4", counted)
	}

	// And the look itself is on the replica, in its place in the order.
	var kinds []string
	if err := replica.Changes(1, func(change Change) bool {
		kinds = append(kinds, change.Kind)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != ChangeRead {
		t.Errorf("the replica's log ends with %v", kinds)
	}
}

// TestAShellCannotDoWhatADeclarationCouldNot: the shell is the same engine
// with the parameters arriving later. Everything a declaration is refused for,
// it is refused for — and it cannot write at all, because a change to a
// production database should be something somebody wrote down and can run
// again.
func TestAShellCannotDoWhatADeclarationCouldNot(t *testing.T) {
	_, store := fresh(t, 106)
	shelf(t, store)

	for _, refused := range []struct {
		why    string
		access Access
		is     error
	}{
		{"a write", Access{Kind: "put", Collection: "books"}, ErrDeclaration},
		{"a delete", Access{Kind: "delete", Collection: "books", Key: "b1"}, ErrDeclaration},
		{"a get with no key", Access{Kind: "get", Collection: "books"}, ErrDeclaration},
		{"a collection that is not there", Access{Kind: "scan", Collection: "nothing"}, ErrNoCollection},
		{"an index that is not there", Access{Kind: "scan", Collection: "books", Index: "by_nothing"}, ErrNoIndex},
		// Refused as a declaration, not as a type error at scan time: the
		// draft is checked before it runs, so the shell cannot reach a state
		// the declaration rules would not allow.
		{"a bound of the wrong type", Access{
			Kind: "scan", Collection: "books", Index: "by_shelf",
			From: &Bound{Values: []any{42.0}},
		}, ErrDeclaration},
	} {
		if _, _, err := store.Explore(Caller{Actor: "ops@acme"}, refused.access); !errors.Is(err, refused.is) {
			t.Errorf("%s: want %v, got %v", refused.why, refused.is, err)
		}
	}

	// A refused access is not in the log either: nothing happened, and an
	// audit trail that records attempts as reads would make "looked at every
	// customer" out of a typo.
	latest, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Explore(Caller{Actor: "ops@acme"}, Access{Kind: "scan", Collection: "nothing"}); err == nil {
		t.Fatal("a collection that is not there was scanned")
	}
	if now, err := store.LatestLSN(); err != nil {
		t.Fatal(err)
	} else if now != latest {
		t.Errorf("a refused access wrote %d log entries", now-latest)
	}
}

// TestALookSurvivesTheMachineStopping: an audit entry that is only in memory
// is one that disappears exactly when somebody wants to read it — after the
// incident that took the process down.
func TestALookSurvivesTheMachineStopping(t *testing.T) {
	disk, store := fresh(t, 107)
	shelf(t, store)

	if _, _, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "get", Collection: "books", Key: "b1",
	}); err != nil {
		t.Fatal(err)
	}

	// Only what reached the disk. Nothing here writes again, so an entry that
	// was recorded but never committed is gone.
	restart := vfs.NewSim(107, vfs.Faults{})
	restart.Restore(disk.Durable())
	pages, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}

	looked := false
	if err := reopened.Changes(1, func(change Change) bool {
		if change.Kind == ChangeRead && change.By.Actor == "ops@acme" {
			looked = true
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !looked {
		t.Error("the machine stopped and took the record of who had been looking with it")
	}
}

// TestTheDraftIsWhatWasActuallyAsked: the shell hands back a declaration, and
// a declaration that differs from what ran is worse than none — somebody will
// commit it believing it was tested by the exploring they just did.
func TestTheDraftIsWhatWasActuallyAsked(t *testing.T) {
	_, store := fresh(t, 108)
	shelf(t, store)

	// Counting is counting. Drafted as a scan it would return a thousand
	// documents to whoever ran it next, for a question that wanted a number.
	counted, howMany, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "count", Collection: "books", Index: "by_shelf",
		From: &Bound{Values: []any{"history"}}, To: &Bound{Values: []any{"history"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if counted.Action != ActionCount {
		t.Errorf("a count was drafted as %q", counted.Action)
	}
	if howMany.Count != 3 || len(howMany.Rows) != 0 {
		t.Errorf("counting history gave %d and %d rows", howMany.Count, len(howMany.Rows))
	}

	// Asking for two fields and being handed a declaration that returns whole
	// documents is how a projection quietly stops being one.
	projected, rows, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "get", Collection: "books", Key: "b2", Projection: []string{"title"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Projection) != 1 || projected.Projection[0] != "title" {
		t.Errorf("the draft returns %v", projected.Projection)
	}
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 1 || rows.Rows[0]["title"] != "the second" {
		t.Errorf("asked for the title and got %v", rows.Rows)
	}

	// An exclusive bound is where the operator said the edge is. Carried
	// wrongly, the draft returns one row more than the exploring did — which
	// is the row somebody was trying to exclude.
	_, inclusive, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "scan", Collection: "books", Index: "by_shelf",
		From: &Bound{Values: []any{"history", 1.0}}, To: &Bound{Values: []any{"history"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drafted, exclusive, err := store.Explore(Caller{Actor: "ops@acme"}, Access{
		Kind: "scan", Collection: "books", Index: "by_shelf",
		From: &Bound{Values: []any{"history", 1.0}, Exclusive: true},
		To:   &Bound{Values: []any{"history"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inclusive.Count != 3 || exclusive.Count != 2 {
		t.Errorf("inclusive found %d and exclusive found %d, want 3 and 2", inclusive.Count, exclusive.Count)
	}
	if drafted.From == nil || !drafted.From.Exclusive {
		t.Error("the draft lost the exclusive bound, so running it returns a row the exploring did not")
	}
}

// TestAskingWhatIsHereIsAlsoLooking: finding out what a database holds is the
// first thing anybody asks, welcome or not. A log that records the scan but
// not the question that found the collection to scan tells half the story.
func TestAskingWhatIsHereIsAlsoLooking(t *testing.T) {
	_, store := fresh(t, 109)
	shelf(t, store)

	before, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}

	here, err := store.WhatIsHere(Caller{Actor: "ops@acme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(here.Collections) != 1 || here.Collections[0].Name != "books" {
		t.Fatalf("what is here came back as %+v", here.Collections)
	}
	if len(here.Collections[0].Indexes) != 1 || here.Collections[0].Indexes[0].Name != "by_shelf" {
		t.Errorf("the indexes did not come with it: %+v", here.Collections[0].Indexes)
	}

	var asked []Change
	if err := store.Changes(before+1, func(change Change) bool {
		asked = append(asked, change)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].Kind != ChangeRead || asked[0].By.Operation != "catalogue" {
		t.Fatalf("asking what is here left %+v", asked)
	}
	if asked[0].By.Actor != "ops@acme" {
		t.Errorf("the log says %q asked", asked[0].By.Actor)
	}
}
