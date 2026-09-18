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

// TestTheEndsOfARangeSwapWhenTheScanIsReversed: From is where a walk starts and
// To is where it ends, whichever way it goes, so a reversed walk makes From the
// upper end and To the lower.
//
// The failure this guards is quiet: written the wrong way round a reversed scan
// hands back nothing, or hands back everything, and both look like an answer.
// So both the right way round and the wrong way round are asserted here.
func TestTheEndsOfARangeSwapWhenTheScanIsReversed(t *testing.T) {
	_, collection := declared(t, 122)
	fill(t, collection)

	forward := keysOf(scan(t, collection, "by_slug", Range{
		From: &Bound{Values: []any{"s1"}},
		To:   &Bound{Values: []any{"s4"}},
	}))
	if fmt.Sprint(forward) != fmt.Sprint([]string{"a1", "a2", "a3", "a4"}) {
		t.Fatalf("forward from s1 to s4 gave %v", forward)
	}

	// The same stretch, entered from the other end: From is now s4.
	reverse := keysOf(scan(t, collection, "by_slug", Range{
		From:      &Bound{Values: []any{"s4"}},
		To:        &Bound{Values: []any{"s1"}},
		Direction: Reverse,
	}))
	if fmt.Sprint(reverse) != fmt.Sprint([]string{"a4", "a3", "a2", "a1"}) {
		t.Errorf("reverse from s4 to s1 gave %v", reverse)
	}

	// Both ends excluded, walking down: s4 is where it starts from and is left
	// out, s1 is where it ends and is left out.
	trimmed := keysOf(scan(t, collection, "by_slug", Range{
		From:      &Bound{Values: []any{"s4"}, Exclusive: true},
		To:        &Bound{Values: []any{"s1"}, Exclusive: true},
		Direction: Reverse,
	}))
	if fmt.Sprint(trimmed) != fmt.Sprint([]string{"a3", "a2"}) {
		t.Errorf("reverse from s4 exclusive to s1 exclusive gave %v", trimmed)
	}

	// Written the wrong way round — the ends left as they were for a forward
	// walk — the reversed scan is empty rather than quietly the whole index.
	// This is the shape of the mistake, recorded so that it stays loud.
	backwards := keysOf(scan(t, collection, "by_slug", Range{
		From:      &Bound{Values: []any{"s1"}},
		To:        &Bound{Values: []any{"s4"}},
		Direction: Reverse,
	}))
	if len(backwards) != 0 {
		t.Errorf("a reversed scan with the ends the forward way round gave %v", backwards)
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
	// off by one entry is visible, and a1 is the entry it would lose.
	var bounded []string
	if err := collection.walkRange(Range{
		From:      &Bound{Values: []any{"a4"}},
		To:        &Bound{Values: []any{"a1"}},
		Direction: Reverse,
	}, func(key any, _ map[string]any) bool {
		bounded = append(bounded, fmt.Sprint(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(bounded) != fmt.Sprint([]string{"a4", "a3", "a2", "a1"}) {
		t.Errorf("reversed from a4 down to a1 gave %v", bounded)
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
		"from": "s5", "to": "s0", "direction": DirectionReverse,
	})
	if got := slugsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s5", "s4", "s3"}) {
		t.Errorf("reverse gave %v", got)
	}
	if !reverse.Truncated {
		t.Error("the reversed scan stopped at its limit and did not say so")
	}

	// A stretch that fits says so, both ways round.
	short := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s1", "to": "s3", "direction": DirectionForward,
	})
	if short.Count != 3 || short.Truncated {
		t.Errorf("forward over three rows: %d rows, truncated=%v", short.Count, short.Truncated)
	}
	short = invoke(t, store, "articles.slugs", map[string]any{
		"from": "s3", "to": "s1", "direction": DirectionReverse,
	})
	if short.Count != 3 || short.Truncated {
		t.Errorf("reverse over three rows: %d rows, truncated=%v", short.Count, short.Truncated)
	}
}

// TestADirectionReachesNothingTheForwardScanCouldNot: the constraint the whole
// design rests on. A direction is allowed to be an argument only because it
// widens nothing — same index, same two declared ends, same limit — and an
// argument that widened a scan would be the one thing declared operations exist
// to make impossible.
func TestADirectionReachesNothingTheForwardScanCouldNot(t *testing.T) {
	store, collection := declared(t, 125)
	fill(t, collection)
	declareOp(t, store, eitherWay())

	forward := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s1", "to": "s3", "direction": DirectionForward,
	})
	reverse := invoke(t, store, "articles.slugs", map[string]any{
		"from": "s3", "to": "s1", "direction": DirectionReverse,
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

	result := invoke(t, store, "articles.newest", map[string]any{"from": "s5", "to": "s0"})
	if got := slugsOf(result.Rows); fmt.Sprint(got) != fmt.Sprint([]string{"s5", "s4", "s3"}) {
		t.Errorf("a declaration fixed at reverse gave %v", got)
	}

	// And a caller cannot pass one, because the operation does not take one.
	if _, err := store.Invoke(Caller{}, "articles.newest", 0, map[string]any{
		"from": "s5", "to": "s0", "direction": DirectionForward,
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
