package store

import (
	"errors"
	"fmt"
	"testing"
)

// This file is round 3's front door: reach_test.go measures the shapes at the
// Collection, below DeclareOperation, on purpose (half of them are refused
// and still have to be measured). These tests go through DeclareOperation and
// Invoke instead, because a bug at the boundary between the two — a check
// that only ever ran in one of them — is exactly the kind of thing a
// Collection-only measurement cannot see.

// TestAnExclusiveEndAloneIsRefusedAtDeclarationNotJustMeasured is QA's
// original leak, declared through the real front door rather than measured
// directly. Round 2 shipped "accepted iff the two reachable sets are equal"
// without a test that ever declared this shape; QA measured it by hand and it
// leaks — a row at the floor of by_slug (id "", slug "") never comes back on
// a forward call and comes back on a single reverse one.
func TestAnExclusiveEndAloneIsRefusedAtDeclarationNotJustMeasured(t *testing.T) {
	store, _ := declared(t, 138)

	_, err := store.DeclareOperation(Operation{
		Name:       "articles.leaky",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_slug",
		Input: []Parameter{
			{Name: "lo", Type: TypeString, Required: true},
			{Name: "hi", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:      &Endpoint{Terms: []Term{{Arg: "lo"}}, Exclusive: true},
		To:        &Endpoint{Terms: []Term{{Arg: "hi"}}},
		Direction: &Term{Arg: "direction"},
		Limit:     50,
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("QA's leak (exclusive From only, direction an argument): want ErrDeclaration, got %v", err)
	}
}

// TestExclusivityMismatchRefusesOnlyTheOneLeakingShape hunts for the mistake
// the house rules ask every "not allowed to..." to be tested against: a rule
// that refuses more than it means to. Every shape below is one the new
// mệnh đề 2 must NOT touch — it stays declarable — so if boundsSurviveReversal
// over-reaches, this is where it shows up rather than in a QA round.
func TestExclusivityMismatchRefusesOnlyTheOneLeakingShape(t *testing.T) {
	store, _ := declared(t, 139)

	accept := func(name string, from, to *Endpoint) {
		t.Helper()
		_, err := store.DeclareOperation(Operation{
			Name:       name,
			Collection: "articles",
			Action:     ActionScan,
			Index:      "by_slug",
			Input: []Parameter{
				{Name: "lo", Type: TypeString, Required: true},
				{Name: "hi", Type: TypeString, Required: true},
				{Name: "direction", Type: TypeString, Required: true},
			},
			From:      from,
			To:        to,
			Direction: &Term{Arg: "direction"},
			Limit:     50,
		})
		if err != nil {
			t.Errorf("%s: want accepted, got %v", name, err)
		}
	}

	accept("both ends arguments, both exclusive",
		&Endpoint{Terms: []Term{{Arg: "lo"}}, Exclusive: true},
		&Endpoint{Terms: []Term{{Arg: "hi"}}, Exclusive: true})
	accept("both ends arguments, both inclusive",
		&Endpoint{Terms: []Term{{Arg: "lo"}}},
		&Endpoint{Terms: []Term{{Arg: "hi"}}})
	accept("an argument inclusive at From, To left unwritten",
		&Endpoint{Terms: []Term{{Arg: "lo"}}},
		nil)
	// The exception: two ends written as the very same terms are one point
	// (or one prefix), open on at most one side of itself, which is empty
	// from either direction regardless of which side that is. Both the
	// argument-valued and the constant-valued version of this have to survive
	// — an argument pair is what a keyset pager's cursor looks like.
	accept("the same argument at both ends, mismatched exclusivity",
		&Endpoint{Terms: []Term{{Arg: "lo"}}, Exclusive: true},
		&Endpoint{Terms: []Term{{Arg: "lo"}}})
	accept("the same constant at both ends, mismatched exclusivity",
		&Endpoint{Terms: []Term{{Value: "s3"}}, Exclusive: true},
		&Endpoint{Terms: []Term{{Value: "s3"}}})
	accept("the same constant at both ends, both inclusive",
		&Endpoint{Terms: []Term{{Value: "s3"}}},
		&Endpoint{Terms: []Term{{Value: "s3"}}})
}

// TestATwoWayPagerWithBothEndsExclusiveDeclaresAndReadsBothWays is the shape
// mục 1 says the tightened rule leaves fully available: a keyset pager whose
// cursor and edge are both arguments and both exclusive (nobody re-sees the
// row they last read, whichever way they are paging). It goes through
// DeclareOperation and Invoke — the real front door — rather than
// Collection.Scan, since that is the whole of what a caller of this store
// actually has.
func TestATwoWayPagerWithBothEndsExclusiveDeclaresAndReadsBothWays(t *testing.T) {
	store, collection := declared(t, 137)
	fill(t, collection)

	declareOp(t, store, Operation{
		Name:       "articles.page",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_slug",
		Input: []Parameter{
			{Name: "from", Type: TypeString, Required: true},
			{Name: "to", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:       &Endpoint{Terms: []Term{{Arg: "from"}}, Exclusive: true},
		To:         &Endpoint{Terms: []Term{{Arg: "to"}}, Exclusive: true},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"slug"},
		Limit:      10,
	})

	forward := invoke(t, store, "articles.page", map[string]any{
		"from": "s1", "to": "s4", "direction": DirectionForward,
	})
	if got := slugsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s2", "s3"}) {
		t.Errorf("forward from s1 exclusive to s4 exclusive gave %v", got)
	}

	reverse := invoke(t, store, "articles.page", map[string]any{
		"from": "s4", "to": "s1", "direction": DirectionReverse,
	})
	if got := slugsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s3", "s2"}) {
		t.Errorf("reverse from s4 exclusive to s1 exclusive gave %v", got)
	}
}

// TestByAuthorMissingPublishedSitsAtTheFloorOfItsAuthorsGroup is mục 1's test
// #2: the leak in mệnh đề 2 is not a trick of the empty string. by_author is
// (author ascending, published number, Descending, MissingLast). Descending
// flips the byte order of published and MissingLast then sorts a document
// with no published date after that flipped order — so within one author's
// group, the row missing the field sits at the very floor, byte for byte the
// same shape as "" at the floor of by_slug. This is measured directly through
// Collection.Scan, over every published value a caller could plausibly pass,
// because the declaration this measures is refused and Invoke can never
// reach a shape that was never declared.
func TestByAuthorMissingPublishedSitsAtTheFloorOfItsAuthorsGroup(t *testing.T) {
	store, collection := declared(t, 140)
	fill(t, collection)

	// ann's fourth article never got a published date.
	put(t, collection, map[string]any{
		"id": "aX", "author": "ann", "title": "Article X", "slug": "sX",
	})

	reachesRow := func(p float64, direction Direction) bool {
		t.Helper()
		within := Range{
			From:      &Bound{Values: []any{"ann", p}, Exclusive: true},
			To:        &Bound{Values: []any{"ann"}},
			Direction: direction,
		}
		found := false
		if err := collection.Scan("by_author", within, func(one Found) bool {
			if fmt.Sprint(one.Key) == "aX" {
				found = true
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return found
	}

	// fill() wrote published 0..5 for ann and bob; these straddle every real
	// value and go far past them on both sides. Whatever a caller passes,
	// forward never reaches the row missing published and reverse always does
	// — in one call, with no need to try more than one direction's worth of
	// values.
	for _, p := range []float64{-1e9, -1, 0, 2.5, 5, 1e9} {
		if reachesRow(p, Forward) {
			t.Errorf("forward with the exclusive end at (ann, %v) reached the row missing published", p)
		}
		if !reachesRow(p, Reverse) {
			t.Errorf("reverse with the exclusive end at (ann, %v) did not reach the row missing published", p)
		}
	}

	_, err := store.DeclareOperation(Operation{
		Name:       "articles.by_author_page",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_author",
		Input: []Parameter{
			{Name: "p", Type: TypeNumber, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:      &Endpoint{Terms: []Term{{Value: "ann"}, {Arg: "p"}}, Exclusive: true},
		To:        &Endpoint{Terms: []Term{{Value: "ann"}}},
		Direction: &Term{Arg: "direction"},
		Limit:     50,
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Errorf("the shape just measured to leak: want ErrDeclaration, got %v", err)
	}
}
