package cli

import (
	"net"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// TestAnOperatorWhoTypesTheEndsBackwardsSeesOneLineNotAGoError is task 0045's
// mục 3.4 fourth caller of stretch(): Explore, which is what an operator
// typing "from ... to ..." at the shell runs underneath. The same refusal
// that DeclareOperation and Invoke get must reach here too, and it must
// arrive the way every other shell mistake does — one line the operator can
// read and try again from, not a stack trace and not the session ending.
//
// Modelled directly on TestTheShellAgainstARealServer (shell_live_test.go):
// a real store, a real listener, a real signed connection, and a line typed
// exactly as an operator would type it.
func TestAnOperatorWhoTypesTheEndsBackwardsSeesOneLineNotAGoError(t *testing.T) {
	secret := "the-secret-this-server-was-started-with"

	live, err := server.New(server.Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })

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
	// "poetry" sorts after "history", so writing them this way round is
	// exactly the mistake task 0045 exists to stop reading as an empty shelf.
	// "ls" runs before it and "exit" after: if the backwards scan aborted the
	// session instead of just failing its own command, Shell would return
	// before reaching "exit" and the "collection books" line from "ls" — the
	// t.Fatalf below and the strings.Contains check on "ls"'s output are what
	// catch that, not anything "exit" itself prints (it prints nothing; see
	// shell.go's "exit" case).
	typing := strings.NewReader(strings.Join([]string{
		`ls`,
		`scan books by_shelf from "poetry" to "history"`,
		`exit`,
	}, "\n") + "\n")

	if err := Shell(client, "books", typing, out); err != nil {
		t.Fatalf("the shell session itself ended in error, rather than the one command failing: %v", err)
	}
	printed := out.String()

	if !strings.Contains(printed, "the from bound sorts after the to bound") {
		t.Errorf("the shell did not print a readable explanation of the backwards scan:\n%s", printed)
	}
	if !strings.Contains(printed, "collection books") {
		t.Errorf("ls, typed before the bad scan, did not run:\n%s", printed)
	}
	// The session kept going after the mistake: a stack trace or a session
	// that quit would mean the shell is treating this as a Go error instead
	// of an operator's error to read and correct.
	if strings.Contains(printed, "goroutine ") || strings.Contains(printed, "panic:") {
		t.Errorf("the shell printed something that looks like a raw Go crash, not an operator-readable line:\n%s", printed)
	}
}
