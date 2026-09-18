package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// These three bounds are the actual answer to "how wide does a table over a
// universal claim have to be before it stops being a sample". QA measured
// this directly rather than picking a round number: with the array test's
// length loop capped at 8, the map test's key-count loop capped at 6, and
// the depth test's nest loop capped at 6, five typo-shaped mutants survived
// — a mutant is not obligated to be caught the moment it exists, but a
// wall that sits exactly on a table's own boundary (compare only the first
// N elements/keys, skip exactly index N, treat anything past depth N as
// equal) means the boundary was doing the mutant's job for it. Widening to
// these three numbers killed every one of them, and nobody has since found
// a typo-shaped bug that only shows up past them. That is still a bounded
// claim, not a proof for every possible length — say so here instead of
// letting a test named "every element" or "every depth" imply otherwise,
// and if a future mutant survives specifically past one of these numbers,
// the fix is to raise the constant again and record why, not to patch in
// one more hand-picked case.
const (
	maxArrayAxisLength = 37 // TestSameValueComparesEveryArrayElementNotJustTheFirst
	maxMapAxisKeys     = 23 // TestSameValueRequiresEveryMapKeyToBePresentNotJustCounted
	maxNestDepth       = 17 // TestSameValueAgreesOnEqualAndDifferentValuesAtEveryDepth
)

// sameValueDomain is every shape of value JSON can hand sameValue: the six
// JSON kinds (null, bool, string, number, array, object), each present in
// more than one instance so equal-vs-different is exercised too, and each
// container present empty, with a plain element, and holding another
// container so recursion actually recurses. It also carries a bare Go int
// alongside the float64s — real calls never produce one (see
// TestJSONNumbersAreFloat64AtEveryDepthOnBothSidesOfTheWire below), but a
// Term built in Go source, the way every test in this package builds one,
// does, so the domain a Go caller of this store can reach is wider than the
// domain a network caller can.
func sameValueDomain() []any {
	return []any{
		nil,
		true,
		false,
		"a",
		"b",
		"",
		0.0,
		1.0,
		2.0,
		1, // int: the Go-API-only shape asNumber also has to accept
		[]any{},
		[]any{1.0},
		[]any{2.0},
		[]any{1},
		[]any{1.0, 2.0},
		[]any{2.0, 1.0},
		[]any{1.0, 2.0, 3.0},
		[]any{nil},
		[]any{[]any{1.0}},
		[]any{map[string]any{"a": 1.0}},
		map[string]any{},
		map[string]any{"a": 1.0},
		map[string]any{"a": 2.0},
		map[string]any{"a": 1},
		map[string]any{"a": 1.0, "b": 2.0},
		map[string]any{"a": []any{1.0, 2.0}},
	}
}

// TestSameValueNeverPanicsOnAnyPairJSONCanProduce is the table the "not
// panic" promise has to survive. sameValueDomain is the entire domain, not a
// sample of it — every JSON kind, every container shape a batch condition can
// be checked against — and this runs every pair of it, in both directions,
// which is what closes the pre-fix bug: left == right panicked exactly when
// both sides shared a dynamic type Go cannot compare, and both a []any and a
// map[string]any pair are in this domain on both sides.
func TestSameValueNeverPanicsOnAnyPairJSONCanProduce(t *testing.T) {
	domain := sameValueDomain()
	for i, a := range domain {
		for j, b := range domain {
			t.Run(fmt.Sprintf("%d_vs_%d", i, j), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("sameValue(%#v, %#v) panicked: %v", a, b, r)
					}
				}()
				_ = sameValue(a, b)
			})
		}
	}
}

