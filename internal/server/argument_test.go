package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// widgetsScanOps declares a collection whose one index is on a field typed
// "any" — the shape task 0044 is about. matches(TypeAny, x) accepts every Go
// value the JSON decoder can produce, including a slice or a map, and
// keys.Encode refuses exactly those. An argument typed "any" reaches the same
// door from the call side: bind() (invoke.go) and check() (ops.go, at
// declaration time) both let it through no matter what the field it is bound
// against declares — even the primary key, whose own type is fixed to string
// or number and would otherwise never see a slice.
//
// Six operations: the clustered index or the declared one, crossed with which
// end of the range carries the caller's value — From, To, or both — since
// bound() is called once per end and a mutation could fix only one call.
func widgetsScanOps(t *testing.T, server *Server, account, name string) {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.Declare(store.Spec{
		Name: "widgets",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_tags",
			Fields: []store.Field{{Path: "tags", Type: store.TypeAny, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	arg := store.Endpoint{Terms: []store.Term{{Arg: "t"}}}
	for _, one := range []struct {
		name  string
		index string
		from  *store.Endpoint
		to    *store.Endpoint
	}{
		{"widgets.clustered_both", store.ClusteredIndex, &arg, &arg},
		{"widgets.clustered_from", store.ClusteredIndex, &arg, nil},
		{"widgets.clustered_to", store.ClusteredIndex, nil, &arg},
		{"widgets.tags_both", "by_tags", &arg, &arg},
		{"widgets.tags_from", "by_tags", &arg, nil},
		{"widgets.tags_to", "by_tags", nil, &arg},
	} {
		operation := store.Operation{
			Name: one.name, Collection: "widgets", Action: store.ActionScan, Index: one.index,
			Input: []store.Parameter{{Name: "t", Type: store.TypeAny, Required: true}},
			From:  one.from, To: one.to, Limit: 10,
		}
		if _, err := db.DeclareOperation(operation); err != nil {
			t.Fatalf("declare %q: %v", one.name, err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestNoCallerBoundEverComesBackFailed is task 0044's central claim, measured
// on the wire the operation-invoking code actually runs, not merely reasoned
// about: a value the caller (or an earlier batch step) supplied, of a type
// the declaration allowed ("any"), that keys cannot turn into bytes, must
// reach the client as code=argument — a mistake it can act on — never as
// code=failed, which means "we do not know what happened" for a mistake that
// is, in fact, known exactly.
//
// The table is every combination task 0044 §3 asks for: six value shapes
// (five keys.Encode refuses, plus a plain string as the counter-example row
// that this table must NOT flag) times six operations (clustered index or a
// declared one, crossed with which end carries the value). Both
// errors.Is(err, store.ErrArgument) and codeFor(err) == "argument" are
// checked on every failing case — errors.Is alone would still pass if bound()
// wrapped the error in ErrArgument but codeFor's table had no entry for it,
// which is exactly the gap that let this bug reach a client as "failed" in
// the first place: errors.Is proves the Go value is right, not that the byte
// a client switches on is.
func TestNoCallerBoundEverComesBackFailed(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	widgetsScanOps(t, server, "acct", "widgets-db")

	db, release, err := server.Store("acct", "widgets-db")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	shapes := []struct {
		name    string
		value   any
		wantErr bool
	}{
		{"array", []any{"a", "b"}, true},
		{"object", map[string]any{"k": "v"}, true},
		{"empty array", []any{}, true},
		{"empty object", map[string]any{}, true},
		{"nested array", []any{[]any{"x"}}, true},
		{"a plain string — the control row, must not be flagged", "solo", false},
	}

	for _, op := range []string{
		"widgets.clustered_both", "widgets.clustered_from", "widgets.clustered_to",
		"widgets.tags_both", "widgets.tags_from", "widgets.tags_to",
	} {
		for _, shape := range shapes {
			t.Run(op+"/"+shape.name, func(t *testing.T) {
				_, err := db.Invoke(store.Caller{}, op, 0, map[string]any{"t": shape.value})

				if !shape.wantErr {
					if err != nil {
						t.Fatalf("a value keys.Encode accepts still failed: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatal("want an error, got none")
				}
				if !errors.Is(err, store.ErrArgument) {
					t.Errorf("errors.Is(err, store.ErrArgument) is false for %v", err)
				}
				if got := codeFor(err); got != "argument" {
					t.Errorf("codeFor(%v) = %q, want %q", err, got, "argument")
				}
			})
		}
	}
}

// TestAPutWithAnUnencodableAnyFieldFailsAsTypeNotFailed exercises 1.1's other
// reachable spot: entriesForIndex (collection.go) builds an index entry for
// every declared index on every write, and a field declared "any" accepts a
// slice or a map exactly the way a scan's argument does. Before this task
// that value reached keys.Encode unwrapped, and the resulting error had no
// entry in codeFor's table — code=failed for a document whose shape is
// entirely known: it does not fit an index built for something else.
//
// This is document data, not a caller's argument list, so the wrap is
// ErrType — the same sentinel a document already fails on a few lines above
// in collection.go, when a field typed more narrowly holds the wrong Go type
// — never ErrArgument, which is scan.go's bound() wrapping for the
// equivalent failure on a scan's bound values.
func TestAPutWithAnUnencodableAnyFieldFailsAsTypeNotFailed(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	widgetsScanOps(t, server, "acct", "widgets-write")

	db, release, err := server.Store("acct", "widgets-write")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	widgets, err := db.Collection("widgets")
	if err != nil {
		t.Fatal(err)
	}

	// The scalar control row: the field being declared "any" is not itself
	// what fails, so an ordinary string must still write cleanly.
	if _, err := widgets.Put(map[string]any{"id": "w-ok", "tags": "solo"}); err != nil {
		t.Fatalf("a scalar value in an 'any' field must still write: %v", err)
	}

	_, err = widgets.Put(map[string]any{"id": "w-bad", "tags": []any{"a", "b"}})
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !errors.Is(err, store.ErrType) {
		t.Errorf("errors.Is(err, store.ErrType) is false for %v", err)
	}
	if got := codeFor(err); got != "type" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "type")
	}
	if errors.Is(err, store.ErrArgument) {
		t.Error("a document's own field is not the caller's argument list, so this must not also be ErrArgument")
	}
}

// TestClusteredBoundNamesThePrimaryKeyField closes a gap the axis-crossing
// table above cannot see by itself. walkRange (scan.go) builds its own Field
// for the clustered index — the primary key's own Path and Type — separately
// from Scan(), which simply reuses the declared index's Fields. bound() wraps
// in ErrArgument regardless of what Path that Field carries, so a version of
// walkRange that built an empty Field{} there (dropping Path and Type) would
// still pass every errors.Is / codeFor check in
// TestNoCallerBoundEverComesBackFailed — measured while writing this task: it
// does, silently, on both the "_key" axis and every other assertion in this
// file. Only a check on the message's content, naming the actual key field,
// tells the two apart.
func TestClusteredBoundNamesThePrimaryKeyField(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	widgetsScanOps(t, server, "acct", "widgets-clustered-name")

	db, release, err := server.Store("acct", "widgets-clustered-name")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = db.Invoke(store.Caller{}, "widgets.clustered_from", 0, map[string]any{"t": []any{"a", "b"}})
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), `"id"`) {
		t.Errorf("bound error on the clustered index does not name the primary key field (%q): %v", "id", err)
	}
}
