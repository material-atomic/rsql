package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/ulid"
)

// atMonth makes a store whose generated keys land in a month of your choosing,
// so that a test can write "January" and "February" documents without waiting.
func atMonth(t *testing.T, store *Store, when time.Time) {
	t.Helper()
	at := when
	store.Identifiers(ulid.With(func() time.Time { at = at.Add(time.Millisecond); return at }, endless{}))
}

// entries is a collection divided by time: a partition per month, a ulid key.
func entriesByMonth(t *testing.T, store *Store, keep int) *Collection {
	t.Helper()

	made, err := store.Declare(Spec{
		Name:      "entries",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth, Keep: keep},
		Indexes: []Index{{
			Name:   "by_account",
			Fields: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
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

// TestADocumentGoesInThePartitionItsKeySays: the key decides, and nothing
// else, so every read and write already knows which file to open.
func TestADocumentGoesInThePartitionItsKeySays(t *testing.T) {
	_, _, store := partitioned(t, 300)
	entries := entriesByMonth(t, store, 0)

	atMonth(t, store, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	january, err := entries.Put(map[string]any{"account": "cash", "amount": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	atMonth(t, store, time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
	february, err := entries.Put(map[string]any{"account": "cash", "amount": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if names := store.Parts(); len(names) != 2 || names[0] != "entries-2026-01" || names[1] != "entries-2026-02" {
		t.Fatalf("the months are %v", names)
	}

	// Read back by key: no search, because the key says where it is.
	for _, one := range []struct {
		key    any
		amount float64
	}{{january, 1.0}, {february, 2.0}} {
		document, found, err := entries.Get(one.key)
		if err != nil || !found {
			t.Fatalf("%v: %v, %v", one.key, found, err)
		}
		if document["amount"] != one.amount {
			t.Errorf("%v came back as %v", one.key, document["amount"])
		}
	}

	// And deleting reaches into the right one.
	if removed, err := entries.Delete(january); err != nil || !removed {
		t.Fatalf("deleting the january entry: %v, %v", removed, err)
	}
	if _, found, err := entries.Get(january); err != nil || found {
		t.Errorf("the january entry is still there: %v, %v", found, err)
	}
	if _, found, err := entries.Get(february); err != nil || !found {
		t.Errorf("deleting january took february with it: %v, %v", found, err)
	}
}

// TestAScanCrossesThePartitionsInOrder: partition names sort in the order the
// periods happened and the key is a ulid, so walking them in turn is walking
// the collection in key order.
func TestAScanCrossesThePartitionsInOrder(t *testing.T) {
	_, _, store := partitioned(t, 301)
	entries := entriesByMonth(t, store, 0)

	wanted := []any{}
	for _, month := range []time.Month{time.January, time.February, time.March} {
		atMonth(t, store, time.Date(2026, month, 10, 0, 0, 0, 0, time.UTC))
		for i := 0; i < 3; i++ {
			key, err := entries.Put(map[string]any{"account": "cash", "amount": float64(i)})
			if err != nil {
				t.Fatal(err)
			}
			wanted = append(wanted, key)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Along the clustered index — the key itself.
	seen := []any{}
	if err := entries.Walk(func(key any, _ map[string]any) bool {
		seen = append(seen, key)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != fmt.Sprint(wanted) {
		t.Errorf("walking gave\n  %v\nwant\n  %v", seen, wanted)
	}

	// And along a declared index, with its one field fixed.
	found := []any{}
	if err := entries.Scan("by_account", Range{
		From: &Bound{Values: []any{"cash"}}, To: &Bound{Values: []any{"cash"}},
	}, func(entry Found) bool {
		found = append(found, entry.Key)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(found) != fmt.Sprint(wanted) {
		t.Errorf("scanning gave\n  %v\nwant\n  %v", found, wanted)
	}
}

// TestAScanStopsWhenTheCallerHasHadEnough: the caller is done with the scan,
// not with the partition it happened to be in. A scan that carried on into the
// next file would return more rows than the limit that was declared.
func TestAScanStopsWhenTheCallerHasHadEnough(t *testing.T) {
	_, _, store := partitioned(t, 302)
	entries := entriesByMonth(t, store, 0)

	// Three months, so that stopping in the first leaves two more to be
	// wrongly walked into.
	for _, month := range []time.Month{time.January, time.February, time.March} {
		atMonth(t, store, time.Date(2026, month, 10, 0, 0, 0, 0, time.UTC))
		for i := 0; i < 3; i++ {
			if _, err := entries.Put(map[string]any{"account": "cash"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	count := 0
	if err := entries.Walk(func(any, map[string]any) bool {
		count++
		return count < 2
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("the walk stopped after %d, want 2", count)
	}

	count = 0
	if err := entries.Scan("by_account", Range{
		From: &Bound{Values: []any{"cash"}}, To: &Bound{Values: []any{"cash"}},
	}, func(Found) bool {
		count++
		return count < 2
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("the scan stopped after %d, want 2", count)
	}
}

// TestOldPartitionsFallOffAsNewOnesStart is what partitions are for. And it
// happens when data arrives rather than on a clock, so a database nobody has
// written to for a year does not lose the year the moment somebody opens it.
func TestOldPartitionsFallOffAsNewOnesStart(t *testing.T) {
	_, shelf, store := partitioned(t, 303)
	entries := entriesByMonth(t, store, 2)

	for _, month := range []time.Month{time.January, time.February, time.March} {
		atMonth(t, store, time.Date(2026, month, 10, 0, 0, 0, 0, time.UTC))
		if _, err := entries.Put(map[string]any{"account": "cash"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if names := store.Parts(); len(names) != 2 || names[0] != "entries-2026-02" {
		t.Fatalf("keeping two months left %v", names)
	}
	if _, here := shelf.disks["entries-2026-01.part"]; here {
		t.Error("the month that fell off still has its file")
	}

	// Opening and reading changes nothing: what ages a collection is new data.
	if err := entries.Walk(func(any, map[string]any) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	if names := store.Parts(); len(names) != 2 {
		t.Errorf("reading it dropped a month: %v", names)
	}
}

// TestWhatMayBeDeclaredOnAPartitionedCollection: everything about how a scan
// can be written follows from indexes being local to their partition, and all
// of it is decided where the operation is declared rather than where it runs.
func TestWhatMayBeDeclaredOnAPartitionedCollection(t *testing.T) {
	_, _, store := partitioned(t, 304)

	// A time partition needs a ulid key, because that is what carries the time.
	if _, err := store.Declare(Spec{
		Name: "bad", Key: Key{Path: "id", Type: TypeString},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
	}); !errors.Is(err, ErrDeclaration) {
		t.Errorf("a time partition with a key the writer chooses: %v", err)
	}
	for _, wrong := range []*Partition{
		{By: ByTime, Every: "fortnight"},
		// Saying nothing is not a period. Left in, every key would land in one
		// partition with an empty name — a divided collection that is not
		// divided, which nothing downstream would complain about.
		{By: ByTime},
		{By: ByTime, Every: EveryMonth, Into: 4},
		{By: ByHash, Into: 1},
		{By: ByHash, Into: 4, Keep: 2},
		{By: "size", Into: 4},
	} {
		if _, err := store.Declare(Spec{
			Name: "bad", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}, Partition: wrong,
		}); !errors.Is(err, ErrDeclaration) {
			t.Errorf("%+v was accepted: %v", wrong, err)
		}
	}

	entries := entriesByMonth(t, store, 0)
	_ = entries

	// A scan along the key is always fine: partition order is key order.
	if _, err := store.DeclareOperation(Operation{
		Name: "entries.recent", Collection: "entries", Action: ActionScan, Limit: 10,
	}); err != nil {
		t.Errorf("a scan along the key of a time-partitioned collection: %v", err)
	}

	// A scan on a declared index must fix its fields and let only the key vary.
	if _, err := store.DeclareOperation(Operation{
		Name: "entries.of_account", Collection: "entries", Action: ActionScan, Index: "by_account", Limit: 10,
		Input: []Parameter{{Name: "account", Type: TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "account"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "account"}}},
	}); err != nil {
		t.Errorf("a scan that fixes its one field: %v", err)
	}

	// Ranging over the field instead would return every account of January
	// before every account of February, which is not an order anybody asked
	// for. Refused where it is written.
	for _, loose := range []Operation{
		{Name: "entries.all", Collection: "entries", Action: ActionScan, Index: "by_account", Limit: 10},
		{
			Name: "entries.from", Collection: "entries", Action: ActionScan, Index: "by_account", Limit: 10,
			Input: []Parameter{{Name: "a", Type: TypeString, Required: true}, {Name: "b", Type: TypeString, Required: true}},
			From:  &Endpoint{Terms: []Term{{Arg: "a"}}},
			To:    &Endpoint{Terms: []Term{{Arg: "b"}}},
		},
	} {
		if _, err := store.DeclareOperation(loose); !errors.Is(err, ErrDeclaration) {
			t.Errorf("%q was accepted: %v", loose.Name, err)
		}
	}
}

// TestAHashPartitionedCollectionIsReadByKey: hash order is no order, so
// nothing that crosses those partitions can come back ordered — and rather
// than return rows in an order nobody can rely on, scanning one is refused.
func TestAHashPartitionedCollectionIsReadByKey(t *testing.T) {
	_, _, store := partitioned(t, 305)

	if _, err := store.Declare(Spec{
		Name:      "sessions",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByHash, Into: 4},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DeclareOperation(Operation{
		Name: "sessions.get", Collection: "sessions", Action: ActionGet,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	}); err != nil {
		t.Errorf("reading a hash-partitioned collection by key: %v", err)
	}
	if _, err := store.DeclareOperation(Operation{
		Name: "sessions.all", Collection: "sessions", Action: ActionScan, Limit: 10,
	}); !errors.Is(err, ErrDeclaration) {
		t.Errorf("scanning a hash-partitioned collection: %v", err)
	}

	// The documents really are spread out.
	sessions, err := store.Collection("sessions")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		if _, err := sessions.Put(map[string]any{"n": float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	if names := store.Parts(); len(names) < 2 {
		t.Errorf("24 documents over four partitions landed in %v", names)
	}
}

// TestHowACollectionIsDividedIsNotRedeclared: it decides which file every
// document is in, so changing it means moving all of them — a migration
// somebody runs, not something a redeclaration does quietly.
func TestHowACollectionIsDividedIsNotRedeclared(t *testing.T) {
	_, _, store := partitioned(t, 306)
	entriesByMonth(t, store, 0)

	for _, changed := range []*Partition{
		nil,
		{By: ByTime, Every: EveryDay},
		{By: ByHash, Into: 4},
	} {
		if _, err := store.Declare(Spec{
			Name:      "entries",
			Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
			Partition: changed,
		}); !errors.Is(err, ErrIncompatible) {
			t.Errorf("redeclaring it %+v: %v", changed, err)
		}
	}
}
