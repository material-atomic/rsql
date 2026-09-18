package cli

import (
	"net"
	"strings"
	"testing"

	"github.com/material-atomic/rsql/internal/server"
	"github.com/material-atomic/rsql/internal/store"
)

// TestTheShellAgainstARealServer is the one test here that is not about this
// package agreeing with itself.
//
// The shell, the client, the frames, the server and the engine each have tests
// that pass while the two sides of a boundary disagree — that has happened
// three times in this repository already, and every time it took two processes
// to see it. This runs the whole path: a signed connection this tool made for
// itself, the challenge answered, an access typed as a line, and the rows and
// the draft coming back from a database on the other side of a socket.
func TestTheShellAgainstARealServer(t *testing.T) {
	secret := "the-secret-this-server-was-started-with"

	live, err := server.New(server.Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })

	// Something to look at, declared the way `rsql apply` would.
	db, release, err := live.Store("acme", "books")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Spec{
		Name: "books",
		Key:  store.Key{Path: "id", Type: store.TypeString},
		Indexes: []store.Index{{
			Name:   "by_shelf",
			Fields: []store.Field{{Path: "shelf", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	books, err := db.Collection("books")
	if err != nil {
		t.Fatal(err)
	}
	for _, book := range []map[string]any{
		{"id": "b1", "shelf": "history", "title": "the first"},
		{"id": "b2", "shelf": "poetry", "title": "the second"},
	} {
		if _, err := books.Put(book); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
	release()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = live.Serve(listener) }()

	client, err := connect(options{secret: secret, account: "acme", db: "books"}, listener.Addr().String(), true)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	out := &strings.Builder{}
	typing := strings.NewReader(strings.Join([]string{
		`ls`,
		`get books "b1"`,
		`scan books by_shelf from "history" to "history"`,
		`declare books.on_a_shelf`,
		`exit`,
	}, "\n") + "\n")

	if err := Shell(client, "books", typing, out); err != nil {
		t.Fatal(err)
	}
	printed := out.String()

	for _, wanted := range []string{
		"collection books",
		"index by_shelf",
		`"title":"the first"`,
		`"name": "books.on_a_shelf"`,
		`"index": "by_shelf"`,
	} {
		if !strings.Contains(printed, wanted) {
			t.Errorf("the session did not contain %q:\n%s", wanted, printed)
		}
	}
	// The poetry book is outside the bounds that were typed. A shell that
	// returned it would mean the bounds never left this process.
	if strings.Contains(printed, "the second") {
		t.Errorf("the scan came back with a book outside its bounds:\n%s", printed)
	}

	// And what the operator did is in the database's own log, under a name.
	db, release, err = live.Store("acme", "books")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	reads := 0
	if err := db.Changes(1, func(change store.Change) bool {
		if change.Kind == store.ChangeRead && change.By.Actor == "acme (operator)" {
			reads++
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	// The catalogue twice — once when the shell opened, which is how it knows
	// what to offer when somebody presses tab, and once for `ls` — then the
	// get and the scan. `declare` reads nothing: it prints what the server
	// already sent back.
	//
	// Opening a shell therefore leaves a line in the log saying somebody
	// opened a shell, before they have typed anything. That is not an
	// accident worth removing.
	if reads != 4 {
		t.Errorf("the log holds %d operator reads, want 4", reads)
	}
}
