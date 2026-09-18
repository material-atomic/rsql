package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/vfs"
)

// declared is a store with the articles collection and a few operations on it,
// which is as much of a database as most of these tests need.
func declared(t *testing.T, seed int64) (*Store, *Collection) {
	t.Helper()

	_, store := fresh(t, seed)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	return store, collection
}

func declareOp(t *testing.T, store *Store, operation Operation) Operation {
	t.Helper()
	stored, err := store.DeclareOperation(operation)
	if err != nil {
		t.Fatalf("declare %q: %v", operation.Name, err)
	}
	return stored
}

func invoke(t *testing.T, store *Store, name string, arguments map[string]any) Result {
	t.Helper()
	result, err := store.Invoke(Caller{}, name, 0, arguments)
	if err != nil {
		t.Fatalf("invoke %q: %v", name, err)
	}
	return result
}

// byAuthor reads one author's articles, newest first, at most three of them.
func byAuthor() Operation {
	return Operation{
		Name:       "articles.by_author",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_author",
		Input:      []Parameter{{Name: "author", Type: TypeString, Required: true}},
		From:       &Endpoint{Terms: []Term{{Arg: "author"}}},
		To:         &Endpoint{Terms: []Term{{Arg: "author"}}},
		Projection: []string{"id", "title"},
		Limit:      3,
	}
}

func fill(t *testing.T, collection *Collection) {
	t.Helper()
	for i := 0; i < 6; i++ {
		author := "ann"
		if i%3 == 0 {
			author = "bob"
		}
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("a%d", i), "author": author,
			"published": float64(i), "title": fmt.Sprintf("Article %d", i),
			"slug": fmt.Sprintf("s%d", i),
		})
	}
}

func TestAnOperationIsTheOnlyWayIn(t *testing.T) {
	store, collection := declared(t, 20)
	fill(t, collection)
	declareOp(t, store, byAuthor())

	result := invoke(t, store, "articles.by_author", map[string]any{"author": "ann"})
	if result.Count != 3 || len(result.Rows) != 3 {
		t.Fatalf("got %d rows: %v", len(result.Rows), result.Rows)
	}
	// Newest first, which is how the index was declared, and only the fields
	// the operation projects.
	if result.Rows[0]["id"] != "a5" {
		t.Errorf("the first row is %v", result.Rows[0])
	}
	for _, row := range result.Rows {
		if len(row) != 2 || row["title"] == nil {
			t.Errorf("a row came back as %v", row)
		}
	}

	// An operation nobody declared does not run.
	if _, err := store.Invoke(Caller{}, "articles.everything", 0, nil); !errors.Is(err, ErrNoOperation) {
		t.Errorf("want ErrNoOperation, got %v", err)
	}
}

func TestALimitIsWhatTheOperationCosts(t *testing.T) {
	store, collection := declared(t, 21)
	fill(t, collection)
	declareOp(t, store, byAuthor())

	// Four of ann's articles, three of which fit: the answer says it is partial
	// rather than pretending to be everything.
	result := invoke(t, store, "articles.by_author", map[string]any{"author": "ann"})
	if !result.Truncated {
		t.Error("a scan that stopped at its limit did not say so")
	}

	// Two of bob's, which is under the limit.
	result = invoke(t, store, "articles.by_author", map[string]any{"author": "bob"})
	if result.Count != 2 || result.Truncated {
		t.Errorf("bob: %d rows, truncated=%v", result.Count, result.Truncated)
	}

	// And a scan that does not declare a limit cannot be declared at all: its
	// cost is not known, which is the one thing declaring is for.
	unbounded := byAuthor()
	unbounded.Name = "articles.unbounded"
	unbounded.Limit = 0
	if _, err := store.DeclareOperation(unbounded); !errors.Is(err, ErrDeclaration) {
		t.Errorf("want ErrDeclaration, got %v", err)
	}
}

