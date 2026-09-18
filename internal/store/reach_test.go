package store

import (
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"
)

// This file exists to break one sentence: "a direction widens nothing" — the
// set of rows a declaration can hand back is the same whichever direction the
// caller picks.
//
// Rounds one through three tried to keep that sentence true while From and To
// swapped which one was the walk's upper end depending on Direction, and
// refused whichever shape of declaration that swap made unsafe: a constant at
// one end (round one), the two ends disagreeing about Exclusive (round two),
// the two ends written at different widths (round three, caught by the
// Reviewer, not by this file — reachShapes writes one term per end and never
// exercised width). Round four removed the swap instead of refusing a fourth
// shape.
//
// So this file no longer has an accepted/refused axis. Every shape below
// declares without error now, because there is no rule left in checkDirection
// to refuse any of them for. What is left to check is sharper than "declared
// or not": for every pair of argument values, the forward call and the
// reverse call of the SAME declaration must (a) both match an oracle computed
// straight off the fixture rows, by ordinary Go comparisons rather than
// through bound() or walk(), and (b) be exactly each other's rows backwards —
// not merely the same set, the same order reversed, which is the one thing
// Direction is allowed to change.
//
// The list of shapes stayed, even with the rule gone that used to refuse some
// of them: they are still a useful cross-section of ways a bound gets
// written, and TestTheTwoBoundsNeverDependOnDirection (direction_v4_test.go)
// reuses this exact list for a second, independent check at the byte level.
//
// This round found the enumeration itself had a hole: the DOMAIN a caller's
// argument may range over had "" in it, but no actual ROW ever carried "".
// Crossing an argument domain against itself only ever sees what a caller
// could pass; it says nothing about what is stored, and round three's leak
// was about a row sitting at the floor of the index's own byte encoding, not
// about "" as a value somebody happened to type. So the fixture carries a row
// at that floor (fillWithFloor, below) and the assertions compare against it
// directly, not through the domain. QA's mutate27.py measured this by
// removing "" from the domain and watching the harness report MORE leaking
// shapes, not fewer, back when there was still something to leak — "" in the
// domain, with no row there to find, was quietly absorbing calls that would
// otherwise land past every row and come back empty either way, which reads
// as "the two directions agree" even though neither direction reached
// anything.
//
// Two things this file measures and does NOT decide are written down here
// once rather than at every place they would otherwise look unexamined:
//
//   - Optional arguments with no default are the one shape checkDirection
//     still refuses outright (see TestADirectionThatCannotBeWorkedOutIs...
//     in direction_test.go) — a bound whose argument is optional can fall
//     away entirely at call time, which is task 0016's hole, not this file's.
//     QA checked by hand that it does not leak on reachability even so (both
//     directions' unions equal the whole index), and nothing in this round
//     changes that.
//   - Limit is not part of what this file measures. declareReach writes
//     Limit: 50 because a scan must declare one to be valid at all, and
//     TestALimitStopsAScanFromEitherEnd (direction_test.go) already measures
//     that it cuts both directions the same way. TestALimitCanHandBack...
//     (direction_v4_test.go) measures the one thing Limit is allowed to do
//     that a bound is not: hand back different rows from either end of the
//     SAME stretch. Neither is a bound this file's oracle applies.
//
// Composite bounds (more than one term at an end) and a Term{Step: ...} at a
// bound are outside reachShapes on purpose: it writes exactly one term per
// end, and its oracle only knows how to check a bound that shape against real
// rows. The composite column is measured instead by
// TestTheTwoBoundsNeverDependOnDirection and TestByAuthorFloorRowReadsOne...
// (direction_v4_test.go), and a Step term at a scan bound is refused by
// check() before it is ever compared to anything (QA measured this in round
// three). Widening reachShapes to write several terms per end is a task of
// its own if anyone needs it measured this way instead.

// reachEnd is one end of a stretch as a declaration may write it: a constant
// nobody can move, an argument the caller fills in, or nothing at all.
type reachEnd struct {
	arg       string
	fixed     string
	constant  bool
	exclusive bool
}

func anArgument(name string) reachEnd { return reachEnd{arg: name} }
func aConstant(value string) reachEnd { return reachEnd{fixed: value, constant: true} }
func nothing() reachEnd               { return reachEnd{} }

