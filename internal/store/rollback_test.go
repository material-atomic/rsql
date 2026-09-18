package store

import (
	"errors"
	"fmt"
	"sort"
	"testing"
)

// everything is the documents of a collection, by key, in key order.
func everything(t *testing.T, collection *Collection) ([]string, []string) {
	t.Helper()

	documents := map[string]string{}
	if err := collection.Walk(func(key any, document map[string]any) bool {
		documents[fmt.Sprint(key)] = fmt.Sprint(document["title"])
		return true
	}); err != nil {
		t.Fatal(err)
	}

	keys := make([]string, 0, len(documents))
	for key := range documents {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	titles := make([]string, len(keys))
	for i, key := range keys {
		titles[i] = documents[key]
	}
	return keys, titles
}

// Everything a transaction did, gone: the documents, the index entries, the
// log, and the declarations. Copy-on-write makes this cheap, but cheap is not
// the point — the point is that half of it must never survive.
func TestRollbackLeavesNothingOfTheTransaction(t *testing.T) {
	_, store := fresh(t, 70)

	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("kept-%02d", i), "author": "ann",
			"published": float64(i), "slug": fmt.Sprintf("s%02d", i),
		})
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	committed, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}

	// A transaction that does a great deal and is then abandoned.
	for i := 0; i < 50; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("gone-%02d", i), "author": "bob",
			"published": float64(i), "slug": fmt.Sprintf("g%02d", i),
		})
	}
	for i := 0; i < 20; i++ {
		if _, err := collection.Delete(fmt.Sprintf("kept-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Declare(Spec{Name: "notes", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeclareOperation(byAuthor()); err != nil {
		t.Fatal(err)
	}

	if err := store.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The documents that were committed, and only those.
	after, err := store.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := everything(t, after)
	if len(keys) != 50 {
		t.Fatalf("%d documents survived, want the 50 that were committed", len(keys))
	}
	for _, key := range keys {
		if key[:5] != "kept-" {
			t.Fatalf("a document from the abandoned transaction survived: %s", key)
		}
	}

	// The indexes agree with them.
	checkAgreement(t, after, func() map[string]map[string]any {
		model := map[string]map[string]any{}
		if err := after.Walk(func(key any, document map[string]any) bool {
			model[key.(string)] = document
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return model
	}(), -1)

	// The log stopped where the commit did.
	latest, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if latest != committed {
		t.Errorf("the log is at %d, the last commit was %d", latest, committed)
	}

	// The declarations made in the abandoned transaction are not there.
	if _, err := store.Collection("notes"); !errors.Is(err, ErrNoCollection) {
		t.Errorf("a collection declared in the abandoned transaction survived: %v", err)
	}
	if _, found, err := store.Operation("articles.by_author", 0); err == nil && found {
		t.Error("an operation declared in the abandoned transaction survived")
	}
}

// A handle taken before a rollback must not quietly write into a collection
// that has been rebuilt underneath it.
func TestAHandleFromBeforeARollbackIsRefused(t *testing.T) {
	_, store := fresh(t, 71)

	held, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	put(t, held, map[string]any{"id": "a", "title": "One"})
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	put(t, held, map[string]any{"id": "b", "title": "Two"})
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, err := held.Put(map[string]any{"id": "c", "title": "Three"}); !errors.Is(err, ErrNoCollection) {
		t.Errorf("an old handle wrote after a rollback: %v", err)
	}

	// Asking again gives one that works.
	fresh, err := store.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Put(map[string]any{"id": "c", "title": "Three"}); err != nil {
		t.Errorf("a handle taken after the rollback: %v", err)
	}
}

// Rolling back must not lose the space the abandoned transaction used, and
// must not hand out pages the committed state is still using.
func TestRollbackGivesTheSpaceBackWithoutLosingAnything(t *testing.T) {
	_, store := fresh(t, 72)

	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		put(t, collection, map[string]any{"id": fmt.Sprintf("k%03d", i), "title": "committed"})
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	settled := store.pages.Meta().PageCount

	// Abandoned, several times over. Each one must leave the file where it was.
	for round := 0; round < 5; round++ {
		again, err := store.Collection("articles")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 200; i++ {
			put(t, again, map[string]any{"id": fmt.Sprintf("k%03d", i), "title": fmt.Sprintf("round %d", round)})
		}
		if err := store.Rollback(); err != nil {
			t.Fatal(err)
		}

		if grown := store.pages.Meta().PageCount; grown != settled {
			t.Fatalf("round %d: the file is %d pages, was %d before the abandoned work", round, grown, settled)
		}
	}

	// And what was committed still reads, with its indexes intact.
	after, err := store.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	keys, values := everything(t, after)
	if len(keys) != 200 {
		t.Fatalf("%d documents, want 200", len(keys))
	}
	for i, value := range values {
		if value != "committed" {
			t.Fatalf("%s reads as %q", keys[i], value)
		}
	}

	// The next real commit still works, which is what says the freelist and
	// the page count came back to a state that can be written from.
	put(t, after, map[string]any{"id": "after-all-that", "title": "committed"})
	if err := store.Commit(); err != nil {
		t.Fatalf("committing after rollbacks: %v", err)
	}
	if _, found, err := after.Get("after-all-that"); err != nil || !found {
		t.Errorf("the write after the rollbacks: %v, %v", found, err)
	}
}

// Rolling back when there is nothing to roll back is not an error, and does
// not lose what is committed.
func TestRollingBackNothingIsHarmless(t *testing.T) {
	_, store := fresh(t, 73)

	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	put(t, collection, map[string]any{"id": "a", "title": "One"})
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := store.Rollback(); err != nil {
			t.Fatalf("rollback %d: %v", i, err)
		}
	}

	after, err := store.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := after.Get("a"); err != nil || !found {
		t.Errorf("the committed document: %v, %v", found, err)
	}
}

// The page a transaction replaced is marked as rubbish. If the transaction is
// then abandoned, the live tree points at that page again — and anything still
// holding it as rubbish will hand it out to be written over.
//
// Nothing complains when that happens. The commit succeeds, the page is
// written, and the damage is found later by whoever reads through the pointer
// that still leads there. So the free list is read back from the committed
// state on rollback, and this is the test that says it was.
func TestAbandonedWorkDoesNotLeaveTheLiveTreeMarkedAsRubbish(t *testing.T) {
	_, store := fresh(t, 74)

	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	const count = 300
	for i := 0; i < count; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("k%03d", i), "title": "committed",
			"author": "ann", "published": float64(i), "slug": fmt.Sprintf("s%03d", i),
		})
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Rewriting everything replaces every page the tree is made of, so the
	// whole live tree is on the rubbish heap when this is abandoned.
	again, err := store.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		put(t, again, map[string]any{
			"id": fmt.Sprintf("k%03d", i), "title": "abandoned",
			"author": "bob", "published": float64(i), "slug": fmt.Sprintf("a%03d", i),
		})
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Several real transactions, because a page marked as rubbish is not handed
	// out at once: it waits for the transaction after the one that freed it.
	// This is what gives it every chance to be handed out.
	for round := 0; round < 4; round++ {
		after, err := store.Collection("articles")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			put(t, after, map[string]any{
				"id": fmt.Sprintf("later-%d-%02d", round, i), "title": "later",
				"author": "cat", "published": float64(i), "slug": fmt.Sprintf("l%d%02d", round, i),
			})
		}
		if err := store.Commit(); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	// Everything committed before the abandoned work must still be there and
	// still say what it said.
	final, err := store.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("k%03d", i)
		document, found, err := final.Get(key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if !found {
			t.Fatalf("%s is gone", key)
		}
		if document["title"] != "committed" {
			t.Fatalf("%s reads as %v", key, document["title"])
		}
	}

	// And the indexes lead to them, which is where a page handed out twice
	// shows up as an entry pointing at something else entirely.
	entries := scan(t, final, "by_author", Range{
		From: &Bound{Values: []any{"ann"}},
		To:   &Bound{Values: []any{"ann"}},
	})
	if len(entries) != count {
		t.Fatalf("the index holds %d of ann's %d articles", len(entries), count)
	}
	check := func() map[string]map[string]any {
		model := map[string]map[string]any{}
		if err := final.Walk(func(key any, document map[string]any) bool {
			model[key.(string)] = document
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return model
	}()
	checkAgreement(t, final, check, -1)
}
