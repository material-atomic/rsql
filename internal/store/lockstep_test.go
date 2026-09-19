package store

import (
	"errors"
	"strings"
	"testing"
)

// Task 0054 exists because three places in internal/store read the i-th
// element of one list while walking another — scanAcross's and
// totalsAcross's Terms[i], and endpointBound's implicit "every term" loop —
// and every existing index or rollup group in this repo has exactly one
// field. At N=1, "the i-th element" and "the first element" are the same
// expression, so a suite built entirely on N=1 fixtures cannot tell a
// lockstep bug from a correct one: it is not that the tests are shallow, it
// is that the property under test ("every field is read at its own
// position, in order") has no N>1 case to be false on.
//
// lockstepStore is the one fixture every test in this file shares, built
// wide enough that the eleven mutations Planner 0054 found surviving (see
// the task doc, mục 1.1) each have a case that can only pass by reading the
// right field at the right position. Every test below either tries to make
// a "must be refused" claim false (a case the refusal does not catch), or
// tries to make an "must be accepted" claim false (a case an over-tight
// check refuses) — house rule: a universal claim needs a table trying to
// break it, not a single sample that happens to hold.

// lockstepStore declares two collections sharing the same field shapes.
//
// Two collections, not one, is a fact of the code rather than a taste of the
// fixture: scanAcross (ops.go) refuses a scan of a PARTITIONED collection
// that leaves any field of its index ranging, before refusedBackwardsRange
// ever gets a chance to judge the values. So "every field must be pinned"
// (scanAcross's rule) and "a backwards range must be refused" (0045's rule)
// need different collections to exercise — a partitioned one for the first,
// an unpartitioned one for the second, or the first rule refuses the
// declaration before the second is ever reached. Measured directly: Planner
// hit this once (a probe named for the wrong function) before splitting the
// fixture in two.
func lockstepStore(t *testing.T, seed int64) *Store {
	t.Helper()
	// partitioned, not fresh: "wide" below declares a Partition, and a
	// database opened without somewhere to keep one (store.Keep) refuses
	// every write to it with ErrNoFiles rather than silently going
	// unpartitioned.
	_, _, store := partitioned(t, seed)

	if _, err := store.Declare(Spec{
		Name:      "wide",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
		Indexes: []Index{
			// w1: N=1, the width every existing index and rollup group in
			// this repo has. Kept in the table on purpose — it is the
			// negative control that proves the wide indexes below are
			// adding a real axis rather than a differently-flavored N=1.
			{Name: "w1", Fields: []Field{
				{Path: "a", Type: TypeString, Missing: MissingSkip},
			}},
			// w2: N=2, and the second field is DESCENDING while the first
			// is not. That difference in direction is what makes
			// fields[0].encoding() and fields[1].encoding() two different
			// byte shapes rather than two spellings of the same one — with
			// same-direction fields, a bug that reads fields[0] everywhere
			// produces the identical bytes a correct read would, and
			// nothing distinguishes them. See mục 4.2 of the task doc.
			{Name: "w2", Fields: []Field{
				{Path: "a", Type: TypeString, Missing: MissingSkip},
				{Path: "n", Type: TypeNumber, Descending: true, Missing: MissingLast},
			}},
			// w3: N=3. This is the width that catches a mutant clamping its
			// index — fields[min(i,1)] — which is indistinguishable from
			// fields[i] at every i<=1 and therefore invisible at N<=2 no
			// matter how carefully an N<=2 table is written.
			{Name: "w3", Fields: []Field{
				{Path: "a", Type: TypeString, Missing: MissingSkip},
				{Path: "n", Type: TypeNumber, Descending: true, Missing: MissingLast},
				{Path: "b", Type: TypeBool, Missing: MissingFirst},
			}},
			// w2any: the one index field in this fixture declared "any",
			// which is the only way constantIsEncodable (ops.go) is ever
			// asked to refuse anything on the SCAN side — matches() lets a
			// slice or a map satisfy TypeAny, and keys.Encode is what
			// actually refuses those.
			{Name: "w2any", Fields: []Field{
				{Path: "a", Type: TypeString, Missing: MissingSkip},
				{Path: "z", Type: TypeAny, Missing: MissingLast},
			}},
		},
		Rollups: []Rollup{{
			Name: "g2",
			Group: []Field{
				{Path: "account", Type: TypeString, Missing: MissingSkip},
				// Descending, unlike account. Every rollup group in this
				// repo before this task had exactly one field, so
				// keys.EncodeKey (internal/keys) and encodings()
				// (collection.go) have never been asked to carry more than
				// one field for a rollup — and if this field shared
				// account's direction, a version of keys.EncodeKey that
				// encoded every value with fields[0] would produce the
				// identical bytes a correct per-position encode does,
				// because Missing never changes the bytes of a value that
				// is actually present and Descending is the only thing that
				// does. See TestARollupGroupWithTwoDifferentDirectionsTotalsEachCombinationSeparately.
				{Path: "kind", Type: TypeString, Descending: true, Missing: MissingSkip},
			},
			Count: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// plain: the same w2/w3 widths again, unpartitioned, so a backwards-range
	// declaration reaches refusedBackwardsRange instead of being refused
	// earlier by scanAcross for a reason that has nothing to do with
	// direction.
	if _, err := store.Declare(Spec{
		Name: "plain",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{
			{Name: "w2", Fields: []Field{
				{Path: "a", Type: TypeString, Missing: MissingSkip},
				{Path: "n", Type: TypeNumber, Descending: true, Missing: MissingLast},
			}},
			{Name: "w3", Fields: []Field{
				{Path: "a", Type: TypeString, Missing: MissingSkip},
				{Path: "n", Type: TypeNumber, Descending: true, Missing: MissingLast},
				{Path: "b", Type: TypeBool, Missing: MissingFirst},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	return store
}

// mustNameOnly asserts a refusal names exactly one field. It is written as a
// dương-âm pair on purpose: the field names in this fixture are one
// character long ("a", "n", "b", "z"), and every refusal here begins with
// "sapedb/store:" — a string that itself contains "n" — so a bare
// strings.Contains(err, wanted) without the negative half would pass no
// matter which field the message actually names. house-rules.md names this
// trap exactly: quote the field ("\"n\"", not "n"), and always pair the
// positive assertion with a negative one for the field it must NOT name.
func mustNameOnly(t *testing.T, err error, wanted string, notThese ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; wanted a refusal naming %s", wanted)
	}
	if !errors.Is(err, ErrDeclaration) && !errors.Is(err, ErrType) && !errors.Is(err, ErrArgument) {
		t.Fatalf("refused with an unclassified error: %v", err)
	}
	if !strings.Contains(err.Error(), wanted) {
		t.Errorf("the refusal does not name %s: %v", wanted, err)
	}
	for _, other := range notThese {
		if strings.Contains(err.Error(), other) {
			t.Errorf("the refusal names %s, which is the wrong field: %v", other, err)
		}
	}
}

// TestAScanOfAPartitionedCollectionRefusesALooseFieldAtEveryPosition is
// scanAcross's headline rule, tried at N=2 and N=3, with the loose field at
// every position a 3-field index has — first, middle and last. A table that
// only ever leaves the LAST field loose cannot tell fields[i] from
// fields[len-1]; this one leaves the MIDDLE field loose too, which only a
// real index of the middle Fields[i]/Terms[i] distinguishes from either end.
func TestAScanOfAPartitionedCollectionRefusesALooseFieldAtEveryPosition(t *testing.T) {
	store := lockstepStore(t, 5401)

	for _, c := range []struct {
		name  string
		index string
		from  []Term
		to    []Term
		field string
		not   []string
	}{
		{"w2, second (last) field loose", "w2",
			[]Term{{Value: "x"}, {Value: 9.0}}, []Term{{Value: "x"}, {Value: 1.0}},
			`"n"`, []string{`"a"`}},
		{"w3, MIDDLE field loose", "w3",
			[]Term{{Value: "x"}, {Value: 9.0}, {Value: true}}, []Term{{Value: "x"}, {Value: 1.0}, {Value: true}},
			`"n"`, []string{`"a"`, `"b"`}},
		{"w3, LAST field loose", "w3",
			[]Term{{Value: "x"}, {Value: 9.0}, {Value: false}}, []Term{{Value: "x"}, {Value: 9.0}, {Value: true}},
			`"b"`, []string{`"a"`, `"n"`}},
		{"w3, FIRST field loose", "w3",
			[]Term{{Value: "x"}, {Value: 9.0}, {Value: true}}, []Term{{Value: "y"}, {Value: 9.0}, {Value: true}},
			`"a"`, []string{`"n"`, `"b"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := store.DeclareOperation(Operation{
				Name: "loose." + c.name, Collection: "wide", Action: ActionScan,
				Index: c.index, Limit: 10,
				From: &Endpoint{Terms: c.from}, To: &Endpoint{Terms: c.to},
			})
			mustNameOnly(t, err, c.field, c.not...)
		})
	}
}

// TestAScanOfAPartitionedCollectionAcceptsEveryFieldPinnedAtEveryWidth is
// the widening half of the rule above (task 0054 mục 5, P22) — a patch that
// closes "a loose field is refused" by over-tightening the check would show
// up here, refusing a scan that pins ALL of its fields. Three widths: N=1
// is the shape every pre-existing index has and must keep working; N=2 and
// N=3 are the new ones.
func TestAScanOfAPartitionedCollectionAcceptsEveryFieldPinnedAtEveryWidth(t *testing.T) {
	store := lockstepStore(t, 5402)
	for _, c := range []struct {
		name  string
		index string
		terms []Term
	}{
		{"w1 (N=1) pinned", "w1", []Term{{Value: "x"}}},
		{"w2 (N=2) pinned", "w2", []Term{{Value: "x"}, {Value: 9.0}}},
		{"w3 (N=3) pinned", "w3", []Term{{Value: "x"}, {Value: 9.0}, {Value: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := store.DeclareOperation(Operation{
				Name: "widen." + c.name, Collection: "wide", Action: ActionScan,
				Index: c.index, Limit: 10,
				From: &Endpoint{Terms: c.terms}, To: &Endpoint{Terms: c.terms},
			}); err != nil {
				t.Fatalf("a fully pinned scan on %s was refused: %v", c.index, err)
			}
		})
	}
}

// TestARollupReadOfAPartitionedCollectionRefusesALooseGroupField is
// totalsAcross's version of the same rule, on a two-field Group — a shape
// that, before this task, did not exist anywhere in the repo, so
// totalsAcross's loop had never run past i=0.
func TestARollupReadOfAPartitionedCollectionRefusesALooseGroupField(t *testing.T) {
	store := lockstepStore(t, 5403)
	// "kind" is declared Descending (see lockstepStore's doc), so "b" then
	// "a" is the FORWARD direction for it — chosen on purpose so this hits
	// totalsAcross's "must fix kind" refusal rather than
	// refusedBackwardsRange's, which runs first in validateOperation and
	// would otherwise catch a genuinely backwards pair for the wrong
	// reason. Measured: "in" then "out" (the natural-looking ascending
	// order) is BACKWARDS for a descending field and is refused by
	// refusedBackwardsRange instead, never reaching totalsAcross at all.
	_, err := store.DeclareOperation(Operation{
		Name: "loose.g2", Collection: "wide", Action: ActionTotals,
		Rollup: "g2", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "cash"}, {Value: "b"}}},
		To:   &Endpoint{Terms: []Term{{Value: "cash"}, {Value: "a"}}},
	})
	mustNameOnly(t, err, `"kind"`, `"account"`)
}

// TestARollupReadOfAPartitionedCollectionAcceptsAFullyPinnedGroup is the
// widening half (P24): a rollup read that pins every group field must still
// be accepted.
func TestARollupReadOfAPartitionedCollectionAcceptsAFullyPinnedGroup(t *testing.T) {
	store := lockstepStore(t, 5404)
	if _, err := store.DeclareOperation(Operation{
		Name: "widen.g2", Collection: "wide", Action: ActionTotals,
		Rollup: "g2", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "cash"}, {Value: "in"}}},
		To:   &Endpoint{Terms: []Term{{Value: "cash"}, {Value: "in"}}},
	}); err != nil {
		t.Fatalf("a fully pinned rollup read was refused: %v", err)
	}
}

// TestABoundIsTypeCheckedAgainstItsOwnFieldNotAnother covers validateOperation's
// check() call on both branches of its switch (ActionScan and ActionTotals):
// a wrongly-typed constant must be refused naming the field it was actually
// written against, not fields[0]. The scan half uses w3 so the wrong value
// can land in the middle field (n) or the last one (b); the rollup half
// mirrors it on g2's second field, which is the one that had no coverage
// anywhere in the repo before this task (mục 1.1: M6 sống, M7 chết — the
// same line of code in the two branches of one switch).
func TestABoundIsTypeCheckedAgainstItsOwnFieldNotAnother(t *testing.T) {
	store := lockstepStore(t, 5405)

	_, err := store.DeclareOperation(Operation{
		Name: "type.n", Collection: "wide", Action: ActionScan,
		Index: "w3", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "x"}, {Value: "not a number"}}},
	})
	mustNameOnly(t, err, `"n"`, `"a"`, `"b"`)

	_, err = store.DeclareOperation(Operation{
		Name: "type.b", Collection: "wide", Action: ActionScan,
		Index: "w3", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "x"}, {Value: 9.0}, {Value: 7.0}}},
	})
	mustNameOnly(t, err, `"b"`, `"a"`, `"n"`)

	// The rollup branch of the same check() call — the equivalent path a
	// table built only from the scan side would never reach.
	_, err = store.DeclareOperation(Operation{
		Name: "type.kind", Collection: "wide", Action: ActionTotals,
		Rollup: "g2", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "cash"}, {Value: 3.0}}},
	})
	mustNameOnly(t, err, `"kind"`, `"account"`)
}

// TestAnUnencodableConstantBoundIsNamedAtItsOwnField is constantIsEncodable's
// SCAN-side refusal (validateOperation, M9 in the task doc): the second
// field of w2any is declared "any", so matches() lets a slice through
// check() above, and keys.Encode is the thing that actually refuses it. The
// rollup-side twin of this same line (M8 / task 0054 mục 1.3, "P12") is NOT
// tested here on purpose: Rollup.validate() refuses TypeAny for a Group
// field, so constantIsEncodable's rollup branch has no declaration in this
// repo that can ever make it return an error — see the comment beside that
// line in ops.go, which predates this task (759daeb) and already says so.
// Writing a test that could only pass by asserting an error that can never
// happen would be exactly the fake test the task forbids.
func TestAnUnencodableConstantBoundIsNamedAtItsOwnField(t *testing.T) {
	store := lockstepStore(t, 5406)
	_, err := store.DeclareOperation(Operation{
		Name: "unencodable.z", Collection: "wide", Action: ActionScan,
		Index: "w2any", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "x"}, {Value: []any{1.0, 2.0}}}},
	})
	mustNameOnly(t, err, `"z"`, `"a"`)
}

// TestAWriteWithAnUnencodableFieldNamesItsOwnField is the WRITE-side
// equivalent of the test above — QA 0044's debt (mục 1), confirmed still
// open at 5a482ea (M14 SURVIVED) and closed here. entriesForIndex
// (collection.go) is a parallel path to constantIsEncodable, not the same
// function, and house-rules.md's "chỗ đầu tiên phải tự soi là ĐƯỜNG TƯƠNG
// ĐƯƠNG" is exactly the shape this pair is: a read path that was tested and
// a write path, doing the equivalent check, that was not.
func TestAWriteWithAnUnencodableFieldNamesItsOwnField(t *testing.T) {
	_, store := fresh(t, 5407)
	collection, err := store.Declare(Spec{
		Name: "writes",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{{Name: "w2any", Fields: []Field{
			{Path: "a", Type: TypeString, Missing: MissingSkip},
			{Path: "z", Type: TypeAny, Missing: MissingLast},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = collection.Put(map[string]any{"a": "x", "z": []any{1.0}})
	mustNameOnly(t, err, `"z"`, `"a"`)
}

// TestABackwardsRangeInASecondOrThirdFieldIsRefused is endpointBound's rule
// (M3 in the task doc): a declaration whose SECOND or THIRD field runs
// backwards on a DESCENDING field must be refused as a declaration, even
// though the first field is fine. Run on "plain" (unpartitioned), because
// on "wide" scanAcross refuses this before refusedBackwardsRange is ever
// reached — see lockstepStore's own doc for why two collections exist. This
// is also 0054's sibling to 0016: refusedBackwardsRange has no way to see
// this at all unless endpointBound actually carries every term, which is
// the very thing M3 breaks.
func TestABackwardsRangeInASecondOrThirdFieldIsRefused(t *testing.T) {
	store := lockstepStore(t, 5408)

	// w2's second field is DESCENDING, so ascending values there are
	// backwards even though the first field (a="x") is pinned identically
	// at both ends.
	_, err := store.DeclareOperation(Operation{
		Name: "back.w2", Collection: "plain", Action: ActionScan,
		Index: "w2", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "x"}, {Value: 1.0}}},
		To:   &Endpoint{Terms: []Term{{Value: "x"}, {Value: 5.0}}},
	})
	if err == nil {
		t.Fatal("a declaration backwards in its SECOND field was accepted")
	}
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("refused, but not as a declaration: %v", err)
	}

	// Same shape, three fields: backwards only in the middle one (n,
	// DESCENDING), with the first and third pinned identically at both
	// ends.
	_, err = store.DeclareOperation(Operation{
		Name: "back.w3", Collection: "plain", Action: ActionScan,
		Index: "w3", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "x"}, {Value: 1.0}, {Value: true}}},
		To:   &Endpoint{Terms: []Term{{Value: "x"}, {Value: 5.0}, {Value: true}}},
	})
	if err == nil {
		t.Fatal("a declaration backwards in its SECOND of three fields was accepted")
	}
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("refused, but not as a declaration: %v", err)
	}
}

// TestACorrectlyOrderedDescendingRangeIsAcceptedAndReadsAllRows is the
// widening half of the test above (task 0054 mục 5, P23): a stretch that IS
// ordered correctly for a descending field must be accepted, and it must
// come back with the right ROWS, not just no error — an over-tight
// refusedBackwardsRange could satisfy "no error" while still, say, refusing
// every call underneath. w3's three-field case is checked for acceptance
// only, as the task's own table asks for; w2's is checked for row count too.
func TestACorrectlyOrderedDescendingRangeIsAcceptedAndReadsAllRows(t *testing.T) {
	store := lockstepStore(t, 5409)
	collection, err := store.Collection("plain")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []float64{1, 2, 3, 4, 5} {
		put(t, collection, map[string]any{"a": "x", "n": n})
	}
	// A control document outside the "a" bucket the range below asks for —
	// present so a mutant that drops the first field's pin entirely would
	// show up as an extra row.
	put(t, collection, map[string]any{"a": "y", "n": 3.0})

	if _, err := store.DeclareOperation(Operation{
		Name: "fwd.w2", Collection: "plain", Action: ActionScan,
		Index: "w2", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "x"}, {Value: 5.0}}},
		To:   &Endpoint{Terms: []Term{{Value: "x"}, {Value: 1.0}}},
	}); err != nil {
		t.Fatalf("a correctly ordered descending stretch was refused: %v", err)
	}
	result, err := store.Invoke(Caller{}, "fwd.w2", 0, nil)
	if err != nil {
		t.Fatalf("invoking the accepted stretch failed: %v", err)
	}
	if result.Count != 5 {
		t.Fatalf("From[x,5]/To[x,1] on w2 returned %d rows, want 5: %v", result.Count, result.Rows)
	}

	if _, err := store.DeclareOperation(Operation{
		Name: "fwd.w3", Collection: "plain", Action: ActionScan,
		Index: "w3", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: "x"}, {Value: 5.0}, {Value: true}}},
		To:   &Endpoint{Terms: []Term{{Value: "x"}, {Value: 1.0}, {Value: true}}},
	}); err != nil {
		t.Fatalf("a correctly ordered three-field descending stretch was refused: %v", err)
	}
}

// TestAThreeFieldScanReadsExactRowsProvingEachFieldUsesItsOwnPosition is the
// one test in this file that reads real data back through a three-field
// index rather than only checking that a declaration is accepted or
// refused — task 0054 mục 4.3's row for boundAt/entriesForIndex/encodings/
// keys.EncodeKey/keys.DecodeKey ("một bound không được mã hoá bằng encoding
// của trường đầu"), and the ONLY case in this file that distinguishes
// fields[2] from a mutant clamping it to fields[min(2,1)] — P16 in the task
// doc. That mutant is byte-for-byte identical to correct code at every
// index <= 1, so no N<=2 fixture, however carefully written, can ever make
// it fail: only a real bound with three values, actually walked against
// real data, can.
//
// w3's fields are a (string, ascending), n (number, DESCENDING) and b
// (bool, ascending, MissingFirst) — three different encoding shapes, so a
// mutant using the wrong field's encoding() at any position changes the
// bytes rather than accidentally agreeing with the right one.
func TestAThreeFieldScanReadsExactRowsProvingEachFieldUsesItsOwnPosition(t *testing.T) {
	store := lockstepStore(t, 5410)
	collection, err := store.Collection("plain")
	if err != nil {
		t.Fatal(err)
	}

	type doc struct {
		a string
		n float64
		b bool
	}
	docs := []doc{
		{"x", 1, false}, // outside the n=2 bound: must not come back
		{"x", 2, false}, // inside
		{"x", 2, true},  // inside
		{"x", 3, true},  // outside the n=2 bound: must not come back
		{"y", 2, false}, // outside the "a" bound: must not come back
	}
	for _, d := range docs {
		put(t, collection, map[string]any{"a": d.a, "n": d.n, "b": d.b})
	}

	// a="x", n=2 exactly, b from false through true inclusive — the only
	// two rows that must come back are (x,2,false) and (x,2,true).
	found := scan(t, collection, "w3", Range{
		From: &Bound{Values: []any{"x", 2.0, false}},
		To:   &Bound{Values: []any{"x", 2.0, true}},
	})

	if len(found) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(found), found)
	}
	want := [][]any{
		{"x", 2.0, false},
		{"x", 2.0, true},
	}
	for i, entry := range found {
		if len(entry.Values) != 3 {
			t.Fatalf("entry %d has %d values, want 3: %v", i, len(entry.Values), entry.Values)
		}
		got := []any{entry.Values[0], entry.Values[1], entry.Values[2]}
		if got[0] != want[i][0] || got[1] != want[i][1] || got[2] != want[i][2] {
			t.Errorf("entry %d = %v, want %v", i, got, want[i])
		}
	}
}

// TestARollupGroupWithTwoDifferentDirectionsTotalsEachCombinationSeparately
// exercises keys.EncodeKey and keys.DecodeKey with a two-field rollup group
// where the two fields do NOT share a direction — the shape that, before
// this task, existed nowhere in the repo (task doc mục 1.2: "nhóm rollup
// nhiều hơn một trường: KHÔNG CÓ, ở bất kỳ đâu"). Every write to a group
// goes through groupKey (rollup.go), which calls keys.EncodeKey once for
// the whole Group; every read decodes the same bytes with keys.DecodeKey.
// A version of either that used fields[0]'s encoding for every position
// would, for account (ascending) and kind (DESCENDING), write kind's tag
// byte uncomplemented and then try to read it back as though it were
// complemented — which keys.Decode does not silently misread as a
// different value, it refuses with ErrTag, because the byte no longer
// matches any known tag. So this is not a silent-corruption case; a
// lockstep bug here surfaces as Totals returning an error where the
// unmutated code returns four distinct, correctly-labeled rows.
func TestARollupGroupWithTwoDifferentDirectionsTotalsEachCombinationSeparately(t *testing.T) {
	store := lockstepStore(t, 5411)
	collection, err := store.Collection("wide")
	if err != nil {
		t.Fatal(err)
	}

	writes := []struct {
		account, kind string
	}{
		{"acme", "orders"},
		{"acme", "orders"},
		{"acme", "refunds"},
		{"beta", "orders"},
	}
	for _, w := range writes {
		put(t, collection, map[string]any{"account": w.account, "kind": w.kind})
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	rows := allTotals(t, collection, "g2", Range{})
	counts := map[string]int{}
	for _, row := range rows {
		if len(row.Group) != 2 {
			t.Fatalf("a row's Group has %d values, want 2: %v", len(row.Group), row.Group)
		}
		account, ok1 := row.Group[0].(string)
		kind, ok2 := row.Group[1].(string)
		if !ok1 || !ok2 {
			t.Fatalf("a row's Group values are not both strings: %v", row.Group)
		}
		counts[account+"/"+kind] = row.Count
	}

	want := map[string]int{"acme/orders": 2, "acme/refunds": 1, "beta/orders": 1}
	if len(counts) != len(want) {
		t.Fatalf("got %d groups, want %d: %v", len(counts), len(want), counts)
	}
	for key, count := range want {
		if counts[key] != count {
			t.Errorf("group %q has count %d, want %d (all counts: %v)", key, counts[key], count, counts)
		}
	}
}
