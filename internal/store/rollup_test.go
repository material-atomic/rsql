package store

import (
	"errors"
	"testing"
	"time"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/vfs"
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