// TestSameValueAgreesOnEqualAndDifferentValuesAtEveryDepth is the other half
// of "not panic": a comparison that stopped panicking by always answering
// true, or always false, or by giving up after one level, would pass the
// test above and still be useless. Every case here is checked both ways,
// because nothing in sameValue's signature promises it is symmetric and a
// mutant that special-cases one side over the other should not get to hide
// behind an argument order this suite never tries in reverse.
//
// QA's round on this task caught the name promising more than the literal
// cases below checked: "every depth" stopped being true of the body at
// depth two. The loop at the end of this function is the fix — it walks a
// generated nest from depth 0 to depth maxNestDepth instead of a
// handwritten literal, so "every depth" is a claim about a parameter, not
// about how many cases somebody remembered to type in by hand.
//
// A later QA round measured that a bound of 6 was itself a wall, not a
// closed axis: a mutant that treats anything nested past depth 7 as equal
// survived, because nothing in this file ever built a value that deep.
// Widening to maxNestDepth (17, see the constant's own comment) killed it
// with no other change. This is still a bounded claim, not an infinite one
// — say so plainly: what this loop closes is "every depth from 0 through
// maxNestDepth", not "every depth that exists". Nobody has produced a
// typo-shaped bug that only shows up past that bound; if one turns up, the
// fix is to raise the constant again, not to add one more hand-picked case.
func TestSameValueAgreesOnEqualAndDifferentValuesAtEveryDepth(t *testing.T) {
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"nil equals nil", nil, nil, true},
		{"nil is not false", nil, false, false},
		{"nil is not an empty array", nil, []any{}, false},
		{"nil is not an empty object", nil, map[string]any{}, false},
		// asNumber has no case for nil and returns 0 for it, same as it does
		// for any type it does not recognize — that is fine as long as the
		// number branch is never entered with a nil operand. It is guarded by
		// left != nil and right != nil separately (not "both or neither"),
		// so this is the one value pair where dropping either guard alone
		// would make sameValue agree that nil is the number zero.
		{"nil is not the number zero", nil, 0.0, false},

		{"empty arrays are equal", []any{}, []any{}, true},
		{"equal arrays", []any{1.0, 2.0}, []any{1.0, 2.0}, true},
		{"same elements, different order", []any{1.0, 2.0}, []any{2.0, 1.0}, false},
		{"a longer array with the same prefix", []any{1.0, 2.0}, []any{1.0, 2.0, 3.0}, false},
		{"an array is not an object even sharing a shape", []any{1.0}, map[string]any{"0": 1.0}, false},
		{"a nested array compared by value, not by identity", []any{[]any{1.0}}, []any{[]any{1.0}}, true},
		{"a nested array that differs one level down", []any{[]any{1.0}}, []any{[]any{2.0}}, false},

		{"empty objects are equal", map[string]any{}, map[string]any{}, true},
		{"equal objects", map[string]any{"a": 1.0}, map[string]any{"a": 1.0}, true},
		{"a different value under the same key", map[string]any{"a": 1.0}, map[string]any{"a": 2.0}, false},
		{"an object with one extra key", map[string]any{"a": 1.0}, map[string]any{"a": 1.0, "b": 2.0}, false},
		{"a nested object that differs one level down",
			map[string]any{"a": map[string]any{"b": 1.0}},
			map[string]any{"a": map[string]any{"b": 2.0}}, false},

		// The rule the godoc names: 1 and 1.0 are the same number, at the
		// top and at every depth a container can put one, because a
		// document written through the Go API and a document written from
		// JSON must satisfy the same condition the same way.
		{"an int equals a float64 at the outer layer", 1, 1.0, true},
		{"an int equals a float64 inside an array", []any{1}, []any{1.0}, true},
		{"an int equals a float64 inside an object", map[string]any{"n": 1}, map[string]any{"n": 1.0}, true},
		{"a different number inside an array is still different", []any{1.0}, []any{2.0}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameValue(c.a, c.b); got != c.want {
				t.Errorf("sameValue(%#v, %#v) = %v, want %v", c.a, c.b, got, c.want)
			}
			if got := sameValue(c.b, c.a); got != c.want {
				t.Errorf("sameValue is not symmetric here: sameValue(%#v, %#v) = %v, want %v", c.b, c.a, got, c.want)
			}
		})
	}

	// "Every depth", checked at every depth from 0 (no container at all)
	// through maxNestDepth, rather than at the two depths the literal cases
	// above happen to reach. nest and nestMap build the container fresh for
	// each depth instead of being handwritten, so this is the same claim as
	// the cases above, parameterized instead of sampled.
	nest := func(depth int, leaf any) any {
		v := leaf
		for i := 0; i < depth; i++ {
			v = []any{v}
		}
		return v
	}
	nestMap := func(depth int, leaf any) any {
		v := leaf
		for i := 0; i < depth; i++ {
			v = map[string]any{"k": v}
		}
		return v
	}
	for depth := 0; depth <= maxNestDepth; depth++ {
		t.Run(fmt.Sprintf("array nest depth %d, equal", depth), func(t *testing.T) {
			if !sameValue(nest(depth, 1.0), nest(depth, 1.0)) {
				t.Errorf("equal values nested %d deep in arrays compared unequal", depth)
			}
		})
		t.Run(fmt.Sprintf("array nest depth %d, differ at the bottom", depth), func(t *testing.T) {
			if sameValue(nest(depth, 1.0), nest(depth, 2.0)) {
				t.Errorf("different values nested %d deep in arrays compared equal", depth)
			}
		})
		t.Run(fmt.Sprintf("array nest depth %d, int equals float at the bottom", depth), func(t *testing.T) {
			if !sameValue(nest(depth, 1), nest(depth, 1.0)) {
				t.Errorf("the int/float64 rule was dropped %d deep in arrays", depth)
			}
		})
		t.Run(fmt.Sprintf("object nest depth %d, equal", depth), func(t *testing.T) {
			if !sameValue(nestMap(depth, 1.0), nestMap(depth, 1.0)) {
				t.Errorf("equal values nested %d deep in objects compared unequal", depth)
			}
		})
		t.Run(fmt.Sprintf("object nest depth %d, differ at the bottom", depth), func(t *testing.T) {
			if sameValue(nestMap(depth, 1.0), nestMap(depth, 2.0)) {
				t.Errorf("different values nested %d deep in objects compared equal", depth)
			}
		})
		// QA's next round caught that this loop had the int/float64 case for
		// the array branch (nest) but not for the object branch (nestMap) —
		// "every depth" was only true of arrays, half the reason a mutant
		// that dropped the object recursion still passed everything the
		// name promised to check.
		t.Run(fmt.Sprintf("object nest depth %d, int equals float at the bottom", depth), func(t *testing.T) {
			if !sameValue(nestMap(depth, 1), nestMap(depth, 1.0)) {
				t.Errorf("the int/float64 rule was dropped %d deep in objects", depth)
			}
		})
	}
}

