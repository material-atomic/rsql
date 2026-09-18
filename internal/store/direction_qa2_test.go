package store

import (
	"fmt"
	"testing"
)

// TestAnExclusiveEndIsOutsideTheStretchWhicheverWayTheWalkEnters pins the two
// far-bound comparisons in walk, one per direction.
//
// The clustered walk is the one place a bound lands exactly on a stored key —
// an entry of a secondary index carries the primary key on its tail, so a bound
// on the declared fields alone is never equal to one — which makes it the only
// walk where inclusive and exclusive are told apart by a row rather than by
// arithmetic. QA found the forward half of this untested: turning
// `Compare(key, upper) >= 0` into `> 0` hands the bound row back and the whole
// repo stayed green. The reversed half was already covered; it is here beside
// its mirror so the next person changing the `bounded` closure in walk
// (scan.go, the two Compare calls) breaks both at once. That closure, not
// bound(), is where these two live: bound() works out the byte position each
// end sits at, and `bounded` decides which side of it a key has to be on.
//
// Round four removed the swap that used to put To in the "far end, entered
// first" position when reversed: From and To no longer trade places, so the
// reversed cases below read the SAME declared stretch as the forward ones,
// direction flipped, rather than a mirrored one written with the ends
// exchanged. A5 was s4 exclusive under the old convention (the walk's far
// end being where it starts, which used to be the lower one); under this one
// it stays To, the high end, in both directions — the far end for a reversed
// walk is now From, the low end, which is why the exclusive bound moves
// there below.
//
// reach_test.go used to miss this: round three's version of checkShape
// unioned every call's rows across the whole argument domain before
// comparing directions, and a row excluded by one call was reached by
// another, so the union saturated and an off-by-one in a single call left it
// unchanged. Round four rewrote checkShape to compare each call against an
// oracle computed straight off the fixture rather than against the union, and
// that per-call comparison does catch this mutation on its own —
// TestEveryShapeOfDeclarationDeclaresAndReadsOneStretchBothWays turns red on
// exactly this change to the far-end comparison. This test stays anyway: it
// pins the two comparisons at the byte level, at the place they are written
// rather than through rows read out of a harness, on the one walk (clustered)
// where a bound can land exactly on a stored key — which is a narrower and
// cheaper thing to read than the shape harness when it goes red.
func TestAnExclusiveEndIsOutsideTheStretchWhicheverWayTheWalkEnters(t *testing.T) {
	_, collection := declared(t, 133)
	fill(t, collection)

	read := func(within Range) []string {
		t.Helper()
		got := []string{}
		if err := collection.walkRange(within, func(key any, _ map[string]any) bool {
			got = append(got, fmt.Sprint(key))
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	for _, one := range []struct {
		what   string
		within Range
		want   []string
	}{
		{
			// Forward: To is the far end, and a4 is a document that exists.
			what: "forward, the far end (To) exclusive",
			within: Range{From: &Bound{Values: []any{"a1"}},
				To: &Bound{Values: []any{"a4"}, Exclusive: true}},
			want: []string{"a1", "a2", "a3"},
		},
		{
			what: "forward, the far end (To) inclusive",
			within: Range{From: &Bound{Values: []any{"a1"}},
				To: &Bound{Values: []any{"a4"}}},
			want: []string{"a1", "a2", "a3", "a4"},
		},
		{
			// Reversed, the SAME declared stretch, entered from the other
			// side: From is now the far end — the one the walk starts below
			// and stops just short of — and a1 is a document that exists.
			what: "reversed, the far end (From) exclusive",
			within: Range{From: &Bound{Values: []any{"a1"}, Exclusive: true},
				To: &Bound{Values: []any{"a4"}}, Direction: Reverse},
			want: []string{"a4", "a3", "a2"},
		},
		{
			what: "reversed, the far end (From) inclusive",
			within: Range{From: &Bound{Values: []any{"a1"}},
				To: &Bound{Values: []any{"a4"}}, Direction: Reverse},
			want: []string{"a4", "a3", "a2", "a1"},
		},
	} {
		got := read(one.within)
		if fmt.Sprint(got) != fmt.Sprint(one.want) {
			t.Errorf("%s: got %v, want %v", one.what, got, one.want)
		}
	}
}
