package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// slugsOf is the slug of every row a scan handed back, in the order it did.
func slugsOf(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = fmt.Sprint(row["slug"])
	}
	return out
}

// eitherWay reads a stretch of the slug index, at most three rows, in whichever
// direction the caller asks for. One declaration, two orders — which is the
// whole point of the direction being an argument rather than a second
// operation with a second index behind it.
func eitherWay() Operation {
	return Operation{
		Name:       "articles.slugs",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_slug",
		Input: []Parameter{
			{Name: "from", Type: TypeString, Required: true},
			{Name: "to", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Default: DirectionForward},
		},
		From:       &Endpoint{Terms: []Term{{Arg: "from"}}},
		To:         &Endpoint{Terms: []Term{{Arg: "to"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"id", "slug"},
		Limit:      3,
	}
}

// TestAReversedScanIsTheSameEntriesBackwards: the two walks are over one index,
// so they must see one set of entries. Anything else means the reverse walk is
// reaching somewhere the forward one does not, or missing something it does —
// and the entry at either end is where that would show up first.
func TestAReversedScanIsTheSameEntriesBackwards(t *testing.T) {
	_, collection := declared(t, 120)
	fill(t, collection)

	forward := keysOf(scan(t, collection, "by_slug", Range{}))
	reverse := keysOf(scan(t, collection, "by_slug", Range{Direction: Reverse}))

	if len(forward) != 6 {
		t.Fatalf("the forward scan saw %d entries: %v", len(forward), forward)
	}
	if len(reverse) != len(forward) {
		t.Fatalf("forward saw %v and reverse saw %v", forward, reverse)
	}
	for i := range forward {
		if forward[i] != reverse[len(reverse)-1-i] {
			t.Fatalf("forward %v is not reverse %v backwards", forward, reverse)
		}
	}
}

// TestReversingAnIndexFlipsTheWholeDeclaredOrderNotEachField: by_author is
// declared (author ascending, published descending). Read in reverse it is
// (author descending, published ascending) — the declared order, read back to
// front.
//
// It is NOT (author descending, published descending). That is a third order,
// and whoever wants it still needs an index of their own. The two are written
// out here in full rather than compared against the implementation, because a
// scan compared only with itself agrees with itself whichever of the two it is
// doing.
func TestReversingAnIndexFlipsTheWholeDeclaredOrderNotEachField(t *testing.T) {
	_, collection := declared(t, 121)
	fill(t, collection)

	// fill writes a0..a5 with published 0..5, where a0 and a3 are bob's and
	// the rest are ann's.
	forward := keysOf(scan(t, collection, "by_author", Range{}))
	want := []string{"a5", "a4", "a2", "a1", "a3", "a0"} // ann newest first, then bob
	if fmt.Sprint(forward) != fmt.Sprint(want) {
		t.Fatalf("the declared order is %v, want %v", forward, want)
	}

	reverse := keysOf(scan(t, collection, "by_author", Range{Direction: Reverse}))

	// bob before ann (author descending), and within each author oldest first
	// (published ascending, the opposite of how the field was declared).
	wantReversed := []string{"a0", "a3", "a1", "a2", "a4", "a5"}
	if fmt.Sprint(reverse) != fmt.Sprint(wantReversed) {
		t.Errorf("reversed, the index reads %v, want %v", reverse, wantReversed)
	}

	// The order a per-field flip would have produced, spelled out so that the
	// mistake is named rather than merely absent.
	eachFieldFlipped := []string{"a3", "a0", "a5", "a4", "a2", "a1"}
	if fmt.Sprint(reverse) == fmt.Sprint(eachFieldFlipped) {
		t.Errorf("reversing flipped each field instead of the declared order: %v", reverse)
	}
}

// TestReverseReadsTheSameStretchBackwardsNotADifferentOne: From is always the
// low end of a stretch and To is always the high end, in both directions —
// Direction changes only the order rows come back in, never which rows they
// are. A caller who wants the read reversed changes Direction and nothing
// else; From and To are not rewritten to swap which one is "the far end".
//
// This replaces TestTheEndsOfARangeSwapWhenTheScanIsReversed, which asserted
// the opposite: that reversing required From and To to trade places. Three
// rounds of review each found a different declaration where that swap let a
// caller-chosen direction reach rows no forward call of the same declaration
// could, so round four removed the swap rather than refuse a fourth shape.
// See scan.go's Range doc and task 0012, round 4, mục 1.
func TestReverseReadsTheSameStretchBackwardsNotADifferentOne(t *testing.T) {
	_, collection := declared(t, 122)
	fill(t, collection)

	forward := keysOf(scan(t, collection, "by_slug", Range{
		From: &Bound{Values: []any{"s1"}},
		To:   &Bound{Values: []any{"s4"}},
	}))
	if fmt.Sprint(forward) != fmt.Sprint([]string{"a1", "a2", "a3", "a4"}) {
		t.Fatalf("forward from s1 to s4 gave %v", forward)
	}

	// The SAME declaration — From is still s1, To is still s4 — read the
	// other way. Nothing about the bounds changed, only the order.
	reverse := keysOf(scan(t, collection, "by_slug", Range{
		From:      &Bound{Values: []any{"s1"}},
		To:        &Bound{Values: []any{"s4"}},
		Direction: Reverse,
	}))
	if fmt.Sprint(reverse) != fmt.Sprint([]string{"a4", "a3", "a2", "a1"}) {
		t.Errorf("reverse over the same declared stretch gave %v", reverse)
	}

	// Both ends excluded: exclusivity does not depend on direction either.
	trimmed := keysOf(scan(t, collection, "by_slug", Range{
		From:      &Bound{Values: []any{"s1"}, Exclusive: true},
		To:        &Bound{Values: []any{"s4"}, Exclusive: true},
		Direction: Reverse,
	}))
	if fmt.Sprint(trimmed) != fmt.Sprint([]string{"a3", "a2"}) {
		t.Errorf("reverse with both ends exclusive gave %v", trimmed)
	}

	// Writing the ends the OLD way round — the convention a swapped-ends
	// design once required for a reversed read, high value in From, low
	// value in To — used to read as empty, whichever direction was asked
	// for: From is unconditionally the low end, and s4 sorts after s1, so the
	// walk correctly found no rows between a low end that sorts after the
	// high end — a result nothing inside the store could tell apart from a
	// stretch that is legitimately empty. Task 0045 changed this from a
	// silent empty answer into ErrArgument, in both directions: this is the
	// declaration-can-never-match-anything shape, not the merely-empty-today
	// one (that shape is `lower == upper`, and it still runs — see
	// TestAPinnedEmptyRangeStillRuns in direction_0045_test.go).
	//
	// The mutation this test used to close ("bring the swap back") is now
	// closed a level down, in stretch() itself (scan.go) and in
	// TestTheTwoBoundsNeverDependOnDirection (direction_v4_test.go) — this
	// test's job narrows to confirming Scan (the caller-facing entry point,
	// not stretch() directly) surfaces that refusal rather than swallowing
	// it, in both directions.
	if err := collection.Scan("by_slug", Range{
		From:      &Bound{Values: []any{"s4"}},
		To:        &Bound{Values: []any{"s1"}},
		Direction: Reverse,
	}, func(Found) bool { return true }); !errors.Is(err, ErrArgument) {
		t.Errorf("From above To, reverse: want ErrArgument, got %v", err)
	}
	if err := collection.Scan("by_slug", Range{
		From: &Bound{Values: []any{"s4"}},
		To:   &Bound{Values: []any{"s1"}},
	}, func(Found) bool { return true }); !errors.Is(err, ErrArgument) {
		t.Errorf("From above To, forward: want ErrArgument, got %v", err)
	}
}

// TestReversingTheClusteredWalkGivesTheKeysBackwards: the primary key is an
// index like any other, the clustered one, so it takes a direction too.
func TestReversingTheClusteredWalkGivesTheKeysBackwards(t *testing.T) {
	_, collection := declared(t, 123)
	fill(t, collection)

	var forward, reverse []string
	if err := collection.Walk(func(key any, _ map[string]any) bool {
		forward = append(forward, fmt.Sprint(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := collection.walkRange(Range{Direction: Reverse}, func(key any, _ map[string]any) bool {
		reverse = append(reverse, fmt.Sprint(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}

	if fmt.Sprint(forward) != fmt.Sprint([]string{"a0", "a1", "a2", "a3", "a4", "a5"}) {
		t.Fatalf("the documents walk in order %v", forward)
	}
	if fmt.Sprint(reverse) != fmt.Sprint([]string{"a5", "a4", "a3", "a2", "a1", "a0"}) {
		t.Errorf("reversed, the documents walk in order %v", reverse)
	}

	// On the clustered index a bound can land exactly on a stored key, because
	// the key of a document is the whole of its index entry and nothing is
	// appended after it. So this is the one walk where the inclusive end being
	// off by one entry is visible, and a1 is the entry it would lose. From is
	// still the low end (a1) and To the high end (a4) even though the walk is
	// reversed — it is the same declaration TestReverseReadsTheSameStretch...
	// checks on by_slug, checked again here on the clustered index.
	var bounded []string
	if err := collection.walkRange(Range{
		From:      &Bound{Values: []any{"a1"}},
		To:        &Bound{Values: []any{"a4"}},
		Direction: Reverse,
	}, func(key any, _ map[string]any) bool {
		bounded = append(bounded, fmt.Sprint(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(bounded) != fmt.Sprint([]string{"a4", "a3", "a2", "a1"}) {
		t.Errorf("reversed over a1..a4 gave %v", bounded)
	}
}

// TestALimitStopsAScanFromEitherEnd: the limit is the declaration of what the
// operation costs, and the direction is not a way out of it. Three rows and a
// truthful Truncated both ways.
func TestALimitStopsAScanFromEitherEnd(t *testing.T) {
	store, collection := declared(t, 124)
	fill(t, collection)
	declareOp(t, store, eitherWay())

	forward := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s0", "to": "s5", "direction": DirectionForward,
	})
	if got := slugsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s0", "s1", "s2"}) {
		t.Errorf("forward gave %v", got)
	}
	if !forward.Truncated {
		t.Error("the forward scan stopped at its limit and did not say so")
	}

	reverse := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s0", "to": "s5", "direction": DirectionReverse,
	})
	if got := slugsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s5", "s4", "s3"}) {
		t.Errorf("reverse gave %v", got)
	}
	if !reverse.Truncated {
		t.Error("the reversed scan stopped at its limit and did not say so")
	}

	// A stretch that fits says so, both ways round. From is still the low
	// end and To still the high end for the reverse call — nothing about
	// the bounds changes, only the read order.
	short := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s1", "to": "s3", "direction": DirectionForward,
	})
	if short.Count != 3 || short.Truncated {
		t.Errorf("forward over three rows: %d rows, truncated=%v", short.Count, short.Truncated)
	}
	short = invoke(t, store, "articles.slugs", map[string]any{
		"from": "s1", "to": "s3", "direction": DirectionReverse,
	})
	if short.Count != 3 || short.Truncated {
		t.Errorf("reverse over three rows: %d rows, truncated=%v", short.Count, short.Truncated)
	}
}

// TestADirectionReachesNothingTheForwardScanCouldNot: a direction is allowed
// to be an argument only because it widens nothing — same index, same two
// declared ends, same limit — and an argument that widened a scan would be
// the one thing declared operations exist to make impossible.
//
// Rounds one through three called this "the constraint the whole design
// rests on", and it was true only of this one shape — both ends arguments —
// which is exactly the shape where the swap those rounds kept could never go
// wrong. Round four makes the same claim of every shape a bound can be
// written in, not by widening this test but by removing what made it narrow:
// see TestTheTwoBoundsNeverDependOnDirection (direction_v4_test.go) for the
// general proof, and reach_test.go for the row-level version of it. This test
// stays as the smallest, most direct case.
func TestADirectionReachesNothingTheForwardScanCouldNot(t *testing.T) {
	store, collection := declared(t, 125)
	fill(t, collection)
	declareOp(t, store, eitherWay())

	forward := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s1", "to": "s3", "direction": DirectionForward,
	})
	reverse := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s1", "to": "s3", "direction": DirectionReverse,
	})

	seen := map[string]int{}
	for _, row := range forward.Rows {
		seen[fmt.Sprint(row["slug"])]++
	}
	for _, row := range reverse.Rows {
		seen[fmt.Sprint(row["slug"])]++
	}
	for _, slug := range []string{"s1", "s2", "s3"} {
		if seen[slug] != 2 {
			t.Errorf("%q was seen %d times over the two directions, want 2", slug, seen[slug])
		}
	}
	for _, slug := range []string{"s0", "s4", "s5"} {
		if seen[slug] != 0 {
			t.Errorf("%q is outside the declared stretch and was reached %d times", slug, seen[slug])
		}
	}

	// The index is the declared one too: reversing does not reach the other
	// index, and the rows still carry only what the projection names.
	for _, row := range reverse.Rows {
		if len(row) != 2 || row["id"] == nil {
			t.Errorf("a reversed row came back as %v", row)
		}
	}
}

// TestADirectionCanBeFixedInTheDeclaration: the same field takes a constant, so
// an operation that is only ever "newest first" says so once and its callers
// pass nothing. One mechanism, both ways of deciding.
func TestADirectionCanBeFixedInTheDeclaration(t *testing.T) {
	store, collection := declared(t, 126)
	fill(t, collection)

	fixed := eitherWay()
	fixed.Name = "articles.newest"
	fixed.Input = []Parameter{
		{Name: "from", Type: TypeString, Required: true},
		{Name: "to", Type: TypeString, Required: true},
	}
	fixed.Direction = &Term{Value: DirectionReverse}
	declareOp(t, store, fixed)

	// From is still the low end and To the high end even though the
	// declaration always reads backwards.
	result := invoke(t, store, "articles.newest", map[string]any{"from": "s0", "to": "s5"})
	if got := slugsOf(result.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s5", "s4", "s3"}) {
		t.Errorf("a declaration fixed at reverse gave %v", got)
	}

	// And a caller cannot pass one, because the operation does not take one.
	if _, err := store.Invoke(Caller{}, "articles.newest", 0, map[string]any{
		"from": "s0", "to": "s5", "direction": DirectionForward,
	}); !errors.Is(err, ErrArgument) {
		t.Errorf("want ErrArgument, got %v", err)
	}
}

// TestADirectionThatCannotBeWorkedOutIsRefusedWhereItIsWritten: every one of
// these would otherwise turn into rows in an order nobody asked for, found by
// whoever was reading them rather than by whoever wrote the declaration.
func TestADirectionThatCannotBeWorkedOutIsRefusedWhereItIsWritten(t *testing.T) {
	store, collection := declared(t, 127)
	fill(t, collection)

	for _, refused := range []struct {
		why   string
		build func(Operation) Operation
	}{
		{
			// "descending" is what a field is. A walk is reversed, and the two
			// mean different orders on a composite index, so the word that
			// invites the confusion is not accepted.
			why: "a constant that is not one of the two words",
			build: func(o Operation) Operation {
				o.Direction = &Term{Value: "descending"}
				return o
			},
		},
		{
			why: "an argument that was never declared",
			build: func(o Operation) Operation {
				o.Direction = &Term{Arg: "whichever"}
				return o
			},
		},
		{
			why: "an argument that is not a string",
			build: func(o Operation) Operation {
				o.Input = append(o.Input, Parameter{Name: "backwards", Type: TypeNumber, Required: true})
				o.Direction = &Term{Arg: "backwards"}
				return o
			},
		},
		{
			why: "an optional argument with no default, which is two orders in one name",
			build: func(o Operation) Operation {
				o.Input = []Parameter{
					{Name: "from", Type: TypeString, Required: true},
					{Name: "to", Type: TypeString, Required: true},
					{Name: "direction", Type: TypeString},
				}
				return o
			},
		},
		{
			why: "a default that is not a direction",
			build: func(o Operation) Operation {
				o.Input = []Parameter{
					{Name: "from", Type: TypeString, Required: true},
					{Name: "to", Type: TypeString, Required: true},
					{Name: "direction", Type: TypeString, Default: "sideways"},
				}
				return o
			},
		},
		{
			why: "no source at all",
			build: func(o Operation) Operation {
				o.Direction = &Term{}
				return o
			},
		},
	} {
		operation := refused.build(eitherWay())
		operation.Name = "articles.refused"
		if _, err := store.DeclareOperation(operation); !errors.Is(err, ErrDeclaration) {
			t.Errorf("%s: want ErrDeclaration, got %v", refused.why, err)
		}
	}

	// A direction on something that does not walk an index is a word nothing
	// reads, which is how a caller comes to believe a promise nobody made.
	if _, err := store.DeclareOperation(Operation{
		Name:       "articles.one",
		Collection: "articles",
		Action:     ActionGet,
		Input:      []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Key:        &Term{Arg: "id"},
		Direction:  &Term{Value: DirectionReverse},
	}); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a direction on a get: want ErrDeclaration, got %v", err)
	}
}

