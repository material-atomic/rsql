package store

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/vfs"
)

// populate fills a store with two collections, some operations and a few
// hundred documents, which is enough for a dump to have every kind of line in
// it.
func populate(t *testing.T, store *Store) *Collection {
	t.Helper()

	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Declare(Spec{Name: "notes", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}}); err != nil {
		t.Fatal(err)
	}
	notes, err := store.Collection("notes")
	if err != nil {
		t.Fatal(err)
	}

	declareOp(t, store, byAuthor())
	changed := byAuthor()
	changed.Limit = 1
	declareOp(t, store, changed)

	for i := 0; i < 200; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("k%03d", i), "author": []string{"ann", "bob"}[i%2],
			"published": float64(i), "title": fmt.Sprintf("t%d", i),
			"slug": fmt.Sprintf("s%03d", i), "tags": []any{"x", fmt.Sprintf("t%d", i%5)},
		})
		put(t, notes, map[string]any{"body": fmt.Sprintf("note %d", i)})
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	return collection
}

func TestADumpRestoresIntoTheSameDatabase(t *testing.T) {
	_, primary := fresh(t, 50)
	populate(t, primary)

	out := &bytes.Buffer{}
	lsn, err := primary.Dump(out)
	if err != nil {
		t.Fatal(err)
	}
	if lsn == 0 {
		t.Fatal("the dump reports no change number")
	}

	_, restored := fresh(t, 51)
	at, err := restored.Restore(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if at != lsn {
		t.Errorf("the restore is at %d, the dump was taken at %d", at, lsn)
	}

	sameStores(t, primary, restored)

	// Operations came too, every version of them.
	result, err := restored.Invoke(Caller{}, "articles.by_author", 1, map[string]any{"author": "ann"})
	if err != nil {
		t.Fatalf("the older version did not survive the dump: %v", err)
	}
	if result.Count != 3 {
		t.Errorf("version 1 returned %d rows", result.Count)
	}
}

// The reason a dump and the log go together: restore the one, follow the
// other, and end up where the primary is now — even though the log no longer
// reaches back far enough to replay from nothing.
func TestARestoreThenTheLogCatchesUp(t *testing.T) {
	_, primary := fresh(t, 52)
	collection := populate(t, primary)

	out := &bytes.Buffer{}
	lsn, err := primary.Dump(out)
	if err != nil {
		t.Fatal(err)
	}

	// The primary keeps working, keeping a window of log that is short — but
	// still longer than the work done since the dump, which is what the window
	// has to be for a restore plus the log to reach the present.
	primary.Retain(200)
	for i := 200; i < 300; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("k%03d", i), "author": "cat",
			"published": float64(i), "title": fmt.Sprintf("t%d", i), "slug": fmt.Sprintf("s%03d", i),
		})
	}
	if _, err := collection.Delete("k000"); err != nil {
		t.Fatal(err)
	}

	oldest, err := primary.OldestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if oldest <= 1 {
		t.Fatal("this test needs a log that no longer reaches the beginning")
	}
	follow, err := primary.CanFollow(lsn + 1)
	if err != nil {
		t.Fatal(err)
	}
	if !follow {
		t.Fatalf("the log starts at %d, past the dump at %d: nothing could catch up", oldest, lsn)
	}

	_, replica := fresh(t, 53)
	at, err := replica.Restore(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	for _, change := range changes(t, primary, at+1) {
		if err := replica.Apply(change); err != nil {
			t.Fatalf("apply %d (%s): %v", change.LSN, change.Kind, err)
		}
	}

	sameStores(t, primary, replica)
}