// TestSameValueComparesEveryArrayElementNotJustTheFirst closes an axis QA's
// round on this task exposed rather than patching it with one more example:
// sameValueDomain never paired two arrays that agreed at index 0 and
// disagreed further in, so a mutant that stops comparing after the first
// element survived every test in this file. That mutant is not academic —
// sameValue(["a","b"], ["a","z"]) answering true is exactly the shape of an
// optimistic-locking hole a client can trigger: a batch condition of
// tags = ["a","b"] would wrongly match, and overwrite, a document whose tags
// are actually ["a","z"]. This is a table over which index is the one that
// differs, not a single repro, because house-rules says a universal
// property needs every position on its axis checked, not the one position a
// bug happened to be found at (the "một khẳng định phổ quát cần một BẢNG"
// rule).
func TestSameValueComparesEveryArrayElementNotJustTheFirst(t *testing.T) {
	if !sameValue([]any{"a", "b", "c"}, []any{"a", "b", "c"}) {
		t.Fatal("sameValue of two identical arrays = false; the table below is meaningless if this fails")
	}

	cases := []struct {
		name string
		a, b []any
	}{
		{"QA's exact repro: differ at the last index of a 2-element array",
			[]any{"a", "b"}, []any{"a", "z"}},
		{"differ at index 0 of 3, the rest identical",
			[]any{"a", "b", "c"}, []any{"z", "b", "c"}},
		{"differ at index 1 (the middle) of 3, the rest identical",
			[]any{"a", "b", "c"}, []any{"a", "z", "c"}},
		{"differ at index 2 (the last) of 3, the rest identical",
			[]any{"a", "b", "c"}, []any{"a", "b", "z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if sameValue(c.a, c.b) {
				t.Errorf("sameValue(%#v, %#v) = true, want false", c.a, c.b)
			}
			if sameValue(c.b, c.a) {
				t.Errorf("reversed: sameValue(%#v, %#v) = true, want false", c.b, c.a)
			}
		})
	}

	// QA's next round on this task caught that the table above still only
	// closes the axis at one length (three elements): every mutant it wrote
	// that special-cased a length other than three, or stopped after index
	// 0/1/2/3 specifically, survived because sameValueDomain and the cases
	// above never exercised a length outside {2, 3}. This loop is the fix,
	// parameterized the same way TestSameValueAgreesOnEqualAndDifferentValues
	// AtEveryDepth's depth loop already is: every length from 1 through
	// maxArrayAxisLength, every index that length has, so "every element" is
	// a claim about two parameters instead of about how many lengths
	// somebody remembered to type by hand. Three things are checked at each
	// (length, index) cell, because QA's mutants split into three different
	// ways of being wrong there: comparing values that actually differ (the
	// ordinary axis), keeping the int/1.0-float64 rule at a position other
	// than the one a literal case happens to reach (a mutant that recurses
	// only at index 0 and falls back to bare DeepEqual everywhere else would
	// get this wrong, because DeepEqual does not know 1 and 1.0 are the same
	// number), and a nested container whose own difference sits further down
	// than the position being varied.
	//
	// That third check used to nest exactly two levels deep, a fixed literal
	// rather than a swept parameter, and a further QA round caught exactly
	// what that shape of claim always misses: a mutant whose recursion goes
	// wrong specifically past depth two (QA's example differed four levels
	// down) passed every cell here, because nothing in this file ever built
	// a nested difference that deep at a non-zero index. nestDiffAt below
	// closes that the same way the length axis itself was closed — as a
	// parameter, not a second hand-picked literal — by sweeping the nesting
	// depth from 1 through maxNestSubDepth at every (length, index) cell.
	build := func(length int) []any {
		v := make([]any, length)
		for i := range v {
			v[i] = float64(i)
		}
		return v
	}
	// nestDiffAt builds a slice of `length` numbers whose element at idx is
	// instead a chain of `depth` nested one-element arrays wrapping leaf.
	// depth 1 is what the earlier, unparameterized version of this test
	// hard-coded as "two levels" (one array layer plus the leaf); sweeping
	// it is what lets this cell catch a mutant whose container recursion
	// only agrees with sameValue up to some depth short of maxNestSubDepth.
	nestDiffAt := func(length, idx, depth int, leaf any) []any {
		v := build(length)
		nested := leaf
		for i := 0; i < depth; i++ {
			nested = []any{nested}
		}
		v[idx] = nested
		return v
	}
	const maxNestSubDepth = 6 // QA's repro needed 4; this leaves headroom without the O(length^2) cost of tying it to maxArrayAxisLength.
	for length := 1; length <= maxArrayAxisLength; length++ {
		for idx := 0; idx < length; idx++ {
			differs, same := build(length), build(length)
			same[idx] = "not a number at all"
			t.Run(fmt.Sprintf("length %d differs at index %d", length, idx), func(t *testing.T) {
				if sameValue(differs, same) || sameValue(same, differs) {
					t.Errorf("arrays of length %d differing only at index %d compared equal", length, idx)
				}
			})

			intAt, floatAt := build(length), build(length)
			intAt[idx] = int(idx)
			t.Run(fmt.Sprintf("length %d int/float64 at index %d", length, idx), func(t *testing.T) {
				if !sameValue(intAt, floatAt) || !sameValue(floatAt, intAt) {
					t.Errorf("an int at index %d of %d stopped equalling its float64 twin", idx, length)
				}
			})

			for depth := 1; depth <= maxNestSubDepth; depth++ {
				nestedA := nestDiffAt(length, idx, depth, 1.0)
				nestedB := nestDiffAt(length, idx, depth, 2.0)
				t.Run(fmt.Sprintf("length %d nested %d deep differs at index %d", length, depth, idx), func(t *testing.T) {
					if sameValue(nestedA, nestedB) || sameValue(nestedB, nestedA) {
						t.Errorf("arrays of length %d holding a container %d levels deep that differs at index %d compared equal", length, depth, idx)
					}
				})
			}
		}
	}
}