func butExclusive(e reachEnd) reachEnd {
	e.exclusive = true
	return e
}

func (e reachEnd) written() bool { return e.constant || e.arg != "" }

func (e reachEnd) term() Term {
	if e.constant {
		return Term{Value: e.fixed}
	}
	return Term{Arg: e.arg}
}

// bound is this end for one particular argument value.
func (e reachEnd) bound(value string) *Bound {
	if !e.written() {
		return nil
	}
	if e.constant {
		value = e.fixed
	}
	return &Bound{Values: []any{value}, Exclusive: e.exclusive}
}

// reachShape is one declaration, named by what it is rather than by what it
// does, so a failure says which shape leaked.
type reachShape struct {
	what string
	from reachEnd
	to   reachEnd

	// pairedArgument is true when From and To name the SAME argument. Invoke
	// has exactly one slot for that name, so a real call can never give the
	// two ends different values — crossing a domain against itself for both
	// ends independently would manufacture a call nobody could make and
	// measure a shape that does not exist. checkShape() reads this to walk
	// the domain once instead of crossing it with itself.
	pairedArgument bool
}

func sorted(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, fmt.Sprintf("%q", value))
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}

// reachRow is one document, as far as this file measures it: the value the
// index stores for it, which is what a bound compares against, and its
// primary key, which is what identifies the row. The two differ for a
// secondary index (by_slug's value is the slug, its key is the document's id)
// and coincide for the clustered walk, where the key is the whole of what is
// stored.
type reachRow struct {
	key   string
	value string
}

