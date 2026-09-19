package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// This file is task 0049's D10 lethality tests (§8: "một trong ba chỗ quay
// lại %v trần — con QUAN TRỌNG NHẤT, chạy ba lần, mỗi lần một chỗ") plus two
// tests backing the "blocked by structure" rows of task 0049 §4's %v sweep
// table for sites this task did not touch.

// checkedRequire declares a widgets collection and a batch operation with
// one Require condition on the "tags" field — the exact shape satisfies
// (batch.go) formats into ErrCondition, used by all three D10 tests below.
func checkedRequire(t *testing.T, seed int64) (*Store, *Collection) {
	t.Helper()
	_, s := fresh(t, seed)
	widgets, err := s.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}})
	if err != nil {
		t.Fatalf("declare widgets: %v", err)
	}
	yes := true
	if _, err := s.DeclareOperation(Operation{
		Name: "widgets.check", Collection: "widgets", Action: ActionBatch,
		Input: []Parameter{
			{Name: "id", Type: TypeString, Required: true},
			{Name: "t", Type: TypeAny, Required: true},
		},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "widgets", Key: &Term{Arg: "id"}, Exists: &yes,
			Require: []Condition{{Path: "tags", Equals: &Term{Arg: "t"}}},
			Set:     map[string]Term{"ok": {Value: true}},
		}},
	}); err != nil {
		t.Fatalf("declare widgets.check: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("commit declarations: %v", err)
	}
	return s, widgets
}

// elevenLongValues is an 11-element slice whose last element is a marker
// string a raw %v would print in full and describe() must cut — the same
// device describe_test.go's own count-boundary cases use, reused here so a
// revert to a bare %v at any of the three call sites is visible without
// needing a genuinely self-referential value (that end-to-end, crash-shaped
// claim is TestArgDoorCyclicValueNoLongerCrashesThroughSatisfiesItReturns
// ErrCondition in cycle_test.go; these three tests are about the exact
// call site, not about survival).
func elevenLongValues() []any {
	out := make([]any, 11)
	for i := range out {
		out[i] = i
	}
	out[10] = "MARKER-INDEX-10-UNIQUE"
	return out
}

// --- D10, site 1: batch.go's "has no %q, and it must be %v" (wanted) -------
//
// The field is simply absent from the document, so satisfies reaches the
// first of the two error-formatting lines, not the second.
func TestSatisfiesTruncatesWantedWhenTheFieldIsAbsent(t *testing.T) {
	s, widgets := checkedRequire(t, 601)
	if _, err := widgets.Put(map[string]any{"id": "w1"}); err != nil { // no "tags" field at all
		t.Fatalf("put: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err := s.Invoke(Caller{}, "widgets.check", 0, map[string]any{
		"id": "w1", "t": elevenLongValues(),
	})
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("want ErrCondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("error does not show a cut marker — wanted was not truncated: %v", err)
	}
	if strings.Contains(err.Error(), "MARKER-INDEX-10-UNIQUE") {
		t.Fatalf("error prints the 11th element of wanted in full — describe() was bypassed: %v", err)
	}
}

