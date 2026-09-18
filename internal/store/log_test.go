package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/vfs"
)

func changes(t *testing.T, store *Store, from uint64) []Change {
	t.Helper()
	var all []Change
	if err := store.Changes(from, func(change Change) bool {
		all = append(all, change)
		return true
	}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	return all
}

func kinds(all []Change) []string {
	out := make([]string, len(all))
	for i, change := range all {
		out[i] = change.Kind
	}
	return out
}

func TestEveryChangeIsInTheLogInOrder(t *testing.T) {
	_, store := fresh(t, 40)
	store.Clock(func() time.Time { return time.UnixMilli(1700000000000) })

	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	put(t, collection, map[string]any{"id": "a", "title": "One"})
	put(t, collection, map[string]any{"id": "b", "title": "Two"})
	if _, err := collection.Delete("a"); err != nil {
		t.Fatal(err)
	}

	all := changes(t, store, 0)
	if got := fmt.Sprint(kinds(all)); got != "[declare put put delete]" {
		t.Fatalf("the log reads %v", got)
	}

	for i, change := range all {
		if change.LSN != uint64(i+1) {
			t.Errorf("entry %d is numbered %d", i, change.LSN)
		}
		if change.At != 1700000000000 {
			t.Errorf("entry %d is stamped %d", i, change.At)
		}
	}
	if all[1].Key != "a" || all[1].Document["title"] != "One" {
		t.Errorf("the first put reads %+v", all[1])
	}
	if all[3].Key != "a" || all[3].Document != nil {
		t.Errorf("the delete reads %+v", all[3])
	}

	latest, err := store.LatestLSN()
	if err != nil || latest != 4 {
		t.Errorf("the latest entry is %d (%v)", latest, err)
	}

	// Reading from part way along starts there.
	if got := changes(t, store, 3); len(got) != 2 || got[0].LSN != 3 {
		t.Errorf("reading from 3 gave %v", kinds(got))
	}

	// A delete of nothing is not a change, so it is not an entry.
	if _, err := collection.Delete("nothing"); err != nil {
		t.Fatal(err)
	}
	if got := changes(t, store, 0); len(got) != 4 {
		t.Errorf("deleting nothing wrote an entry: %v", kinds(got))
	}
}

func TestTheLogSaysWhoDidItAndWhy(t *testing.T) {
	store, collection := declared(t, 41)
	fill(t, collection)
	declareOp(t, store, Operation{
		Name: "articles.write", Collection: "articles", Action: ActionPut,
		Input:    []Parameter{{Name: "id", Type: TypeString, Required: true}, {Name: "title", Type: TypeString, Required: true}},
		Document: map[string]Term{"id": {Arg: "id"}, "title": {Arg: "title"}},
	})

	before, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Invoke(
		Caller{Actor: "ann@example", WriteID: "w-1"},
		"articles.write", 0, map[string]any{"id": "z", "title": "Written"},
	); err != nil {
		t.Fatal(err)
	}

	all := changes(t, store, before+1)
	if len(all) != 1 {
		t.Fatalf("the call wrote %d entries", len(all))
	}
	if all[0].By.Operation != "articles.write" || all[0].By.Version != 1 {
		t.Errorf("the entry names %+v", all[0].By)
	}
	if all[0].By.Actor != "ann@example" || all[0].By.WriteID != "w-1" {
		t.Errorf("the entry says %+v", all[0].By)
	}
}

func TestAWriteThatArrivesTwiceIsAppliedOnce(t *testing.T) {
	store, _ := declared(t, 42)
	declareOp(t, store, Operation{
		Name: "articles.add", Collection: "articles", Action: ActionInsert,
		Input:    []Parameter{{Name: "title", Type: TypeString, Required: true}},
		Document: map[string]Term{"title": {Arg: "title"}},
	})

	caller := Caller{WriteID: "retry-me"}
	first, err := store.Invoke(caller, "articles.add", 0, map[string]any{"title": "Once"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Repeated != 0 {
		t.Errorf("the first call reported a repeat at %d", first.Repeated)
	}

	// The connection dropped and the caller tried again with the same id.
	second, err := store.Invoke(caller, "articles.add", 0, map[string]any{"title": "Once"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Key != first.Key {
		t.Errorf("the retry made a second document: %v then %v", first.Key, second.Key)
	}
	if second.Repeated == 0 {
		t.Error("the retry did not say it was one")
	}

	count := 0
	collection, _ := store.Collection("articles")
	if err := collection.Walk(func(any, map[string]any) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("there are %d documents, want 1", count)
	}

	// A different id is a different write.
	if _, err := store.Invoke(Caller{WriteID: "another"}, "articles.add", 0, map[string]any{"title": "Twice"}); err != nil {
		t.Fatal(err)
	}
	count = 0
	if err := collection.Walk(func(any, map[string]any) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("there are %d documents, want 2", count)
	}
}

// The test the whole log exists for: another database fed nothing but the
// entries ends up holding exactly the same thing.
func TestAReplicaFedTheLogEndsUpTheSame(t *testing.T) {
	_, primary := fresh(t, 43)
	collection, err := primary.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Declare(Spec{Name: "notes", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}}); err != nil {
		t.Fatal(err)
	}
	notes, _ := primary.Collection("notes")

	for i := 0; i < 120; i++ {
		key := fmt.Sprintf("k%03d", i%40)
		switch i % 4 {
		case 0, 1:
			put(t, collection, map[string]any{
				"id": key, "author": []string{"ann", "bob"}[i%2],
				"published": float64(i), "title": fmt.Sprintf("t%d", i),
				"slug": fmt.Sprintf("s%03d", i), "tags": []any{"x", fmt.Sprintf("t%d", i%3)},
			})
		case 2:
			if _, err := collection.Delete(key); err != nil {
				t.Fatal(err)
			}
		case 3:
			put(t, notes, map[string]any{"body": fmt.Sprintf("note %d", i)})
		}
	}

	// An index added part way through, so the replica has to follow the
	// declaration as well as the documents.
	extra := articles()
	extra.Indexes = append(extra.Indexes, Index{
		Name:   "by_title",
		Fields: []Field{{Path: "title", Type: TypeString, Missing: MissingSkip}},
	})
	if _, err := primary.Declare(extra); err != nil {
		t.Fatal(err)
	}
	put(t, collection, map[string]any{"id": "last", "title": "After the index"})
	if err := primary.Drop("notes"); err != nil {
		t.Fatal(err)
	}

	_, replica := fresh(t, 44)
	for _, change := range changes(t, primary, 0) {
		if err := replica.Apply(change); err != nil {
			t.Fatalf("apply %d (%s): %v", change.LSN, change.Kind, err)
		}
	}

	sameStores(t, primary, replica)

	// Applying the whole log again changes nothing: a replica that loses its
	// place and rewinds is an ordinary event.
	for _, change := range changes(t, primary, 0) {
		if err := replica.Apply(change); err != nil {
			t.Fatalf("replaying %d: %v", change.LSN, err)
		}
	}
	sameStores(t, primary, replica)

	// An entry out of order is refused rather than left as a hole.
	if err := replica.Apply(Change{LSN: 9999, Kind: ChangePut, Collection: "articles"}); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("want ErrOutOfOrder, got %v", err)
	}
}

// sameStores compares two databases through everything they can be asked:
// their collections, their documents, and every index entry.
func sameStores(t *testing.T, first, second *Store) {
	t.Helper()

	names := first.Collections()
	if len(names) != len(second.Collections()) {
		t.Fatalf("one holds %v, the other %v", names, second.Collections())
	}

	for _, name := range names {
		here, err := first.Collection(name)
		if err != nil {
			t.Fatal(err)
		}
		there, err := second.Collection(name)
		if err != nil {
			t.Fatalf("the replica has no %q", name)
		}
		if fmt.Sprint(here.Spec()) != fmt.Sprint(there.Spec()) {
			t.Fatalf("%q is declared differently:\n %v\n %v", name, here.Spec(), there.Spec())
		}

		documents := map[string]string{}
		if err := here.Walk(func(key any, document map[string]any) bool {
			documents[fmt.Sprint(key)] = fmt.Sprint(document)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		mirrored := map[string]string{}
		if err := there.Walk(func(key any, document map[string]any) bool {
			mirrored[fmt.Sprint(key)] = fmt.Sprint(document)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if len(documents) != len(mirrored) {
			t.Fatalf("%q holds %d documents here and %d there", name, len(documents), len(mirrored))
		}
		for key, document := range documents {
			if mirrored[key] != document {
				t.Fatalf("%q %s reads as %s here and %s there", name, key, document, mirrored[key])
			}
		}

		for _, index := range here.Spec().Indexes {
			ours := fmt.Sprint(scan(t, here, index.Name, Range{}))
			theirs := fmt.Sprint(scan(t, there, index.Name, Range{}))
			if ours != theirs {
				t.Fatalf("index %q of %q differs:\n %s\n %s", index.Name, name, ours, theirs)
			}
		}
	}
}

func TestOperationsTravelDownTheLogToo(t *testing.T) {
	store, collection := declared(t, 45)
	fill(t, collection)
	declareOp(t, store, byAuthor())
	changed := byAuthor()
	changed.Limit = 1
	declareOp(t, store, changed)

	_, replica := fresh(t, 46)
	for _, change := range changes(t, store, 0) {
		if err := replica.Apply(change); err != nil {
			t.Fatalf("apply %d: %v", change.LSN, err)
		}
	}

	// Both versions arrived, and the newest is the one that runs.
	result, err := replica.Invoke(Caller{}, "articles.by_author", 0, map[string]any{"author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != 2 || result.Count != 1 {
		t.Errorf("the replica ran version %d and returned %d rows", result.Version, result.Count)
	}
	if _, err := replica.Invoke(Caller{}, "articles.by_author", 1, map[string]any{"author": "ann"}); err != nil {
		t.Errorf("the older version did not travel: %v", err)
	}
}

func TestTheLogIsTrimmedToWhatWasAskedFor(t *testing.T) {
	_, store := fresh(t, 47)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	store.Retain(20)
	for i := 0; i < 100; i++ {
		if _, err := collection.PutBy(
			Attribution{WriteID: fmt.Sprintf("w%03d", i)},
			map[string]any{"id": fmt.Sprintf("k%03d", i), "title": "x"},
		); err != nil {
			t.Fatal(err)
		}
	}

	all := changes(t, store, 0)
	if len(all) > 21 {
		t.Fatalf("the log holds %d entries with a cap of 20", len(all))
	}
	latest, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if all[len(all)-1].LSN != latest {
		t.Errorf("the newest entry is %d, the counter says %d", all[len(all)-1].LSN, latest)
	}

	oldest, err := store.OldestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if oldest != all[0].LSN {
		t.Errorf("the oldest entry is %d, the log starts at %d", oldest, all[0].LSN)
	}
	// A subscriber further behind than the cap can tell, which is the point of
	// being able to ask.
	if oldest <= 1 {
		t.Errorf("nothing was trimmed: the log still starts at %d", oldest)
	}

	// The write ids of trimmed entries go with them, so a retry from before the
	// window applies again rather than reading a record that is no longer
	// backed by an entry.
	if _, _, found, err := store.Wrote("w000"); err != nil || found {
		t.Errorf("a trimmed write id is still recorded: %v, %v", found, err)
	}
	if _, _, found, err := store.Wrote("w099"); err != nil || !found {
		t.Errorf("the newest write id is missing: %v, %v", found, err)
	}

	// Everything still in the log applies to a replica, which is the promise
	// the cap is measured against.
	_, replica := fresh(t, 48)
	if err := replica.Apply(all[0]); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("a replica starting mid-log: want ErrOutOfOrder, got %v", err)
	}
}

func TestTheLogSurvivesARestart(t *testing.T) {
	disk, store := fresh(t, 49)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	put(t, collection, map[string]any{"id": "a", "title": "One"})
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	before := changes(t, store, 0)

	restart := vfs.NewSim(49, vfs.Faults{})
	restart.Restore(disk.Durable())
	pages, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}

	after := changes(t, reopened, 0)
	if fmt.Sprint(kinds(after)) != fmt.Sprint(kinds(before)) {
		t.Fatalf("the log came back as %v, was %v", kinds(after), kinds(before))
	}

	// And the numbering carries on rather than starting again, which it would
	// if the counter lived only in memory.
	collection, _ = reopened.Collection("articles")
	put(t, collection, map[string]any{"id": "b", "title": "Two"})
	latest, err := reopened.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if latest != uint64(len(before))+1 {
		t.Errorf("after a restart the next entry is %d, the log had %d", latest, len(before))
	}
}
