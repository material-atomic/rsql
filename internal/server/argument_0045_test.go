package server

import (
	"errors"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// TestABackwardsRangeReachesTheWireAsArgumentOrDeclaration is task 0045's
// wire-level measurement: the two Go sentinels (store.ErrDeclaration,
// store.ErrArgument) a backwards from/to produces both have entries in
// codeFor's table already (task 0044 and earlier), but nothing before this
// task exercised THIS shape through it. errors.Is alone would still pass if
// codeFor's table lost its entry for one of the two — checking both is what
// makes this a wire-contract test rather than a Go-internals test.
func TestABackwardsRangeReachesTheWireAsArgumentOrDeclaration(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	db, release, err := server.Store("acct", "backwards-db")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.Declare(store.Spec{
		Name: "people",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_name",
			Fields: []store.Field{{Path: "name", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Declare time: both ends constants, "z" after "a" — refused before the
	// operation exists at all, so there is no name to invoke.
	_, declareErr := db.DeclareOperation(store.Operation{
		Name: "people.backwards_constant", Collection: "people", Action: store.ActionScan, Index: "by_name", Limit: 10,
		From: &store.Endpoint{Terms: []store.Term{{Value: "z"}}},
		To:   &store.Endpoint{Terms: []store.Term{{Value: "a"}}},
	})
	if !errors.Is(declareErr, store.ErrDeclaration) {
		t.Fatalf("errors.Is(declareErr, store.ErrDeclaration) is false for %v", declareErr)
	}
	if got := codeFor(declareErr); got != "declaration" {
		t.Errorf("codeFor(%v) = %q, want %q", declareErr, got, "declaration")
	}

	// Call time: the same shape reached through arguments.
	stored, err := db.DeclareOperation(store.Operation{
		Name: "people.range", Collection: "people", Action: store.ActionScan, Index: "by_name", Limit: 10,
		Input: []store.Parameter{
			{Name: "lo", Type: store.TypeString, Required: true},
			{Name: "hi", Type: store.TypeString, Required: true},
		},
		From: &store.Endpoint{Terms: []store.Term{{Arg: "lo"}}},
		To:   &store.Endpoint{Terms: []store.Term{{Arg: "hi"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, invokeErr := db.Invoke(store.Caller{}, stored.Name, stored.Version, map[string]any{"lo": "z", "hi": "a"})
	if !errors.Is(invokeErr, store.ErrArgument) {
		t.Fatalf("errors.Is(invokeErr, store.ErrArgument) is false for %v", invokeErr)
	}
	if got := codeFor(invokeErr); got != "argument" {
		t.Errorf("codeFor(%v) = %q, want %q", invokeErr, got, "argument")
	}
}
