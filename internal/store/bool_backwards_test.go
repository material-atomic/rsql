package store

import "testing"

// TestABackwardsBoolRangeIsAcceptedRatherThanRefused pins a real, measured
// gap in task 0045's promise, found while building task 0054's case table
// (mục 3.4 of the task doc) rather than something this task fixes.
//
// Task 0045 made stretch() (scan.go) refuse a From that sorts after To,
// rather than silently reading as an empty stretch. The refusal is keyed on
// lower == upper being LEFT ALONE (not refused) because that shape usually
// means "an Exclusive bound pinned against itself" or, on a number field,
// two inclusive ends one ULP apart — both genuinely empty ranges a caller
// meant to write.
//
// A bool field breaks that assumption completely rather than at one edge:
// it has exactly two values, so keys.Encode(false) and keys.Encode(true)
// are one step apart, and EVERY backwards pair on a bool field — not some
// of them — lands on lower == upper. Task 0045's promise therefore covers
// none of the domain on a bool field: this test's own name says what is
// actually true today, not "every backwards range is refused" (which is
// false and must not be written anywhere as though it were).
//
// This is deliberately not fixed here — see the comment 0054 added to
// stretch()'s doc (scan.go) for the measurements and for why a fix needs its
// own widen-the-net measurement this task has no budget for. The gap is not
// a data-loss bug: the wrong answer is an empty one, not a wrong one, which
// is exactly what makes it safe to leave pinned rather than guessed at.
func TestABackwardsBoolRangeIsAcceptedRatherThanRefused(t *testing.T) {
	_, store := fresh(t, 7701)
	collection, err := store.Declare(Spec{
		Name: "flags",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{{Name: "by_flag", Fields: []Field{
			{Path: "b", Type: TypeBool, Missing: MissingFirst},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []bool{true, false, true} {
		if _, err := collection.Put(map[string]any{"b": v}); err != nil {
			t.Fatal(err)
		}
	}

	// The constant-endpoint path: both ends are written into the
	// declaration itself, so refusedBackwardsRange (ops.go) is the function
	// that would have to catch this at declare time, and does not.
	op := Operation{
		Name: "flags.backwards", Collection: "flags", Action: ActionScan,
		Index: "by_flag", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: true}}},
		To:   &Endpoint{Terms: []Term{{Value: false}}},
	}
	if _, err := store.DeclareOperation(op); err != nil {
		t.Fatalf("From=true To=false on a bool field was refused at declare (the gap this test pins has closed — see stretch()'s doc comment and narrow this test rather than deleting it): %v", err)
	}
	result, err := store.Invoke(Caller{}, "flags.backwards", 0, nil)
	if err != nil {
		t.Fatalf("invoking the accepted backwards range failed rather than reading empty: %v", err)
	}
	if result.Count != 0 {
		t.Fatalf("From=true To=false on a bool field returned %d rows, want 0 (empty, not an error, is the gap this test pins)", result.Count)
	}

	// The argument path: only stretch() (scan.go) ever sees the values, at
	// call time, since neither end is a constant the declaration could have
	// judged in advance.
	argOp := Operation{
		Name: "flags.arg", Collection: "flags", Action: ActionScan,
		Index: "by_flag", Limit: 10,
		Input: []Parameter{{Name: "lo", Type: TypeBool, Required: true}, {Name: "hi", Type: TypeBool, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "lo"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "hi"}}},
	}
	if _, err := store.DeclareOperation(argOp); err != nil {
		t.Fatalf("the argument-shaped operation was refused at declare: %v", err)
	}
	result, err = store.Invoke(Caller{}, "flags.arg", 0, map[string]any{"lo": true, "hi": false})
	if err != nil {
		t.Fatalf("invoking lo=true hi=false failed rather than reading empty: %v", err)
	}
	if result.Count != 0 {
		t.Fatalf("lo=true hi=false on a bool field returned %d rows, want 0", result.Count)
	}
}
