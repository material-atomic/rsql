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
}

func sorted(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, fmt.Sprintf("%q", value))
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}

// reaches is every row this pair of ends can put in front of a caller walking
// the index that way, over every pair of values the caller could pass.
func reaches(t *testing.T, collection *Collection, index string, shape reachShape,
	domain []string, direction Direction) map[string]bool {

	t.Helper()
	seen := map[string]bool{}

	froms, tos := domain, domain
	if shape.from.arg == "" {
		froms = []string{""}
	}
	if shape.to.arg == "" {
		tos = []string{""}
	}
	for _, low := range froms {
		for _, high := range tos {
			within := Range{
				From:      shape.from.bound(low),
				To:        shape.to.bound(high),
				Direction: direction,
			}
			var err error
			if index == ClusteredIndex {
				// The clustered walk is the documents themselves, and it is a
				// different call: walkRange rather than Scan. It is in here
				// because it is the only walk whose bounds can land exactly on
				// a stored key.
				err = collection.walkRange(within, func(key any, _ map[string]any) bool {
					seen[fmt.Sprint(key)] = true
					return true
				})
			} else {
				err = collection.Scan(index, within, func(one Found) bool {
					seen[fmt.Sprint(one.Values[0])] = true
					return true
				})
			}
			if err != nil {
				t.Fatalf("%s at %q/%q: %v", shape.what, low, high, err)
			}
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
// list is the point: leaving a shape out is how the last round's hole survived.
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
			// values that are no row at all — including the empty string,
			// which is the bottom of the type and the one place an exclusive
			// bound has nothing below it to be stepped past from.
			domain: []string{"", "s0", "s1", "s2", "s3", "s3x", "s4", "s5", "s6", "t"},
		},
		{
			index: ClusteredIndex, pin: "a3",
			domain: []string{"", "a0", "a1", "a2", "a3", "a3x", "a4", "a5", "a6", "b"},
		},
	} {
		t.Run(over.index, func(t *testing.T) {
			store, collection := declared(t, 130)
			fill(t, collection)

			for i, shape := range reachShapes(over.pin) {
				forward := reaches(t, collection, over.index, shape, over.domain, Forward)
				reverse := reaches(t, collection, over.index, shape, over.domain, Reverse)
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
