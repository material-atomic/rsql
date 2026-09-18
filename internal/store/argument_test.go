package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestBoundWithTooManyValuesStaysDeclaration guards the one error bound()
// (scan.go) returns that must NOT become ErrArgument: a bound carrying more
// values than the index has fields is a shape mismatch in the declaration
// itself, not a caller's value bound() failed to encode, and task 0044 §1.2
// says explicitly not to wrap it. A mutation that wraps every error bound()
// returns in ErrArgument — "bọc rộng tay một dòng" — passes every other test
// in this file and is caught here.
//
// Called directly rather than through Invoke: the ordinary declaration path
// already refuses an endpoint with more terms than fields before bound() is
// ever reached (ops.go's len(endpoint.Terms) > len(fields) check), so this
// is bound()'s own defensive twin of that check — dead through every route
// this repo declares an operation by, but not dead code, the same distinction
// task 0044's own text draws between it and the panic path constantIsEncodable
// closed off ahead of time.
func TestBoundWithTooManyValuesStaysDeclaration(t *testing.T) {
	_, collection := declared(t, 601)

	fields := []Field{{Path: "slug", Type: TypeString, Missing: MissingSkip}}
	_, err := collection.bound([]byte{0x01}, fields, &Bound{Values: []any{"a", "b"}}, false)
	if !errors.Is(err, ErrDeclaration) {
		t.Errorf("a bound with more values than fields: want ErrDeclaration, got %v", err)
	}
	if errors.Is(err, ErrArgument) {
		t.Error("a shape mismatch is a declaration problem, not a caller's value — must not also be ErrArgument")
	}
}

// TestBoundNamesTheFieldAndSideForAnUnencodableValue is the direct proof of
// task 0044 §1.2's "câu lỗi phải mang đủ ba thứ": which end (from/to), which
// field, and keys' own error (which is where the Go type — %T — comes from).
// Not "does not silently swallow the underlying error" as a general
// assertion: an exact substring match on all three, plus a check that the
// SECOND field of a two-field bound is the one named when it is the second
// value that fails, which a version reporting fields[0].Path unconditionally
// would still pass every single-field test in this file.
func TestBoundNamesTheFieldAndSideForAnUnencodableValue(t *testing.T) {
	_, collection := declared(t, 602)

	fields := []Field{
		{Path: "author", Type: TypeAny, Missing: MissingSkip},
		{Path: "slug", Type: TypeAny, Missing: MissingSkip},
	}
	bad := []any{"a", "b"}

	fromErr := mustBoundErr(t, collection, fields, &Bound{Values: []any{bad}}, false)
	if !errors.Is(fromErr, ErrArgument) {
		t.Fatalf("from bound, unencodable value: want ErrArgument, got %v", fromErr)
	}
	for _, want := range []string{"the from bound", `"author"`, "[]interface {}"} {
		if !strings.Contains(fromErr.Error(), want) {
			t.Errorf("bound error %q does not mention %q", fromErr.Error(), want)
		}
	}
	if strings.Contains(fromErr.Error(), "the to bound") {
		t.Errorf("bound error %q names the wrong side", fromErr.Error())
	}

	toErr := mustBoundErr(t, collection, fields, &Bound{Values: []any{bad}}, true)
	if !errors.Is(toErr, ErrArgument) {
		t.Fatalf("to bound, unencodable value: want ErrArgument, got %v", toErr)
	}
	// "to" alone is not a safe substring here: ErrArgument's own text is
	// "sapedb/store: …", and "store" contains "to" (s-[to]-re) — so a mutation
	// that always writes side = "from" (dropping the `if upper { side = "to" }`
	// branch entirely) still leaves "to" somewhere in the string and this
	// assertion would pass regardless of which side the error actually names.
	// Measured: that exact mutation survives on "to" alone; "the to bound" is
	// the phrase side actually produces, and is not a substring of anything
	// else in the message.
	if !strings.Contains(toErr.Error(), "the to bound") {
		t.Errorf("upper bound error %q does not say which side it is", toErr.Error())
	}
	if strings.Contains(toErr.Error(), "the from bound") {
		t.Errorf("bound error %q names the wrong side", toErr.Error())
	}

	// The second field, not the first, is the one that fails here — the
	// message must say "slug", not "author", which is what proves the field
	// name comes from a per-value loop rather than from fields[0] reported
	// regardless of which value actually failed.
	secondErr := mustBoundErr(t, collection, fields, &Bound{Values: []any{"ok", bad}}, false)
	if !errors.Is(secondErr, ErrArgument) {
		t.Fatalf("second field unencodable: want ErrArgument, got %v", secondErr)
	}
	if !strings.Contains(secondErr.Error(), `"slug"`) {
		t.Errorf("bound error %q does not name the failing field (slug)", secondErr.Error())
	}
	if strings.Contains(secondErr.Error(), `"author"`) {
		t.Errorf("bound error %q names the field that did NOT fail", secondErr.Error())
	}
}

func mustBoundErr(t *testing.T, collection *Collection, fields []Field, at *Bound, upper bool) error {
	t.Helper()
	_, err := collection.bound([]byte{0x01}, fields, at, upper)
	if err == nil {
		t.Fatal("want an error, got none")
	}
	return err
}

