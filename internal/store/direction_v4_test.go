package store

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// This file is round four's front door. Three rounds refused a growing list
// of shapes because From and To swapped which one was the upper end of the
// stretch depending on Direction — a constant at one end, the two ends
// disagreeing about Exclusive, the two ends written at different widths, and
// (found while writing this round) a pinned constant plus a small Limit.
// Round four removed the swap instead of refusing a fourth shape: From is the
// low end and To is the high end, in both directions, and stretch() proves it
// with byte comparisons rather than with a fixture and a domain of argument
// values. See scan.go's Range doc and ops.go's checkDirection for what that
// leaves behind.

// TestTheTwoBoundsNeverDependOnDirection is the direct proof every other test
// in this round leans on. stretch() computes the two byte positions a walk
// starts and stops at, and this calls it twice — Direction forward, then
// reversed, every other field of the Range held constant — and requires the
// SAME bytes back. Not "the same rows", which a bug shared between the
// fixture and the code under test could hide; the identical []byte, before a
// single key is ever looked up.
//
// This is also where composite bounds (more than one term per end) and a
// mismatch between From.Exclusive and To.Exclusive get their coverage.
// reachShapes (reach_test.go) writes exactly one term per end on purpose —
// its oracle only knows how to check a single-term bound against real rows —
// so the shapes that leaked in rounds two and three are added here directly,
// against stretch(), where no oracle is needed at all.
func TestTheTwoBoundsNeverDependOnDirection(t *testing.T) {
	_, collection := declared(t, 150)
	fill(t, collection)

	check := func(t *testing.T, what string, prefix []byte, fields []Field, from, to *Bound) {
		t.Helper()
		fl, fu, err := collection.stretch(Range{From: from, To: to, Direction: Forward}, prefix, fields)
		if err != nil {
			t.Fatalf("%s: forward: %v", what, err)
		}
		rl, ru, err := collection.stretch(Range{From: from, To: to, Direction: Reverse}, prefix, fields)
		if err != nil {
			t.Fatalf("%s: reverse: %v", what, err)
		}
		if !bytes.Equal(fl, rl) || !bytes.Equal(fu, ru) {
			t.Errorf("%s: forward stretch is (%x, %x), reverse stretch is (%x, %x) — a bound must not depend on Direction",
				what, fl, fu, rl, ru)
		}
	}

	// The 20 shapes reach_test.go already enumerates, one term per end, over
	// both indexes and the same domains it uses. Repeating them here at the
	// byte level rather than the row level is deliberate: it is the same
	// shapes, checked a second, independent way.
	for _, over := range []struct {
		index  string
		pin    string
		domain []string
	}{
		{index: "by_slug", pin: "s3", domain: []string{"", "s0", "s1", "s2", "s3", "s3x", "s4", "s5", "s6", "t"}},
		{index: ClusteredIndex, pin: "a3", domain: []string{"", "a0", "a1", "a2", "a3", "a3x", "a4", "a5", "a6", "b"}},
	} {
		t.Run(over.index, func(t *testing.T) {
			var prefix []byte
			var fields []Field
			if over.index == ClusteredIndex {
				prefix = collection.documents()
				fields = []Field{{Path: collection.spec.Key.Path, Type: collection.spec.Key.Type, Missing: MissingSkip}}
			} else {
				index, found := collection.index(over.index)
				if !found {
					t.Fatalf("%q is not declared", over.index)
				}
				prefix = collection.entries(*index)
				fields = index.Fields
			}

			shapes := reachShapes(over.pin)
			for _, shape := range shapes {
				for _, low := range over.domain {
					for _, high := range over.domain {
						check(t, shape.what, prefix, fields, shape.from.bound(low), shape.to.bound(high))
					}
				}
			}
		})
	}

	// Composite: by_author is two fields (author, published). Widths and
	// Exclusive mismatched between the two ends on purpose — this is exactly
	// what round three's Reviewer found leaking (a two-term From against a
	// one-term To) and what round two found leaking (Exclusive set on one end
	// and not the other) — neither shape can be written with reachShapes,
	// which writes one term per end.
	author, found := collection.index("by_author")
	if !found {
		t.Fatal("by_author is not declared")
	}
	prefix := collection.entries(*author)
	fields := author.Fields

	for _, p := range []float64{-1e9, -1, 0, 1, 2.5, 5, 1e9} {
		check(t, "composite, From two terms / To one term", prefix, fields,
			&Bound{Values: []any{"ann", p}}, &Bound{Values: []any{"ann"}})
		check(t, "composite, From two terms exclusive / To one term", prefix, fields,
			&Bound{Values: []any{"ann", p}, Exclusive: true}, &Bound{Values: []any{"ann"}})
		check(t, "composite, From one term / To two terms exclusive", prefix, fields,
			&Bound{Values: []any{"ann"}}, &Bound{Values: []any{"ann", p}, Exclusive: true})
		check(t, "composite, both ends two terms, From exclusive only", prefix, fields,
			&Bound{Values: []any{"ann", p}, Exclusive: true}, &Bound{Values: []any{"bob", p}})
		check(t, "composite, both ends two terms, both exclusive", prefix, fields,
			&Bound{Values: []any{"ann", p}, Exclusive: true}, &Bound{Values: []any{"bob", p}, Exclusive: true})
	}
}