// rowsOf reads every row of one index, unbounded and forward — the widest
// call there is — so the oracle in reaches has a ground truth that was not
// itself computed by bound() or walk().
func rowsOf(t *testing.T, collection *Collection, index string) []reachRow {
	t.Helper()
	var rows []reachRow
	var err error
	if index == ClusteredIndex {
		err = collection.walkRange(Range{}, func(key any, _ map[string]any) bool {
			k := fmt.Sprint(key)
			rows = append(rows, reachRow{key: k, value: k})
			return true
		})
	} else {
		err = collection.Scan(index, Range{}, func(one Found) bool {
			rows = append(rows, reachRow{key: fmt.Sprint(one.Key), value: fmt.Sprint(one.Values[0])})
			return true
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// expectedReach is the oracle: the rows one call with these argument values
// ought to hand back, worked out from the fixture with ordinary Go string
// comparisons and never through bound() or walk() — so a bug shared between
// the code and the check cannot cancel itself out. It is only asked to be
// correct for a bound written as one term on a string field in ascending
// order, which is exactly what reachShapes writes and checkShape measures;
// see the file comment above for what that leaves out and where that is
// measured instead.
//
// There is no Direction parameter any more. shape.from is always the low end
// and shape.to is always the high end — that is round four's whole point,
// and this oracle says so by construction rather than by asserting it: the
// same two lines compute what forward and reverse both have to match.
func expectedReach(rows []reachRow, shape reachShape, argFrom, argTo string) map[string]bool {
	value := func(end reachEnd, argValue string) string {
		if end.constant {
			return end.fixed
		}
		return argValue
	}

	seen := map[string]bool{}
	for _, row := range rows {
		if shape.from.written() {
			v := value(shape.from, argFrom)
			if shape.from.exclusive {
				if row.value <= v {
					continue
				}
			} else if row.value < v {
				continue
			}
		}
		if shape.to.written() {
			v := value(shape.to, argTo)
			if shape.to.exclusive {
				if row.value >= v {
					continue
				}
			} else if row.value > v {
				continue
			}
		}
		seen[row.key] = true
	}
	return seen
}

// reversedList is the list backwards, for comparing a reverse call's rows
// against a forward call's rows exactly — not merely as sets, which would
// miss the one thing Direction is allowed to change.
func reversedList(list []string) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[len(list)-1-i] = v
	}
	return out
}

// checkShape walks a shape's declaration over every pair of argument values
// the domain allows, both directions, and checks three things per pair:
//
//   - the forward call's rows match expectedReach, the oracle computed
//     straight off the fixture;
//   - the reverse call's rows match the SAME oracle — not a swapped one,
//     because From and To no longer swap;
//   - the reverse call's rows are exactly the forward call's rows backwards,
//     which is the one thing Direction is allowed to change and the thing
//     the two bullets above do not, on their own, pin down (they would both
//     hold if a mutation reordered rows within a call without touching which
//     rows there were).
//
// It replaces round three's reaches(), which unioned every call's rows
// across the whole domain before comparing directions — a union that
// absorbed a single call's off-by-one into rows some other call already
// contributed. See TestAnExclusiveEndIsOutsideTheStretchWhicheverWayTheWalk
// Enters in direction_qa2_test.go for the mutation that survived exactly that
// hole; checking every call on its own, forward and reverse both, is why this
// version does not need that test's help to catch it.
func checkShape(t *testing.T, collection *Collection, index string, shape reachShape,
	domain []string, rows []reachRow) {

	t.Helper()

	type argPair struct{ low, high string }
	var pairs []argPair
	if shape.pairedArgument {
		for _, v := range domain {
			pairs = append(pairs, argPair{v, v})
		}
	} else {
		froms, tos := domain, domain
		if shape.from.arg == "" {
			froms = []string{""}
		}
		if shape.to.arg == "" {
			tos = []string{""}
		}
		for _, low := range froms {
			for _, high := range tos {
				pairs = append(pairs, argPair{low, high})
			}
		}
	}

	walk := func(direction Direction, low, high string) []string {
		within := Range{From: shape.from.bound(low), To: shape.to.bound(high), Direction: direction}
		var got []string
		var err error
		if index == ClusteredIndex {
			// The clustered walk is the documents themselves, and it is a
			// different call: walkRange rather than Scan. It is in here
			// because it is the only walk whose bounds can land exactly on a
			// stored key.
			err = collection.walkRange(within, func(key any, _ map[string]any) bool {
				got = append(got, fmt.Sprint(key))
				return true
			})
		} else {
			err = collection.Scan(index, within, func(one Found) bool {
				// Identified by the row (its primary key), not by the value
				// the index stores for it. Two documents sharing an indexed
				// value are two rows; grouping by the value would let a
				// widened scan that only picks up a second document with a
				// value already seen read as "reached nothing new".
				//
				// Nothing in this file measures that, and this is the place
				// to say so rather than leave the line above reading as a
				// proven property. fill() gives every document its own slug
				// (s0..s5, plus the floor row's ""), so no two rows in this
				// fixture share an indexed value, and there is no other index
				// here for them to share one on. Reading Values[0] here
				// instead would go red at once, but on the slug not being the
				// id (s3 against a3) rather than on two rows collapsing into
				// one — which is a different failure, and no evidence about
				// the sentence above. Measuring it needs a non-unique index
				// with two documents on the same value, which is a fixture to
				// add, not a reason to invent a collision that would not
				// actually collide.
				got = append(got, fmt.Sprint(one.Key))
				return true
			})
		}
		if err != nil {
			t.Fatalf("%s %v at %q/%q: %v", shape.what, direction, low, high, err)
		}
		return got
	}

	sortedRaw := func(list []string) string {
		out := make([]string, len(list))
		copy(out, list)
		sort.Strings(out)
		quoted := make([]string, len(out))
		for i, v := range out {
			quoted[i] = fmt.Sprintf("%q", v)
		}
		return fmt.Sprint(quoted)
	}

	for _, p := range pairs {
		forward := walk(Forward, p.low, p.high)
		reverse := walk(Reverse, p.low, p.high)
		want := expectedReach(rows, shape, p.low, p.high)

		if sortedRaw(forward) != sorted(want) {
			t.Errorf("%s forward at %q/%q: the walk gave %v, the oracle says %v",
				shape.what, p.low, p.high, sortedRaw(forward), sorted(want))
		}
		if sortedRaw(reverse) != sorted(want) {
			t.Errorf("%s reverse at %q/%q: the walk gave %v, the oracle says %v",
				shape.what, p.low, p.high, sortedRaw(reverse), sorted(want))
		}
		if fmt.Sprint(reverse) != fmt.Sprint(reversedList(forward)) {
			t.Errorf("%s at %q/%q: reverse %v is not forward %v backwards",
				shape.what, p.low, p.high, reverse, forward)
		}
	}
}

// declareReach writes the shape down as a real operation with the direction as
// an argument, and hands back what the store said about it.
func declareReach(store *Store, name, index string, shape reachShape) error {
	operation := Operation{
		Name:       name,
		Collection: "articles",
		Action:     ActionScan,
		Index:      index,
		Input: []Parameter{
			{Name: "lo", Type: TypeString, Required: true},
			{Name: "hi", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"id", "slug"},
		Limit:      50,
	}
	if shape.from.written() {
		operation.From = &Endpoint{Terms: []Term{shape.from.term()}, Exclusive: shape.from.exclusive}
	}
	if shape.to.written() {
		operation.To = &Endpoint{Terms: []Term{shape.to.term()}, Exclusive: shape.to.exclusive}
	}
	_, err := store.DeclareOperation(operation)
	return err
}

// reachShapes is every way a bound can be written, crossed with itself. The
// list is the point: leaving a shape out is how earlier rounds' holes
// survived, and the count is asserted where this is used so that trimming the
// list is itself a failure.
//
// None of these are refused any more — that is round four's change — so the
// list is no longer split between shapes that leaked and shapes that did
// not. It stays as a cross-section of ways a bound gets written (an
// argument, a constant, absent, exclusive, paired with itself) because
// checkShape still has to be right for all of them, and because
// TestTheTwoBoundsNeverDependOnDirection (direction_v4_test.go) reuses this
// exact list for the byte-level version of the same check.
func reachShapes(pin string) []reachShape {
	lo, hi := anArgument("lo"), anArgument("hi")
	return []reachShape{
		{what: "both ends arguments", from: lo, to: hi},
		{what: "both ends arguments, From exclusive", from: butExclusive(lo), to: hi},
		{what: "both ends arguments, To exclusive", from: lo, to: butExclusive(hi)},
		{what: "an argument at From, nothing at To", from: lo, to: nothing()},
		{what: "nothing at From, an argument at To", from: nothing(), to: hi},
		{what: "neither end written", from: nothing(), to: nothing()},
		{what: "the same constant at both ends", from: aConstant(pin), to: aConstant(pin)},
		{what: "the same constant at both ends, From exclusive", from: butExclusive(aConstant(pin)), to: aConstant(pin)},
		{what: "a constant at From, an argument at To", from: aConstant(pin), to: hi},
		{what: "an argument at From, a constant at To", from: lo, to: aConstant(pin)},
		{what: "a constant at From, nothing at To", from: aConstant(pin), to: nothing()},
		{what: "nothing at From, a constant at To", from: nothing(), to: aConstant(pin)},
		{what: "constants at both ends, different values", from: aConstant(pin), to: aConstant(pin + "x")},

		// Round 3's additions.
		{what: "both ends arguments, both exclusive", from: butExclusive(lo), to: butExclusive(hi)},
		{what: "an argument at From exclusive, nothing at To", from: butExclusive(lo), to: nothing()},
		{what: "nothing at From, an argument at To exclusive", from: nothing(), to: butExclusive(hi)},
		{what: "the same argument at both ends", from: anArgument("lo"), to: anArgument("lo"),
			pairedArgument: true},
		{what: "the same argument at both ends, From exclusive", from: butExclusive(anArgument("lo")), to: anArgument("lo"),
			pairedArgument: true},
		{what: "the same constant at both ends, To exclusive", from: aConstant(pin), to: butExclusive(aConstant(pin))},
		{what: "the same constant at both ends, both exclusive", from: butExclusive(aConstant(pin)), to: butExclusive(aConstant(pin))},
	}
}

// TestEveryShapeOfDeclarationDeclaresAndReadsOneStretchBothWays runs the
// whole list of shapes against a secondary index and against the clustered
// one, which is the only index where a bound can land exactly on a real key:
// every entry of a secondary index carries the primary key on its tail, so a
// bound on the declared fields alone never equals one.
//
// Round three's version of this test had an accepted/refused axis: a shape
// was right to declare exactly when its forward and reverse reachable sets
// came out equal, and wrong otherwise. There is no such axis now — nothing
// here is refused for its direction — so what is left is checkShape's three
// per-call checks (see its doc comment), run for every shape on the list.
func TestEveryShapeOfDeclarationDeclaresAndReadsOneStretchBothWays(t *testing.T) {
	for _, over := range []struct {
		index  string
		pin    string
		domain []string
	}{
		{
			index: "by_slug", pin: "s3",
			// Values on both sides of every row, values between rows, and
			// values that are no row at all — including the empty string.
			// "" is not "a value below the lowest row": there is no string
			// below it at all, so an exclusive bound of "" has nothing to
			// step past FROM. Whether that shows up as a leak depends on
			// whether some row actually sits there, which is why "" alone in
			// this list proves nothing — fillWithFloor puts a row at that
			// floor below.
			domain: []string{"", "s0", "s1", "s2", "s3", "s3x", "s4", "s5", "s6", "t"},
		},
		{
			index: ClusteredIndex, pin: "a3",
			domain: []string{"", "a0", "a1", "a2", "a3", "a3x", "a4", "a5", "a6", "b"},
		},
	} {
		t.Run(over.index, func(t *testing.T) {
			store, collection := declared(t, 130)
			fillWithFloor(t, collection)

			shapes := reachShapes(over.pin)
			if len(shapes) != 20 {
				t.Fatalf("reachShapes gave %d shapes, want 20 — this list is the point of the test; a shorter one is a narrower promise wearing the same name", len(shapes))
			}
			rows := rowsOf(t, collection, over.index)

			for i, shape := range shapes {
				name := fmt.Sprintf("reach.%s.%d", over.index, i)
				if err := declareReach(store, name, over.index, shape); err != nil {
					t.Errorf("%s: %q was refused: %v — no shape of declaration is refused for its direction any more",
						over.index, shape.what, err)
				}
				checkShape(t, collection, over.index, shape, over.domain, rows)
			}
		})
	}
}

// fillWithFloor is fill() plus one document at the floor of the type: the
// empty string, for both the slug and the primary key.
//
// fill() writes exactly six rows and several other tests count on that, which
// is why the floor row is added here rather than in fill() itself. It is what
// makes mục 1's leak observable at all: an argument's domain can contain ""
// without any shape looking like it leaks, because "" was never the value of
// an actual row before this. See the file comment above for what QA's
// mutate27.py found when "" was removed from the domain instead of added to
// the fixture — the opposite of what the leak needs.
func fillWithFloor(t *testing.T, collection *Collection) {
	t.Helper()
	fill(t, collection)
	// Put accepts an explicit empty-string key: the check is "found and not
	// nil", and "" is found and is not nil. Auto only generates a key when
	// the field is absent from the document altogether.
	put(t, collection, map[string]any{"id": "", "slug": ""})
}

// A mutation that deletes the `put` call above and leaves fill() alone is not
// caught by this file's own tests: checkShape's oracle is computed from
// whichever rows actually exist, so removing the floor row from the fixture
// removes it from the oracle too, and every assertion stays consistent with
// itself. That was round two's blind spot, measured by round three's QA, and
// it still exists here on purpose — this fixture is not the only place the
// floor row is exercised. TestByAuthorFloorRowReadsOneStretchBothWays and
// TestTheTwoBoundsNeverDependOnDirection (direction_v4_test.go) build their
// own fixtures, and the second does not depend on any row existing at all —
// it compares stretch()'s byte output directly. So the redundancy is real,
// not assumed, and this comment is where that gets checked: a mutation that
// removes the floor row from THIS fixture is one of round four's harness
// mutants (see the task), and it is expected to leave this file blind while
// the package as a whole still catches the underlying regression.

// TestAHalfPinnedPartitionedIndexScanIsStillRefusedByScanAcrossItself checks
// that removing checkDirection's rule (round four) did not accidentally widen
// scanAcross's, which is a different rule with a different reason: a scan on
// a partitioned SECONDARY index must fix every declared field so the
// partitions concatenate in key order, and a constant at one end with an
// argument at the other never fixes anything. That refusal has nothing to do
// with Direction — it fires the same with no Direction at all — so it is
// unaffected by this round and is here to say so.
func TestAHalfPinnedPartitionedIndexScanIsStillRefusedByScanAcrossItself(t *testing.T) {
	_, _, store := partitioned(t, 132)
	entriesByMonth(t, store, 0)

	// One account, read either way: pinned with the same constant at both ends,
	// which is the shape scanAcross forces anyway. It stays declarable.
	pinned := Operation{
		Name:       "entries.either_way",
		Collection: "entries",
		Action:     ActionScan,
		Index:      "by_account",
		Input:      []Parameter{{Name: "direction", Type: TypeString, Required: true}},
		From:       &Endpoint{Terms: []Term{{Value: "cash"}}},
		To:         &Endpoint{Terms: []Term{{Value: "cash"}}},
		Direction:  &Term{Arg: "direction"},
		Limit:      10,
	}
	if _, err := store.DeclareOperation(pinned); err != nil {
		t.Fatalf("one account read either way should still be declarable: %v", err)
	}

	// The same pin at one end only. scanAcross refuses this for its own
	// reason — a partitioned secondary index scan must fix every field — not
	// because of anything to do with direction; a fixed direction is refused
	// exactly the same way.
	half := pinned
	half.Name = "entries.half"
	half.To = &Endpoint{Terms: []Term{{Arg: "edge"}}}
	half.Input = append([]Parameter{{Name: "edge", Type: TypeString, Required: true}}, half.Input...)
	if _, err := store.DeclareOperation(half); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a half-pinned partitioned scan, direction an argument: want ErrDeclaration, got %v", err)
	}
	half.Direction = &Term{Value: DirectionForward}
	half.Name = "entries.half_fixed"
	if _, err := store.DeclareOperation(half); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a half-pinned partitioned scan, direction fixed: want ErrDeclaration, got %v", err)
	}
}

// TestAConstantAtOneEndOfAPartitionedClusteredWalkDeclaresAndReadsBothWays is
// the shape TestAConstantIsRefusedEvenWhereScanAcrossWouldHaveCaughtIt used to
// be named for: the clustered walk of a partitioned collection goes through
// neither scanAcross nor an index at all, so whatever a constant at one end
// only does here is entirely up to checkDirection. Rounds one through three
// refused it — a constant at one end was exactly the shape
// boundsSurviveReversal existed to catch. Round four removed that rule along
// with the reason for it, so this shape now declares, and forward and
// reverse read the same stretch, backwards on the second call, same as every
// other shape in this file.
func TestAConstantAtOneEndOfAPartitionedClusteredWalkDeclaresAndReadsBothWays(t *testing.T) {
	_, _, store := partitioned(t, 132)
	entries := entriesByMonth(t, store, 0)

	var written []string
	for _, when := range []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
	} {
		atMonth(t, store, when)
		for i := 0; i < 2; i++ {
			written = append(written, fmt.Sprint(put(t, entries, map[string]any{"account": "cash"})))
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	clustered := Operation{
		Name:       "entries.from_floor",
		Collection: "entries",
		Action:     ActionScan,
		Index:      ClusteredIndex,
		Input: []Parameter{
			{Name: "edge", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:       &Endpoint{Terms: []Term{{Value: ""}}},
		To:         &Endpoint{Terms: []Term{{Arg: "edge"}}},
		Direction:  &Term{Arg: "direction"},
		Projection: []string{"id"},
		Limit:      10,
	}
	declareOp(t, store, clustered)

	// Keys are ulids and partition names sort in the order the periods
	// happened, so written is already forward key order: the floor of the
	// string type up to written[2] is written[0], written[1], written[2].
	forward := invoke(t, store, "entries.from_floor", map[string]any{"edge": written[2], "direction": DirectionForward})
	if got := idsOf(forward.Rows); fmt.Sprint(got) != fmt.Sprint(written[:3]) {
		t.Errorf("forward from the floor to the third key gave %v, want %v", got, written[:3])
	}

	reverse := invoke(t, store, "entries.from_floor", map[string]any{"edge": written[2], "direction": DirectionReverse})
	want := []string{written[2], written[1], written[0]}
	if got := idsOf(reverse.Rows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("reverse over the same declared stretch gave %v, want %v", got, want)
	}
}
