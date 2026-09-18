package store

import (
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/ulid"
	"github.com/material-atomic/rsql/internal/vfs"
)

// endless is randomness that never runs out and never changes, which is what a
// test wants from it.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i*31 + 7)
	}
	return len(p), nil
}

func fresh(t *testing.T, seed int64) (*vfs.SimDisk, *Store) {
	t.Helper()

	disk := vfs.NewSim(seed, vfs.Faults{})
	pages, err := pager.Create(disk, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	store, err := Open(pages)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A clock and randomness that do not move, so that generated keys are the
	// same on every run. The randomness repeats rather than running out: a
	// test that fails at the four hundredth identifier is a test about its own
	// fixture.
	at := int64(1700000000000)
	store.Identifiers(ulid.With(
		func() time.Time { at++; return time.UnixMilli(at) },
		endless{},
	))
	return disk, store
}

// articles is the declaration most of these tests work against.
func articles() Spec {
	return Spec{
		Name: "articles",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{
			{
				Name: "by_author",
				Fields: []Field{
					{Path: "author", Type: TypeString, Missing: MissingSkip},
					{Path: "published", Type: TypeNumber, Descending: true, Missing: MissingLast},
				},
				Include: []string{"title"},
			},
			{
				Name:   "by_slug",
				Unique: true,
				Fields: []Field{{Path: "slug", Type: TypeString, Missing: MissingSkip}},
			},
			{
				Name:   "by_tag",
				Array:  "tags",
				Fields: []Field{{Path: "tags", Type: TypeString, Missing: MissingSkip}},
			},
		},
	}
}

func put(t *testing.T, collection *Collection, document map[string]any) any {
	t.Helper()
	key, err := collection.Put(document)
	if err != nil {
		t.Fatalf("put %v: %v", document, err)
	}
	return key
}

func scan(t *testing.T, collection *Collection, index string, within Range) []Found {
	t.Helper()
	var entries []Found
	if err := collection.Scan(index, within, func(one Found) bool {
		entries = append(entries, one)
		return true
	}); err != nil {
		t.Fatalf("scan %s: %v", index, err)
	}
	return entries
}

func keysOf(entries []Found) []string {
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = fmt.Sprint(entry.Key)
	}
	return out
}

func TestADocumentComesBackByItsKey(t *testing.T) {
	_, store := fresh(t, 1)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	key := put(t, collection, map[string]any{"title": "First", "author": "ann", "slug": "first"})
	if _, ok := key.(string); !ok {
		t.Fatalf("the generated key is %T", key)
	}

	document, found, err := collection.Get(key)
	if err != nil || !found {
		t.Fatalf("get: found=%v, err=%v", found, err)
	}
	if document["title"] != "First" {
		t.Errorf("the document came back as %v", document)
	}
	// The key is in the document, not only in the address of it.
	if document["id"] != key {
		t.Errorf("the document carries id %v, and is stored under %v", document["id"], key)
	}

	if _, found, err := collection.Get("nothing-like-this-key-at-all"); err != nil || found {
		t.Errorf("a key that is not there: found=%v, err=%v", found, err)
	}
}