// TestADirectionTheCallerMisspellsIsRefusedRatherThanGuessed: the argument is
// checked at call time as well, because a string parameter can hold any string.
// Reading "reverze" as forward would be wrong, look right, and say nothing.
func TestADirectionTheCallerMisspellsIsRefusedRatherThanGuessed(t *testing.T) {
	store, collection := declared(t, 128)
	fill(t, collection)
	declareOp(t, store, eitherWay())

	if _, err := store.Invoke(Caller{}, "articles.slugs", 0, map[string]any{
		"from": "s0", "to": "s5", "direction": "reverze",
	}); !errors.Is(err, ErrArgument) {
		t.Errorf("want ErrArgument, got %v", err)
	}
}

// TestAReversedScanWalksThePartitionsBackwardsToo: a reversed walk reverses the
// list of partitions as well as each tree in it. Reversing only the trees leaves
// an order that is right inside each partition and wrong across them — January's
// entries newest-first, then February's — which no single-partition test can see
// and which looks entirely plausible in a list of rows.
func TestAReversedScanWalksThePartitionsBackwardsToo(t *testing.T) {
	_, _, store := partitioned(t, 129)
	entries := entriesByMonth(t, store, 0)

	var written []string
	for _, when := range []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
	} {
		atMonth(t, store, when)
		for i := 0; i < 2; i++ {
			written = append(written, fmt.Sprint(put(t, entries, map[string]any{
				"account": "cash", "amount": float64(i),
			})))
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Three partitions, or this test is about one tree.
	trees, err := entries.across()
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) != 3 {
		t.Fatalf("the collection is in %d partitions", len(trees))
	}

	// Keys are ulids and partition names sort in the order the periods
	// happened, so what was written is already what forward order is.
	forward := keysOf(scan(t, entries, "by_account", Range{
		From: &Bound{Values: []any{"cash"}},
		To:   &Bound{Values: []any{"cash"}},
	}))
	if fmt.Sprint(forward) != fmt.Sprint(written) {
		t.Fatalf("forward across partitions gave %v, want %v", forward, written)
	}

	reverse := keysOf(scan(t, entries, "by_account", Range{
		From:      &Bound{Values: []any{"cash"}},
		To:        &Bound{Values: []any{"cash"}},
		Direction: Reverse,
	}))
	for i := range written {
		if reverse[i] != written[len(written)-1-i] {
			t.Fatalf("reverse across partitions gave %v, want %v backwards", reverse, written)
		}
	}

	// The same thing on the clustered walk, which is the one a partitioned
	// collection is usually read by.
	var down []string
	if err := entries.walkRange(Range{Direction: Reverse}, func(key any, _ map[string]any) bool {
		down = append(down, fmt.Sprint(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for i := range written {
		if down[i] != written[len(written)-1-i] {
			t.Fatalf("the reversed clustered walk gave %v, want %v backwards", down, written)
		}
	}
}

// TestAReversedScanStopsEarlyAcrossPartitions: stopping is the whole scan
// stopping, not this partition stopping — the same contract the forward walk
// has, and worth its own test because the reversed walk reaches the partitions
// in the other order and could easily stop in the wrong one.
func TestAReversedScanStopsEarlyAcrossPartitions(t *testing.T) {
	_, _, store := partitioned(t, 130)
	entries := entriesByMonth(t, store, 0)

	var written []string
	for _, when := range []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
	} {
		atMonth(t, store, when)
		written = append(written, fmt.Sprint(put(t, entries, map[string]any{"account": "cash"})))
		written = append(written, fmt.Sprint(put(t, entries, map[string]any{"account": "cash"})))
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	var seen []string
	if err := entries.Scan("by_account", Range{
		From:      &Bound{Values: []any{"cash"}},
		To:        &Bound{Values: []any{"cash"}},
		Direction: Reverse,
	}, func(one Found) bool {
		seen = append(seen, fmt.Sprint(one.Key))
		return len(seen) < 3
	}); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 3 {
		t.Fatalf("the scan saw %d entries after being stopped at 3: %v", len(seen), seen)
	}
	// The newest three, which means it crossed from February into January and
	// stopped there rather than starting again.
	want := []string{written[3], written[2], written[1]}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("stopping early gave %v, want %v", seen, want)
	}
}

// idsOf is the id of every row a scan handed back, in the order it did.
func idsOf(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = fmt.Sprint(row["id"])
	}
	return out
}

// TestAConstantAtOneEndDeclaresAndReadsOneStretchBothWays: a constant written
// into a bound is the one thing about a stretch a caller cannot move. It is
// how a declaration pins a floor, or a tenant, and hands the rest of the
// range over to the call.
//
// Rounds one through three refused a constant written at one end only,
// beside a caller-chosen direction — reasoning that reversing swapped which
// end of the stretch was the floor, so the constant stopped being one the
// moment the caller said "reverse". That reasoning does not apply any more:
// nothing swaps. From is the low end and To is the high end regardless of
// Direction, so a constant at either end is a floor (or a ceiling) no matter
// which way the walk is read, exactly the way it already was for a fixed
// direction. There is nothing left here to refuse.
func TestAConstantAtOneEndDeclaresAndReadsOneStretchBothWays(t *testing.T) {
	store, collection := declared(t, 128)
	fill(t, collection)

	// The shape round one refused outright: a constant floor at From, an
	// argument at To.
	floored := eitherWay()
	floored.Name = "articles.floored"
	floored.Input = []Parameter{
		{Name: "to", Type: TypeString, Required: true},
		{Name: "direction", Type: TypeString, Default: DirectionForward},
	}
	floored.From = &Endpoint{Terms: []Term{{Value: "s3"}}}
	floored.To = &Endpoint{Terms: []Term{{Arg: "to"}}}
	declareOp(t, store, floored)

	forward := invoke(t, store, "articles.floored", map[string]any{"to": "s5", "direction": DirectionForward})
	if got := slugsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s3", "s4", "s5"}) {
		t.Errorf("forward from the constant floor s3 up to s5 gave %v", got)
	}
	reverse := invoke(t, store, "articles.floored", map[string]any{"to": "s5", "direction": DirectionReverse})
	if got := slugsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s5", "s4", "s3"}) {
		t.Errorf("reverse over the same declared stretch gave %v", got)
	}

	// The mirror, and the case where the other end is not written at all: an
	// absent end is the whole rest of the index, same as it always was.
	ceilinged := floored
	ceilinged.Name = "articles.ceilinged"
	ceilinged.From = &Endpoint{Terms: []Term{{Arg: "to"}}}
	ceilinged.To = &Endpoint{Terms: []Term{{Value: "s3"}}}
	declareOp(t, store, ceilinged)

	lonely := floored
	lonely.Name = "articles.lonely"
	lonely.From = nil
	lonely.To = &Endpoint{Terms: []Term{{Value: "s3"}}}
	declareOp(t, store, lonely)

	// A constant does not have to be the FIRST value of a bound. by_author is
	// (author ascending, published descending), so pinning the author as an
	// argument and the published date as a constant leaves the constant at
	// position two — and it now reads one stretch both ways there exactly as
	// it does at position one.
	deep := Operation{
		Name:       "articles.deep_floor",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_author",
		Input: []Parameter{
			{Name: "author", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:       &Endpoint{Terms: []Term{{Arg: "author"}, {Value: 3.0}}},
		To:         &Endpoint{Terms: []Term{{Arg: "author"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"id"},
		Limit:      10,
	}
	declareOp(t, store, deep)

	// by_author is (author ascending, published descending); the constant
	// 3.0 at the second position of From means "published <= 3", so ann's
	// rows with published 1 and 2 (a1, a2) are the ones this reaches.
	deepForward := invoke(t, store, "articles.deep_floor", map[string]any{"author": "ann", "direction": DirectionForward})
	if got := idsOf(deepForward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a2", "a1"}) {
		t.Errorf("forward with a constant at the second bound value gave %v", got)
	}
	deepReverse := invoke(t, store, "articles.deep_floor", map[string]any{"author": "ann", "direction": DirectionReverse})
	if got := idsOf(deepReverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a1", "a2"}) {
		t.Errorf("reverse over the same declared stretch gave %v", got)
	}

	// And the same declaration with the constant matched at both ends still
	// pins rather than floors, as it did before this round — round two's
	// mệnh đề 1, which this round keeps.
	deep.Name = "articles.deep_pin"
	deep.To = &Endpoint{Terms: []Term{{Arg: "author"}, {Value: 3.0}}}
	declareOp(t, store, deep)

	// A direction the declaration fixes is nobody's choice but the schema
	// author's, so a constant bound beside it is theirs to write — unchanged
	// by this round.
	fixed := floored
	fixed.Name = "articles.fixed_floor"
	fixed.Input = []Parameter{{Name: "to", Type: TypeString, Required: true}}
	fixed.Direction = &Term{Value: DirectionReverse}
	declareOp(t, store, fixed)
}

// TestAPinnedConstantSurvivesBeingReadFromEitherEnd: "one author, newest
// first or oldest first" pins the author with a constant at BOTH ends, which
// is what scanAcross already makes every partitioned scan do. The pin holds
// whichever way the walk enters, so the two directions see one set of rows —
// true of this declaration before round four as well as after, and kept here
// as a fixed point while TestAConstantAtOneEndDeclaresAndReadsOneStretch...
// above covers the shape that used to be refused beside it.
func TestAPinnedConstantSurvivesBeingReadFromEitherEnd(t *testing.T) {
	store, collection := declared(t, 129)
	fill(t, collection)

	pinned := Operation{
		Name:       "articles.anns",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_author",
		Input:      []Parameter{{Name: "direction", Type: TypeString, Required: true}},
		From:       &Endpoint{Terms: []Term{{Value: "ann"}}},
		To:         &Endpoint{Terms: []Term{{Value: "ann"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"id"},
		Limit:      10,
	}
	declareOp(t, store, pinned)

	// by_author is (author ascending, published descending), and ann wrote
	// a1, a2, a4 and a5.
	forward := invoke(t, store, "articles.anns", map[string]any{"direction": DirectionForward})
	if got := idsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a5", "a4", "a2", "a1"}) {
		t.Errorf("forward over the pinned author gave %v", got)
	}
	reverse := invoke(t, store, "articles.anns", map[string]any{"direction": DirectionReverse})
	if got := idsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"a1", "a2", "a4", "a5"}) {
		t.Errorf("reverse over the pinned author gave %v", got)
	}

	// bob's articles are on the same index on both sides of ann's, and neither
	// direction reaches them.
	for _, row := range append(forward.Rows, reverse.Rows...) {
		if id := fmt.Sprint(row["id"]); id == "a0" || id == "a3" {
			t.Errorf("the pin let %q through", id)
		}
	}
}
