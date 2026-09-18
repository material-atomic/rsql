package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/material-atomic/rsql/internal/store"
	"github.com/material-atomic/rsql/internal/wire"
)

// looked records what the shell asked for, so a test can check the line that
// was typed against the access that went out.
type looked struct {
	asked  []store.Access
	answer wire.Explored
	here   store.Catalogue
	fail   error
}

func (l *looked) WhatIsHere() (store.Catalogue, error) { return l.here, l.fail }

func (l *looked) Explore(access store.Access) (wire.Explored, error) {
	l.asked = append(l.asked, access)
	return l.answer, l.fail
}

func typed(t *testing.T, look Looking, lines ...string) string {
	t.Helper()
	out := &strings.Builder{}
	if err := Shell(look, "main", strings.NewReader(strings.Join(lines, "\n")+"\n"), out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestWhatWasTypedIsWhatGoesOut: the shell's whole job is that a line somebody
// typed becomes exactly one access with exactly the bounds they meant. A
// keyword read as a value, or a bound quietly dropped, is a different question
// answered without anybody noticing.
func TestWhatWasTypedIsWhatGoesOut(t *testing.T) {
	for _, one := range []struct {
		line   string
		wanted store.Access
	}{
		{`get books "b1"`, store.Access{Kind: "get", Collection: "books", Key: "b1"}},
		{`get books 12`, store.Access{Kind: "get", Collection: "books", Key: 12.0}},

		{`scan books`, store.Access{Kind: "scan", Collection: "books"}},
		{`scan books by_shelf`, store.Access{Kind: "scan", Collection: "books", Index: "by_shelf"}},

		{`scan books by_shelf from "history" to "history"`, store.Access{
			Kind: "scan", Collection: "books", Index: "by_shelf",
			From: &store.Bound{Values: []any{"history"}},
			To:   &store.Bound{Values: []any{"history"}},
		}},

		// Several values are one composite bound, not several bounds.
		{`scan books by_shelf from "history" 2 to "history" 9`, store.Access{
			Kind: "scan", Collection: "books", Index: "by_shelf",
			From: &store.Bound{Values: []any{"history", 2.0}},
			To:   &store.Bound{Values: []any{"history", 9.0}},
		}},

		// after and before are the same bounds, excluded.
		{`scan books by_shelf after "history" 1 before "history" 9`, store.Access{
			Kind: "scan", Collection: "books", Index: "by_shelf",
			From: &store.Bound{Values: []any{"history", 1.0}, Exclusive: true},
			To:   &store.Bound{Values: []any{"history", 9.0}, Exclusive: true},
		}},

		{`count books by_shelf from "poetry"`, store.Access{
			Kind: "count", Collection: "books", Index: "by_shelf",
			From: &store.Bound{Values: []any{"poetry"}},
		}},

		{`scan books limit 5 fields title shelf`, store.Access{
			Kind: "scan", Collection: "books", Limit: 5,
			Projection: []string{"title", "shelf"},
		}},

		// Order does not matter, and a scan with no index is the clustered one.
		{`scan books fields title limit 5 from "a"`, store.Access{
			Kind: "scan", Collection: "books", Limit: 5,
			Projection: []string{"title"},
			From:       &store.Bound{Values: []any{"a"}},
		}},
	} {
		look := &looked{}
		typed(t, look, one.line)
		if len(look.asked) != 1 {
			t.Errorf("%q asked for %d accesses", one.line, len(look.asked))
			continue
		}
		if !reflect.DeepEqual(look.asked[0], one.wanted) {
			t.Errorf("%q\n  went out as %+v\n  wanted      %+v", one.line, look.asked[0], one.wanted)
		}
	}
}

// TestAShellRefusesWhatItDoesNotUnderstand: a shell that ignores a word it
// does not know runs a different question from the one that was typed, and
// says nothing about it.
func TestAShellRefusesWhatItDoesNotUnderstand(t *testing.T) {
	for _, one := range []struct {
		line   string
		saying string
	}{
		// A line from another database's query language: the first wrong word
		// lands in the index position, so the shell says what it understood
		// rather than complaining about the second wrong word alone.
		{`scan books where shelf = "history"`, `read as: scan books, index "where"`},
		{`scan books by_shelf from history`, `history is not a value`},
		{`get books b1`, `b1 is not a value`},
		{`get books`, `get takes one key`},
		{`get books "a" "b"`, `get takes one key`},
		{`scan`, `scan what?`},
		{`scan books limit`, `limit what?`},
		{`scan books limit 0`, `whole number`},
		{`scan books limit 1.5`, `whole number`},
		{`scan books limit "ten"`, `whole number`},
		{`scan books from`, `from what?`},
		{`scan books fields`, `fields what?`},
		{`delete books "b1"`, `there is no "delete"`},
		{`put books "b1"`, `there is no "put"`},
		{`db.books.find({})`, `there is no "db.books.find({})"`},
	} {
		look := &looked{}
		printed := typed(t, look, one.line)
		if !strings.Contains(printed, one.saying) {
			t.Errorf("%q printed\n  %s\n  wanted something with %q", one.line, strings.TrimSpace(printed), one.saying)
		}
		if len(look.asked) != 0 {
			t.Errorf("%q was refused and went out anyway as %+v", one.line, look.asked[0])
		}
	}
}

// TestAMistakeDoesNotEndTheSession: an operator who mistypes a bound at two in
// the morning should see why and try again, not be dropped.
func TestAMistakeDoesNotEndTheSession(t *testing.T) {
	look := &looked{}
	printed := typed(t, look, `scan books where x = 1`, ``, `scan books by_shelf from "history"`)

	if !strings.Contains(printed, `index "where"`) {
		t.Errorf("the mistake was not explained: %s", printed)
	}
	if len(look.asked) != 1 {
		t.Fatalf("after the mistake, %d accesses went out", len(look.asked))
	}
	if look.asked[0].Index != "by_shelf" {
		t.Errorf("the line after the mistake went out as %+v", look.asked[0])
	}
}

// TestExploringEndsInSomethingToDeclare: the point of the shell is that it
// produces a declaration. What it prints must be the draft the server sent
// back for what actually ran — anything reconstructed here could differ from
// it, and the difference would only show in production.
func TestExploringEndsInSomethingToDeclare(t *testing.T) {
	look := &looked{answer: wire.Explored{
		Draft: store.Operation{
			Name: "books.scan", Collection: "books", Action: store.ActionScan,
			Index: "by_shelf", Limit: 1000, Version: 3,
		},
	}}

	printed := typed(t, look, `scan books by_shelf`, `declare books.on_a_shelf`)

	if !strings.Contains(printed, `"name": "books.on_a_shelf"`) {
		t.Errorf("the declaration was not named: %s", printed)
	}
	if !strings.Contains(printed, `"index": "by_shelf"`) || !strings.Contains(printed, `"limit": 1000`) {
		t.Errorf("the declaration is not what ran: %s", printed)
	}
	// The version is the server's, assigned when something is declared. Printing
	// one would produce a file that claims a version it was never given.
	if strings.Contains(printed, `"version": 3`) {
		t.Errorf("the draft carried a version nobody declared: %s", printed)
	}

	// And with nothing looked at yet, there is nothing to declare.
	empty := typed(t, &looked{}, `declare`)
	if !strings.Contains(empty, "nothing has been looked at yet") {
		t.Errorf("declaring first said: %s", empty)
	}
}

// TestTheShellSaysWhatTheServerSaid: a refusal reaches the person typing,
// rather than being swallowed into an empty answer that looks like "there is
// nothing there".
func TestTheShellSaysWhatTheServerSaid(t *testing.T) {
	look := &looked{fail: &wire.ErrRefused{
		Code: "no_collection", Message: "rsql/store: there is no collection called \"boks\"",
	}}
	printed := typed(t, look, `scan boks`)
	if !strings.Contains(printed, "no collection") || !strings.Contains(printed, "no_collection") {
		t.Errorf("the refusal came out as: %s", printed)
	}
}

// TestLookingAroundBeforeKnowingTheNames: an operator who does not know what
// is here cannot type anything at all, so this is the first thing the shell
// has to answer.
func TestLookingAroundBeforeKnowingTheNames(t *testing.T) {
	look := &looked{here: store.Catalogue{
		Collections: []store.Spec{{
			Name: "books", Key: store.Key{Path: "id", Type: store.TypeString},
			Indexes: []store.Index{{Name: "by_shelf", Fields: []store.Field{
				{Path: "shelf"}, {Path: "at"},
			}}},
		}},
		Operations: []store.Operation{{
			Name: "books.add", Collection: "books", Action: store.ActionInsert, Version: 2,
		}},
	}}

	printed := typed(t, look, `ls`)
	for _, wanted := range []string{"collection books", "key id string", "index by_shelf", "shelf, at", "operation books.add v2 insert"} {
		if !strings.Contains(printed, wanted) {
			t.Errorf("ls did not mention %q:\n%s", wanted, printed)
		}
	}
}

// TestAShellThatCannotReachItsServerSaysSo covers the one thing the loop does
// with an error it cannot recover from.
func TestAShellThatCannotReachItsServerSaysSo(t *testing.T) {
	look := &looked{fail: errors.New("the connection went away")}
	printed := typed(t, look, `ls`)
	if !strings.Contains(printed, "the connection went away") {
		t.Errorf("a broken connection came out as: %s", printed)
	}
}