// TestSameValueRequiresEveryMapKeyToBePresentNotJustCounted is the map
// counterpart of the test above, and the other axis QA's round found open:
// sameValueDomain never had two maps of the same length whose key sets
// differed, so a mutant that drops the "is this key even present in the
// other map" check survived. It survives on non-nil values too (a missing
// key's read comes back as sameValue against a real value, which is false
// either way), which is why the row that actually kills it is the one where
// the value under the differing key is nil on both sides: a missing key's
// zero-value in Go is also nil, so a check that forgets to ask "present?"
// sees nil == nil and calls two different-shaped objects equal —
// sameValue({"a":null}, {"b":null}) = true is QA's exact repro, and it is
// the same optimistic-locking hole as the array case above: a batch
// condition would match a document it should not.
func TestSameValueRequiresEveryMapKeyToBePresentNotJustCounted(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]any
	}{
		{"same size, disjoint keys, ordinary non-nil values",
			map[string]any{"a": 1.0}, map[string]any{"b": 1.0}},
		{"QA's exact repro: same size, disjoint keys, both values nil",
			map[string]any{"a": nil}, map[string]any{"b": nil}},
		{"same size, one key shared, the differing key's values are both nil",
			map[string]any{"shared": 1.0, "a": nil}, map[string]any{"shared": 1.0, "b": nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if sameValue(c.a, c.b) {
				t.Errorf("sameValue(%#v, %#v) = true, want false: different key sets must never compare equal", c.a, c.b)
			}
			if sameValue(c.b, c.a) {
				t.Errorf("reversed: sameValue(%#v, %#v) = true, want false", c.b, c.a)
			}
		})
	}

	// Control: the axis above must not make an ordinary equal pair, with a
	// nil value included, look unequal.
	if !sameValue(map[string]any{"a": nil, "b": 1.0}, map[string]any{"a": nil, "b": 1.0}) {
		t.Error("equal maps sharing a nil-valued key compared unequal")
	}

	// QA's next round found the same gap here that it found in the array
	// test above: the three cases handwritten before this point only ever
	// use a map of one or two keys, so a mutant that checks only the first
	// key range happens to visit, or only the first four keys, survived —
	// two keys was never enough to tell "checked every key" apart from
	// "checked one key and got lucky about which one Go's random map order
	// picked first". This loop parameterizes the key count from 1 through
	// maxMapAxisKeys and, within each count, which key is the one that
	// differs, the same way the array axis above was widened. The "renamed
	// key, both nil" case is the one that actually distinguishes "is this
	// key present in the other map" from "do the two maps have equal
	// values": a mutant that only compares by key COUNT and VALUE would
	// call two maps with disjoint key sets equal whenever every value under
	// those disjoint keys happens to be nil, because a missing key's zero
	// value in Go is also nil.
	//
	// A further QA round measured that 6 was, again, a wall rather than a
	// closed axis: a mutant that only checks the first 6 keys survived,
	// because a count of 6 makes "the first 6 keys" and "every key" the
	// same set. maxMapAxisKeys widens past that so a mutant with a
	// first-N-keys shortcut for any small N gets caught once count exceeds
	// it.
	buildMap := func(count int) map[string]any {
		m := make(map[string]any, count)
		for i := 0; i < count; i++ {
			m[fmt.Sprintf("key%d", i)] = float64(i)
		}
		return m
	}
	for count := 1; count <= maxMapAxisKeys; count++ {
		for idx := 0; idx < count; idx++ {
			key := fmt.Sprintf("key%d", idx)

			differs, same := buildMap(count), buildMap(count)
			same[key] = "not a number at all"
			t.Run(fmt.Sprintf("%d keys, value differs under %s", count, key), func(t *testing.T) {
				if sameValue(differs, same) || sameValue(same, differs) {
					t.Errorf("maps of %d keys differing only under %q compared equal", count, key)
				}
			})

			renamedA, renamedB := buildMap(count), buildMap(count)
			renamedA[key] = nil
			delete(renamedB, key)
			renamedB["a different key entirely"] = nil
			t.Run(fmt.Sprintf("%d keys, %s renamed and both sides nil", count, key), func(t *testing.T) {
				if sameValue(renamedA, renamedB) || sameValue(renamedB, renamedA) {
					t.Errorf("maps of %d keys whose key sets differ under %q, both nil, compared equal", count, key)
				}
			})

			intAt, floatAt := buildMap(count), buildMap(count)
			intAt[key] = int(idx)
			t.Run(fmt.Sprintf("%d keys, int/float64 under %s", count, key), func(t *testing.T) {
				if !sameValue(intAt, floatAt) || !sameValue(floatAt, intAt) {
					t.Errorf("an int under %q of %d keys stopped equalling its float64 twin", key, count)
				}
			})
		}
	}
}