func TestArgumentsCannotReachPastWhereTheyWereDeclared(t *testing.T) {
	store, collection := declared(t, 22)
	fill(t, collection)
	declareOp(t, store, byAuthor())

	// An argument the operation does not declare is refused, not ignored. A
	// caller that believes something was applied when it was not is worse off
	// than one that is told no.
	if _, err := store.Invoke(Caller{}, "articles.by_author", 0, map[string]any{
		"author": "ann", "limit": 1000,
	}); !errors.Is(err, ErrArgument) {
		t.Errorf("an undeclared argument: want ErrArgument, got %v", err)
	}

	// A required argument that is not given is refused rather than defaulted.
	if _, err := store.Invoke(Caller{}, "articles.by_author", 0, nil); !errors.Is(err, ErrArgument) {
		t.Errorf("a missing argument: want ErrArgument, got %v", err)
	}

	// An argument of the wrong type is refused before it can sort somewhere
	// nothing is looking.
	if _, err := store.Invoke(Caller{}, "articles.by_author", 0, map[string]any{
		"author": 7.0,
	}); !errors.Is(err, ErrArgument) {
		t.Errorf("an argument of the wrong type: want ErrArgument, got %v", err)
	}
}

func TestADeclarationIsCheckedWhenItIsMadeNotWhenItRuns(t *testing.T) {
	store, _ := declared(t, 23)

	for name, broken := range map[string]func(Operation) Operation{
		"an index that is not there": func(o Operation) Operation { o.Index = "by_nothing"; return o },
		"a collection that is not there": func(o Operation) Operation {
			o.Collection = "nowhere"
			return o
		},
		"an action nobody has heard of": func(o Operation) Operation { o.Action = "upsert"; return o },
		"a bound on an argument that does not exist": func(o Operation) Operation {
			o.From = &Endpoint{Terms: []Term{{Arg: "writer"}}}
			return o
		},
		"a bound whose argument is the wrong type": func(o Operation) Operation {
			o.Input = []Parameter{{Name: "author", Type: TypeNumber, Required: true}}
			return o
		},
		"more bound values than the index has fields": func(o Operation) Operation {
			o.From = &Endpoint{Terms: []Term{{Arg: "author"}, {Value: 1.0}, {Value: 2.0}}}
			return o
		},
		"a term that is both an argument and a value": func(o Operation) Operation {
			o.From = &Endpoint{Terms: []Term{{Arg: "author", Value: "ann"}}}
			return o
		},
		"a term that is neither": func(o Operation) Operation {
			o.From = &Endpoint{Terms: []Term{{}}}
			return o
		},
		"two arguments of one name": func(o Operation) Operation {
			o.Input = append(o.Input, Parameter{Name: "author", Type: TypeString})
			return o
		},
		"a default of the wrong type": func(o Operation) Operation {
			o.Input = []Parameter{{Name: "author", Type: TypeString, Default: 3.0}}
			return o
		},
		"required and defaulted at once": func(o Operation) Operation {
			o.Input = []Parameter{{Name: "author", Type: TypeString, Required: true, Default: "ann"}}
			return o
		},
	} {
		t.Run(name, func(t *testing.T) {
			operation := broken(byAuthor())
			operation.Name = "articles.broken"
			if _, err := store.DeclareOperation(operation); err == nil {
				t.Error("the declaration was accepted")
			}
		})
	}
}

func TestWritingIsDeclaredTheSameWay(t *testing.T) {
	store, collection := declared(t, 24)

	declareOp(t, store, Operation{
		Name: "articles.write", Collection: "articles", Action: ActionInsert,
		Input: []Parameter{
			{Name: "title", Type: TypeString, Required: true},
			{Name: "author", Type: TypeString, Required: true},
			{Name: "slug", Type: TypeString},
		},
		Document: map[string]Term{
			"title":     {Arg: "title"},
			"author":    {Arg: "author"},
			"slug":      {Arg: "slug"},
			"published": {Value: 0.0},
			"source":    {Value: "api"},
		},
	})

	result := invoke(t, store, "articles.write", map[string]any{"title": "Hello", "author": "ann", "slug": "hello"})
	if result.Changed != 1 || result.Key == nil {
		t.Fatalf("the insert reported %+v", result)
	}

	document, found, err := collection.Get(result.Key)
	if err != nil || !found {
		t.Fatalf("the document: %v, %v", found, err)
	}
	// Constants come from the declaration, so a caller cannot choose them.
	if document["source"] != "api" || document["published"] != 0.0 {
		t.Errorf("the written document is %v", document)
	}

	// An optional argument that was not passed leaves its field out rather
	// than writing a null over it.
	second := invoke(t, store, "articles.write", map[string]any{"title": "Second", "author": "ann"})
	document, _, _ = collection.Get(second.Key)
	if _, carries := document["slug"]; carries {
		t.Errorf("an argument nobody passed was written anyway: %v", document)
	}
}