func TestAKeyThatWasGivenIsKept(t *testing.T) {
	_, store := fresh(t, 2)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	key := put(t, collection, map[string]any{"id": "chosen", "title": "Mine"})
	if key != "chosen" {
		t.Errorf("the key is %v", key)
	}

	// And a collection that does not generate keys refuses a document without
	// one, rather than inventing an address for it.
	spec := articles()
	spec.Name = "strict"
	spec.Key.Auto = ""
	strict, err := store.Declare(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Put(map[string]any{"title": "No key"}); !errors.Is(err, ErrNoKey) {
		t.Errorf("want ErrNoKey, got %v", err)
	}
}

func TestDocumentsComeBackInKeyOrder(t *testing.T) {
	_, store := fresh(t, 3)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	var written []string
	for i := 0; i < 50; i++ {
		key := put(t, collection, map[string]any{"title": fmt.Sprintf("Article %d", i)})
		written = append(written, key.(string))
	}

	var walked []string
	if err := collection.Walk(func(key any, _ map[string]any) bool {
		walked = append(walked, key.(string))
		return true
	}); err != nil {
		t.Fatal(err)
	}

	if !sort.StringsAreSorted(walked) {
		t.Error("the walk is not in key order")
	}
	// Generated keys carry the time they were made, so writing in order is
	// storing in order — which is the whole reason the default key is a ULID.
	if fmt.Sprint(walked) != fmt.Sprint(written) {
		t.Errorf("walked %v, wrote %v", walked, written)
	}
}

func TestAnIndexFindsDocumentsByAField(t *testing.T) {
	_, store := fresh(t, 4)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	for _, document := range []map[string]any{
		{"id": "a", "author": "ann", "published": 3.0, "title": "Ann three", "slug": "a3"},
		{"id": "b", "author": "ann", "published": 1.0, "title": "Ann one", "slug": "a1"},
		{"id": "c", "author": "bob", "published": 2.0, "title": "Bob two", "slug": "b2"},
		{"id": "d", "author": "ann", "published": 2.0, "title": "Ann two", "slug": "a2"},
		{"id": "e", "title": "Nobody wrote this", "slug": "x"},
	} {
		put(t, collection, document)
	}

	// The whole index: by author, then by publication date descending, which
	// is what the declaration asked for.
	all := scan(t, collection, "by_author", Range{})
	if got := fmt.Sprint(keysOf(all)); got != "[a d b c]" {
		t.Errorf("the index reads %v, want [a d b c]", got)
	}

	// A document with no author is not in this index at all: the field was
	// declared to skip them.
	for _, entry := range all {
		if entry.Key == "e" {
			t.Error("a document with no author is in an index that skips them")
		}
	}

	// One author, by giving a prefix of the index.
	ann := scan(t, collection, "by_author", Range{
		From: &Bound{Values: []any{"ann"}},
		To:   &Bound{Values: []any{"ann"}},
	})
	if got := fmt.Sprint(keysOf(ann)); got != "[a d b]" {
		t.Errorf("ann's articles read %v, want [a d b]", got)
	}

	// The included field comes back without the document being read.
	for _, entry := range ann {
		if entry.Include["title"] == nil {
			t.Errorf("entry %v carries no title", entry.Key)
		}
		if len(entry.Include) != 1 {
			t.Errorf("entry %v carries %v, and only the title was included", entry.Key, entry.Include)
		}
	}
}

func TestARangeIsWhereItWasAskedToStartAndStop(t *testing.T) {
	_, store := fresh(t, 5)
	spec := Spec{
		Name: "readings",
		Key:  Key{Path: "id", Type: TypeString},
		Indexes: []Index{{
			Name:   "by_value",
			Fields: []Field{{Path: "value", Type: TypeNumber, Missing: MissingSkip}},
		}},
	}
	collection, err := store.Declare(spec)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		put(t, collection, map[string]any{"id": fmt.Sprintf("r%02d", i), "value": float64(i)})
	}

	for _, want := range []struct {
		name  string
		from  *Bound
		to    *Bound
		first string
		count int
	}{
		{"everything", nil, nil, "r00", 10},
		{"from three", &Bound{Values: []any{3.0}}, nil, "r03", 7},
		{"after three", &Bound{Values: []any{3.0}, Exclusive: true}, nil, "r04", 6},
		{"up to three", nil, &Bound{Values: []any{3.0}}, "r00", 4},
		{"below three", nil, &Bound{Values: []any{3.0}, Exclusive: true}, "r00", 3},
		{"three to five", &Bound{Values: []any{3.0}}, &Bound{Values: []any{5.0}}, "r03", 3},
		{"nothing there", &Bound{Values: []any{100.0}}, nil, "", 0},
	} {
		t.Run(want.name, func(t *testing.T) {
			entries := scan(t, collection, "by_value", Range{From: want.from, To: want.to})
			if len(entries) != want.count {
				t.Fatalf("%d entries, want %d: %v", len(entries), want.count, keysOf(entries))
			}
			if want.count > 0 && fmt.Sprint(entries[0].Key) != want.first {
				t.Errorf("starts at %v, want %v", entries[0].Key, want.first)
			}
		})
	}
}

