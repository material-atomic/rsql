package store

import (
	"bytes"
	"errors"
	"testing"
)

// This file is task 0045's front door: "a From that sorts after To is a
// nonsensical statement, and a nonsensical statement must be said out loud
// as close as possible to where it was written." Two places say it —
// validateOperation (ops.go), when both ends are constants, and stretch()
// (scan.go), when either end is an argument — and this file measures both,
// plus the one shape that must NOT be refused: two ends pinned to the same
// point on purpose.

// TestABackwardsRangeIsNeverSilentlyEmpty is the bảng ca for mục 6's first
// row: every axis a backwards range can be written on, crossed with both
// directions. house-rules ("một khẳng định phổ quát cần một BẢNG") — each
// row differs from its neighbours on exactly one axis, and every row must
// come back ErrArgument or ErrDeclaration, in BOTH Forward and Reverse,
// never a silently empty result.
func TestABackwardsRangeIsNeverSilentlyEmpty(t *testing.T) {
	store, collection := declared(t, 1450)
	fill(t, collection)

	// widgets carries a plain ascending number field, kept separate from
	// by_author's Descending "published" so the numeric row below is
	// unambiguous about which way is "backwards".
	widgets, err := store.Declare(Spec{
		Name: "widgets",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{{
			Name:   "by_weight",
			Fields: []Field{{Path: "weight", Type: TypeNumber, Missing: MissingSkip}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []struct {
		name          string
		declare       func(name string) Operation
		invoke        map[string]any // nil for an all-constant (declare-time) shape
		wantAtDeclare error          // non-nil: DeclareOperation itself must fail this way
	}{
		{
			name: "both ends constants, by_slug",
			declare: func(name string) Operation {
				return Operation{
					Name: name, Collection: "articles", Action: ActionScan, Index: "by_slug", Limit: 10,
					From: &Endpoint{Terms: []Term{{Value: "s5"}}},
					To:   &Endpoint{Terms: []Term{{Value: "s1"}}},
				}
			},
			wantAtDeclare: ErrDeclaration,
		},
		{
			name: "both ends arguments, by_slug",
			declare: func(name string) Operation {
				return Operation{
					Name: name, Collection: "articles", Action: ActionScan, Index: "by_slug", Limit: 10,
					Input: []Parameter{{Name: "lo", Type: TypeString, Required: true}, {Name: "hi", Type: TypeString, Required: true}},
					From:  &Endpoint{Terms: []Term{{Arg: "lo"}}},
					To:    &Endpoint{Terms: []Term{{Arg: "hi"}}},
				}
			},
			invoke: map[string]any{"lo": "s5", "hi": "s1"},
		},
		{
			name: "constant at From, argument at To, backwards",
			declare: func(name string) Operation {
				return Operation{
					Name: name, Collection: "articles", Action: ActionScan, Index: "by_slug", Limit: 10,
					Input: []Parameter{{Name: "hi", Type: TypeString, Required: true}},
					From:  &Endpoint{Terms: []Term{{Value: "s5"}}},
					To:    &Endpoint{Terms: []Term{{Arg: "hi"}}},
				}
			},
			invoke: map[string]any{"hi": "s1"},
		},
		{
			name: "argument at From, constant at To, backwards",
			declare: func(name string) Operation {
				return Operation{
					Name: name, Collection: "articles", Action: ActionScan, Index: "by_slug", Limit: 10,
					Input: []Parameter{{Name: "lo", Type: TypeString, Required: true}},
					From:  &Endpoint{Terms: []Term{{Arg: "lo"}}},
					To:    &Endpoint{Terms: []Term{{Value: "s1"}}},
				}
			},
			invoke: map[string]any{"lo": "s5"},
		},
		{
			// Composite, mismatched width: From names two fields of by_author
			// (author, published), To names only the first — and the two
			// authors are different, "bob" sorting after "ann", so the whole
			// of bob's group sorts after the point To names, whatever width
			// To is written at.
			name: "composite, mismatched width, different author groups",
			declare: func(name string) Operation {
				return Operation{
					Name: name, Collection: "articles", Action: ActionScan, Index: "by_author", Limit: 10,
					From: &Endpoint{Terms: []Term{{Value: "bob"}, {Value: 9.0}}},
					To:   &Endpoint{Terms: []Term{{Value: "ann"}}},
				}
			},
			wantAtDeclare: ErrDeclaration,
		},
		{
			name: "the clustered index (_key)",
			declare: func(name string) Operation {
				return Operation{
					Name: name, Collection: "articles", Action: ActionScan, Index: ClusteredIndex, Limit: 10,
					Input: []Parameter{{Name: "lo", Type: TypeString, Required: true}, {Name: "hi", Type: TypeString, Required: true}},
					From:  &Endpoint{Terms: []Term{{Arg: "lo"}}},
					To:    &Endpoint{Terms: []Term{{Arg: "hi"}}},
				}
			},
			invoke: map[string]any{"lo": "a5", "hi": "a1"},
		},
		{
			// The mutation this closes: comparing the raw values ("10" and
			// "2" as strings, or as interface{} with a naive less-than) gives
			// the WRONG answer — "10" sorts before "2" lexically, and 2 < 10
			// numerically means a value-level compare would call this
			// forward, not backwards. The right answer comes from comparing
			// the ENCODED bytes, which keys.Encode orders numerically: 10 is
			// really the high end here, so From:10/To:2 is really backwards
			// and must be refused.
			name: "numeric, byte order not string order (10 vs 2)",
			declare: func(name string) Operation {
				return Operation{
					Name: name, Collection: "widgets", Action: ActionScan, Index: "by_weight", Limit: 10,
					From: &Endpoint{Terms: []Term{{Value: 10.0}}},
					To:   &Endpoint{Terms: []Term{{Value: 2.0}}},
				}
			},
			wantAtDeclare: ErrDeclaration,
		},
	} {
		t.Run(id.name, func(t *testing.T) {
			for _, direction := range []struct {
				name  string
				value string
			}{
				{"forward", DirectionForward},
				{"reverse", DirectionReverse},
			} {
				t.Run(direction.name, func(t *testing.T) {
					operation := id.declare(t.Name())
					if id.invoke != nil {
						operation.Input = append(operation.Input, Parameter{Name: "direction", Type: TypeString, Required: true})
						operation.Direction = &Term{Arg: "direction"}
					}

					stored, err := store.DeclareOperation(operation)

					if id.wantAtDeclare != nil {
						if !errors.Is(err, id.wantAtDeclare) {
							t.Fatalf("declare: want %v, got %v", id.wantAtDeclare, err)
						}
						return
					}
					if err != nil {
						t.Fatalf("declare: %v", err)
					}

					args := map[string]any{"direction": direction.value}
					for k, v := range id.invoke {
						args[k] = v
					}
					if _, err := store.Invoke(Caller{}, stored.Name, stored.Version, args); !errors.Is(err, ErrArgument) {
						t.Errorf("invoke: want ErrArgument, got %v", err)
					}
				})
			}
		})
	}
	_ = widgets
}

// TestAPinnedEmptyRangeStillRuns is mục 6's second row and the task's own
// "most important mutation": lower == upper is a range someone wrote on
// purpose — an Exclusive bound pinned against an Inclusive one at the SAME
// point — and it must keep running rather than join the backwards case. Both
// of the two ways to pin it (Exclusive at From, or Exclusive at To) are
// covered, because they exercise different arms of boundAt's
// `upper != at.Exclusive` line.
func TestAPinnedEmptyRangeStillRuns(t *testing.T) {
	_, collection := declared(t, 1451)
	fill(t, collection)

	for _, shape := range []struct {
		name string
		from *Bound
		to   *Bound
	}{
		{"From exclusive, To inclusive, same point", &Bound{Values: []any{"s3"}, Exclusive: true}, &Bound{Values: []any{"s3"}}},
		{"From inclusive, To exclusive, same point", &Bound{Values: []any{"s3"}}, &Bound{Values: []any{"s3"}, Exclusive: true}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			for _, direction := range []Direction{Forward, Reverse} {
				got := keysOf(scan(t, collection, "by_slug", Range{From: shape.from, To: shape.to, Direction: direction}))
				if len(got) != 0 {
					t.Errorf("direction=%v: got %v, want an empty (not refused) result", direction, got)
				}
			}
		})
	}
}

// TestBothEndsExclusiveAtOnePointIsRefused is the shape immediately next to
// TestAPinnedEmptyRangeStillRuns that is NOT the same: Exclusive at BOTH ends
// of one point asks the walk to start after everything the point's prefix
// covers while also stopping before any of it, which inverts the two byte
// positions (successor(x) > x always) rather than merely describing an empty
// set of values. reach_test.go measures this at declare time, for the one
// shape on its list that is all-constant; this measures both halves
// directly: the all-constant declare-time refusal, and the same shape
// reached through an argument at call time.
func TestBothEndsExclusiveAtOnePointIsRefused(t *testing.T) {
	store, collection := declared(t, 1452)
	fill(t, collection)

	t.Run("both ends constants", func(t *testing.T) {
		_, err := store.DeclareOperation(Operation{
			Name: "articles.pinch_constant", Collection: "articles", Action: ActionScan, Index: "by_slug", Limit: 10,
			From: &Endpoint{Terms: []Term{{Value: "s3"}}, Exclusive: true},
			To:   &Endpoint{Terms: []Term{{Value: "s3"}}, Exclusive: true},
		})
		if !errors.Is(err, ErrDeclaration) {
			t.Errorf("want ErrDeclaration, got %v", err)
		}
	})

	t.Run("both ends arguments, at call time", func(t *testing.T) {
		if err := collection.Scan("by_slug", Range{
			From: &Bound{Values: []any{"s3"}, Exclusive: true},
			To:   &Bound{Values: []any{"s3"}, Exclusive: true},
		}, func(Found) bool { return true }); !errors.Is(err, ErrArgument) {
			t.Errorf("want ErrArgument, got %v", err)
		}
	})
}

// TestAnUnboundedAboveScanIsNeverRefused is mục 6's third row: the
// `upper != nil` guard in stretch() exists so that a caller who wrote no
// upper bound at all is never compared against one. `Walk` (dump) and a scan
// naming only From are the two everyday shapes; the successor()-returns-nil
// case underneath both of them is tested directly against stretch(), with a
// synthetic all-0xff prefix, since no real collection prefix in this repo
// ends in every byte 0xff.
func TestAnUnboundedAboveScanIsNeverRefused(t *testing.T) {
	_, collection := declared(t, 1453)
	fill(t, collection)

	// Walk/dump: Range{} entirely, both directions.
	for _, direction := range []Direction{Forward, Reverse} {
		var got []string
		if err := collection.walkRange(Range{Direction: direction}, func(key any, _ map[string]any) bool {
			got = append(got, key.(string))
			return true
		}); err != nil {
			t.Errorf("Walk, direction=%v: %v", direction, err)
		}
		if len(got) != 6 {
			t.Errorf("Walk, direction=%v: got %d rows, want 6", direction, len(got))
		}
	}

	// A scan naming only From: no To at all.
	for _, direction := range []Direction{Forward, Reverse} {
		got := keysOf(scan(t, collection, "by_slug", Range{From: &Bound{Values: []any{"s3"}}, Direction: direction}))
		if len(got) != 3 {
			t.Errorf("From only, direction=%v: got %v, want 3 rows (s3, s4, s5)", direction, got)
		}
	}

	// The byte-level edge stretch() actually guards against: a prefix that is
	// every byte 0xff, so successor(prefix) is genuinely nil rather than a
	// large-but-real byte string. Constructed directly because no real
	// collection or index prefix in this repo has this shape.
	fields := []Field{{Path: "x", Type: TypeString, Missing: MissingSkip}}
	prefix := []byte{0xff, 0xff, 0xff}
	lower, upper, err := collection.stretch(Range{}, prefix, fields)
	if err != nil {
		t.Fatalf("an all-0xff prefix with no bounds: %v", err)
	}
	if upper != nil {
		t.Fatalf("successor of an all-0xff prefix should be nil, got %x", upper)
	}
	if !bytes.Equal(lower, prefix) {
		t.Errorf("lower should be the prefix itself, got %x", lower)
	}
}

// TestAMissingEndAtCallTimeIsNeverRefused is mục 6's fourth row and task
// 0016's boundary: `bounds()` (invoke.go) drops a whole Bound when its
// argument was not passed, in both directions, and that must read as
// "unbounded on that side", never as a backwards range — dropping a Bound
// is not the same as pinning it to something that could compare backwards.
func TestAMissingEndAtCallTimeIsNeverRefused(t *testing.T) {
	store, collection := declared(t, 1454)
	fill(t, collection)

	declareOp(t, store, Operation{
		Name: "articles.paged_either_way", Collection: "articles", Action: ActionScan, Index: "by_slug", Limit: 10,
		Input: []Parameter{
			{Name: "lo", Type: TypeString},
			{Name: "hi", Type: TypeString},
			{Name: "direction", Type: TypeString, Required: true},
		},
		From:      &Endpoint{Terms: []Term{{Arg: "lo"}}},
		To:        &Endpoint{Terms: []Term{{Arg: "hi"}}},
		Direction: &Term{Arg: "direction"},
	})

	for _, direction := range []string{DirectionForward, DirectionReverse} {
		// Neither given: the whole index, both ends absent.
		result, err := store.Invoke(Caller{}, "articles.paged_either_way", 0, map[string]any{"direction": direction})
		if err != nil {
			t.Fatalf("direction=%s, neither end given: %v", direction, err)
		}
		if result.Count != 6 {
			t.Errorf("direction=%s, neither end given: %d rows, want 6", direction, result.Count)
		}

		// Only "hi" given: From falls away, an unbounded-below scan.
		result, err = store.Invoke(Caller{}, "articles.paged_either_way", 0, map[string]any{"direction": direction, "hi": "s3"})
		if err != nil {
			t.Fatalf("direction=%s, From absent: %v", direction, err)
		}
		if result.Count != 4 { // s0, s1, s2, s3 inclusive
			t.Errorf("direction=%s, From absent: %d rows, want 4", direction, result.Count)
		}

		// Only "lo" given: To falls away, an unbounded-above scan.
		result, err = store.Invoke(Caller{}, "articles.paged_either_way", 0, map[string]any{"direction": direction, "lo": "s3"})
		if err != nil {
			t.Fatalf("direction=%s, To absent: %v", direction, err)
		}
		if result.Count != 3 { // s3, s4, s5
			t.Errorf("direction=%s, To absent: %d rows, want 3", direction, result.Count)
		}
	}
	_ = collection
}

// TestADeclarationAcceptsAMixedEndUntilACallProvesItBackwards is mục 6's
// sixth row: refusedBackwardsRange (ops.go) only runs when EVERY term of
// BOTH endpoints is a constant — the moment either end is an argument, the
// value does not exist yet, and DeclareOperation must accept the shape. The
// same declaration then refuses exactly the calls that turn out backwards,
// and runs exactly the ones that do not.
func TestADeclarationAcceptsAMixedEndUntilACallProvesItBackwards(t *testing.T) {
	store, collection := declared(t, 1455)
	fill(t, collection)

	operation := Operation{
		Name: "articles.from_s4_up_to_t", Collection: "articles", Action: ActionScan, Index: "by_slug", Limit: 10,
		Input: []Parameter{{Name: "t", Type: TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Value: "s4"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "t"}}},
	}
	stored := declareOp(t, store, operation)

	if _, err := store.Invoke(Caller{}, stored.Name, stored.Version, map[string]any{"t": "s1"}); !errors.Is(err, ErrArgument) {
		t.Errorf("t=s1 (backwards): want ErrArgument, got %v", err)
	}
	result, err := store.Invoke(Caller{}, stored.Name, stored.Version, map[string]any{"t": "s5"})
	if err != nil {
		t.Fatalf("t=s5 (forwards): %v", err)
	}
	if result.Count != 2 { // s4, s5
		t.Errorf("t=s5: %d rows, want 2", result.Count)
	}
	_ = collection
}

// TestATotalsInheritsTheSameRefusal is mục 3.4's Totals row and mục 6's
// seventh row: Collection.Totals calls the same stretch() Scan does (task
// 0043), so it inherits the refusal for free — but "for free" is a claim
// about the CODE, and this is the measurement that it is also true of the
// declared path (validateOperation's ActionTotals branch) and the Invoke
// path, with the right error class on each.
func TestATotalsInheritsTheSameRefusal(t *testing.T) {
	_, store := fresh(t, 1456)
	lines := takings(t, store)
	for _, account := range []string{"a", "b", "c", "d", "e"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}

	// Declare time: both ends constants, backwards.
	if _, err := store.DeclareOperation(Operation{
		Name: "lines.backwards_constant", Collection: "lines", Action: ActionTotals, Rollup: "per_account", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "d"}}},
		To:   &Endpoint{Terms: []Term{{Value: "b"}}},
	}); !errors.Is(err, ErrDeclaration) {
		t.Errorf("declare, both ends constant and backwards: want ErrDeclaration, got %v", err)
	}

	// Call time: an argument makes the same shape backwards.
	stored := declareOp(t, store, Operation{
		Name: "lines.by_account_range", Collection: "lines", Action: ActionTotals, Rollup: "per_account", Limit: 10,
		Input: []Parameter{{Name: "lo", Type: TypeString, Required: true}, {Name: "hi", Type: TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "lo"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "hi"}}},
	})
	if _, err := store.Invoke(Caller{}, stored.Name, stored.Version, map[string]any{"lo": "d", "hi": "b"}); !errors.Is(err, ErrArgument) {
		t.Errorf("invoke, backwards arguments: want ErrArgument, got %v", err)
	}
	result, err := store.Invoke(Caller{}, stored.Name, stored.Version, map[string]any{"lo": "b", "hi": "d"})
	if err != nil {
		t.Fatalf("invoke, forwards arguments: %v", err)
	}
	if result.Count != 3 { // b, c, d
		t.Errorf("b..d: %d rows, want 3", result.Count)
	}

	// And directly at the Collection.Totals level, the same stretch() call
	// Scan uses — the level TestTotalsRowsMatchATreeWalkBoundedByStretchesOwn
	// Bytes (rollup_test.go) already anchors for the forward, non-backwards
	// case.
	if err := lines.Totals("per_account", Range{
		From: &Bound{Values: []any{"d"}}, To: &Bound{Values: []any{"b"}},
	}, func(Totals) bool { return true }); !errors.Is(err, ErrArgument) {
		t.Errorf("Collection.Totals, backwards bounds: want ErrArgument, got %v", err)
	}
}