// TestSameTermDistinguishesEveryFieldOfTerm is the generated-not-hand-listed
// table task 0044 §3 asks for: walk reflect.TypeOf(Term{}), and for every
// field build a pair that differs in exactly that one field, requiring
// sameTerm to say they are not the same term.
//
// The field-count assertion's job is narrower than it looks, and is worth
// stating precisely rather than the more flattering thing it sounds like. A
// field ADDED to Term with no matching entry in alternates is already caught
// by the loop's own "no alternate value" guard below, with or without this
// count — the loop only ever visits fields Term actually has, and a name
// missing from the map fails there regardless. What only this line catches
// is the opposite drift: a field REMOVED from Term while alternates still
// carries a stale entry for it. The loop cannot see that at all, since it
// never iterates a name Term no longer has — measured by deleting this
// check and adding one unmatched entry to the map: the test passes
// silently. Kept for that one direction, not for "Term grows a field."
func TestSameTermDistinguishesEveryFieldOfTerm(t *testing.T) {
	base := Term{Arg: "a1", Value: "v1", Constant: true, Step: "s1", Field: "f1"}

	alternates := map[string]any{
		"Arg":      "a2",
		"Value":    "v2",
		"Constant": false,
		"Step":     "s2",
		"Field":    "f2",
	}

	typ := reflect.TypeOf(base)
	if typ.NumField() != len(alternates) {
		t.Fatalf("Term has %d fields but this test's coverage table has %d — "+
			"a field was added or removed on one side without the other; see "+
			"this test's own doc comment for which direction this line actually "+
			"catches", typ.NumField(), len(alternates))
	}

	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		alt, known := alternates[name]
		if !known {
			t.Fatalf("Term field %q has no alternate value in this test's table", name)
		}

		variant := base
		reflect.ValueOf(&variant).Elem().Field(i).Set(reflect.ValueOf(alt))

		if sameTerm(base, variant) {
			t.Errorf("terms differing only in %s compared equal: %+v vs %+v", name, base, variant)
		}
	}

	if !sameTerm(base, base) {
		t.Error("a term compared with an identical copy of itself should be equal")
	}
}

// TestScanAcrossRefusesTwoDifferentArguments and
// TestTotalsAcrossRefusesTwoDifferentArguments exercise sameTerm through the
// real declaration path with terms that differ in Arg rather than in Value —
// the existing TestScanAcrossTellsTwoConstantsApart / TestTotalsAcross... in
// direction_v4_test.go already cover a Value difference, so these close the
// other axis DeepEqual-on-the-whole-struct has to hold for: two different
// argument names are not "the same term" either, even though neither one has
// a value yet for == or DeepEqual to compare. Each pairs with a same-argument
// control that must still declare cleanly, since a version that refused ANY
// two Arg terms — not just two different ones — would pass a test that only
// checked the refusal.
func TestScanAcrossRefusesTwoDifferentArguments(t *testing.T) {
	_, _, store := partitioned(t, 654)
	entriesByMonth(t, store, 0)

	twoArgs := Operation{
		Name:       "entries.two_args",
		Collection: "entries",
		Action:     ActionScan,
		Index:      "by_account",
		Input: []Parameter{
			{Name: "a", Type: TypeString, Required: true},
			{Name: "b", Type: TypeString, Required: true},
		},
		From:  &Endpoint{Terms: []Term{{Arg: "a"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "b"}}},
		Limit: 10,
	}
	if _, err := store.DeclareOperation(twoArgs); !errors.Is(err, ErrDeclaration) {
		t.Errorf("From arg %q and To arg %q on a partitioned index: want ErrDeclaration, got %v", "a", "b", err)
	}

	sameArg := twoArgs
	sameArg.Name = "entries.same_arg"
	sameArg.To = &Endpoint{Terms: []Term{{Arg: "a"}}}
	declareOp(t, store, sameArg)
}

func TestTotalsAcrossRefusesTwoDifferentArguments(t *testing.T) {
	_, _, store := partitioned(t, 655)

	if _, err := store.Declare(Spec{
		Name:      "lines",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	twoArgs := Operation{
		Name: "lines.two_args", Collection: "lines", Action: ActionTotals, Rollup: "per_account", Limit: 10,
		Input: []Parameter{
			{Name: "a", Type: TypeString, Required: true},
			{Name: "b", Type: TypeString, Required: true},
		},
		From: &Endpoint{Terms: []Term{{Arg: "a"}}},
		To:   &Endpoint{Terms: []Term{{Arg: "b"}}},
	}
	if _, err := store.DeclareOperation(twoArgs); !errors.Is(err, ErrDeclaration) {
		t.Errorf("From arg %q and To arg %q on a partitioned rollup: want ErrDeclaration, got %v", "a", "b", err)
	}

	sameArg := twoArgs
	sameArg.Name = "lines.same_arg"
	sameArg.To = &Endpoint{Terms: []Term{{Arg: "a"}}}
	declareOp(t, store, sameArg)
}