func TestAUniqueIndexRefusesASecondDocument(t *testing.T) {
	_, store := fresh(t, 6)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	put(t, collection, map[string]any{"id": "a", "slug": "hello"})

	if _, err := collection.Put(map[string]any{"id": "b", "slug": "hello"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	// And the refused document was not half written.
	if _, found, _ := collection.Get("b"); found {
		t.Error("the document that was refused is stored anyway")
	}
	entries := scan(t, collection, "by_slug", Range{})
	if len(entries) != 1 {
		t.Errorf("the index holds %d entries, want 1", len(entries))
	}

	// A document may keep its own value when it is rewritten.
	if _, err := collection.Put(map[string]any{"id": "a", "slug": "hello", "title": "Changed"}); err != nil {
		t.Errorf("rewriting a document with its own value: %v", err)
	}
	// And the value is free again once the document holding it is gone.
	if _, err := collection.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := collection.Put(map[string]any{"id": "b", "slug": "hello"}); err != nil {
		t.Errorf("after the first document went, the value is still refused: %v", err)
	}
}

func TestAnArrayFieldPutsTheDocumentUnderEachElement(t *testing.T) {
	_, store := fresh(t, 7)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	put(t, collection, map[string]any{"id": "a", "tags": []any{"go", "storage", "go"}})
	put(t, collection, map[string]any{"id": "b", "tags": []any{"storage"}})
	put(t, collection, map[string]any{"id": "c", "tags": "go"}) // not a list, but still findable
	put(t, collection, map[string]any{"id": "d"})

	storage := scan(t, collection, "by_tag", Range{
		From: &Bound{Values: []any{"storage"}},
		To:   &Bound{Values: []any{"storage"}},
	})
	if got := fmt.Sprint(keysOf(storage)); got != "[a b]" {
		t.Errorf("storage reads %v, want [a b]", got)
	}

	found := scan(t, collection, "by_tag", Range{
		From: &Bound{Values: []any{"go"}},
		To:   &Bound{Values: []any{"go"}},
	})
	if got := fmt.Sprint(keysOf(found)); got != "[a c]" {
		t.Errorf("go reads %v, want [a c]", got)
	}
	// "go" twice in one array is one entry, not two.
	if len(found) != 2 {
		t.Errorf("a repeated element was indexed %d times", len(found))
	}
}

func TestARewriteLeavesNothingBehind(t *testing.T) {
	_, store := fresh(t, 8)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	put(t, collection, map[string]any{"id": "a", "author": "ann", "published": 1.0, "slug": "s1", "tags": []any{"x", "y"}})
	put(t, collection, map[string]any{"id": "a", "author": "bob", "published": 2.0, "slug": "s2", "tags": []any{"z"}})

	// Nothing about the old version may still be in any index.
	for _, want := range []struct {
		index string
		value any
		count int
	}{
		{"by_author", "ann", 0},
		{"by_author", "bob", 1},
		{"by_slug", "s1", 0},
		{"by_slug", "s2", 1},
		{"by_tag", "x", 0},
		{"by_tag", "y", 0},
		{"by_tag", "z", 1},
	} {
		entries := scan(t, collection, want.index, Range{
			From: &Bound{Values: []any{want.value}},
			To:   &Bound{Values: []any{want.value}},
		})
		if len(entries) != want.count {
			t.Errorf("%s %v: %d entries, want %d", want.index, want.value, len(entries), want.count)
		}
	}

	// And a delete takes all of them with it.
	if removed, err := collection.Delete("a"); err != nil || !removed {
		t.Fatalf("delete: %v, %v", removed, err)
	}
	for _, index := range []string{"by_author", "by_slug", "by_tag"} {
		if entries := scan(t, collection, index, Range{}); len(entries) != 0 {
			t.Errorf("%s still holds %d entries after the document was deleted", index, len(entries))
		}
	}
}

func TestAValueOfTheWrongTypeIsRefused(t *testing.T) {
	_, store := fresh(t, 9)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	// The index was told this field is a string. A number would sort somewhere
	// else entirely, where nothing looking for it would pass.
	if _, err := collection.Put(map[string]any{"id": "a", "author": 7.0}); !errors.Is(err, ErrType) {
		t.Errorf("want ErrType, got %v", err)
	}
	if _, err := collection.Put(map[string]any{"id": "b", "tags": []any{"ok", 3.0}}); !errors.Is(err, ErrType) {
		t.Errorf("an array of the wrong type: want ErrType, got %v", err)
	}
	if _, err := collection.Put(map[string]any{"id": 4.0}); !errors.Is(err, ErrType) {
		t.Errorf("a key of the wrong type: want ErrType, got %v", err)
	}

	// Null is a value the document carries, as opposed to a field it does not
	// have, and both are indexable.
	if _, err := collection.Put(map[string]any{"id": "c", "author": nil, "published": 1.0}); err != nil {
		t.Errorf("null in an indexed field: %v", err)
	}
}

func TestWhatIsDeclaredIsWhatIsStored(t *testing.T) {
	disk, store := fresh(t, 10)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	put(t, collection, map[string]any{"id": "a", "author": "ann", "published": 1.0, "slug": "s"})
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(10, vfs.Faults{})
	restart.Restore(disk.Durable())
	pages, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}

	after, err := reopened.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(after.Spec()); got != fmt.Sprint(collection.Spec()) {
		t.Errorf("the declaration came back as %v", got)
	}

	document, found, err := after.Get("a")
	if err != nil || !found || document["author"] != "ann" {
		t.Errorf("the document came back as %v (%v, %v)", document, found, err)
	}
	if entries := scan(t, after, "by_author", Range{}); len(entries) != 1 {
		t.Errorf("the index came back with %d entries", len(entries))
	}

	if _, err := reopened.Collection("nothing"); !errors.Is(err, ErrNoCollection) {
		t.Errorf("want ErrNoCollection, got %v", err)
	}
}

func TestAnIndexAddedLaterIsBuiltFromWhatIsThere(t *testing.T) {
	_, store := fresh(t, 11)
	spec := articles()
	spec.Indexes = spec.Indexes[:1] // by_author only
	collection, err := store.Declare(spec)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("a%02d", i), "author": "ann",
			"published": float64(i), "slug": fmt.Sprintf("s%02d", i),
		})
	}

	// Declared again with the unique index on the end: it has to be filled
	// from the documents already stored, or it would be an index that agrees
	// with the future and not the past.
	collection, err = store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	if entries := scan(t, collection, "by_slug", Range{}); len(entries) != 20 {
		t.Fatalf("the new index holds %d entries, want 20", len(entries))
	}
	if _, err := collection.Put(map[string]any{"id": "new", "slug": "s05"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("the rebuilt unique index does not hold: %v", err)
	}

	// Dropping one takes its entries with it.
	fewer := articles()
	fewer.Indexes = fewer.Indexes[:1]
	collection, err = store.Declare(fewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Scan("by_slug", Range{}, func(Found) bool { return true }); !errors.Is(err, ErrNoIndex) {
		t.Errorf("want ErrNoIndex, got %v", err)
	}
	if entries := scan(t, collection, "by_author", Range{}); len(entries) != 20 {
		t.Errorf("dropping one index disturbed another: %d entries", len(entries))
	}
}

func TestAnIndexCannotChangeShapeUnderItsOwnName(t *testing.T) {
	_, store := fresh(t, 12)
	if _, err := store.Declare(articles()); err != nil {
		t.Fatal(err)
	}

	changed := articles()
	changed.Indexes[1].Unique = false
	if _, err := store.Declare(changed); !errors.Is(err, ErrIncompatible) {
		t.Errorf("want ErrIncompatible, got %v", err)
	}

	moved := articles()
	moved.Key.Path = "key"
	if _, err := store.Declare(moved); !errors.Is(err, ErrIncompatible) {
		t.Errorf("a moved primary key: want ErrIncompatible, got %v", err)
	}
}

func TestADeclarationThatMakesNoSenseIsRefused(t *testing.T) {
	_, store := fresh(t, 13)

	for name, spec := range map[string]func(Spec) Spec{
		"no name":                                 func(s Spec) Spec { s.Name = ""; return s },
		"a nested primary key":                    func(s Spec) Spec { s.Key.Path = "meta.id"; return s },
		"a primary key of no type":                func(s Spec) Spec { s.Key.Type = ""; return s },
		"a generated number key":                  func(s Spec) Spec { s.Key.Type = TypeNumber; s.Key.Auto = "ulid"; return s },
		"an index with no fields":                 func(s Spec) Spec { s.Indexes[0].Fields = nil; return s },
		"a field of no type":                      func(s Spec) Spec { s.Indexes[0].Fields[0].Type = "date"; return s },
		"missing left unsaid":                     func(s Spec) Spec { s.Indexes[0].Fields[0].Missing = ""; return s },
		"two indexes of one name":                 func(s Spec) Spec { s.Indexes[1].Name = s.Indexes[0].Name; return s },
		"one field twice":                         func(s Spec) Spec { s.Indexes[0].Fields[1].Path = "author"; return s },
		"spreading over a field it does not have": func(s Spec) Spec { s.Indexes[2].Array = "other"; return s },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Declare(spec(articles())); err == nil {
				t.Error("the declaration was accepted")
			}
		})
	}
}