// TestALimitCanHandBackDifferentRowsFromEitherEndOfAPinnedStretch is the
// counter-example that ruled out patching the swap a fourth time (round 4,
// mục 1). TestAPinnedConstantSurvivesBeingReadFromEitherEnd (direction_test.go)
// established that a constant pinned identically at both ends is read as one
// set of rows both ways — with a Limit large enough to see the whole set.
// Shrink the Limit and that stops being true, and no shape-based rule in
// checkDirection could ever have caught it: Limit is not part of From or To.
//
// This is not a leak. "The ten most recent rows" and "the ten oldest rows"
// being different sets is what a caller-chosen direction under a limit is
// FOR — the test exists so that the boundary between that and an actual
// widening is written down as rows rather than as an assertion nobody
// checked.
func TestALimitCanHandBackDifferentRowsFromEitherEndOfAPinnedStretch(t *testing.T) {
	store, collection := declared(t, 151)
	fill(t, collection)

	pinned := Operation{
		Name:       "articles.anns_page",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_author",
		Input:      []Parameter{{Name: "direction", Type: TypeString, Required: true}},
		From:       &Endpoint{Terms: []Term{{Value: "ann"}}},
		To:         &Endpoint{Terms: []Term{{Value: "ann"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"id"},
		Limit:      2,
	}
	declareOp(t, store, pinned)

	// by_author is (author ascending, published descending); ann's rows in
	// declared order are a5, a4, a2, a1.
	forward := invoke(t, store, "articles.anns_page", map[string]any{"direction": DirectionForward})
	if got := idsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a5", "a4"}) {
		t.Errorf("forward over the pinned author, limit 2, gave %v", got)
	}
	if !forward.Truncated {
		t.Error("forward stopped at its limit and did not say so")
	}

	reverse := invoke(t, store, "articles.anns_page", map[string]any{"direction": DirectionReverse})
	if got := idsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a1", "a2"}) {
		t.Errorf("reverse over the pinned author, limit 2, gave %v", got)
	}
	if !reverse.Truncated {
		t.Error("reverse stopped at its limit and did not say so")
	}

	// The two answers do not share a row. No argument this declaration takes
	// could turn one into the other — there is no argument at all — and no
	// rule about From and To could ever have refused this, because the
	// difference comes entirely from Limit.
	for _, id := range idsOf(forward.Rows) {
		for _, other := range idsOf(reverse.Rows) {
			if id == other {
				t.Errorf("forward and reverse share %q, and this test is supposed to show they need not", id)
			}
		}
	}
}