// A snapshot is the database as one transaction left it, and stays that way
// however much is written afterwards. That is what lets a dump be taken from a
// running database without stopping it.
func TestASnapshotDoesNotMoveWhileTheWriterDoes(t *testing.T) {
	_, store := fresh(t, 54)
	collection := populate(t, store)

	snapshot, err := store.Take()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()

	// Everything changes underneath: new documents, rewritten ones, deletions,
	// and enough commits that pages would be handed out again.
	for round := 0; round < 5; round++ {
		for i := 0; i < 200; i++ {
			put(t, collection, map[string]any{
				"id": fmt.Sprintf("k%03d", i), "author": "changed",
				"published": float64(i), "title": fmt.Sprintf("round %d", round), "slug": fmt.Sprintf("n%03d", i),
			})
		}
		for i := 0; i < 50; i++ {
			if _, err := collection.Delete(fmt.Sprintf("k%03d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	held := 0
	if err := snapshot.Walk("articles", func(_ any, document map[string]any) bool {
		held++
		if strings.HasPrefix(fmt.Sprint(document["title"]), "round ") {
			t.Fatalf("the snapshot sees work done after it was taken: %v", document)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if held != 200 {
		t.Errorf("the snapshot holds %d documents, want the 200 there were", held)
	}

	// And a dump taken from it is of that state, not of this one.
	out := &bytes.Buffer{}
	if err := snapshot.Dump(out); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("round ")) {
		t.Error("the dump holds work done after the snapshot was taken")
	}

	_, restored := fresh(t, 55)
	if _, err := restored.Restore(bytes.NewReader(out.Bytes())); err != nil {
		t.Fatal(err)
	}
	count := 0
	articles, _ := restored.Collection("articles")
	if err := articles.Walk(func(any, map[string]any) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 200 {
		t.Errorf("the restored dump holds %d documents", count)
	}
}

// A dump that was cut short looks exactly like a dump of a smaller database.
// The only thing between that and silent data loss is the line at the end.
func TestADumpThatWasCutShortIsRefused(t *testing.T) {
	_, primary := fresh(t, 56)
	populate(t, primary)

	out := &bytes.Buffer{}
	if _, err := primary.Dump(out); err != nil {
		t.Fatal(err)
	}
	whole := out.String()
	lines := strings.Split(strings.TrimRight(whole, "\n"), "\n")

	for _, cut := range []int{len(lines) - 1, len(lines) / 2, 3, 1} {
		_, restored := fresh(t, 57)
		partial := strings.Join(lines[:cut], "\n") + "\n"

		if _, err := restored.Restore(strings.NewReader(partial)); !errors.Is(err, ErrDumpIncomplete) && !errors.Is(err, ErrDumpFormat) {
			t.Errorf("%d of %d lines: want a refusal, got %v", cut, len(lines), err)
		}
	}

	// A trailer that disagrees with what came before it is a refusal too: it is
	// the count, not the presence of a last line, that is being checked.
	miscounted := strings.Replace(whole,
		lines[len(lines)-1],
		strings.Replace(lines[len(lines)-1], `"documents":400`, `"documents":399`, 1), 1)
	_, restored := fresh(t, 58)
	if _, err := restored.Restore(strings.NewReader(miscounted)); !errors.Is(err, ErrDumpIncomplete) {
		t.Errorf("a miscounted trailer: want ErrDumpIncomplete, got %v", err)
	}
}

func TestWhatIsNotADumpIsRefused(t *testing.T) {
	// Every one of these is otherwise a complete, well-formed dump, so what
	// refuses it is the check it is about and not something further along.
	header := `{"kind":"header","format":1,"lsn":7}` + "\n"
	end := `{"kind":"end","lsn":7,"documents":0}` + "\n"

	for name, text := range map[string]string{
		"nothing at all":                   "",
		"not json":                         "this is not a dump\n" + end,
		"no header":                        `{"kind":"collection","spec":{"name":"x"}}` + "\n" + end,
		"a format from later":              `{"kind":"header","format":99,"lsn":7}` + "\n" + end,
		"a line nobody knows":              header + `{"kind":"wat"}` + "\n" + end,
		"more after the end":               header + end + end,
		"an end at another lsn":            header + `{"kind":"end","lsn":8,"documents":0}` + "\n",
		"a collection with no declaration": header + `{"kind":"collection"}` + "\n" + end,
		"an operation with nothing in it":  header + `{"kind":"operation"}` + "\n" + end,
	} {
		t.Run(name, func(t *testing.T) {
			_, store := fresh(t, 59)
			if _, err := store.Restore(strings.NewReader(text)); err == nil {
				t.Error("it was restored anyway")
			}
		})
	}

	// And the shape those are all variations of does restore, so the test is
	// about what is wrong with each and not about the shape itself.
	_, store := fresh(t, 64)
	if at, err := store.Restore(strings.NewReader(header + end)); err != nil || at != 7 {
		t.Errorf("an empty dump: %d, %v", at, err)
	}
}

func TestARestoreNeedsAnEmptyDatabase(t *testing.T) {
	_, primary := fresh(t, 60)
	populate(t, primary)

	out := &bytes.Buffer{}
	if _, err := primary.Dump(out); err != nil {
		t.Fatal(err)
	}

	// Into itself: a mixture of two databases is neither of them, and looks
	// like a working one.
	if _, err := primary.Restore(bytes.NewReader(out.Bytes())); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("want ErrNotEmpty, got %v", err)
	}

	// A database with no collections but a history is not empty either.
	_, used := fresh(t, 61)
	if _, err := used.Declare(Spec{Name: "gone", Key: Key{Path: "id", Type: TypeString}}); err != nil {
		t.Fatal(err)
	}
	if err := used.Drop("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := used.Restore(bytes.NewReader(out.Bytes())); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("a database with a history: want ErrNotEmpty, got %v", err)
	}
}

// The hazard the retention cap creates, stated as a question a caller can ask:
// a consumer that has fallen further behind than the window is one that must
// be rebuilt, and it has to be able to find that out rather than discover it as
// a gap.
func TestAConsumerCanAskWhetherItHasFallenTooFarBehind(t *testing.T) {
	_, store := fresh(t, 62)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	// Nothing trimmed yet: everything from the beginning is still followable,
	// and one past the end is "up to date" rather than "too far behind".
	for i := 0; i < 10; i++ {
		put(t, collection, map[string]any{"id": fmt.Sprintf("k%02d", i), "title": "x"})
	}
	latest, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []uint64{1, latest, latest + 1} {
		if follow, err := store.CanFollow(from); err != nil || !follow {
			t.Errorf("from %d: %v, %v", from, follow, err)
		}
	}

	store.Retain(5)
	for i := 10; i < 40; i++ {
		put(t, collection, map[string]any{"id": fmt.Sprintf("k%02d", i), "title": "x"})
	}

	oldest, err := store.OldestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if follow, err := store.CanFollow(oldest - 1); err != nil || follow {
		t.Errorf("one before the window: %v, %v", follow, err)
	}
	if follow, err := store.CanFollow(oldest); err != nil || !follow {
		t.Errorf("the first entry still kept: %v, %v", follow, err)
	}
	if follow, err := store.CanFollow(1); err != nil || follow {
		t.Error("a consumer from the beginning was told it could carry on")
	}
}

// A database nobody has written to has nothing to follow and nothing to miss.
func TestAnEmptyLogIsNotBehind(t *testing.T) {
	_, store := fresh(t, 63)
	if follow, err := store.CanFollow(1); err != nil || !follow {
		t.Errorf("an empty database: %v, %v", follow, err)
	}
}

// A snapshot is a state the database was actually in. Work that has not been
// committed is not one of those, so it is not in the snapshot — and a dump
// taken while a transaction is half written holds the state before it, whole,
// rather than half of it.
func TestASnapshotHoldsTheLastCommitAndNotWorkInProgress(t *testing.T) {
	_, store := fresh(t, 65)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		put(t, collection, map[string]any{"id": fmt.Sprintf("k%02d", i), "title": "committed"})
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	committed, err := store.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}

	// A transaction in progress: new documents, a rewrite and a delete, none of
	// it committed.
	for i := 20; i < 40; i++ {
		put(t, collection, map[string]any{"id": fmt.Sprintf("k%02d", i), "title": "in progress"})
	}
	put(t, collection, map[string]any{"id": "k00", "title": "in progress"})
	if _, err := collection.Delete("k01"); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.Take()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()

	if snapshot.LSN() != committed {
		t.Errorf("the snapshot is at change %d, the last commit was %d", snapshot.LSN(), committed)
	}

	seen := 0
	if err := snapshot.Walk("articles", func(key any, document map[string]any) bool {
		seen++
		if document["title"] != "committed" {
			t.Fatalf("the snapshot holds uncommitted work: %v", document)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 20 {
		t.Errorf("the snapshot holds %d documents, want the 20 that were committed", seen)
	}
}

// The end-to-end statement of what encryption at rest is for: a file taken off
// the disk holds none of the documents in it, and the database still works.
func TestAnEncryptedDatabaseKeepsItsDocumentsOffTheDisk(t *testing.T) {
	key := []byte("the key for this database only")

	disk := vfs.NewSim(66, vfs.Faults{})
	pages, err := pager.CreateWith(disk, pager.Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}

	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("k%02d", i), "author": "ann",
			"title": fmt.Sprintf("Bí mật số %d", i), "slug": fmt.Sprintf("s%02d", i),
			"published": float64(i),
		})
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	image := disk.Durable()
	// Not the documents, not the field names, not the names of the collections
	// or the indexes — all of those are written through the same pages.
	for _, text := range []string{"Bí mật số 7", "author", "ann", "articles", "by_author", "s07"} {
		if bytes.Contains(image, []byte(text)) {
			t.Errorf("the file holds %q in the open", text)
		}
	}

	// And with the key it is an ordinary database.
	restart := vfs.NewSim(66, vfs.Faults{})
	restart.Restore(image)
	reopened, err := pager.OpenWith(restart, pager.Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(reopened)
	if err != nil {
		t.Fatal(err)
	}

	after, err := again.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	document, found, err := after.Get("k07")
	if err != nil || !found {
		t.Fatalf("get: %v, %v", found, err)
	}
	if document["title"] != "Bí mật số 7" {
		t.Errorf("the document reads %v", document)
	}
	if entries := scan(t, after, "by_author", Range{}); len(entries) != 50 {
		t.Errorf("the index came back with %d entries", len(entries))
	}
}