func TestDroppingACollectionTakesEverythingWithIt(t *testing.T) {
	_, store := fresh(t, 14)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Declare(Spec{Name: "notes", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 30; i++ {
		put(t, collection, map[string]any{"id": fmt.Sprintf("a%02d", i), "author": "ann", "slug": fmt.Sprint(i)})
		put(t, other, map[string]any{"body": fmt.Sprint(i)})
	}

	if err := store.Drop("articles"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Collection("articles"); !errors.Is(err, ErrNoCollection) {
		t.Errorf("want ErrNoCollection, got %v", err)
	}

	// The other collection is untouched: dropping one must not be a range that
	// reaches into its neighbour.
	count := 0
	if err := other.Walk(func(any, map[string]any) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 30 {
		t.Errorf("the other collection holds %d documents, want 30", count)
	}

	// And the name is free again.
	if _, err := store.Declare(articles()); err != nil {
		t.Fatal(err)
	}
	if entries := scan(t, mustGet(t, store, "articles"), "by_author", Range{}); len(entries) != 0 {
		t.Errorf("the new collection was born holding %d index entries", len(entries))
	}
}

func mustGet(t *testing.T, store *Store, name string) *Collection {
	t.Helper()
	collection, err := store.Collection(name)
	if err != nil {
		t.Fatal(err)
	}
	return collection
}

// The invariant that matters most, checked over a long run of random work:
// every index entry describes a document that is there, and every document is
// in every index that should hold it.
func TestTheIndexesAlwaysAgreeWithTheDocuments(t *testing.T) {
	_, store := fresh(t, 15)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}

	authors := []string{"ann", "bob", "cat"}
	model := map[string]map[string]any{}
	slugs := map[string]string{} // slug -> key, to keep the unique index honest

	for step := 0; step < 4000; step++ {
		key := fmt.Sprintf("k%03d", step%300)

		switch step % 5 {
		case 0, 1, 2:
			document := map[string]any{"id": key, "title": fmt.Sprintf("t%d", step)}
			if step%3 != 0 {
				document["author"] = authors[step%len(authors)]
				document["published"] = float64(step % 50)
			}
			if step%4 != 0 {
				document["slug"] = fmt.Sprintf("s%03d", step%137)
			}
			if step%7 != 0 {
				document["tags"] = []any{fmt.Sprintf("t%d", step%11), fmt.Sprintf("t%d", step%3)}
			}

			slug, _ := document["slug"].(string)
			if holder, taken := slugs[slug]; slug != "" && taken && holder != key {
				if _, err := collection.Put(document); !errors.Is(err, ErrDuplicate) {
					t.Fatalf("step %d: a taken slug was accepted: %v", step, err)
				}
				continue
			}

			if _, err := collection.Put(document); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
			if was, ok := model[key]; ok {
				if slug, _ := was["slug"].(string); slug != "" {
					delete(slugs, slug)
				}
			}
			model[key] = document
			if slug != "" {
				slugs[slug] = key
			}

		case 3:
			if was, ok := model[key]; ok {
				if slug, _ := was["slug"].(string); slug != "" {
					delete(slugs, slug)
				}
			}
			if _, err := collection.Delete(key); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
			delete(model, key)

		case 4:
			if step%200 != 4 {
				continue
			}
			if err := store.Commit(); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
			checkAgreement(t, collection, model, step)
		}
	}

	checkAgreement(t, collection, model, -1)
}

// checkAgreement works out what the indexes should hold from the documents
// themselves, and compares that with what they do hold.
func checkAgreement(t *testing.T, collection *Collection, model map[string]map[string]any, step int) {
	t.Helper()

	stored := map[string]map[string]any{}
	if err := collection.Walk(func(key any, document map[string]any) bool {
		stored[key.(string)] = document
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(model) {
		t.Fatalf("step %d: %d documents stored, %d in the model", step, len(stored), len(model))
	}
	for key, document := range model {
		if stored[key] == nil {
			t.Fatalf("step %d: %s is missing", step, key)
		}
		if fmt.Sprint(stored[key]["title"]) != fmt.Sprint(document["title"]) {
			t.Fatalf("step %d: %s reads as %v", step, key, stored[key])
		}
	}

	for _, index := range collection.Spec().Indexes {
		want := map[string]int{}
		for key, document := range model {
			for _, value := range indexed(document, index) {
				want[value+"\x00"+key]++
			}
		}

		got := map[string]int{}
		for _, entry := range scan(t, collection, index.Name, Range{}) {
			got[fmt.Sprint(entry.Values)+"\x00"+entry.Key.(string)]++
		}

		if len(want) != len(got) {
			t.Fatalf("step %d: index %q holds %d entries, the documents call for %d",
				step, index.Name, len(got), len(want))
		}
		for entry := range want {
			if got[entry] == 0 {
				t.Fatalf("step %d: index %q is missing %q", step, index.Name, entry)
			}
		}
	}
}

// indexed is what one index should hold for one document, written out from the
// declaration rather than from the code that maintains the index.
func indexed(document map[string]any, index Index) []string {
	values := make([]any, len(index.Fields))
	spread := -1

	for i, field := range index.Fields {
		value, found := at(document, field.Path)
		if !found {
			if field.Missing == MissingSkip {
				return nil
			}
			values[i] = "absent"
			continue
		}
		if field.Path == index.Array {
			spread = i
		}
		values[i] = value
	}

	elements := []any{nil}
	if spread >= 0 {
		elements = spreadOf(values[spread])
	}

	seen := map[string]bool{}
	var out []string
	for _, element := range elements {
		if spread >= 0 {
			values[spread] = element
		}
		printed := fmt.Sprint(values)
		if seen[printed] {
			continue
		}
		seen[printed] = true
		out = append(out, printed)
	}
	return out
}

// A handle held across a declaration must see the declaration now in force.
// One still pointing at the old one would write documents with no entry in an
// index that exists, and nothing would say so until a query came back short.
func TestAHandleHeldAcrossADeclarationMaintainsTheNewIndex(t *testing.T) {
	_, store := fresh(t, 16)
	spec := articles()
	spec.Indexes = spec.Indexes[:1]
	held, err := store.Declare(spec)
	if err != nil {
		t.Fatal(err)
	}
	put(t, held, map[string]any{"id": "before", "author": "ann", "slug": "s-before"})

	// A second index is declared through the store, while `held` stays what the
	// caller has.
	if _, err := store.Declare(articles()); err != nil {
		t.Fatal(err)
	}
	put(t, held, map[string]any{"id": "after", "author": "ann", "slug": "s-after"})

	entries := scan(t, held, "by_slug", Range{})
	if len(entries) != 2 {
		t.Fatalf("the new index holds %d entries, want both documents: %v", len(entries), keysOf(entries))
	}

	// And a delete through the old handle clears the new index too.
	if removed, err := held.Delete("after"); err != nil || !removed {
		t.Fatal(err)
	}
	if entries := scan(t, held, "by_slug", Range{}); len(entries) != 1 {
		t.Errorf("after a delete the index holds %d entries", len(entries))
	}
}

func TestAHandleToADroppedCollectionRefusesToWrite(t *testing.T) {
	_, store := fresh(t, 17)
	held, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	put(t, held, map[string]any{"id": "a", "title": "One"})

	if err := store.Drop("articles"); err != nil {
		t.Fatal(err)
	}

	// Writing through it would fill a keyspace nothing points at, and the next
	// collection given that id would inherit the documents.
	if _, err := held.Put(map[string]any{"id": "b", "title": "Two"}); !errors.Is(err, ErrNoCollection) {
		t.Errorf("put: want ErrNoCollection, got %v", err)
	}
	if _, err := held.Delete("a"); !errors.Is(err, ErrNoCollection) {
		t.Errorf("delete: want ErrNoCollection, got %v", err)
	}
}