// --- D10, site 2: batch.go's "has %q of %v, ... it must be %v" (value) -----
//
// The field IS present, holding the long value, and does not match a short
// "wanted" — satisfies reaches the second error line, and this checks its
// FIRST %v (now %s via describe), the stored value.
func TestSatisfiesTruncatesValueOnAMismatch(t *testing.T) {
	s, widgets := checkedRequire(t, 602)
	if _, err := widgets.Put(map[string]any{"id": "w1", "tags": elevenLongValues()}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err := s.Invoke(Caller{}, "widgets.check", 0, map[string]any{
		"id": "w1", "t": "something-short-that-does-not-match",
	})
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("want ErrCondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("error does not show a cut marker — value was not truncated: %v", err)
	}
	if strings.Contains(err.Error(), "MARKER-INDEX-10-UNIQUE") {
		t.Fatalf("error prints the 11th element of value in full — describe() was bypassed: %v", err)
	}
}

// --- D10, site 3: batch.go's "has %q of %v, ... it must be %v" (wanted) ----
//
// Same mismatch branch as site 2, but this time the SHORT value is what is
// stored and the LONG one is what was wanted — pins the second %v of that
// line down independently of the first.
func TestSatisfiesTruncatesWantedOnAMismatch(t *testing.T) {
	s, widgets := checkedRequire(t, 603)
	if _, err := widgets.Put(map[string]any{"id": "w1", "tags": "something-short"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err := s.Invoke(Caller{}, "widgets.check", 0, map[string]any{
		"id": "w1", "t": elevenLongValues(),
	})
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("want ErrCondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("error does not show a cut marker — wanted was not truncated: %v", err)
	}
	if strings.Contains(err.Error(), "MARKER-INDEX-10-UNIQUE") {
		t.Fatalf("error prints the 11th element of wanted in full — describe() was bypassed: %v", err)
	}
}

// --- %v sweep, "blocked by structure" claims, measured not asserted --------

// TestADirectionArgumentThatIsASelfReferentialContainerIsRefusedNotFormatted
// backs task 0049 §4's classification of scan.go's directionOf (the %v sweep
// found it at the one call site reachable from a caller, invoke.go's
// directionFor): the "direction" argument is declared TypeString wherever
// it is used (checkDirection, ops.go, refuses anything else at declare
// time), and bind() (invoke.go) checks every argument against its declared
// type with matches() before any operation code — directionOf included —
// ever sees it. A container can never pass a TypeString check, so it can
// never reach directionOf's own %v at all. Measured here by handing Invoke
// the shape that would matter if that were false: a self-referential map,
// as the direction.
func TestADirectionArgumentThatIsASelfReferentialContainerIsRefusedNotFormatted(t *testing.T) {
	store, collection := declared(t, 604)
	fill(t, collection)
	declareOp(t, store, eitherWay())

	selfRef := map[string]any{}
	selfRef["self"] = selfRef

	_, err := store.Invoke(Caller{}, "articles.slugs", 0, map[string]any{
		"from": "a", "to": "z", "direction": selfRef,
	})
	if !errors.Is(err, ErrArgument) {
		t.Fatalf("a self-referential direction: want ErrArgument (rejected by bind() before directionOf ever runs), got %v", err)
	}
}

// TestAPrimaryKeyThatIsASelfReferentialContainerIsRefusedNotFormatted backs
// task 0049 §4's classification of invoke.go's ActionInsert duplicate-key
// error (the other %v-on-any call site the sweep found that this task does
// not touch): the "id" parameter here is declared TypeAny on purpose, so
// bind() does NOT filter it — the claim under test is the NEXT gate,
// collection.Get inside the ActionInsert branch, which calls encodeKey,
// which calls matches(spec.Key.Type, value). A collection's primary key
// type can only ever be declared TypeString or TypeNumber (spec.go refuses
// TypeAny for a key at Declare time), so a []any can never pass that check
// either, and the code returns before it ever reaches the %v line that
// would have formatted it.
func TestAPrimaryKeyThatIsASelfReferentialContainerIsRefusedNotFormatted(t *testing.T) {
	_, s := fresh(t, 605)
	if _, err := s.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := s.DeclareOperation(Operation{
		Name: "widgets.make", Collection: "widgets", Action: ActionInsert,
		Input:    []Parameter{{Name: "id", Type: TypeAny, Required: true}},
		Document: map[string]Term{"id": {Arg: "id"}},
	}); err != nil {
		t.Fatalf("declare widgets.make: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	selfRef := []any{nil}
	selfRef[0] = selfRef

	done := make(chan error, 1)
	go func() {
		_, err := s.Invoke(Caller{}, "widgets.make", 0, map[string]any{"id": selfRef})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("a self-referential primary key was accepted; it should have been refused as the wrong type")
		}
		if !errors.Is(err, ErrType) {
			t.Fatalf("want ErrType (the primary key's declared type is string, not this), got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Invoke with a self-referential primary key did not return within 2s")
	}
}