// widgetsForConditions declares one collection with no index at all — these
// tests are about the comparison inside a condition, not about anything a
// scan needs, and an index this suite never reads from would only be a
// second thing that could be declared wrong.
func widgetsForConditions(t *testing.T, store *Store) *Collection {
	t.Helper()
	collection, err := store.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}})
	if err != nil {
		t.Fatalf("declare widgets: %v", err)
	}
	return collection
}

// TestAConstantConditionMayBeAnArrayAndIsCheckedRatherThanRefused is door one
// of the three the task names: {"equals": {"value": [...]}} written straight
// into the declaration. Task 0042 chose not to refuse this at declaration
// time — a batch condition does not encode into an index key the way a scan
// bound does, so there is nothing here that would fail for every caller, and
// refusing it would take away a working feature to dodge a bug that belongs
// in the comparison instead. This checks it is a working feature: a matching
// array condition lets the write through, and the array is compared by
// value against the array actually stored on the document, not swallowed by
// declaration-time validation.
func TestAConstantConditionMayBeAnArrayAndIsCheckedRatherThanRefused(t *testing.T) {
	_, store := fresh(t, 520)
	widgets := widgetsForConditions(t, store)

	yes := true
	if _, err := store.DeclareOperation(Operation{
		Name: "widgets.retag_if", Collection: "widgets", Action: ActionBatch,
		Input: []Parameter{
			{Name: "id", Type: TypeString, Required: true},
			{Name: "tags", Type: TypeAny, Required: true},
		},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "widgets",
			Key: &Term{Arg: "id"}, Exists: &yes,
			Require: []Condition{{Path: "tags", Equals: &Term{Value: []any{"a", "b"}}}},
			Set:     map[string]Term{"tags": {Arg: "tags"}},
		}},
	}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := widgets.Put(map[string]any{"id": "w1", "tags": []any{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	// A batch refuses to start on top of uncommitted work (batch.go's own
	// header explains why), and the Put above is exactly that until this
	// commits it.
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// The condition is exactly the stored array: the write goes through.
	if _, err := store.Invoke(Caller{}, "widgets.retag_if", 0, map[string]any{
		"id": "w1", "tags": []any{"x", "y"},
	}); err != nil {
		t.Fatalf("a matching constant array condition should not be refused: %v", err)
	}
	after, found, err := widgets.Get("w1")
	if err != nil || !found {
		t.Fatalf("get w1: found=%v err=%v", found, err)
	}
	if !sameValue(after["tags"], []any{"x", "y"}) {
		t.Errorf("the matching write did not land: tags = %v", after["tags"])
	}

	// The condition no longer matches the array now stored: the write is
	// refused, and this must be ErrCondition, never a panic.
	_, err = store.Invoke(Caller{}, "widgets.retag_if", 0, map[string]any{
		"id": "w1", "tags": []any{"z"},
	})
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("a non-matching constant array condition: want ErrCondition, got %v", err)
	}
	after, _, _ = widgets.Get("w1")
	if !sameValue(after["tags"], []any{"x", "y"}) {
		t.Errorf("a refused batch must not write: tags = %v", after["tags"])
	}
}

// TestAnArgumentDeclaredAnyCanCarryAnArrayThatNoDeclarationRuleSees is door
// two, and the reason task 0042 rejected the declaration-time fix entirely.
// The parameter "t" is declared TypeAny, and matches(TypeAny, value) accepts
// every value there is (store.go's matches) — a declaration-time check has
// no value to look at until a call arrives, and by then it is too late to
// refuse only some calls. This is the shape the QA panic actually happened
// through: an ordinary operation call, not a schema edit, handed an array to
// a parameter nothing in the declaration said couldn't be one.
func TestAnArgumentDeclaredAnyCanCarryAnArrayThatNoDeclarationRuleSees(t *testing.T) {
	_, store := fresh(t, 521)
	widgets := widgetsForConditions(t, store)

	yes := true
	if _, err := store.DeclareOperation(Operation{
		Name: "widgets.retag_matching", Collection: "widgets", Action: ActionBatch,
		Input: []Parameter{
			{Name: "id", Type: TypeString, Required: true},
			// "t" is where the caller's array arrives. It is declared "any"
			// because a condition's Equals is always checked as TypeAny
			// (ops.go), and there is no narrower type to give it that would
			// not also refuse the ordinary scalar case.
			{Name: "t", Type: TypeAny, Required: true},
			{Name: "tags", Type: TypeAny, Required: true},
		},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "widgets",
			Key: &Term{Arg: "id"}, Exists: &yes,
			Require: []Condition{{Path: "tags", Equals: &Term{Arg: "t"}}},
			Set:     map[string]Term{"tags": {Arg: "tags"}},
		}},
	}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := widgets.Put(map[string]any{"id": "w1", "tags": []any{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Before the fix, this exact call is the QA repro: an array from the
	// caller compared against an array from the document, both []interface{},
	// panicking the whole process rather than answering this one call.
	if _, err := store.Invoke(Caller{}, "widgets.retag_matching", 0, map[string]any{
		"id": "w1", "t": []any{"a", "b"}, "tags": []any{"x", "y"},
	}); err != nil {
		t.Fatalf("a matching array argument condition should not be refused: %v", err)
	}
	after, found, err := widgets.Get("w1")
	if err != nil || !found {
		t.Fatalf("get w1: found=%v err=%v", found, err)
	}
	if !sameValue(after["tags"], []any{"x", "y"}) {
		t.Errorf("the matching write did not land: tags = %v", after["tags"])
	}

	// A non-matching array argument is refused, not crashed.
	_, err = store.Invoke(Caller{}, "widgets.retag_matching", 0, map[string]any{
		"id": "w1", "t": []any{"a", "b", "c"}, "tags": []any{"z"},
	})
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("a non-matching array argument condition: want ErrCondition, got %v", err)
	}
	after, _, _ = widgets.Get("w1")
	if !sameValue(after["tags"], []any{"x", "y"}) {
		t.Errorf("a refused batch must not write: tags = %v", after["tags"])
	}
}

// TestAConditionSourcedFromAnEarlierStepIsAlwaysAKeyNeverAnArrayOrObject is
// door three: {"equals": {"step": "..."}}. Unlike the other two doors, this
// one really cannot hand sameValue a slice or a map — resolveIn returns
// whatever an earlier step produced, and a step only ever produces a
// document's primary key, whose declared type spec.validate restricts to
// TypeString or TypeNumber (spec.go). This is measured here rather than
// taken on faith: the key this test's step produces is a plain string, run
// through the exact same call path and the exact same sameValue as the other
// two doors, both when it matches and when it does not.
//
// QA's round on this task caught that the name is a universal claim
// ("always... never") while the body only ever ran one step producing one
// string — that one instance is not what makes the claim universal. What
// makes it universal is spec.validate refusing to declare a collection whose
// key is anything but TypeString or TypeNumber (spec.go:137): there is no
// schema anywhere that could hand a step an array or object key, because
// there is no schema anywhere with an array or object PRIMARY key at all.
// The block below measures that rule directly, for every other type the
// store knows the name of, instead of leaving it as prose the rest of this
// test only happens to be consistent with.
func TestAConditionSourcedFromAnEarlierStepIsAlwaysAKeyNeverAnArrayOrObject(t *testing.T) {
	_, store := fresh(t, 522)

	for _, badType := range []string{TypeBool, TypeAny, "array", "object"} {
		_, err := store.Declare(Spec{Name: "rejected_" + badType, Key: Key{Path: "id", Type: badType}})
		if !errors.Is(err, ErrDeclaration) {
			t.Errorf("a collection keyed on %q should be refused with ErrDeclaration (spec.go:137), got %v", badType, err)
		}
	}

	for _, spec := range []Spec{
		{Name: "tickets", Key: Key{Path: "id", Type: TypeString}},
		{Name: "assignments", Key: Key{Path: "id", Type: TypeString}},
	} {
		if _, err := store.Declare(spec); err != nil {
			t.Fatalf("declare %q: %v", spec.Name, err)
		}
	}

	if _, err := store.DeclareOperation(Operation{
		Name: "assignments.claim", Collection: "assignments", Action: ActionBatch,
		Input: []Parameter{
			{Name: "assignment", Type: TypeString, Required: true},
			{Name: "ticket", Type: TypeString, Required: true},
		},
		Steps: []Step{
			{
				// Explicit key rather than "auto", so the test knows exactly
				// what this step produces instead of having to read it back.
				Name: "ticket", Action: ActionInsert, Collection: "tickets",
				Document: map[string]Term{"id": {Arg: "ticket"}},
			},
			{
				Action: ActionUpdate, Collection: "assignments",
				Key:     &Term{Arg: "assignment"},
				Require: []Condition{{Path: "ticket", Equals: &Term{Step: "ticket", Field: "key"}}},
				Set:     map[string]Term{"claimed": {Value: true}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	assignments, err := store.Collection("assignments")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assignments.Put(map[string]any{"id": "a1", "ticket": "tkt-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := assignments.Put(map[string]any{"id": "a2", "ticket": "tkt-9"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// a1's "ticket" field is exactly the key the new ticket step produces:
	// the condition matches.
	if _, err := store.Invoke(Caller{}, "assignments.claim", 0, map[string]any{
		"assignment": "a1", "ticket": "tkt-1",
	}); err != nil {
		t.Fatalf("a matching step-sourced condition should not be refused: %v", err)
	}
	claimed, found, err := assignments.Get("a1")
	if err != nil || !found {
		t.Fatalf("get a1: found=%v err=%v", found, err)
	}
	if claimed["claimed"] != true {
		t.Errorf("a1 was not claimed: %v", claimed)
	}

	// a2's "ticket" field does not match the new ticket's key: refused, not
	// crashed.
	_, err = store.Invoke(Caller{}, "assignments.claim", 0, map[string]any{
		"assignment": "a2", "ticket": "tkt-2",
	})
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("a non-matching step-sourced condition: want ErrCondition, got %v", err)
	}
	unclaimed, _, _ := assignments.Get("a2")
	if unclaimed["claimed"] == true {
		t.Errorf("a refused batch must not write: %v", unclaimed)
	}
}

// TestDirectionOfRefusesAnArrayOrObjectWithoutPanicking pins down why
// directionOf (scan.go) is the safe half of the class this task scanned for,
// rather than just asserting it in prose. Go only panics comparing two
// interface values when they share a dynamic type that is itself
// uncomparable; a switch's case values here are always the two direction
// string constants, so a []any or a map[string]any on the value side never
// meets a same-typed case to panic against — it just fails to match any of
// them and falls through to the error. That is a property of what the other
// side of every case in this switch is, not of directionOf's own code, so it
// stops holding the moment a case compares against something read from a
// declaration instead of written as a literal — which is exactly the shape
// this task's scan is watching for. Kept in this task's own test file rather
// than in scan.go's, because scan.go belongs to 0012 round 5 while this runs.
func TestDirectionOfRefusesAnArrayOrObjectWithoutPanicking(t *testing.T) {
	for _, value := range []any{[]any{"forward"}, map[string]any{}} {
		t.Run(fmt.Sprintf("%#v", value), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("directionOf(%#v) panicked: %v", value, r)
				}
			}()
			if _, err := directionOf(value); err == nil {
				t.Errorf("directionOf(%#v) = nil error, want a refusal", value)
			}
		})
	}
}

// TestNumbersAreFloat64OnEveryPathThatFeedsSameValue is the measurement task
// 0042 asked for instead of a guess. It has been renamed and narrowed twice
// now, both times because QA measured the gap between the name and the body
// rather than taking the name's word for it:
//
//   - Round one's name ("...OnBothSidesOfTheWire") called json.Unmarshal
//     directly and never went through ops.go or server.go at all, so "the
//     wire" was read off the standard library's documented behavior, not
//     measured through this store's own code that does the decoding. It
//     also missed that sameValue is never called with (declaration,
//     argument) at all — satisfies (batch.go) calls it with (a document
//     field, wanted), and wanted is resolveIn's result, which may itself
//     have come from a declaration, an argument, or an earlier step.
//   - Round two added the document path for real (Collection.Put/Get, this
//     package's own Marshal/Unmarshal round trip) but left the declaration
//     path as the same kind of bare json.Unmarshal probe round one used —
//     a payload shaped like a declaration, decoded by this test's own call
//     to encoding/json, not by ops.go's. A change to ops.go's own decoder
//     (dec.UseNumber(), say) would leave that subtest green.
//
// This round fixes the declaration path the way round two fixed the
// document path: through the real call. "Depth" is dropped from the name
// because every subtest here already checks several depths inline, not
// because depth stopped mattering — the words that were actually wrong were
// about which code was doing the decoding, and that is what this name is
// narrowed to say now.
func TestNumbersAreFloat64OnEveryPathThatFeedsSameValue(t *testing.T) {
	checkFloat64 := func(t *testing.T, path string, value any) {
		t.Helper()
		if reflect.TypeOf(value) != reflect.TypeOf(float64(0)) {
			t.Errorf("%s decoded as %T, want float64", path, value)
		}
	}

	// The argument path stays a direct json.Unmarshal probe on purpose, not
	// by oversight: this package has no decode step of its own to measure
	// here. bind() (invoke.go) takes whatever Go value is already sitting
	// in the arguments map a caller of the exported Invoke handed it, and
	// checks it against the declared type — it never touches encoding/json.
	// The bytes-to-map decoding that turns a wire call into that map happens
	// one package over, in internal/server (server.go's own json.Unmarshal
	// into call.Arguments), which is outside this task's scope (batch.go and
	// its own tests) and which this file has no reason to import. So this
	// measures a fact about the standard library that every front door —
	// this repo's server.go, or anyone else's — has to rely on, not this
	// package's own code, and the subtest name says exactly that instead of
	// implying otherwise.
	t.Run("argument shape, as encoding/json decodes it (not this package's own code)", func(t *testing.T) {
		const payload = `{
			"value": 1,
			"list": [1, [1, 2]],
			"object": {"n": 1, "nested": {"m": 2}}
		}`
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatal(err)
		}
		checkFloat64(t, "value", decoded["value"])
		list := decoded["list"].([]any)
		checkFloat64(t, "list[0]", list[0])
		inner := list[1].([]any)
		checkFloat64(t, "list[1][0]", inner[0])
		checkFloat64(t, "list[1][1]", inner[1])
		object := decoded["object"].(map[string]any)
		checkFloat64(t, "object.n", object["n"])
		nested := object["nested"].(map[string]any)
		checkFloat64(t, "object.nested.m", nested["m"])
	})

	// The declaration path, for real this time: a constant written with
	// bare Go ints, the way every declaration constant in this package's
	// own tests is written in source, stored by DeclareOperation's Marshal,
	// and read back by store.Operation — the exact call Invoke makes
	// (ops.go:333) before resolveIn ever sees it. No json.Unmarshal call in
	// this subtest belongs to the test; the one that runs belongs to ops.go.
	t.Run("declaration path, through ops.go's own decode via store.Operation", func(t *testing.T) {
		_, store := fresh(t, 524)
		if _, err := store.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}}); err != nil {
			t.Fatal(err)
		}
		yes := true
		if _, err := store.DeclareOperation(Operation{
			Name: "widgets.check_numbers", Collection: "widgets", Action: ActionBatch,
			Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
			Steps: []Step{{
				Action: ActionUpdate, Collection: "widgets", Key: &Term{Arg: "id"}, Exists: &yes,
				Require: []Condition{{Path: "probe", Equals: &Term{Value: map[string]any{
					"value": 1,
					"list":  []any{1, []any{1, 2}},
					"object": map[string]any{
						"n":      1,
						"nested": map[string]any{"m": 2},
					},
				}}}},
				Set: map[string]Term{"ok": {Value: true}},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Commit(); err != nil {
			t.Fatal(err)
		}

		operation, found, err := store.Operation("widgets.check_numbers", 0)
		if err != nil || !found {
			t.Fatalf("Operation: found=%v err=%v", found, err)
		}
		wanted := operation.Steps[0].Require[0].Equals.Value.(map[string]any)
		checkFloat64(t, "declaration.value", wanted["value"])
		list := wanted["list"].([]any)
		checkFloat64(t, "declaration.list[0]", list[0])
		inner := list[1].([]any)
		checkFloat64(t, "declaration.list[1][0]", inner[0])
		checkFloat64(t, "declaration.list[1][1]", inner[1])
		object := wanted["object"].(map[string]any)
		checkFloat64(t, "declaration.object.n", object["n"])
		nested := object["nested"].(map[string]any)
		checkFloat64(t, "declaration.object.nested.m", nested["m"])
	})

	// The document path: written through the Go API with a bare Go int —
	// the same shape an operator-shell insert, or a library caller embedding
	// this store, would write by hand — then read back the only way this
	// store ever reads a document: Put's json.Marshal followed by the read
	// path's json.Unmarshal (collection.go).
	t.Run("document path, through collection.go's own Put/Get round trip", func(t *testing.T) {
		_, store := fresh(t, 523)
		widgets, err := store.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := widgets.Put(map[string]any{
			"id": "x", "n": 1, "list": []any{1, []any{2}}, "obj": map[string]any{"m": 3},
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Commit(); err != nil {
			t.Fatal(err)
		}
		back, found, err := widgets.Get("x")
		if err != nil || !found {
			t.Fatalf("get x: found=%v err=%v", found, err)
		}
		checkFloat64(t, "document.n", back["n"])
		docList := back["list"].([]any)
		checkFloat64(t, "document.list[0]", docList[0])
		checkFloat64(t, "document.list[1][0]", docList[1].([]any)[0])
		checkFloat64(t, "document.obj.m", back["obj"].(map[string]any)["m"])
	})

	// A Term.Value declared in Go source, the way every other test in this
	// package writes one, is not decoded at all — it is whatever untyped
	// constant Go assigned, which defaults to int. That gap only exists on
	// the direct Go-API side; a real client, and a schema loaded from a file
	// with `rsql apply`, both go through json.Unmarshal (measured above) and
	// never produce it.
	direct := Term{Value: 1}
	if reflect.TypeOf(direct.Value) == reflect.TypeOf(float64(0)) {
		t.Fatalf("a Go-literal Term.Value decoded as float64, which would make this measurement moot")
	}
}
