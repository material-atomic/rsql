package store

import (
	"errors"
	"fmt"
	"sort"
	"testing"
)

// This file exists to break one sentence: "a direction widens nothing" — the
// set of rows a declaration can hand back is the same whichever direction the
// caller picks.
//
// The round before this one had a test by that name. It declared both ends as
// arguments, which is the one shape where the sentence is easy to keep, and the
// name bought confidence for every other shape. A constant at one end walked
// straight through it.
//
// So nothing here picks a shape. It enumerates the shapes a bound can be
// written in, crossed with itself, and for each one enumerates every value a
// caller could pass, and compares the two reachable sets row for row. Then it
// asserts the sharper thing: a shape is accepted at declaration time IF AND
// ONLY IF the two sets come out equal. A refusal that is not earned fails this
// as loudly as a widening that is not refused.
//
// The measurement is at the Collection, not through Invoke, because half the
// shapes are ones the store now refuses and they still have to be measured.
//
// This round found the enumeration itself had a hole: the DOMAIN a caller's
// argument may range over had "" in it, but no actual ROW ever carried "".
// Crossing an argument domain against itself only ever sees what a caller
// could pass; it says nothing about what is stored, and the leak in mục 1 is
// about a row sitting at the floor of the index's own byte encoding, not
// about "" as a value somebody happened to type. So the fixture now carries a
// row at that floor (fillWithFloor, below) and the assertions compare against
// it directly, not through the domain. QA's mutate27.py measured this by
// removing "" from the domain and watching the harness report MORE leaking
// shapes, not fewer — "" in the domain, with no row there to find, was
// quietly absorbing calls that would otherwise land past every row and come
// back empty either way, which reads as "the two directions agree" even
// though neither direction reached anything.
//
// Two things this file measures and does NOT decide are written down here
// once rather than at every place they would otherwise look unexamined:
//
//   - Optional arguments with no default are the one shape checkDirection
//     already refuses outright (see TestADirectionThatCannotBeWorkedOutIs...
//     in direction_test.go), so a bound that could fall away at call time
//     never reaches boundsSurviveReversal. That is a real hole in what "a
//     direction widens nothing" covers — the measurement here runs at the
//     Collection, past every argument as Required — but it is task 0016's
//     hole, not this one's: QA checked by hand that it does not leak on
//     reachability (both directions' unions equal the whole index), and nothing
//     in this round makes that hole any different.
//   - Limit is not part of what "reaches" measures. declareReach writes
//     Limit: 50 because a scan must declare one to be valid at all, and
//     TestALimitStopsAScanFromEitherEnd (direction_test.go) already measures
//     that it cuts both directions the same way. It is decoration here, not a
//     bound reaches() applies.
//
// Composite bounds (more than one term at an end) and a Term{Step: ...} at a
// bound are outside reachShapes on purpose: reachShapes writes exactly one
// term per end, so the whole composite column of the shape table is measured
// instead by TestByAuthorMissingPublishedSitsAtTheFloorOfItsAuthorsGroup and
// by direction_test.go's TestTheEndsOfARangeSwapWhenTheScanIsReversed, and a
// Step term at a scan bound is refused by check() before boundsSurviveReversal
// is ever called (QA measured this; see mục 1 of the round 3 task). Widening
// reachShapes to write several terms per end is a task of its own if anyone
// needs it measured this way instead.

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
	why  string

	// pairedArgument is true when From and To name the SAME argument. Invoke
	// has exactly one slot for that name, so a real call can never give the
	// two ends different values — crossing a domain against itself for both
	// ends independently would manufacture a call nobody could make and
	// measure a shape that does not exist. reaches() reads this to walk the
	// domain once instead of crossing it with itself.
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
// order, which is exactly what reachShapes writes and reaches measures; see
// the file comment above for what that leaves out and where that is measured
// instead.
func expectedReach(rows []reachRow, shape reachShape, argFrom, argTo string, direction Direction) map[string]bool {
	// walk() treats From as where the walk starts and To as where it ends,
	// and swaps that when the direction is reversed. "The low end" here means
	// exactly that: whichever of shape.from/shape.to plays From in the
	// direction being measured, and whichever argument value the caller gave
	// that end.
	lowEnd, lowValue := shape.from, argFrom
	highEnd, highValue := shape.to, argTo
	if direction == Reverse {
		lowEnd, lowValue = shape.to, argTo
		highEnd, highValue = shape.from, argFrom
	}
	value := func(end reachEnd, argValue string) string {
		if end.constant {
			return end.fixed
		}
		return argValue
	}

	seen := map[string]bool{}
	for _, row := range rows {
		if lowEnd.written() {
			v := value(lowEnd, lowValue)
			if lowEnd.exclusive {
				if row.value <= v {
					continue
				}
			} else if row.value < v {
				continue
			}
		}
		if highEnd.written() {
			v := value(highEnd, highValue)
			if highEnd.exclusive {
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

// reaches is every row this pair of ends can put in front of a caller walking
// the index that way, over every pair of values the caller could pass. Each
// individual call is also checked against expectedReach — the oracle — before
// its rows are folded into the union this function returns, because the union
// alone absorbs a single call's off-by-one into rows some other call already
// contributed. See TestAnExclusiveEndIsOutsideTheStretchWhicheverWayTheWalkEnters
// in direction_qa2_test.go for the mutation that survived exactly that hole.
func reaches(t *testing.T, collection *Collection, index string, shape reachShape,
	domain []string, rows []reachRow, direction Direction) map[string]bool {

	t.Helper()
	seen := map[string]bool{}

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

	for _, p := range pairs {
		within := Range{
			From:      shape.from.bound(p.low),
			To:        shape.to.bound(p.high),
			Direction: direction,
		}
		call := map[string]bool{}
		var err error
		if index == ClusteredIndex {
			// The clustered walk is the documents themselves, and it is a
			// different call: walkRange rather than Scan. It is in here
			// because it is the only walk whose bounds can land exactly on
			// a stored key.
			err = collection.walkRange(within, func(key any, _ map[string]any) bool {
				call[fmt.Sprint(key)] = true
				return true
			})
		} else {
			err = collection.Scan(index, within, func(one Found) bool {
				// Grouped by the row (its primary key), not by the value the
				// index stores for it. Two documents sharing an indexed value
				// are two rows; grouping by the value would let a widened
				// scan that only picks up a second document with a value
				// already in the set read as "reached nothing new".
				call[fmt.Sprint(one.Key)] = true
				return true
			})
		}
		if err != nil {
			t.Fatalf("%s at %q/%q: %v", shape.what, p.low, p.high, err)
		}

		want := expectedReach(rows, shape, p.low, p.high, direction)
		if sorted(call) != sorted(want) {
			t.Errorf("%s %v at %q/%q: the walk gave %v, the oracle says %v",
				shape.what, direction, p.low, p.high, sorted(call), sorted(want))
		}

		for key := range call {
			seen[key] = true
		}
	}
	return seen
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
// list is the point: leaving a shape out is how the last round's hole
// survived, and the count is asserted where this is used so that trimming the
// list is itself a failure.
//
// The first 13 are the round-two list, unchanged. The last 7 close the gaps
// round two's own QA found: both ends exclusive, an exclusive argument against
// an unwritten end (and its mirror), the same argument named at both ends, and
// the same-terms exception applied to an argument pair as well as a constant
// pair with Exclusive on the other side and on both sides.
func reachShapes(pin string) []reachShape {
	lo, hi := anArgument("lo"), anArgument("hi")
	return []reachShape{
		{what: "both ends arguments", from: lo, to: hi,
			why: "reversing only swaps which end each value the caller passes is"},
		{what: "both ends arguments, From exclusive", from: butExclusive(lo), to: hi,
			why: "an exclusive end the caller controls can always be stepped past from the other side"},
		{what: "both ends arguments, To exclusive", from: lo, to: butExclusive(hi),
			why: "the mirror of the one above"},
		{what: "an argument at From, nothing at To", from: lo, to: nothing(),
			why: "an absent end is the rest of the index, so each direction covers what the other leaves"},
		{what: "nothing at From, an argument at To", from: nothing(), to: hi,
			why: "the mirror of the one above"},
		{what: "neither end written", from: nothing(), to: nothing(),
			why: "the whole index either way"},
		{what: "the same constant at both ends", from: aConstant(pin), to: aConstant(pin),
			why: "a pin confines the stretch whichever way the walk enters it"},
		{what: "the same constant at both ends, From exclusive", from: butExclusive(aConstant(pin)), to: aConstant(pin),
			why: "still a pin, and an empty one in both directions"},
		{what: "a constant at From, an argument at To", from: aConstant(pin), to: hi,
			why: "the floor the constant pins becomes a ceiling, and everything under it comes back"},
		{what: "an argument at From, a constant at To", from: lo, to: aConstant(pin),
			why: "the mirror: the ceiling becomes a floor"},
		{what: "a constant at From, nothing at To", from: aConstant(pin), to: nothing(),
			why: "forward runs from the constant to the end of the index, reverse from it to the start"},
		{what: "nothing at From, a constant at To", from: nothing(), to: aConstant(pin),
			why: "the mirror of the one above"},
		{what: "constants at both ends, different values", from: aConstant(pin), to: aConstant(pin + "x"),
			why: "one direction is the whole stretch and the other is empty"},

		// Round 3's additions.
		{what: "both ends arguments, both exclusive", from: butExclusive(lo), to: butExclusive(hi),
			why: "each end is still something the caller chose, whichever end it lands on after a reversal"},
		{what: "an argument at From exclusive, nothing at To", from: butExclusive(lo), to: nothing(),
			why: "forward can never step past the floor from below it; reversed, the same declaration starts AT the floor and includes it — the two directions disagree about the one row furthest down"},
		{what: "nothing at From, an argument at To exclusive", from: nothing(), to: butExclusive(hi),
			why: "the mirror of the one above"},
		{what: "the same argument at both ends", from: anArgument("lo"), to: anArgument("lo"),
			why:            "one value, asked for once, is the same stretch from either end",
			pairedArgument: true},
		{what: "the same argument at both ends, From exclusive", from: butExclusive(anArgument("lo")), to: anArgument("lo"),
			why:            "an open point is empty from either side, which is the exception that keeps a half-open keyset cursor declarable",
			pairedArgument: true},
		{what: "the same constant at both ends, To exclusive", from: aConstant(pin), to: butExclusive(aConstant(pin)),
			why: "still a pin, empty in both directions — the mirror of the From-exclusive pin above"},
		{what: "the same constant at both ends, both exclusive", from: butExclusive(aConstant(pin)), to: butExclusive(aConstant(pin)),
			why: "a pin open on both sides of itself is still empty either way"},
	}
}

// TestNoShapeOfDeclarationLetsADirectionWidenWhatAScanCanReach runs the whole
// cross-product against a secondary index and against the clustered one, which
// is the only index where a bound can land exactly on a real key: every entry
// of a secondary index carries the primary key on its tail, so a bound on the
// declared fields alone never equals one.
func TestNoShapeOfDeclarationLetsADirectionWidenWhatAScanCanReach(t *testing.T) {
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
				forward := reaches(t, collection, over.index, shape, over.domain, rows, Forward)
				reverse := reaches(t, collection, over.index, shape, over.domain, rows, Reverse)
				same := sorted(forward) == sorted(reverse)

				name := fmt.Sprintf("reach.%s.%d", over.index, i)
				err := declareReach(store, name, over.index, shape)
				accepted := err == nil
				if !accepted && !errors.Is(err, ErrDeclaration) {
					t.Fatalf("%s: %q was refused for the wrong reason: %v", over.index, shape.what, err)
				}

				// The whole assertion, both ways round. A shape that widens and
				// is allowed is the regression this round fixed. A shape that
				// does not widen and is refused is a feature taken away for
				// nothing, which is just as wrong and much quieter.
				if accepted != same {
					t.Errorf("%s: %q — reachable forward %v, reachable reversed %v, declaration accepted=%v (%s)",
						over.index, shape.what, sorted(forward), sorted(reverse), accepted, shape.why)
				}
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
// caught by TestNoShapeOfDeclarationLetsADirectionWidenWhatAScanCanReach on
// its own: measured mutate3.py, disabling mệnh đề 2 with that call removed
// reports zero leaking shapes from this test — this file's own harness goes
// back to exactly round 2's blind spot. It is caught at the package level,
// by TestAnExclusiveEndAloneIsRefusedAtDeclarationNotJustMeasured and
// TestByAuthorMissingPublishedSitsAtTheFloorOfItsAuthorsGroup in
// direction_v3_test.go, which build their own fixtures and do not call this
// function — so the redundancy is real, not assumed. Documented here rather
// than "fixed" further because the fix IS this function; the failure mode is
// "someone deletes the one line that matters", and the next paragraph of
// defence against that is a second, independently-fixtured test, which
// already exists.

// TestAConstantIsRefusedEvenWhereScanAcrossWouldHaveCaughtIt: a partitioned
// collection is where the constant shape looks like somebody else's problem,
// because scanAcross already insists the two ends carry the same terms. It does
// — for the fields a SECONDARY index declares. The clustered walk of a
// partitioned collection goes through neither that check nor an index, so the
// direction has to refuse the constant shape on its own account.
func TestAConstantIsRefusedEvenWhereScanAcrossWouldHaveCaughtIt(t *testing.T) {
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

	// The same pin at one end only. scanAcross would refuse this for its own
	// reason, so it proves nothing about the direction on its own — it is here
	// so that the partitioned column of the table has no blank in it.
	half := pinned
	half.Name = "entries.half"
	half.To = &Endpoint{Terms: []Term{{Arg: "edge"}}}
	half.Input = append([]Parameter{{Name: "edge", Type: TypeString, Required: true}}, half.Input...)
	if _, err := store.DeclareOperation(half); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a half-pinned partitioned scan: want ErrDeclaration, got %v", err)
	}

	// The clustered walk of the same collection is the one scanAcross waves
	// straight through, and it is where the direction check stands alone.
	clustered := Operation{
		Name:       "entries.by_key",
		Collection: "entries",
		Action:     ActionScan,
		Index:      ClusteredIndex,
		Input: []Parameter{
			{Name: "edge", Type: TypeString, Required: true},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:      &Endpoint{Terms: []Term{{Value: "01"}}},
		To:        &Endpoint{Terms: []Term{{Arg: "edge"}}},
		Direction: &Term{Arg: "direction"},
		Limit:     10,
	}
	if _, err := store.DeclareOperation(clustered); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a constant floor on a partitioned clustered walk: want ErrDeclaration, got %v", err)
	}
}