func TestAnInsertWillNotWriteOverADocument(t *testing.T) {
	store, _ := declared(t, 25)

	for _, action := range []string{ActionInsert, ActionPut} {
		declareOp(t, store, Operation{
			Name: "articles." + action, Collection: "articles", Action: action,
			Input:    []Parameter{{Name: "id", Type: TypeString, Required: true}, {Name: "title", Type: TypeString, Required: true}},
			Document: map[string]Term{"id": {Arg: "id"}, "title": {Arg: "title"}},
		})
	}

	if _, err := store.Invoke(Caller{}, "articles.insert", 0, map[string]any{"id": "x", "title": "First"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Invoke(Caller{}, "articles.insert", 0, map[string]any{"id": "x", "title": "Again"}); !errors.Is(err, ErrExists) {
		t.Errorf("want ErrExists, got %v", err)
	}

	// A put is the operation that says it replaces, so it does.
	if _, err := store.Invoke(Caller{}, "articles.put", 0, map[string]any{"id": "x", "title": "Replaced"}); err != nil {
		t.Errorf("put: %v", err)
	}
}

func TestUpdateChangesOnlyTheFieldsItDeclares(t *testing.T) {
	store, collection := declared(t, 26)
	put(t, collection, map[string]any{"id": "a", "title": "Before", "author": "ann", "published": 1.0})

	// "reason" is declared and passed, and goes nowhere: the operation does not
	// put it in Set. An argument is only written where the declaration says.
	declareOp(t, store, Operation{
		Name: "articles.retitle", Collection: "articles", Action: ActionUpdate,
		Input: []Parameter{
			{Name: "id", Type: TypeString, Required: true},
			{Name: "title", Type: TypeString, Required: true},
			{Name: "reason", Type: TypeString},
		},
		Key: &Term{Arg: "id"},
		Set: map[string]Term{"title": {Arg: "title"}},
	})

	result := invoke(t, store, "articles.retitle", map[string]any{"id": "a", "title": "After", "reason": "typo"})
	if result.Changed != 1 {
		t.Fatalf("the update reported %+v", result)
	}

	document, _, _ := collection.Get("a")
	if document["title"] != "After" {
		t.Errorf("the title is %v", document["title"])
	}
	if document["author"] != "ann" || document["published"] != 1.0 {
		t.Errorf("an update touched fields it does not declare: %v", document)
	}
	if _, wrote := document["reason"]; wrote {
		t.Errorf("an argument the operation does not write ended up in the document: %v", document)
	}

	// A document that is not there is not an error, and changed nothing.
	result = invoke(t, store, "articles.retitle", map[string]any{"id": "nope", "title": "x"})
	if result.Changed != 0 {
		t.Errorf("updating nothing reported %+v", result)
	}
}

func TestDeleteAndCountAndTheClusteredWalk(t *testing.T) {
	store, collection := declared(t, 27)
	fill(t, collection)

	declareOp(t, store, Operation{
		Name: "articles.remove", Collection: "articles", Action: ActionDelete,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	})
	declareOp(t, store, Operation{
		Name: "articles.count", Collection: "articles", Action: ActionCount,
		Index: ClusteredIndex,
	})
	declareOp(t, store, Operation{
		Name: "articles.page", Collection: "articles", Action: ActionScan,
		Index: ClusteredIndex,
		Input: []Parameter{{Name: "after", Type: TypeString}},
		From:  &Endpoint{Terms: []Term{{Arg: "after"}}, Exclusive: true},
		Limit: 2,
	})

	if result := invoke(t, store, "articles.count", nil); result.Count != 6 {
		t.Errorf("the count is %d, want 6", result.Count)
	}

	if result := invoke(t, store, "articles.remove", map[string]any{"id": "a3"}); result.Changed != 1 {
		t.Errorf("the delete reported %+v", result)
	}
	if result := invoke(t, store, "articles.remove", map[string]any{"id": "a3"}); result.Changed != 0 {
		t.Errorf("deleting twice reported %+v", result)
	}
	if result := invoke(t, store, "articles.count", nil); result.Count != 5 {
		t.Errorf("the count is %d, want 5", result.Count)
	}

	// A page at a time through the clustered order. The bound is optional, so
	// leaving it out starts at the beginning — which is how paging is written.
	first := invoke(t, store, "articles.page", nil)
	if len(first.Rows) != 2 || first.Rows[0]["id"] != "a0" || !first.Truncated {
		t.Fatalf("the first page is %v (truncated=%v)", first.Rows, first.Truncated)
	}
	second := invoke(t, store, "articles.page", map[string]any{"after": "a1"})
	if len(second.Rows) != 2 || second.Rows[0]["id"] != "a2" {
		t.Fatalf("the second page is %v", second.Rows)
	}
}

func TestAnOperationIsRefusedToACallerWithoutTheScope(t *testing.T) {
	store, collection := declared(t, 28)
	fill(t, collection)

	operation := byAuthor()
	operation.Scopes = []string{"articles:read", "reports"}
	declareOp(t, store, operation)

	if _, err := store.Invoke(Caller{}, "articles.by_author", 0, map[string]any{"author": "ann"}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("no scopes at all: want ErrNotAllowed, got %v", err)
	}
	// Some of them is not enough: the check is every scope, not any.
	if _, err := store.Invoke(Caller{Scopes: []string{"articles:read"}}, "articles.by_author", 0,
		map[string]any{"author": "ann"}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("one scope of two: want ErrNotAllowed, got %v", err)
	}
	if _, err := store.Invoke(Caller{Scopes: []string{"reports", "articles:read", "other"}}, "articles.by_author", 0,
		map[string]any{"author": "ann"}); err != nil {
		t.Errorf("a caller holding both scopes: %v", err)
	}
}

func TestDeclaringAgainMakesAVersionRatherThanReplacing(t *testing.T) {
	store, collection := declared(t, 29)
	fill(t, collection)

	first := declareOp(t, store, byAuthor())
	if first.Version != 1 {
		t.Fatalf("the first version is %d", first.Version)
	}

	changed := byAuthor()
	changed.Limit = 1
	second := declareOp(t, store, changed)
	if second.Version != 2 {
		t.Fatalf("the second version is %d", second.Version)
	}

	// The newest is what runs by default.
	if result := invoke(t, store, "articles.by_author", map[string]any{"author": "ann"}); result.Count != 1 {
		t.Errorf("the default version returned %d rows", result.Count)
	}

	// And the old one is still there to run, and to read: an audit record that
	// names a version has to mean something afterwards.
	result, err := store.Invoke(Caller{}, "articles.by_author", 1, map[string]any{"author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 3 || result.Version != 1 {
		t.Errorf("version 1 returned %d rows at version %d", result.Count, result.Version)
	}

	if _, _, err := store.Operation("articles.by_author", 9); !errors.Is(err, ErrNoOperation) {
		t.Errorf("want ErrNoOperation, got %v", err)
	}

	all, err := store.Operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Version != 2 {
		t.Errorf("the listing is %v", all)
	}
}

func TestDeclarationsSurviveARestart(t *testing.T) {
	disk, store := fresh(t, 30)
	collection, err := store.Declare(articles())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, collection)
	declareOp(t, store, byAuthor())
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(30, vfs.Faults{})
	restart.Restore(disk.Durable())
	pages, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}

	result, err := reopened.Invoke(Caller{}, "articles.by_author", 0, map[string]any{"author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 3 {
		t.Errorf("after a restart the operation returned %d rows", result.Count)
	}
}
