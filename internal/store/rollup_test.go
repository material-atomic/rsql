package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/vfs"
)

// takings is a collection with totals kept per account.
func takings(t *testing.T, store *Store) *Collection {
	t.Helper()

	made, err := store.Declare(Spec{
		Name: "lines",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true,
			Sum:   []string{"amount"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	return made
}

func totalsOf(t *testing.T, collection *Collection, account string) (int, float64, bool) {
	t.Helper()

	var row Totals
	found := false
	if err := collection.Totals("per_account", Range{
		From: &Bound{Values: []any{account}}, To: &Bound{Values: []any{account}},
	}, func(one Totals) bool {
		row, found = one, true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return row.Count, row.Sum["amount"], found
}

// TestATotalMovesWithTheWriteThatChangesIt: the whole point. A counter kept
// anywhere else stops matching the data the first time something goes wrong
// between the two, and nothing announces it.
func TestATotalMovesWithTheWriteThatChangesIt(t *testing.T) {
	_, store := fresh(t, 400)
	lines := takings(t, store)

	first, err := lines.Put(map[string]any{"account": "cash", "amount": 10.0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lines.Put(map[string]any{"account": "cash", "amount": 2.5}); err != nil {
		t.Fatal(err)
	}
	if _, err := lines.Put(map[string]any{"account": "bank", "amount": 100.0}); err != nil {
		t.Fatal(err)
	}

	if count, sum, found := totalsOf(t, lines, "cash"); !found || count != 2 || sum != 12.5 {
		t.Errorf("cash is %d lines and %v, want 2 and 12.5", count, sum)
	}
	if count, sum, found := totalsOf(t, lines, "bank"); !found || count != 1 || sum != 100 {
		t.Errorf("bank is %d lines and %v, want 1 and 100", count, sum)
	}

	// Changing a document moves the total by the difference, which means
	// taking the old one away and putting the new one in.
	if _, err := lines.Put(map[string]any{"id": first, "account": "cash", "amount": 40.0}); err != nil {
		t.Fatal(err)
	}
	if count, sum, _ := totalsOf(t, lines, "cash"); count != 2 || sum != 42.5 {
		t.Errorf("after the change cash is %d lines and %v, want 2 and 42.5", count, sum)
	}

	// Moving it to another group takes it out of one and puts it in the other.
	if _, err := lines.Put(map[string]any{"id": first, "account": "bank", "amount": 40.0}); err != nil {
		t.Fatal(err)
	}
	if count, sum, _ := totalsOf(t, lines, "cash"); count != 1 || sum != 2.5 {
		t.Errorf("after the move cash is %d lines and %v, want 1 and 2.5", count, sum)
	}
	if count, sum, _ := totalsOf(t, lines, "bank"); count != 2 || sum != 140 {
		t.Errorf("after the move bank is %d lines and %v, want 2 and 140", count, sum)
	}

	// And deleting subtracts exactly what the write added.
	if removed, err := lines.Delete(first); err != nil || !removed {
		t.Fatal(err)
	}
	if count, sum, _ := totalsOf(t, lines, "bank"); count != 1 || sum != 100 {
		t.Errorf("after the delete bank is %d lines and %v, want 1 and 100", count, sum)
	}
}

// TestAGroupWithNothingInItIsGone: a row sitting there saying zero would be
// returned by every read that walked past it, for as long as the database
// lives.
func TestAGroupWithNothingInItIsGone(t *testing.T) {
	_, store := fresh(t, 401)
	lines := takings(t, store)

	only, err := lines.Put(map[string]any{"account": "petty", "amount": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, found := totalsOf(t, lines, "petty"); !found {
		t.Fatal("the group was not made")
	}

	if _, err := lines.Delete(only); err != nil {
		t.Fatal(err)
	}
	if _, _, found := totalsOf(t, lines, "petty"); found {
		t.Error("a group with nothing in it is still there")
	}
}

// TestATotalSurvivesARollbackAndACrash: the total and the document are one
// transaction, so neither can be there without the other.
func TestATotalSurvivesARollbackAndACrash(t *testing.T) {
	disk, store := fresh(t, 402)
	lines := takings(t, store)

	if _, err := lines.Put(map[string]any{"account": "cash", "amount": 10.0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// A write that is abandoned takes its contribution with it.
	if _, err := lines.Put(map[string]any{"account": "cash", "amount": 999.0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}
	back, err := store.Collection("lines")
	if err != nil {
		t.Fatal(err)
	}
	if count, sum, _ := totalsOf(t, back, "cash"); count != 1 || sum != 10 {
		t.Errorf("after the rollback cash is %d lines and %v, want 1 and 10", count, sum)
	}

	// And what was committed comes back with its total.
	restart := vfs.NewSim(402, vfs.Faults{})
	restart.Restore(disk.Durable())
	pages, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.Collection("lines")
	if err != nil {
		t.Fatal(err)
	}
	if count, sum, _ := totalsOf(t, after, "cash"); count != 1 || sum != 10 {
		t.Errorf("after the restart cash is %d lines and %v, want 1 and 10", count, sum)
	}
}

// TestARollupDeclaredAfterTheDataIsFilledFromIt, through the same function
// that keeps it up to date — so a rollup declared late cannot differ from one
// declared early.
func TestARollupDeclaredAfterTheDataIsFilledFromIt(t *testing.T) {
	_, store := fresh(t, 403)

	plain, err := store.Declare(Spec{
		Name: "lines", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []float64{1, 2, 3} {
		if _, err := plain.Put(map[string]any{"account": "cash", "amount": amount}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	withTotals, err := store.Declare(Spec{
		Name: "lines", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true, Sum: []string{"amount"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if count, sum, found := totalsOf(t, withTotals, "cash"); !found || count != 3 || sum != 6 {
		t.Errorf("a rollup filled from three lines is %d and %v, want 3 and 6", count, sum)
	}

	// Dropping it takes its rows with it, rather than leaving a keyspace
	// nothing points at.
	if _, err := store.Declare(Spec{
		Name: "lines", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"},
	}); err != nil {
		t.Fatal(err)
	}
	gone, err := store.Collection("lines")
	if err != nil {
		t.Fatal(err)
	}
	if err := gone.Totals("per_account", Range{}, func(Totals) bool { return true }); !errors.Is(err, ErrNoRollup) {
		t.Errorf("reading a dropped rollup: %v", err)
	}
	left := 0
	if err := store.tree.Ascend([]byte{spaceRollups}, func(key, _ []byte) bool {
		if len(key) == 0 || key[0] != spaceRollups {
			return false
		}
		left++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("dropping the rollup left %d rows behind", left)
	}
}

// TestWhatARollupWillNotHold: minimum and maximum are not here, and the reason
// is the reason for everything else in this store. Deleting the current
// maximum means scanning the group to find the next one — unbounded work, at
// write time, that nobody declared.
func TestWhatARollupWillNotHold(t *testing.T) {
	_, store := fresh(t, 404)

	for _, wrong := range []Rollup{
		{Name: "nothing", Group: []Field{{Path: "a", Type: TypeString, Missing: MissingSkip}}},
		{Name: "no path", Count: true, Group: []Field{{Path: "", Type: TypeString, Missing: MissingSkip}}},
		{Name: "no missing", Count: true, Group: []Field{{Path: "a", Type: TypeString}}},
		{Name: "odd type", Count: true, Group: []Field{{Path: "a", Type: "blob", Missing: MissingSkip}}},
		{Name: "sums nothing named", Count: true, Sum: []string{""}},
	} {
		if _, err := store.Declare(Spec{
			Name: "lines", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"},
			Rollups: []Rollup{wrong},
		}); !errors.Is(err, ErrDeclaration) {
			t.Errorf("%q was accepted: %v", wrong.Name, err)
		}
	}

	lines := takings(t, store)

	// A document whose summed field is not a number is refused, rather than
	// quietly left out of a total nobody would then be able to trust.
	if _, err := lines.Put(map[string]any{"account": "cash", "amount": "ten"}); !errors.Is(err, ErrType) {
		t.Errorf("a line whose amount is a word: %v", err)
	}
	if count, _, found := totalsOf(t, lines, "cash"); found && count != 0 {
		t.Errorf("the refused line is in the total: %d", count)
	}
}

// TestARollupOfAPartitionedCollectionNamesOneGroup: each partition holds part
// of every total, so adding them up over a range would mean holding every
// group in the range until the last partition had been walked — memory nobody
// declared, bounded by the data rather than by the operation.
func TestARollupOfAPartitionedCollectionNamesOneGroup(t *testing.T) {
	_, _, store := partitioned(t, 405)

	lines, err := store.Declare(Spec{
		Name:      "lines",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true, Sum: []string{"amount"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// The same account, in two months.
	for _, month := range []time.Month{time.January, time.February} {
		atMonth(t, store, time.Date(2026, month, 10, 0, 0, 0, 0, time.UTC))
		if _, err := lines.Put(map[string]any{"account": "cash", "amount": 5.0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Read as one number, added across the partitions that hold it.
	if count, sum, found := totalsOf(t, lines, "cash"); !found || count != 2 || sum != 10 {
		t.Errorf("cash across two months is %d and %v, want 2 and 10", count, sum)
	}

	if _, err := store.DeclareOperation(Operation{
		Name: "lines.for_account", Collection: "lines", Action: ActionTotals, Rollup: "per_account", Limit: 10,
		Input: []Parameter{{Name: "account", Type: TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "account"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "account"}}},
	}); err != nil {
		t.Errorf("reading one group of a partitioned collection: %v", err)
	}
	if _, err := store.DeclareOperation(Operation{
		Name: "lines.everything", Collection: "lines", Action: ActionTotals, Rollup: "per_account", Limit: 10,
	}); !errors.Is(err, ErrDeclaration) {
		t.Errorf("reading every group of a partitioned collection: %v", err)
	}

	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	answer, err := store.Invoke(Caller{}, "lines.for_account", 0, map[string]any{"account": "cash"})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Count != 1 || answer.Rows[0]["count"] != 2.0 || answer.Rows[0]["amount"] != 10.0 {
		t.Errorf("the declared read came back as %+v", answer.Rows)
	}
}

// allTotals is every row a rollup returns over a stretch, in the order it
// returned them.
func allTotals(t *testing.T, collection *Collection, name string, within Range) []Totals {
	t.Helper()

	rows := []Totals{}
	if err := collection.Totals(name, within, func(one Totals) bool {
		rows = append(rows, one)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestARollupHoldsOnlyWhatItCanDescribe: the two ways a document can fail to
// belong, and neither of them may end with it counted somewhere vague.
func TestARollupHoldsOnlyWhatItCanDescribe(t *testing.T) {
	_, store := fresh(t, 406)
	lines := takings(t, store)

	if _, err := lines.Put(map[string]any{"account": "cash", "amount": 1.0}); err != nil {
		t.Fatal(err)
	}

	// The rollup was told to skip a document with no account, so there is no
	// group for it to be in. Counting it under an absent-value group instead
	// would put it in a row somebody later reads as if it meant something.
	if _, err := lines.Put(map[string]any{"amount": 99.0}); err != nil {
		t.Fatal(err)
	}
	rows := allTotals(t, lines, "per_account", Range{})
	if len(rows) != 1 || rows[0].Count != 1 || rows[0].Sum["amount"] != 1 {
		t.Errorf("a line with no account got into the totals: %+v", rows)
	}

	// An account that is a number is not a group this rollup can hold, and
	// putting it in one anyway would mean a row whose group value is not the
	// type every reader of that rollup was told to expect.
	if _, err := lines.Put(map[string]any{"account": 7.0, "amount": 1.0}); !errors.Is(err, ErrType) {
		t.Errorf("a line whose account is a number: %v", err)
	}
	if rows := allTotals(t, lines, "per_account", Range{}); len(rows) != 1 {
		t.Errorf("the refused line left groups behind: %+v", rows)
	}
}

// TestADocumentTheTotalsRefuseIsNotStored: the total and the document go in
// together, so a document that stops the totals must not be sitting there
// afterwards with nothing counting it.
func TestADocumentTheTotalsRefuseIsNotStored(t *testing.T) {
	_, store := fresh(t, 407)
	lines := takings(t, store)

	if _, err := lines.Put(map[string]any{"id": "line-1", "account": "cash", "amount": "ten"}); !errors.Is(err, ErrType) {
		t.Fatalf("a line whose amount is a word: %v", err)
	}
	if _, found, err := lines.Get("line-1"); err != nil || found {
		t.Errorf("the refused line is stored anyway: found=%v err=%v", found, err)
	}
}

// TestATotalReadStaysInsideWhatItWasAskedFor: a rollup is a stretch of the
// same tree as every other rollup, and only the bounds say where the answer
// ends.
func TestATotalReadStaysInsideWhatItWasAskedFor(t *testing.T) {
	_, store := fresh(t, 408)

	lines, err := store.Declare(Spec{
		Name: "lines",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Rollups: []Rollup{
			{
				Name:  "per_account",
				Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
				Count: true, Sum: []string{"amount"},
			},
			{
				Name:  "per_kind",
				Group: []Field{{Path: "kind", Type: TypeString, Missing: MissingSkip}},
				Count: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []map[string]any{
		{"account": "cash", "kind": "sale", "amount": 1.0},
		{"account": "zebra", "kind": "refund", "amount": 2.0},
	} {
		if _, err := lines.Put(line); err != nil {
			t.Fatal(err)
		}
	}

	// One named group. The account that sorts after it is a row the read walks
	// straight into unless the upper bound stops it.
	rows := allTotals(t, lines, "per_account", Range{
		From: &Bound{Values: []any{"cash"}}, To: &Bound{Values: []any{"cash"}},
	})
	if len(rows) != 1 || rows[0].Count != 1 || rows[0].Sum["amount"] != 1 {
		t.Errorf("one named group came back as %+v", rows)
	}

	// Every group of this rollup and none of the next one's, which sit
	// immediately after them and decode just as well.
	rows = allTotals(t, lines, "per_account", Range{})
	if len(rows) != 2 {
		t.Errorf("all the accounts came back as %+v", rows)
	}
}

// TestTotalsMergedFromPartitionsComeOutInGroupOrder: each partition is walked
// in its own order, so a merge that just put one list after another would hand
// back groups in whatever order the partitions happened to be opened.
func TestTotalsMergedFromPartitionsComeOutInGroupOrder(t *testing.T) {
	_, _, store := partitioned(t, 409)

	lines, err := store.Declare(Spec{
		Name:      "lines",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true, Sum: []string{"amount"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// The earlier partition holds the account that sorts last, so walking the
	// partitions in order gives the groups in the wrong one.
	atMonth(t, store, time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC))
	if _, err := lines.Put(map[string]any{"account": "zebra", "amount": 1.0}); err != nil {
		t.Fatal(err)
	}
	atMonth(t, store, time.Date(2026, time.February, 10, 0, 0, 0, 0, time.UTC))
	if _, err := lines.Put(map[string]any{"account": "apple", "amount": 2.0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	rows := allTotals(t, lines, "per_account", Range{})
	if len(rows) != 2 {
		t.Fatalf("two accounts in two months came back as %+v", rows)
	}
	if rows[0].Group[0] != "apple" || rows[1].Group[0] != "zebra" {
		t.Errorf("the merged rows came out as %v then %v", rows[0].Group, rows[1].Group)
	}
}

// TestARollupDeclaredLateIsFilledFromThisCollectionOnlyAndOnlyOnce: filling a
// new rollup walks documents, and both ways of walking too many of them end
// with a total that is simply wrong from the moment it is created.
func TestARollupDeclaredLateIsFilledFromThisCollectionOnlyAndOnlyOnce(t *testing.T) {
	_, store := fresh(t, 410)
	lines := takings(t, store)

	notes, err := store.Declare(Spec{Name: "notes", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := notes.Put(map[string]any{"account": "cash", "amount": 5.0}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := lines.Put(map[string]any{"account": "cash", "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	both, err := store.Declare(Spec{
		Name: "lines",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Rollups: []Rollup{
			{
				Name:  "per_account",
				Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
				Count: true, Sum: []string{"amount"},
			},
			{Name: "everything", Count: true, Sum: []string{"amount"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The rollup that was already being kept is not run over the documents a
	// second time: it was right before the new one was asked for.
	if count, sum, _ := totalsOf(t, both, "cash"); count != 2 || sum != 2 {
		t.Errorf("declaring a second rollup moved the first: %d lines and %v, want 2 and 2", count, sum)
	}

	// And the new one counts this collection, not every document in the file.
	rows := allTotals(t, both, "everything", Range{})
	if len(rows) != 1 || rows[0].Count != 2 || rows[0].Sum["amount"] != 2 {
		t.Errorf("the new rollup came back as %+v, want one row of 2 and 2", rows)
	}
}

// TestARollupThatChangesShapeIsNotTheSameRollup: keeping the name and changing
// what it holds would leave every reader of the old one reading a number that
// answers a different question.
func TestARollupThatChangesShapeIsNotTheSameRollup(t *testing.T) {
	_, store := fresh(t, 411)
	takings(t, store)

	for what, changed := range map[string]Rollup{
		"sums something else": {
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true, Sum: []string{"fee"},
		},
		"groups by something else": {
			Name:  "per_account",
			Group: []Field{{Path: "kind", Type: TypeString, Missing: MissingSkip}},
			Count: true, Sum: []string{"amount"},
		},
		"stops counting": {
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Sum:   []string{"amount"},
		},
		"treats a missing account differently": {
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingFirst}},
			Count: true, Sum: []string{"amount"},
		},
	} {
		if _, err := store.Declare(Spec{
			Name: "lines", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"},
			Rollups: []Rollup{changed},
		}); !errors.Is(err, ErrIncompatible) {
			t.Errorf("a rollup that %s was accepted: %v", what, err)
		}
	}
}

// TestARollupReadSaysHowManyRowsItMayReturn: a rollup has a row per group and
// nobody knows how many groups there will be, so the read says its own bound
// and says when it hit it.
func TestARollupReadSaysHowManyRowsItMayReturn(t *testing.T) {
	_, store := fresh(t, 412)
	lines := takings(t, store)

	for _, account := range []string{"bank", "cash", "petty"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DeclareOperation(Operation{
		Name: "lines.all", Collection: "lines", Action: ActionTotals, Rollup: "per_account",
	}); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a rollup read with no limit: %v", err)
	}

	if _, err := store.DeclareOperation(Operation{
		Name: "lines.first_two", Collection: "lines", Action: ActionTotals, Rollup: "per_account", Limit: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	answer, err := store.Invoke(Caller{}, "lines.first_two", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Count != 2 || len(answer.Rows) != 2 {
		t.Errorf("a read limited to two rows returned %d", answer.Count)
	}
	if !answer.Truncated {
		t.Error("the read stopped short and did not say so")
	}
}

// TestATotalReadStopsWhenTheCallerDoes, on one tree and on several. A read
// that keeps going after the answer has been refused is work nobody asked for,
// and on a rollup it is work proportional to how many groups exist.
func TestATotalReadStopsWhenTheCallerDoes(t *testing.T) {
	_, store := fresh(t, 413)
	lines := takings(t, store)

	for _, account := range []string{"bank", "cash", "petty"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}

	seen := 0
	if err := lines.Totals("per_account", Range{}, func(Totals) bool {
		seen++
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("a read refused after one row went on to %d", seen)
	}

	// And again where the rows had to be merged out of several partitions,
	// which is a different loop handing them over.
	_, _, split := partitioned(t, 414)
	months, err := split.Declare(Spec{
		Name:      "lines",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true, Sum: []string{"amount"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := split.Commit(); err != nil {
		t.Fatal(err)
	}
	for i, month := range []time.Month{time.January, time.February} {
		atMonth(t, split, time.Date(2026, month, 10, 0, 0, 0, 0, time.UTC))
		for _, account := range []string{"bank", "cash", "petty"} {
			if _, err := months.Put(map[string]any{"account": account, "amount": float64(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := split.Commit(); err != nil {
		t.Fatal(err)
	}

	seen = 0
	if err := months.Totals("per_account", Range{}, func(Totals) bool {
		seen++
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("a merged read refused after one row went on to %d", seen)
	}
}

// TestTheFirstRowOfATotalReadDoesNotCostTheWholeRange: the check in ops.go
// refuses a partitioned rollup read over a range because holding every group
// in it is memory bounded by the data rather than by the operation. The same
// rule has to hold for the ordinary unpartitioned read, or the rule does not
// apply to its own implementation — so a read that wants one row must not pay
// for the thousand behind it.
//
// Measured rather than asserted about, because "streams" is not something a
// return value can say. Two reads of the same data, one that stops at the
// first row and one that takes all of them, and what is compared is how much
// each had to allocate.
func TestTheFirstRowOfATotalReadDoesNotCostTheWholeRange(t *testing.T) {
	_, store := fresh(t, 415)
	lines := takings(t, store)

	for i := 0; i < 600; i++ {
		if _, err := lines.Put(map[string]any{"account": fmt.Sprintf("a%04d", i), "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	cost := func(stopEarly bool) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if err := lines.Totals("per_account", Range{}, func(Totals) bool {
			return !stopEarly
		}); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	everything := cost(false)
	justOne := cost(true)
	if justOne*4 > everything {
		t.Errorf("one row of six hundred cost %d bytes where all of them cost %d", justOne, everything)
	}
}

// --- task 0043: Totals used to compute its own two bounds a second time
// instead of going through stretch(), and no test read a rollup over a
// range with both ends different — so a mutation that swapped the two ends
// of that second copy survived. What follows closes that: a range wide
// enough that swapping, or dropping either end, each give a different wrong
// answer; a structural check that the second copy is gone from the source,
// not just quiet today; and the refusal of Range.Direction on a rollup read
// that round 4 of 0012 already gave the same reason for on a scan.

// groupsOf is the account name of every row Totals returned, in the order it
// returned them — the shape every test below is really comparing.
func groupsOf(t *testing.T, rows []Totals) []string {
	t.Helper()
	names := make([]string, len(rows))
	for i, row := range rows {
		name, ok := row.Group[0].(string)
		if !ok {
			t.Fatalf("row %d's group is %v, not a string", i, row.Group)
		}
		names[i] = name
	}
	return names
}

// TestATotalReadOverARangeWithBothEndsNamedKeepsExactlyWhatIsBetweenThem:
// five groups, read from the second to the fourth. This is the shape no
// earlier test used — every other totals test either read Range{} (no
// bound at all) or pinned the same value at both ends, so a bound that
// swapped From and To, or dropped either one, changed nothing any of them
// could see.
//
// Three different ways to get this wrong give three different wrong
// answers, which is exactly what makes this range worth more than the
// pinned one: swapping the ends leaves nothing between them (empty);
// dropping the upper end walks on to "e"; dropping the lower end starts
// from "a".
func TestATotalReadOverARangeWithBothEndsNamedKeepsExactlyWhatIsBetweenThem(t *testing.T) {
	_, store := fresh(t, 416)
	lines := takings(t, store)

	for _, account := range []string{"a", "b", "c", "d", "e"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}

	rows := allTotals(t, lines, "per_account", Range{
		From: &Bound{Values: []any{"b"}}, To: &Bound{Values: []any{"d"}},
	})
	if got, want := groupsOf(t, rows), []string{"b", "c", "d"}; !slices.Equal(got, want) {
		t.Errorf("b..d came back as %v, want %v", got, want)
	}

	// Self-check, per house-rules' "must NỚI, not just thu hẹp": the same
	// range read one group wider on each side must pick up exactly the one
	// extra neighbor and nothing else — proof the table closes past its own
	// edge rather than only at the point it happened to name.
	rows = allTotals(t, lines, "per_account", Range{
		From: &Bound{Values: []any{"a"}}, To: &Bound{Values: []any{"e"}},
	})
	if got, want := groupsOf(t, rows), []string{"a", "b", "c", "d", "e"}; !slices.Equal(got, want) {
		t.Errorf("a..e came back as %v, want %v", got, want)
	}
}

// TestATotalReadOverARangeWithBothEndsNamedKeepsExactlyWhatIsBetweenThemAcrossPartitions
// is the same shape and the same three-way failure, on a rollup that has to
// merge several partitions before it can answer — the other place the task
// named as untested: totalsAcross's merge could drop a bound while adding
// partitions together without any single-partition test noticing.
func TestATotalReadOverARangeWithBothEndsNamedKeepsExactlyWhatIsBetweenThemAcrossPartitions(t *testing.T) {
	_, _, store := partitioned(t, 417)

	lines, err := store.Declare(Spec{
		Name:      "lines",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true, Sum: []string{"amount"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Split across two months so the read has to merge, and split so that
	// the accounts in the middle of the range (b, d) are not all in the same
	// partition as either end.
	atMonth(t, store, time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC))
	for _, account := range []string{"a", "c", "e"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}
	atMonth(t, store, time.Date(2026, time.February, 10, 0, 0, 0, 0, time.UTC))
	for _, account := range []string{"b", "d"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	rows := allTotals(t, lines, "per_account", Range{
		From: &Bound{Values: []any{"b"}}, To: &Bound{Values: []any{"d"}},
	})
	if got, want := groupsOf(t, rows), []string{"b", "c", "d"}; !slices.Equal(got, want) {
		t.Errorf("b..d merged across two partitions came back as %v, want %v", got, want)
	}
}

// TestCollectionTotalsHasNoBoundOfItsOwn is the structural half of the fix:
// grep, not behavior. Two computations of the same two bounds can agree on
// every byte today and still be two computations — which is exactly the
// shape this task exists to remove, and exactly the shape no row-level test
// can tell apart from one computation, because a correct duplicate and a
// merged call read back identically until the day someone edits only one of
// them. This reads rollup.go's own source and requires that the call this
// task deleted not be there.
func TestCollectionTotalsHasNoBoundOfItsOwn(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find this file's own path")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "rollup.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "c.bound(") {
		t.Error("rollup.go calls c.bound( directly — Totals must reach the two bounds through stretch(), " +
			"the way Scan and walkRange do, not compute them a second time")
	}
}

// TestTotalsRowsMatchATreeWalkBoundedByStretchesOwnBytes ties Totals's
// answer to stretch()'s own output at the byte level, the way
// TestTheTwoBoundsNeverDependOnDirection (direction_v4_test.go) ties a scan
// to it: call stretch() directly for the same Range, walk the raw tree
// between the bytes it returns, and require the rows Totals() handed back
// name exactly the same groups. This is not a test that a duplicate
// bound() calculation would necessarily fail — the deleted one computed the
// identical bytes stretch() does, which is what made it a silent duplicate
// rather than a bug someone would have noticed. What it is a test of: if
// stretch() is changed by a future task without Totals being carried along,
// because Totals no longer has a calculation of its own to fall out of step,
// this comparison is the thing that would go red the moment stretch()'s
// bytes and Totals's rows stop agreeing.
func TestTotalsRowsMatchATreeWalkBoundedByStretchesOwnBytes(t *testing.T) {
	_, store := fresh(t, 418)
	lines := takings(t, store)

	for _, account := range []string{"a", "b", "c", "d", "e"} {
		if _, err := lines.Put(map[string]any{"account": account, "amount": 1.0}); err != nil {
			t.Fatal(err)
		}
	}

	rollup, found := lines.rollup("per_account")
	if !found {
		t.Fatal("per_account is not declared")
	}
	prefix := lines.rollups(*rollup)
	fields := encodings(rollup.Group)
	within := Range{From: &Bound{Values: []any{"b"}}, To: &Bound{Values: []any{"d"}}}

	lower, upper, err := lines.stretch(within, prefix, fields)
	if err != nil {
		t.Fatal(err)
	}
	trees, err := lines.across()
	if err != nil {
		t.Fatal(err)
	}

	var reference []Totals
	if err := lines.totalsIn(trees[0], prefix, fields, lower, upper, func(row Totals) bool {
		reference = append(reference, row)
		return true
	}); err != nil {
		t.Fatal(err)
	}

	rows := allTotals(t, lines, "per_account", within)
	if got, want := groupsOf(t, rows), groupsOf(t, reference); !slices.Equal(got, want) {
		t.Errorf("Totals() gave %v, a walk bounded by stretch()'s own bytes gave %v", got, want)
	}
	// The oracle itself has to be the narrowed set, or agreement is
	// vacuous — both sides empty would "match" and prove nothing.
	if want := []string{"b", "c", "d"}; !slices.Equal(groupsOf(t, reference), want) {
		t.Fatalf("the reference walk is %v, want %v — the oracle is broken, not the code under test", groupsOf(t, reference), want)
	}
}

// TestATotalsReadRefusesAnythingButForward: a rollup read has no order to
// reverse, so within.Direction must be Forward, and Totals says so rather
// than reading rows in an order nobody asked for and nobody would notice —
// task 0012 round 4 gave the identical reason for refusing Direction on a
// declared totals operation ("a word nobody reads is how a caller ends up
// believing a promise nobody made"); this is the same refusal for the one
// caller that round never reached, a Range built in Go and handed to
// Collection.Totals directly.
func TestATotalsReadRefusesAnythingButForward(t *testing.T) {
	_, store := fresh(t, 419)
	lines := takings(t, store)
	if _, err := lines.Put(map[string]any{"account": "cash", "amount": 1.0}); err != nil {
		t.Fatal(err)
	}

	if err := lines.Totals("per_account", Range{Direction: Reverse}, func(Totals) bool { return true }); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a totals read with Direction: Reverse: want ErrDeclaration, got %v", err)
	}

	// Explicit Forward, and no Direction at all, both read normally — this
	// is not a refusal of the field, only of the one value that has nowhere
	// to go, so "just refuse everything" would pass the case above and be
	// caught here instead.
	if rows := allTotals(t, lines, "per_account", Range{Direction: Forward}); len(rows) != 1 {
		t.Errorf("Direction: Forward was refused or changed the answer: %+v", rows)
	}
	if rows := allTotals(t, lines, "per_account", Range{}); len(rows) != 1 {
		t.Errorf("no Direction at all was refused or changed the answer: %+v", rows)
	}
}
