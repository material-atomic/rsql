package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// fieldOf is one field of every row a scan handed back, in the order it did.
func fieldOf(rows []map[string]any, field string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = fmt.Sprint(row[field])
	}
	return out
}

// TestABoundThatFallsAwayUndoesWhatScanAcrossPromised records the one case
// where the promise the reversed walk leans on is not kept.
//
// The argument for reversing the partition list is that scanAcross has already
// guaranteed the partitions concatenated in partition order are in key order,
// so the reverse of that is in reverse key order. scanAcross makes that
// guarantee by insisting a scan on a secondary index fixes every declared
// field and lets only the key vary — but it checks the TERMS, at declaration
// time, and a term may be an optional argument. bounds() drops an endpoint
// whose argument was not given, on purpose: "an operation that takes an
// optional since is unbounded below when nobody passes one."
//
// Put those two together and the fixed field is not fixed at call time. The
// scan then runs the whole index of January, then the whole index of February,
// which is account order inside a partition and partition order across them —
// the exact shape scanAcross exists to refuse.
//
// This asserts what the code does today rather than what it should do, because
// it is not a regression: the forward walk has the same hole and had it before
// direction existed. It is pinned here so that whoever closes it — by making a
// bound term that decides a partitioned scan required, most likely — sees this
// test go red and knows to delete it.
func TestABoundThatFallsAwayUndoesWhatScanAcrossPromised(t *testing.T) {
	_, _, store := partitioned(t, 140)
	entries := entriesByMonth(t, store, 0)

	for _, when := range []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
	} {
		atMonth(t, store, when)
		for _, account := range []string{"cash", "zinc"} {
			put(t, entries, map[string]any{"account": account})
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Both ends name the same optional argument, so scanAcross sees a scan
	// that fixes its only field and lets it through.
	loose := Operation{
		Name:       "entries.loose",
		Collection: "entries",
		Action:     ActionScan,
		Index:      "by_account",
		Input: []Parameter{
			{Name: "account", Type: TypeString},
			{Name: "direction", Type: TypeString, Default: DirectionForward},
		},
		From:       &Endpoint{Terms: []Term{{Arg: "account"}}},
		To:         &Endpoint{Terms: []Term{{Arg: "account"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"account"},
		Limit:      10,
	}
	if _, err := store.DeclareOperation(loose); err != nil {
		t.Fatalf("scanAcross refused this after all, which would be the fix: %v", err)
	}

	// Called with the account, the guarantee holds and both directions are in
	// key order.
	fixed := invoke(t, store, "entries.loose", map[string]any{"account": "cash"})
	if got := fieldOf(fixed.Rows, "account"); fmt.Sprint(got) != fmt.Sprint([]string{"cash", "cash"}) {
		t.Fatalf("with the bound given, the scan gave %v", got)
	}

	// Called without it, the bounds fall away and the scan crosses partitions
	// unbounded. Key order would be cash cash zinc zinc; partition order is
	// what comes back.
	loose2 := invoke(t, store, "entries.loose", map[string]any{})
	got := fieldOf(loose2.Rows, "account")
	if fmt.Sprint(got) == fmt.Sprint([]string{"cash", "cash", "zinc", "zinc"}) {
		t.Fatalf("the hole has been closed and this test should go: %v", got)
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"cash", "zinc", "cash", "zinc"}) {
		t.Errorf("forward across partitions with no bound gave %v", got)
	}

	// Reversed it is the same list backwards — which is exactly what the
	// commit claims, and is no comfort, because the list it reverses was never
	// in key order.
	back := invoke(t, store, "entries.loose", map[string]any{"direction": DirectionReverse})
	if want := fmt.Sprint([]string{"zinc", "cash", "zinc", "cash"}); fmt.Sprint(fieldOf(back.Rows, "account")) != want {
		t.Errorf("reverse across partitions with no bound gave %v, want %v", fieldOf(back.Rows, "account"), want)
	}
}

// TestOneDeclarationReadBothWaysCoversTheSameStretch is what round three's
// TestOneDeclarationReadBothWaysCoversTwoDifferentStretches used to be named
// for the opposite of. That test measured the cost of From and To trading
// roles under Direction: the same argument, same value, direction flipped,
// used to cover two different stretches of the index sharing a single row.
// Round four removed the swap, so the sentence in its own name is now true:
// one declaration, one stretch, read forward or backward.
func TestOneDeclarationReadBothWaysCoversTheSameStretch(t *testing.T) {
	store, collection := declared(t, 141)
	fill(t, collection)

	declareOp(t, store, Operation{
		Name:       "articles.up_to",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_slug",
		Input: []Parameter{
			{Name: "edge", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Default: DirectionForward},
		},
		To:         &Endpoint{Terms: []Term{{Arg: "edge"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"slug"},
		Limit:      10,
	})

	forward := fieldOf(invoke(t, store, "articles.up_to", map[string]any{
		"edge": "s4", "direction": DirectionForward,
	}).Rows, "slug")
	if want := []string{"s0", "s1", "s2", "s3", "s4"}; fmt.Sprint(forward) != fmt.Sprint(want) {
		t.Fatalf("forward to s4 gave %v, want %v", forward, want)
	}

	// The SAME declaration, the SAME argument value, direction flipped. From
	// is unwritten (the whole index below "edge") and To is still "edge" —
	// neither end reads any differently for it, so this is the forward
	// stretch, backwards.
	reverse := fieldOf(invoke(t, store, "articles.up_to", map[string]any{
		"edge": "s4", "direction": DirectionReverse,
	}).Rows, "slug")
	if want := []string{"s4", "s3", "s2", "s1", "s0"}; fmt.Sprint(reverse) != fmt.Sprint(want) {
		t.Fatalf("reverse to s4 gave %v, want %v", reverse, want)
	}
}

// TestADirectionOnARollupReadIsRefusedBecauseTotalsWouldIgnoreIt: Totals takes
// the same Range every scan does and reads within.Direction nowhere, so a
// direction that reached it would be a word in a declaration that changes
// nothing — rows in the order the caller did not ask for, with no error.
//
// The refusal at declaration is what keeps that unreachable, so it is worth a
// test of its own rather than being covered only by the refusal on a get.
func TestADirectionOnARollupReadIsRefusedBecauseTotalsWouldIgnoreIt(t *testing.T) {
	_, store := fresh(t, 142)
	lines := takings(t, store)

	for _, account := range []string{"ann", "bob", "cid"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DeclareOperation(Operation{
		Name:       "lines.totals",
		Collection: "lines",
		Action:     ActionTotals,
		Rollup:     "per_account",
		Input:      []Parameter{{Name: "direction", Type: TypeString, Default: DirectionForward}},
		Direction:  &Term{Arg: "direction"},
		Limit:      10,
	}); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a direction on a rollup read: want ErrDeclaration, got %v", err)
	}

	// And this is why it has to be refused rather than allowed and ignored: a
	// Range that says Reverse reads forward here, silently.
	var groups []string
	if err := lines.Totals("per_account", Range{Direction: Reverse}, func(row Totals) bool {
		groups = append(groups, fmt.Sprint(row.Group[0]))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ann", "bob", "cid"}; fmt.Sprint(groups) != fmt.Sprint(want) {
		t.Errorf("Totals now honours a direction, so the refusal above can go: %v", groups)
	}
}

// TestACountIsTheSameNumberFromEitherEnd: a count does walk an index, which is
// why it nearly kept the right to declare a direction. But it hands back a
// number, and the number is the same from either end — so a direction on a
// count is a word in a declaration that nothing reads, which is the exact
// reason totals is refused one. It is refused here for that reason too, and
// this test holds both halves: the refusal, and the measurement that makes it
// the right refusal.
func TestACountIsTheSameNumberFromEitherEnd(t *testing.T) {
	store, collection := declared(t, 143)
	fill(t, collection)

	counter := Operation{
		Name:       "articles.count",
		Collection: "articles",
		Action:     ActionCount,
		Index:      "by_slug",
		Input:      []Parameter{{Name: "direction", Type: TypeString, Required: true}},
		Direction:  &Term{Arg: "direction"},
		Limit:      4,
	}
	if _, err := store.DeclareOperation(counter); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a direction on a count: want ErrDeclaration, got %v", err)
	}

	// And a fixed one is refused as well: there is nothing to fix, whoever
	// writes it down.
	counter.Direction = &Term{Value: DirectionReverse}
	counter.Input = nil
	if _, err := store.DeclareOperation(counter); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a fixed direction on a count: want ErrDeclaration, got %v", err)
	}

	// The measurement behind the refusal: the same walk with a limit of 4 sees
	// four of the six rows whichever end it starts from, and says it stopped
	// short either way. Different rows, same number.
	for _, direction := range []Direction{Forward, Reverse} {
		seen := 0
		if err := collection.Scan("by_slug", Range{Direction: direction}, func(Found) bool {
			seen++
			return seen < 4
		}); err != nil {
			t.Fatal(err)
		}
		if seen != 4 {
			t.Errorf("counting from one end gave %d, want 4", seen)
		}
	}
}

// TestAReversedScanStopsPartWayDownAPartitionedIndex: stopping early inside the
// FIRST partition a reversed walk reaches — which is the last one a forward
// walk reaches — is the case where reversing the list of trees and stopping the
// whole scan have to agree with each other. Stopping after the first row is the
// sharpest version: one row, from the newest partition, and no others touched.
func TestAReversedScanStopsPartWayDownAPartitionedIndex(t *testing.T) {
	_, _, store := partitioned(t, 144)
	entries := entriesByMonth(t, store, 0)

	var written []string
	for _, when := range []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
	} {
		atMonth(t, store, when)
		written = append(written, fmt.Sprint(put(t, entries, map[string]any{"account": "cash"})))
		written = append(written, fmt.Sprint(put(t, entries, map[string]any{"account": "cash"})))
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, stopAt := range []int{1, 2, 3, 5, 6} {
		var seen []string
		if err := entries.Scan("by_account", Range{
			From:      &Bound{Values: []any{"cash"}},
			To:        &Bound{Values: []any{"cash"}},
			Direction: Reverse,
		}, func(one Found) bool {
			seen = append(seen, fmt.Sprint(one.Key))
			return len(seen) < stopAt
		}); err != nil {
			t.Fatal(err)
		}

		want := make([]string, stopAt)
		for i := range want {
			want[i] = written[len(written)-1-i]
		}
		if fmt.Sprint(seen) != fmt.Sprint(want) {
			t.Errorf("stopped at %d, the reversed scan saw %v, want %v", stopAt, seen, want)
		}
	}
}

// TestAReversedClusteredWalkCrossesEveryPartitionBoundary: the clustered walk
// over a partitioned collection is the one scanAcross waves through without
// looking at anything, so it is the one where reversing the list of trees has
// to be right on its own. Six partitions with one document each means every
// row handed back is a boundary crossing.
func TestAReversedClusteredWalkCrossesEveryPartitionBoundary(t *testing.T) {
	_, _, store := partitioned(t, 145)
	entries := entriesByMonth(t, store, 0)

	var written []string
	for month := 1; month <= 6; month++ {
		atMonth(t, store, time.Date(2026, time.Month(month), 15, 0, 0, 0, 0, time.UTC))
		written = append(written, fmt.Sprint(put(t, entries, map[string]any{"account": "cash"})))
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	trees, err := entries.across()
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) != 6 {
		t.Fatalf("the collection is in %d partitions, so this test is not about boundaries", len(trees))
	}

	var down []string
	if err := entries.walkRange(Range{Direction: Reverse}, func(key any, _ map[string]any) bool {
		down = append(down, fmt.Sprint(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	want := make([]string, len(written))
	for i := range written {
		want[i] = written[len(written)-1-i]
	}
	if fmt.Sprint(down) != fmt.Sprint(want) {
		t.Errorf("the reversed clustered walk gave %v, want %v", down, want)
	}

	// Bounded at both ends, on the clustered index, across partitions: From
	// is written[1] (the low end) and To is written[4] (the high end), same
	// as a forward call over this stretch would be — reversing changes only
	// the read order — and both of them land exactly on a stored key.
	var middle []string
	if err := entries.walkRange(Range{
		From:      &Bound{Values: []any{written[1]}},
		To:        &Bound{Values: []any{written[4]}},
		Direction: Reverse,
	}, func(key any, _ map[string]any) bool {
		middle = append(middle, fmt.Sprint(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{written[4], written[3], written[2], written[1]}; fmt.Sprint(middle) != fmt.Sprint(want) {
		t.Errorf("the bounded reversed clustered walk gave %v, want %v", middle, want)
	}
}
