package store

import (
	"fmt"
	"testing"
)

// This file used to be round three's front door: three shapes — a constant
// at one end only, the two ends disagreeing about Exclusive, and (round
// three's own Reviewer finding, never captured here) the two ends written at
// different widths — were refused because letting the caller pick the
// direction let a call reach rows no forward call of the same declaration
// could, once From and To swapped which one was the walk's upper end.
//
// Round four removed the swap instead of adding a fourth rule for a fourth
// shape. So every test that used to assert ErrDeclaration for one of these
// shapes now asserts the opposite: the shape declares, and forward and
// reverse read one stretch, backwards on the second call. Nothing here
// measures a refusal any more — reach_test.go's
// TestEveryShapeOfDeclarationDeclaresAndReadsOneStretchBothWays and
// direction_v4_test.go's TestTheTwoBoundsNeverDependOnDirection already cover
// the general case; what stays here is the concrete, front-door version of
// exactly the shapes three rounds of review argued about, so a reader who
// followed those rounds can see where each one landed.

// TestAnExclusiveEndAloneDeclaresAndReadsOneStretchBothWays is QA's original
// leak from round three, declared through the real front door. It used to be
// refused: a row at the floor of by_slug (id "", slug "") never came back on
// a forward call and came back on a single reverse one, because Exclusive
// was checked against From and To as if they were fixed roles rather than
// the low and high end reversing swapped. Neither end is fixed to a role by
// Direction any more, so there is nothing left to leak and nothing left to
// refuse: forward and reverse over the same From/To now read the same
// stretch, in opposite order.
func TestAnExclusiveEndAloneDeclaresAndReadsOneStretchBothWays(t *testing.T) {
	store, collection := declared(t, 138)
	fill(t, collection)

	declareOp(t, store, Operation{
		Name:       "articles.leaky",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_slug",
		Input: []Parameter{
			{Name: "lo", Type: TypeString, Required: true},
			{Name: "hi", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:       &Endpoint{Terms: []Term{{Arg: "lo"}}, Exclusive: true},
		To:         &Endpoint{Terms: []Term{{Arg: "hi"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"slug"},
		Limit:      50,
	})

	forward := invoke(t, store, "articles.leaky", map[string]any{
		"lo": "s1", "hi": "s4", "direction": DirectionForward,
	})
	if got := slugsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s2", "s3", "s4"}) {
		t.Errorf("forward, lo=s1 exclusive to hi=s4, gave %v", got)
	}

	reverse := invoke(t, store, "articles.leaky", map[string]any{
		"lo": "s1", "hi": "s4", "direction": DirectionReverse,
	})
	if got := slugsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s4", "s3", "s2"}) {
		t.Errorf("reverse over the same declared stretch gave %v", got)
	}
}

// TestATwoWayPagerWithBothEndsExclusiveDeclaresAndReadsBothWays is the shape
// round three's mục 1 said the tightened rule had to leave available: a
// keyset pager whose cursor and edge are both arguments and both exclusive,
// so nobody re-sees the row they last read, whichever way they are paging.
// It goes through DeclareOperation and Invoke — the real front door — rather
// than Collection.Scan.
//
// Round three needed an exception for this exact shape ("the same argument
// pair is one point, open on at most one side of itself") to keep it
// declarable beside a rule that refused mismatched Exclusive everywhere
// else. Round four needs no exception: reversing changes only the read
// order, so the SAME from/to values work for both calls.
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

	// The SAME from/to as the forward call — nothing to swap any more.
	reverse := invoke(t, store, "articles.page", map[string]any{
		"from": "s1", "to": "s4", "direction": DirectionReverse,
	})
	if got := slugsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s3", "s2"}) {
		t.Errorf("reverse over the same declared stretch gave %v", got)
	}
}

// TestByAuthorMissingPublishedReadsOneStretchBothWaysExclusiveIncluded is
// round three's mục 1 test #2, re-measured: the leak was never a trick of
// the empty string. by_author is (author ascending, published number,
// Descending, MissingLast). Descending flips the byte order of published and
// MissingLast then sorts a document with no published date after that
// flipped order — so within one author's group, the row missing the field
// sits at the very floor, byte for byte the same shape as "" at the floor of
// by_slug.
//
// Round three refused this shape outright because forward could never reach
// the floor row past an exclusive bound while reverse always could, once the
// two ends swapped roles. With nothing swapping, an exclusive bound at
// From — the low end, in both directions — excludes the floor row from BOTH
// calls, which is what "a direction widens nothing" was supposed to mean in
// the first place.
func TestByAuthorMissingPublishedReadsOneStretchBothWaysExclusiveIncluded(t *testing.T) {
	store, collection := declared(t, 140)
	fill(t, collection)

	// ann's fourth article never got a published date.
	put(t, collection, map[string]any{
		"id": "aX", "author": "ann", "title": "Article X", "slug": "sX",
	})

	declareOp(t, store, Operation{
		Name:       "articles.by_author_page",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_author",
		Input: []Parameter{
			{Name: "p", Type: TypeNumber, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:       &Endpoint{Terms: []Term{{Value: "ann"}, {Arg: "p"}}, Exclusive: true},
		To:         &Endpoint{Terms: []Term{{Value: "ann"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"id"},
		Limit:      50,
	})

	// fill() wrote published 0..5 for ann and bob; these straddle every real
	// value and go far past them on both sides. Whatever a caller passes,
	// the exclusive bound at the low end keeps the floor row (aX) out of
	// EVERY call, forward or reverse — no direction ever reaches it through
	// this declaration, which is the point.
	for _, p := range []float64{-1e9, -1, 0, 2.5, 5, 1e9} {
		forward := invoke(t, store, "articles.by_author_page", map[string]any{"p": p, "direction": DirectionForward})
		if got := idsOf(forward.Rows); contains(got, "aX") {
			t.Errorf("forward with p=%v reached the row missing published: %v", p, got)
		}
		reverse := invoke(t, store, "articles.by_author_page", map[string]any{"p": p, "direction": DirectionReverse})
		if got := idsOf(reverse.Rows); contains(got, "aX") {
			t.Errorf("reverse with p=%v reached the row missing published: %v", p, got)
		}
	}

	// A value that DOES reach rows: p=1e9 reaches everything with a real
	// published date, in both directions, backwards of each other.
	forward := invoke(t, store, "articles.by_author_page", map[string]any{"p": 1e9, "direction": DirectionForward})
	reverse := invoke(t, store, "articles.by_author_page", map[string]any{"p": 1e9, "direction": DirectionReverse})
	if got := idsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a5", "a4", "a2", "a1"}) {
		t.Errorf("forward with p=1e9 gave %v", got)
	}
	if got := idsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a1", "a2", "a4", "a5"}) {
		t.Errorf("reverse over the same declared stretch gave %v", got)
	}

	// The unbounded pin (no Exclusive at all) is where the floor row DOES
	// come back, consistently both ways — see
	// TestByAuthorFloorRowReadsOneStretchBothWays in direction_v4_test.go.
}

// contains is a small helper local to this file: whether id is among ids.
func contains(ids []string, id string) bool {
	for _, one := range ids {
		if one == id {
			return true
		}
	}
	return false
}