// TestByAuthorFloorRowReadsOneStretchBothWays is round three's Reviewer
// finding, re-measured against round four's fix. by_author's published field
// is Descending with MissingLast, so a document with no published date sorts
// at the very floor of its author's group — the same shape of trap as the
// empty string at the floor of a string field, but reached through a
// composite bound rather than a lone one.
//
// Pinning the whole group at both ends (no Exclusive, no argument at all) is
// the shape that reads correctly today: the floor row comes back on both
// forward and reverse, in exactly reversed order, which is what "a direction
// widens nothing" was always supposed to mean.
func TestByAuthorFloorRowReadsOneStretchBothWays(t *testing.T) {
	_, collection := declared(t, 152)
	fill(t, collection)
	put(t, collection, map[string]any{
		"id": "aX", "author": "ann", "title": "Article X", "slug": "sX",
	})

	forward := keysOf(scan(t, collection, "by_author", Range{
		From: &Bound{Values: []any{"ann"}},
		To:   &Bound{Values: []any{"ann"}},
	}))
	want := []string{"aX", "a5", "a4", "a2", "a1"}
	if fmt.Sprint(forward) != fmt.Sprint(want) {
		t.Fatalf("forward over the whole ann group gave %v, want %v", forward, want)
	}

	reverse := keysOf(scan(t, collection, "by_author", Range{
		From:      &Bound{Values: []any{"ann"}},
		To:        &Bound{Values: []any{"ann"}},
		Direction: Reverse,
	}))
	for i := range want {
		if reverse[i] != want[len(want)-1-i] {
			t.Fatalf("reverse over the whole ann group gave %v, want %v backwards", reverse, want)
		}
	}
}

// TestAnArrayConstantAtAScanBoundIsRefusedRatherThanPanicking is round three's
// Reviewer finding: an index field may be declared "any" (matches(TypeAny, x)
// accepts everything, including a slice or a map read off a JSON body), and
// scanAcross used to compare such a constant with == against the matching
// term at the other end — which panics rather than returning false when the
// two values are not comparable, on a partitioned collection or not.
//
// The fix is at the point the constant is written down: keys.Encode already
// refuses a slice or a map with ErrNotIndexable, so a constant this hostile
// to every future call of the operation is refused now, before the operation
// is ever stored — not discovered the first time somebody runs it.
func TestAnArrayConstantAtAScanBoundIsRefusedRatherThanPanicking(t *testing.T) {
	_, store := fresh(t, 153)
	widgets, err := store.Declare(Spec{
		Name: "widgets",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{{
			Name:   "by_tags",
			Fields: []Field{{Path: "tags", Type: TypeAny, Missing: MissingSkip}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	array := Operation{
		Name:       "widgets.by_array",
		Collection: "widgets",
		Action:     ActionScan,
		Index:      "by_tags",
		From:       &Endpoint{Terms: []Term{{Value: []any{"a", "b"}}}},
		To:         &Endpoint{Terms: []Term{{Value: []any{"a", "b"}}}},
		Limit:      10,
	}
	if _, err := store.DeclareOperation(array); !errors.Is(err, ErrDeclaration) {
		t.Errorf("an array constant at a scan bound (both ends equal): want ErrDeclaration, got %v", err)
	}

	object := array
	object.Name = "widgets.by_object"
	object.From = &Endpoint{Terms: []Term{{Value: map[string]any{"k": "v"}}}}
	object.To = &Endpoint{Terms: []Term{{Value: map[string]any{"k": "v"}}}}
	if _, err := store.DeclareOperation(object); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a map constant at a scan bound: want ErrDeclaration, got %v", err)
	}

	// NOT refused when the constant is nil: nil encodes fine, unlike a slice
	// or a map, so this checks the refusal above is about what Encode does
	// with the value, not about the field being declared "any".
	null := array
	null.Name = "widgets.by_null"
	null.From = &Endpoint{Terms: []Term{{Value: nil, Constant: true}}}
	null.To = &Endpoint{Terms: []Term{{Value: nil, Constant: true}}}
	declareOp(t, store, null) // null encodes fine; this is the shape that must NOT be refused.

	// A scalar constant on the same "any" field still declares and reads: it
	// is the value that fails to encode, not the field's declared type.
	if _, err := widgets.Put(map[string]any{"id": "w1", "tags": "solo"}); err != nil {
		t.Fatal(err)
	}
	scalar := array
	scalar.Name = "widgets.by_scalar"
	scalar.From = &Endpoint{Terms: []Term{{Value: "solo"}}}
	scalar.To = &Endpoint{Terms: []Term{{Value: "solo"}}}
	declareOp(t, store, scalar)
}

// TestSameTermComparesUncomparableValuesWithoutPanicking is a direct test of
// the helper that replaced == in scanAcross and totalsAcross. Term.Value is
// any, so a naive == on two Terms panics the moment either Value holds a
// slice or a map — this checks the replacement does not, and that it still
// says equal and not-equal in the right places rather than trading a panic
// for a comparison that always agrees or never does.
func TestSameTermComparesUncomparableValuesWithoutPanicking(t *testing.T) {
	slice1 := Term{Value: []any{"x", 1.0}}
	slice2 := Term{Value: []any{"x", 1.0}}
	slice3 := Term{Value: []any{"x", 2.0}}
	if !sameTerm(slice1, slice2) {
		t.Error("two terms holding an equal slice should compare equal")
	}
	if sameTerm(slice1, slice3) {
		t.Error("two terms holding different slices should not compare equal")
	}

	map1 := Term{Value: map[string]any{"k": 1.0}}
	map2 := Term{Value: map[string]any{"k": 1.0}}
	if !sameTerm(map1, map2) {
		t.Error("two terms holding an equal map should compare equal")
	}

	if !sameTerm(Term{Arg: "x"}, Term{Arg: "x"}) {
		t.Error("two argument terms naming the same argument should compare equal")
	}
	if sameTerm(Term{Arg: "x"}, Term{Arg: "y"}) {
		t.Error("two argument terms naming different arguments should not compare equal")
	}
	if sameTerm(Term{Value: "x"}, Term{Arg: "x"}) {
		t.Error("a constant and an argument of the same name are not the same term")
	}
}

// TestScanAcrossTellsTwoConstantsApart and TestTotalsAcrossTellsTwoConstantsApart
// exercise sameTerm through the real declaration path rather than calling it
// directly, on the two functions round three's Reviewer named. Both constants
// here are ordinary comparable strings — == would not have panicked on these,
// and no path in the repo reaches either function with a constant that would
// — so what these two check is that sameTerm still tells them apart
// correctly, which a mutation making it always report "equal" would break.
func TestScanAcrossTellsTwoConstantsApart(t *testing.T) {
	_, _, store := partitioned(t, 154)
	entriesByMonth(t, store, 0)

	differing := Operation{
		Name:       "entries.differing",
		Collection: "entries",
		Action:     ActionScan,
		Index:      "by_account",
		From:       &Endpoint{Terms: []Term{{Value: "cash"}}},
		To:         &Endpoint{Terms: []Term{{Value: "zinc"}}},
		Limit:      10,
	}
	if _, err := store.DeclareOperation(differing); !errors.Is(err, ErrDeclaration) {
		t.Errorf("two different constants ranging over a partitioned index: want ErrDeclaration, got %v", err)
	}
}

func TestTotalsAcrossTellsTwoConstantsApart(t *testing.T) {
	_, _, store := partitioned(t, 155)

	lines, err := store.Declare(Spec{
		Name:      "lines",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = lines

	differing := Operation{
		Name: "lines.differing", Collection: "lines", Action: ActionTotals, Rollup: "per_account", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "cash"}}},
		To:   &Endpoint{Terms: []Term{{Value: "zinc"}}},
	}
	if _, err := store.DeclareOperation(differing); !errors.Is(err, ErrDeclaration) {
		t.Errorf("two different constant groups ranging over a partitioned rollup: want ErrDeclaration, got %v", err)
	}
}
