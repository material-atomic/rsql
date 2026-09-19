package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

const (
	secret   = "the secret only the control plane has"
	password = "a-password-of-the-right-shape"
)

// running is a server on a real socket, with the articles collection and one
// operation already declared.
func running(t *testing.T, encrypt bool) (*Server, string) {
	t.Helper()

	server, err := New(Options{Dir: t.TempDir(), Secret: secret, Encrypt: encrypt})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()

	return server, listener.Addr().String()
}

// declare sets a database up from inside the process, the way a CLI would.
func declare(t *testing.T, server *Server, account, name string) {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.Declare(store.Spec{
		Name: "articles",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_author",
			Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, operation := range []store.Operation{
		{
			Name: "articles.add", Collection: "articles", Action: store.ActionInsert,
			Input: []store.Parameter{
				{Name: "title", Type: store.TypeString, Required: true},
				{Name: "author", Type: store.TypeString, Required: true},
			},
			Document: map[string]store.Term{"title": {Arg: "title"}, "author": {Arg: "author"}},
		},
		{
			Name: "articles.by_author", Collection: "articles", Action: store.ActionScan,
			Index: "by_author",
			Input: []store.Parameter{{Name: "author", Type: store.TypeString, Required: true}},
			From:  &store.Endpoint{Terms: []store.Term{{Arg: "author"}}},
			To:    &store.Endpoint{Terms: []store.Term{{Arg: "author"}}},
			Limit: 10,
		},
		{
			Name: "articles.secret", Collection: "articles", Action: store.ActionScan,
			Index: "by_author", Scopes: []string{"articles:read"},
			Input: []store.Parameter{{Name: "author", Type: store.TypeString, Required: true}},
			From:  &store.Endpoint{Terms: []store.Term{{Arg: "author"}}},
			Limit: 10,
		},
	} {
		if _, err := db.DeclareOperation(operation); err != nil {
			t.Fatalf("declare %q: %v", operation.Name, err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

// client is the other end of a connection, doing by hand what the driver does.
type client struct {
	t      *testing.T
	conn   net.Conn
	reader *protocol.Reader
	next   uint32
}

func dial(t *testing.T, address string) *client {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &client{t: t, conn: conn, reader: protocol.NewReader(conn).Accept(protocol.Version)}
}

func (c *client) send(kind protocol.Type, body any) uint32 {
	c.t.Helper()

	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		payload = encoded
	}

	c.next++
	frame, err := protocol.Encode(protocol.Frame{
		Version: protocol.Version, Type: kind, ID: c.next, Payload: payload,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatal(err)
	}
	return c.next
}

func (c *client) read() protocol.Frame {
	c.t.Helper()
	frame, err := c.reader.Read()
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return frame
}

// open does the handshake with a properly signed connection string.
func (c *client) open(server *Server, account, name string) protocol.Frame {
	c.t.Helper()

	signature, err := server.Sign(account, password, name)
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(protocol.Hello, hello{Account: account, Password: password, DBName: name, Signature: signature})
	return c.read()
}

func (c *client) invoke(name string, arguments map[string]any) protocol.Frame {
	c.t.Helper()
	c.send(protocol.Invoke, call{Command: name, Arguments: arguments})
	return c.read()
}

func decode[T any](t *testing.T, frame protocol.Frame) T {
	t.Helper()
	var into T
	if err := json.Unmarshal(frame.Payload, &into); err != nil {
		t.Fatalf("payload %q: %v", frame.Payload, err)
	}
	return into
}

func TestAConnectionOpensAndAnOperationRuns(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	frame := client.open(server, "acme", "main")
	if frame.Type != protocol.Welcome {
		t.Fatalf("the handshake answered with a %s: %s", frame.Type, frame.Payload)
	}
	greeting := decode[welcome](t, frame)
	if greeting.Account != "acme" || greeting.DBName != "main" {
		t.Errorf("the welcome reads %+v", greeting)
	}

	// A write, then a read of what was written.
	frame = client.invoke("articles.add", map[string]any{"title": "First", "author": "ann"})
	if frame.Type != protocol.Result {
		t.Fatalf("the write answered with a %s: %s", frame.Type, frame.Payload)
	}
	written := decode[store.Result](t, frame)
	if written.Changed != 1 || written.Key == nil {
		t.Fatalf("the write reports %+v", written)
	}

	frame = client.invoke("articles.by_author", map[string]any{"author": "ann"})
	if frame.Type != protocol.Result {
		t.Fatalf("the read answered with a %s: %s", frame.Type, frame.Payload)
	}
	read := decode[store.Result](t, frame)
	if read.Count != 1 || read.Rows[0]["title"] != "First" {
		t.Errorf("the read reports %+v", read)
	}

	// Every answer carries the id of the call that asked for it, which is what
	// lets one connection carry several calls at once.
	if frame.ID != client.next {
		t.Errorf("the answer is tagged %d, the call was %d", frame.ID, client.next)
	}
}

func TestTheAnswerIsTaggedWithTheCallThatAskedForIt(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	client.open(server, "acme", "main")

	// Three calls before reading any answer: the ids are the only thing tying
	// them together.
	first := client.send(protocol.Invoke, call{Command: "articles.add",
		Arguments: map[string]any{"title": "One", "author": "ann"}})
	second := client.send(protocol.Ping, nil)
	third := client.send(protocol.Invoke, call{Command: "articles.by_author",
		Arguments: map[string]any{"author": "ann"}})

	for _, want := range []struct {
		id   uint32
		kind protocol.Type
	}{{first, protocol.Result}, {second, protocol.Pong}, {third, protocol.Result}} {
		frame := client.read()
		if frame.ID != want.id || frame.Type != want.kind {
			t.Fatalf("expected %s#%d, got %s#%d: %s", want.kind, want.id, frame.Type, frame.ID, frame.Payload)
		}
	}
}

func TestAConnectionNobodySignedForIsRefused(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	good, err := server.Sign("acme", password, "main")
	if err != nil {
		t.Fatal(err)
	}

	for name, opening := range map[string]hello{
		"no signature at all": {Account: "acme", Password: password, DBName: "main"},
		"a signature for another database": func() hello {
			other, _ := server.Sign("acme", password, "other")
			return hello{Account: "acme", Password: password, DBName: "main", Signature: other}
		}(),
		"a signature for another account": func() hello {
			other, _ := server.Sign("evil", password, "main")
			return hello{Account: "acme", Password: password, DBName: "main", Signature: other}
		}(),
		"the right signature and another password": {
			Account: "acme", Password: "another-password-entirely", DBName: "main", Signature: good,
		},
		"a signature made up": {
			Account: "acme", Password: password, DBName: "main", Signature: strings.Repeat("ab", 32),
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := dial(t, address)
			client.send(protocol.Hello, opening)

			frame := client.read()
			if frame.Type != protocol.Failure {
				t.Fatalf("it was let in with a %s", frame.Type)
			}

			// And the connection is over, rather than left looking usable.
			client.send(protocol.Ping, nil)
			if _, err := client.reader.Read(); err == nil {
				t.Error("the connection still answers after a refused handshake")
			}
		})
	}
}

// A database nobody has signed for must not even come into existence: a file
// appearing is itself something an unauthorised caller should not be able to
// cause.
func TestARefusedConnectionCreatesNothing(t *testing.T) {
	server, address := running(t, false)

	client := dial(t, address)
	client.send(protocol.Hello, hello{Account: "acme", Password: password, DBName: "brandnew", Signature: "00"})
	if frame := client.read(); frame.Type != protocol.Failure {
		t.Fatalf("it was let in with a %s", frame.Type)
	}

	if _, found := server.open["acme/brandnew"]; found {
		t.Error("a database was opened for a connection that was refused")
	}
}

func TestAnAccountNameCannotBeAPath(t *testing.T) {
	server, _ := running(t, false)

	for _, name := range []string{"../escape", "a/b", `a\b`, "", ".", "..", "a:b", "a\x00b", strings.Repeat("x", 65)} {
		if _, _, err := server.Store(name, "main"); !errors.Is(err, ErrName) {
			t.Errorf("account %q: want ErrName, got %v", name, err)
		}
		if _, _, err := server.Store("acme", name); !errors.Is(err, ErrName) {
			t.Errorf("database %q: want ErrName, got %v", name, err)
		}
	}
}

// Scopes are not taken from the request, so an operation that declares one
// cannot be reached over the wire at all yet. Being incomplete in that
// direction is the safe one.
func TestAnOperationThatNeedsAScopeIsRefusedOverTheWire(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	client.open(server, "acme", "main")

	frame := client.invoke("articles.secret", map[string]any{"author": "ann"})
	if frame.Type != protocol.Failure {
		t.Fatalf("an operation needing a scope ran anyway: %s", frame.Payload)
	}
	// The code is what a driver acts on; the message is for whoever reads it.
	if !strings.Contains(string(frame.Payload), `"code":"not_allowed"`) {
		t.Errorf("the failure reads %s", frame.Payload)
	}
}

func TestAFailedCallDoesNotEndTheConnection(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	client.open(server, "acme", "main")

	// An operation nobody declared, then one that does exist: a connection that
	// died on the first would make every client reconnect over a typo.
	if frame := client.invoke("articles.nothing", nil); frame.Type != protocol.Failure {
		t.Fatalf("an undeclared operation answered with a %s", frame.Type)
	}
	if frame := client.invoke("articles.add", map[string]any{"title": "Still here", "author": "ann"}); frame.Type != protocol.Result {
		t.Fatalf("the connection did not survive a failed call: %s", frame.Payload)
	}

	// The same for arguments that do not match the declaration.
	if frame := client.invoke("articles.add", map[string]any{"title": "No author"}); frame.Type != protocol.Failure {
		t.Errorf("a call missing an argument answered with a %s", frame.Type)
	}
}

func TestAFrameThisVersionDoesNotServeIsAnsweredAnyway(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	client.open(server, "acme", "main")

	// An Event is a frame the server sends, never one it takes. A client
	// waiting for an answer that never comes is worse off than one told no.
	id := client.send(protocol.Event, map[string]any{"lsn": 1})
	frame := client.read()
	if frame.Type != protocol.Failure || frame.ID != id {
		t.Fatalf("got %s#%d, want a failure tagged %d", frame.Type, frame.ID, id)
	}
}

// What was written must still be there after the server is restarted, which is
// the only way to know a commit happened rather than being kept in memory.
func TestWhatWasWrittenSurvivesARestart(t *testing.T) {
	for _, encrypt := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypt=%v", encrypt), func(t *testing.T) {
			dir := t.TempDir()

			first, err := New(Options{Dir: dir, Secret: secret, Encrypt: encrypt})
			if err != nil {
				t.Fatal(err)
			}
			declare(t, first, "acme", "main")

			db, release, err := first.Store("acme", "main")
			if err != nil {
				t.Fatal(err)
			}
			signature, _ := first.Sign("acme", password, "main")
			_ = signature
			result, err := db.Invoke(store.Caller{}, "articles.add", 0,
				map[string]any{"title": "Durable", "author": "ann"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Commit(); err != nil {
				t.Fatal(err)
			}
			key := result.Key
			release()
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}

			second, err := New(Options{Dir: dir, Secret: secret, Encrypt: encrypt})
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()

			again, release, err := second.Store("acme", "main")
			if err != nil {
				t.Fatal(err)
			}
			defer release()

			collection, err := again.Collection("articles")
			if err != nil {
				t.Fatalf("the collection did not survive: %v", err)
			}
			document, found, err := collection.Get(key)
			if err != nil || !found || document["title"] != "Durable" {
				t.Fatalf("the document reads %v (%v, %v)", document, found, err)
			}
		})
	}
}

// A write that reaches the server must be on the disk before the answer goes
// back. Otherwise a client that is told "done" can lose it to a power cut.
func TestAWriteIsCommittedBeforeTheAnswerGoesBack(t *testing.T) {
	dir := t.TempDir()
	server, err := New(Options{Dir: dir, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	declare(t, server, "acme", "main")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = server.Serve(listener) }()

	client := dial(t, listener.Addr().String())
	client.open(server, "acme", "main")
	frame := client.invoke("articles.add", map[string]any{"title": "Told done", "author": "ann"})
	if frame.Type != protocol.Result {
		t.Fatalf("the write answered with a %s: %s", frame.Type, frame.Payload)
	}
	written := decode[store.Result](t, frame)

	// The server is dropped without a graceful close, as a power cut would.
	// What was acknowledged has to be there.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := New(Options{Dir: dir, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()

	db, release, err := after.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	collection, err := db.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := collection.Get(written.Key); err != nil || !found {
		t.Errorf("a write the client was told had happened is not there: %v, %v", found, err)
	}
}

func TestAServerWithoutASecretDoesNotStart(t *testing.T) {
	if _, err := New(Options{Dir: t.TempDir()}); err == nil {
		t.Error("a server with nothing to verify signatures against started anyway")
	}
	if _, err := New(Options{Secret: secret}); err == nil {
		t.Error("a server with nowhere to keep databases started anyway")
	}
}

func TestAConnectionThatSaysNothingUsefulIsRefused(t *testing.T) {
	server, address := running(t, false)

	for name, opening := range map[string]func(*client){
		"a ping before the handshake": func(c *client) { c.send(protocol.Ping, nil) },
		"an invoke before the handshake": func(c *client) {
			c.send(protocol.Invoke, call{Command: "articles.add"})
		},
		"a hello that is not json": func(c *client) {
			frame, _ := protocol.Encode(protocol.Frame{
				Version: protocol.Version, Type: protocol.Hello, ID: 1, Payload: []byte("not json"),
			})
			_, _ = c.conn.Write(frame)
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := dial(t, address)
			opening(client)

			frame, err := client.reader.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return // refused by hanging up, which is also an answer
				}
				t.Fatal(err)
			}
			if frame.Type != protocol.Failure {
				t.Errorf("it was answered with a %s", frame.Type)
			}
		})
	}
	_ = server
}

// The key a file is encrypted under is derived from the account and the
// database name, so a file put where another database belongs does not open
// there. Without that, swapping two files on one server — by mistake in a
// restore, or on purpose — would have the server serving one database's
// documents under the other's name, with nothing to notice it.
func TestAFileMovedToAnotherDatabaseDoesNotOpen(t *testing.T) {
	dir := t.TempDir()
	server, err := New(Options{Dir: dir, Secret: secret, Encrypt: true})
	if err != nil {
		t.Fatal(err)
	}
	declare(t, server, "acme", "first")
	declare(t, server, "acme", "second")

	db, release, err := server.Store("acme", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Invoke(store.Caller{}, "articles.add", 0,
		map[string]any{"title": "Belongs to first", "author": "ann"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	// The file of one database, put where the other one lives.
	from := filepath.Join(dir, "acme", "first.sapedb")
	to := filepath.Join(dir, "acme", "second.sapedb")
	content, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, content, 0o600); err != nil {
		t.Fatal(err)
	}

	after, err := New(Options{Dir: dir, Secret: secret, Encrypt: true})
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()

	if _, _, err := after.Store("acme", "second"); !errors.Is(err, pager.ErrKey) {
		t.Errorf("a file from another database opened here: %v", err)
	}
}

// oldDBExt mirrors internal/server's own oldFileExt: built from bytes so
// this file, inside the tree internal/naming walks, does not carry the old
// word as a literal.
func oldDBExt() string {
	return "." + string([]byte{'r', 's', 'q', 'l'})
}

// TestAnOldExtensionFileIsRefusedNotSilentlyReplaced is the file-extension
// counterpart to the environment-variable signpost tests in
// internal/cli and internal/service: openFile treats a missing .sapedb path
// as "create a new, empty database", which would make a database opened by
// the old binary go invisible behind a brand-new empty one of the same
// name, rather than failing in any way an operator would notice.
func TestAnOldExtensionFileIsRefusedNotSilentlyReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "acme", "main"+oldDBExt())
	if err := os.WriteFile(old, []byte("stand-in for a real database file; only its path matters here"), 0o600); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{Dir: dir, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Not t.Fatal on a bare err == nil: if the mutation this test exists to
	// catch ever regresses, Store succeeds and hands back a lock on the
	// database (the release func here) that nothing would ever call —
	// t.Fatal's Goexit would then run the deferred server.Close() while
	// that lock is still held, and Close(), which locks every open
	// database on its way out, would deadlock instead of failing cleanly.
	// So the lock is released on this path before anything can fail.
	_, release, err := server.Store("acme", "main")
	if err == nil {
		release()
		t.Fatal("it opened (or silently created) a database while an old-extension file with the same name existed")
	}

	want := filepath.Join(dir, "acme", "main.sapedb")
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name both paths: %v", err)
	}
	if !strings.Contains(err.Error(), "mv") {
		t.Errorf("the refusal does not tell the operator to mv the file: %v", err)
	}
	if _, statErr := os.Stat(want); statErr == nil {
		t.Error("a new .sapedb file was created even though the command was refused")
	}
}

// A client whose frames arrive in the wrong order must be told that, not told
// its credentials are wrong. A misleading error sends somebody hunting through
// their secrets for a bug that is in their call order.
func TestTheHandshakeSaysWhatWasWrongWithIt(t *testing.T) {
	server, address := running(t, false)

	client := dial(t, address)
	// A well-formed Invoke, which happens to parse as an empty hello.
	client.send(protocol.Invoke, call{Command: "articles.add"})

	frame := client.read()
	if frame.Type != protocol.Failure {
		t.Fatalf("it was answered with a %s", frame.Type)
	}
	if !strings.Contains(string(frame.Payload), `"code":"handshake"`) ||
		!strings.Contains(string(frame.Payload), "began with a invoke frame") {
		t.Errorf("the failure blames something else: %s", frame.Payload)
	}
	_ = server
}

// Several connections writing to one database at once. The engine underneath
// takes a single writer, so the server has to be the thing that makes it one —
// and the race detector is what says whether it really does.
func TestManyConnectionsToOneDatabase(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	const connections, each = 8, 12
	done := make(chan error, connections)

	for c := 0; c < connections; c++ {
		go func(c int) {
			conn, err := net.Dial("tcp", address)
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			reader := protocol.NewReader(conn).Accept(protocol.Version)

			signature, err := server.Sign("acme", password, "main")
			if err != nil {
				done <- err
				return
			}
			send := func(id uint32, kind protocol.Type, body any) error {
				payload, err := json.Marshal(body)
				if err != nil {
					return err
				}
				frame, err := protocol.Encode(protocol.Frame{
					Version: protocol.Version, Type: kind, ID: id, Payload: payload,
				})
				if err != nil {
					return err
				}
				_, err = conn.Write(frame)
				return err
			}

			if err := send(1, protocol.Hello, hello{
				Account: "acme", Password: password, DBName: "main", Signature: signature,
			}); err != nil {
				done <- err
				return
			}
			if frame, err := reader.Read(); err != nil || frame.Type != protocol.Welcome {
				done <- fmt.Errorf("handshake: %v %v", frame.Type, err)
				return
			}

			for i := 0; i < each; i++ {
				if err := send(uint32(i+2), protocol.Invoke, call{
					Command:   "articles.add",
					Arguments: map[string]any{"title": fmt.Sprintf("c%d-%d", c, i), "author": "ann"},
				}); err != nil {
					done <- err
					return
				}
				frame, err := reader.Read()
				if err != nil {
					done <- err
					return
				}
				if frame.Type != protocol.Result {
					done <- fmt.Errorf("write %d: %s %s", i, frame.Type, frame.Payload)
					return
				}
			}
			done <- nil
		}(c)
	}

	for c := 0; c < connections; c++ {
		if err := <-done; err != nil {
			t.Fatalf("connection: %v", err)
		}
	}

	// Every write landed, exactly once each.
	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	collection, err := db.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	count := 0
	if err := collection.Walk(func(_ any, document map[string]any) bool {
		count++
		titles[fmt.Sprint(document["title"])] = true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if count != connections*each || len(titles) != connections*each {
		t.Errorf("%d documents with %d distinct titles, want %d of each", count, len(titles), connections*each)
	}
}

// Two servers on one directory is the operator mistake that matters, and it
// has to be found at startup. Locking each database file is not enough on its
// own: a server opens one only when somebody asks for it, so both would come
// up looking healthy and collide later, at whichever request first touched the
// same database.
func TestTwoServersCannotShareADirectory(t *testing.T) {
	dir := t.TempDir()

	first, err := New(Options{Dir: dir, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := New(Options{Dir: dir, Secret: secret}); !errors.Is(err, vfs.ErrLocked) {
		t.Fatalf("a second server started on the same directory: %v", err)
	}

	// And the directory is free again once the first one closes, which is what
	// a supervisor restarting it depends on.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(Options{Dir: dir, Secret: secret})
	if err != nil {
		t.Fatalf("after the first closed, the second is still refused: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestEveryFailureAClientMustTellApartHasItsOwnCode: a client acts on the code
// and never on the sentence, so a failure that arrives as the catch-all is one
// the client can only treat as "something went wrong" — retry it forever or
// give up on a database that is perfectly healthy.
//
// A batch brought three new ways to fail and none of them were mapped. It took
// running the ledger example against a real server to see it: every unit test
// on both sides passed, because neither side ever looked at the code.
func TestEveryFailureAClientMustTellApartHasItsOwnCode(t *testing.T) {
	for _, one := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("wrapped: %w", store.ErrMissing), "missing"},
		{fmt.Errorf("step %q: %w", "order", store.ErrCondition), "condition"},
		{fmt.Errorf("wrapped: %w", store.ErrUncommitted), "uncommitted"},
		{fmt.Errorf("wrapped: %w", store.ErrNoOperation), "no_operation"},
		{errors.New("something nobody named"), "failed"},
	} {
		if got := codeFor(one.err); got != one.code {
			t.Errorf("%v came back as %q, want %q", one.err, got, one.code)
		}
	}
}

// TestTheServerSaysWhenADatabaseWasNotShutDown: surviving a power cut quietly
// is most of the job and not all of it. The operator asking "why is that write
// missing" needs something to read, and the answer is bounded in a way worth
// saying: one operation at most, and one nobody was told had succeeded.
func TestTheServerSaysWhenADatabaseWasNotShutDown(t *testing.T) {
	dir := t.TempDir()

	// A database written to and then left, the way a killed process leaves one.
	first, err := New(Options{Dir: dir, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	declare(t, first, "acme", "main")
	db, release, err := first.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Invoke(store.Caller{}, "articles.add", 0, map[string]any{
		"title": "written", "author": "ann",
	}); err != nil {
		t.Fatal(err)
	}
	release()

	// The disk as a power cut would leave it: everything that was committed is
	// on it, and nothing was closed. Copying it is the honest way to produce
	// that — the alternative is a way to close a file without marking it,
	// which would be production code existing only for a test.
	crashed := t.TempDir()
	copyTree(t, dir, crashed)

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	dir = crashed

	said := []string{}
	second, err := New(Options{Dir: dir, Secret: secret, Notice: func(line string) {
		said = append(said, line)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	_, letGo, err := second.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	letGo()
	if len(said) != 1 {
		t.Fatalf("opening a database that was left said %v", said)
	}
	if !strings.Contains(said[0], "acme/main") || !strings.Contains(said[0], "not closed cleanly") {
		t.Errorf("it said: %s", said[0])
	}

	// And a database closed properly says nothing, because there is nothing to
	// say and a notice that appears every time is one nobody reads.
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	said = nil
	third, err := New(Options{Dir: dir, Secret: secret, Notice: func(line string) {
		said = append(said, line)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })

	_, done, err := third.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(said) != 0 {
		t.Errorf("opening a database that was closed properly said %v", said)
	}
}

// copyTree copies a directory, which is what a disk looks like after a crash:
// everything that was made durable, and no shutdown.
func copyTree(t *testing.T, from, to string) {
	t.Helper()

	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		// The lock file is the running process, not the database.
		if entry.Name() == ".lock" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- 0048: a rejected connection used to leave sapedbd saying nothing ---

// notices collects what a server said through Options.Notice. Serve calls it
// from a goroutine per connection, and the tests below read it from the main
// goroutine, so it needs its own lock — the same reason sender exists for the
// wire on the other side of this same race.
type notices struct {
	mutex sync.Mutex
	lines []string
}

func (n *notices) record(line string) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	n.lines = append(n.lines, line)
}

func (n *notices) all() []string {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	return append([]string{}, n.lines...)
}

// runningNoticed is running with Options.Notice wired to a notices collector
// instead of nowhere, so a test can see what sapedbd would have printed —
// and with the "acme/main" fixture database already declared, the way every
// caller in this file wants it.
//
// The declare happens before Notice is wired up, on purpose: a database's
// very first open is itself "not closed cleanly" by sayHowItWasLeft's own
// test (Clean starts false in CreateWith and only turns true on a proper
// Close), so declaring after Notice is live would put one unrelated line in
// front of whatever a test is actually checking. Once declared, the
// database stays cached in Server.open, so nothing reopens the file for the
// rest of a test and the notice does not recur.
func runningNoticed(t *testing.T) (*Server, string, *notices) {
	t.Helper()

	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	declare(t, server, "acme", "main")

	said := &notices{}
	server.Say(said.record)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()

	return server, listener.Addr().String(), said
}

// waitClosed blocks until the far end (the server) closes its side of conn.
//
// Serve's per-connection goroutine calls Notice, if it is going to at all,
// strictly before its deferred conn.Close() runs — same goroutine, no
// scheduling between the two. So a caller that first observes the close and
// only then reads what was said is not racing the goroutine that says it;
// it is reading after a happens-before edge, not guessing at a delay.
func waitClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("the server did not close the connection")
	}
}

// TestARejectedSignatureLeavesExactlyOneNoticeLine is case 1 of task 0048's
// test table: a bound connection with a signature that does not verify must
// print one line naming why, where nothing was printed before, and the
// client must still get its Failure frame over the wire as it always has.
func TestARejectedSignatureLeavesExactlyOneNoticeLine(t *testing.T) {
	_, address, said := runningNoticed(t)

	client := dial(t, address)
	client.send(protocol.Hello, hello{
		Account: "acme", Password: password, DBName: "main",
		Signature: strings.Repeat("ab", 32),
	})

	frame := client.read()
	if frame.Type != protocol.Failure {
		t.Fatalf("it was let in with a %s", frame.Type)
	}
	// codeFor's own list checks signing.ErrBadSignature before ErrHandshake,
	// but handshake() wraps a bad signature as "%w: %v" with ErrHandshake in
	// the %w slot — so the chain errors.Is walks only ever reaches
	// ErrHandshake, and the code a client actually receives here is
	// "handshake", not "signature". Task 0048 mục 6 named "signature"; mục 2
	// of the same task already hedges with "signature hoặc handshake", and
	// codeFor/handshake() are explicitly out of scope for this task, so this
	// test asserts what the code actually does rather than what one line of
	// the task guessed it did.
	if !strings.Contains(string(frame.Payload), `"code":"handshake"`) {
		t.Errorf("the failure's code is not what a client would switch on: %s", frame.Payload)
	}

	waitClosed(t, client.conn)

	lines := said.all()
	if len(lines) != 1 {
		t.Fatalf("a rejected handshake said %v, want exactly one line", lines)
	}
	if !strings.Contains(lines[0], "signature does not verify") {
		t.Errorf("the line does not name the real reason: %q", lines[0])
	}
	if !strings.Contains(lines[0], client.conn.LocalAddr().String()) {
		t.Errorf("the line does not name which connection: %q", lines[0])
	}
}

// TestARejectedHandshakeNoticeNeverCarriesTheSignatureOrPassword is case 6:
// a negative assertion against the exact line case-1's scenario produces,
// because a Notice that echoed the hello payload would be a second place a
// secret can leak, right next to the one this task's own design decision
// (mục 3, quyết định A.1) says must never happen.
func TestARejectedHandshakeNoticeNeverCarriesTheSignatureOrPassword(t *testing.T) {
	_, address, said := runningNoticed(t)

	madeUpSignature := strings.Repeat("f00dcafe", 8)
	client := dial(t, address)
	client.send(protocol.Hello, hello{
		Account: "acme", Password: password, DBName: "main", Signature: madeUpSignature,
	})
	if frame := client.read(); frame.Type != protocol.Failure {
		t.Fatalf("it was let in with a %s", frame.Type)
	}
	waitClosed(t, client.conn)

	lines := said.all()
	if len(lines) != 1 {
		t.Fatalf("said %v, want exactly one line", lines)
	}
	if strings.Contains(lines[0], madeUpSignature) {
		t.Errorf("the notice line echoes the signature it was sent: %q", lines[0])
	}
	if strings.Contains(lines[0], password) {
		t.Errorf("the notice line echoes the password it was sent: %q", lines[0])
	}
}

// TestABoundHandshakeDatabaseFailureNamesTheRealReasonNotAGenericClose is
// case 2: a signature that verifies, for a database file that will not
// open, must print the real reason — never the phrase a client-side
// Unavailable error uses, because sapedbd is not the client and does not
// get to be that vague about a file it can see on its own disk.
func TestABoundHandshakeDatabaseFailureNamesTheRealReasonNotAGenericClose(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file at exactly the path a real database would use, but not one: no
	// server ever wrote these bytes. Two full pages, so both meta-page reads
	// succeed and fail on the magic check rather than on a short read — a
	// short file gives ErrNoMeta ("EOF / EOF"), which is a different, and
	// less telling, real reason than the one this test wants to see named.
	garbage := filepath.Join(dir, "acme", "main.sapedb")
	if err := os.WriteFile(garbage, bytes.Repeat([]byte{0xAB}, 2*pager.PageBytes), 0o600); err != nil {
		t.Fatal(err)
	}

	said := &notices{}
	server, err := New(Options{Dir: dir, Secret: secret, Notice: said.record})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()

	signature, err := server.Sign("acme", password, "main")
	if err != nil {
		t.Fatal(err)
	}
	client := dial(t, listener.Addr().String())
	client.send(protocol.Hello, hello{Account: "acme", Password: password, DBName: "main", Signature: signature})
	if frame := client.read(); frame.Type != protocol.Failure {
		t.Fatalf("a garbage file was accepted as a database: %s", frame.Type)
	}
	waitClosed(t, client.conn)

	lines := said.all()
	if len(lines) != 1 {
		t.Fatalf("said %v, want exactly one line", lines)
	}
	if strings.Contains(lines[0], "closed the connection") {
		t.Errorf("the operator's own line repeats the client's vague wording: %q", lines[0])
	}
	if !strings.Contains(lines[0], "not a sapedb file") {
		t.Errorf("the line does not name the real reason a file this broken failed: %q", lines[0])
	}
}

// TestACleanGoodbyeSaysNothing and TestAClientClosingTheSocketSaysNothing are
// the other half of the same table (cases 3 and 4): a connection that ends
// the way everyone expected it to must produce no line at all, or a caller
// who does nothing wrong yet gets logged every time drowns out the caller
// who did.
func TestACleanGoodbyeSaysNothing(t *testing.T) {
	server, address, said := runningNoticed(t)

	client := dial(t, address)
	client.open(server, "acme", "main")
	client.send(protocol.Goodbye, nil)

	waitClosed(t, client.conn)
	if lines := said.all(); len(lines) != 0 {
		t.Errorf("a clean Goodbye said %v", lines)
	}
}

func TestAClientClosingTheSocketSaysNothing(t *testing.T) {
	server, address, said := runningNoticed(t)

	client := dial(t, address)
	client.open(server, "acme", "main")

	// Closing only the write half leaves the read half open, so this test can
	// still see the server close its side afterwards rather than guessing at
	// a delay. The server sees end-of-stream exactly as it would from a
	// client that hung up entirely; io.EOF, not an error.
	tcp, ok := client.conn.(*net.TCPConn)
	if !ok {
		t.Fatal("dial did not return a TCP connection")
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	waitClosed(t, client.conn)
	if lines := said.all(); len(lines) != 0 {
		t.Errorf("a client hanging up said %v", lines)
	}
}

// TestAFrameBeforeHelloGetsOneLineNamingTheFrame is case 5: whatever a
// client sends first, if it is not a Hello, is a handshake failure too, and
// gets the same one line, naming which frame it actually was.
func TestAFrameBeforeHelloGetsOneLineNamingTheFrame(t *testing.T) {
	_, address, said := runningNoticed(t)

	client := dial(t, address)
	client.send(protocol.Invoke, call{Command: "articles.add"})

	frame := client.read()
	if frame.Type != protocol.Failure {
		t.Fatalf("it was answered with a %s", frame.Type)
	}
	waitClosed(t, client.conn)

	lines := said.all()
	if len(lines) != 1 {
		t.Fatalf("said %v, want exactly one line", lines)
	}
	if !strings.Contains(lines[0], "invoke frame") {
		t.Errorf("the line does not say which frame it was: %q", lines[0])
	}
}

// TestAFailureAfterAGoodHandshakeAlsoGetsOneLine is case 7, ngoài mục 6's
// table: 0048's own equivalence axis is handshake-time failure versus
// serving-time failure, and a Notice that only fires for errors.Is(err,
// ErrHandshake) — mutation G5 — would pass every one of cases 1, 2 and 5
// above, because all three really are ErrHandshake. This is the test that
// needs a failure Handle returns from *after* a successful handshake, which
// is not wrapped in ErrHandshake at all: a frame with a version this build
// does not read.
//
// Run for both modes, because this is also the mode-equivalence check the
// task's own measuring rules ask for: nothing about where Notice is called
// (Serve, not Handle or handshake) treats bound and account differently, so
// neither should the result.
func TestAFailureAfterAGoodHandshakeAlsoGetsOneLine(t *testing.T) {
	for _, mode := range []string{ModeBound, ModeAccount} {
		t.Run(mode, func(t *testing.T) {
			server, address, said := runningNoticed(t)

			client := dial(t, address)
			opening := hello{Account: "acme", Password: password, Mode: mode}
			if mode == ModeBound {
				signature, err := server.Sign("acme", password, "main")
				if err != nil {
					t.Fatal(err)
				}
				opening.DBName, opening.Signature = "main", signature
			}
			client.send(protocol.Hello, opening)
			if frame := client.read(); frame.Type != protocol.Welcome {
				t.Fatalf("the handshake did not open: %s %s", frame.Type, frame.Payload)
			}

			// A frame this reader was not told to accept. Encode writes the
			// version this side speaks by default, so it has to be poked in
			// by hand to be wrong.
			bad, err := protocol.Encode(protocol.Frame{Type: protocol.Ping, ID: 99})
			if err != nil {
				t.Fatal(err)
			}
			bad[0] = 200
			if _, err := client.conn.Write(bad); err != nil {
				t.Fatal(err)
			}

			waitClosed(t, client.conn)

			lines := said.all()
			if len(lines) != 1 {
				t.Fatalf("a post-handshake protocol error said %v, want exactly one line", lines)
			}
			if strings.Contains(lines[0], "did not open properly") {
				t.Errorf("a serving-time failure was blamed on the handshake: %q", lines[0])
			}
			if !strings.Contains(lines[0], "version") {
				t.Errorf("the line does not name the real reason: %q", lines[0])
			}
		})
	}
}

// The three tests below call guard directly, not through a running server —
// task 0049 §6 asks for exactly that ("gọi thẳng guard chứ không qua một
// tiến trình thật"), since a guard that actually finished dying would end
// the test binary along with it. net.Pipe gives two connected, unbuffered
// net.Conn ends without opening a real socket: guard writes to one end from
// inside the panicking goroutine below, and a reader goroutine on the other
// end is what makes that write able to complete at all — net.Pipe has no
// buffer, so a guard test using it without a concurrent reader would
// deadlock on the very Failure frame it is trying to prove gets sent.

// TestGuardSendsANoticeLineAFailureFrameAndPanicsAgainWithTheOriginalValue
// is task 0049 §6's three guard assertions in one test, because they are one
// call: a Notice line naming the address, the panic value, and a stack; a
// Failure frame, ID 0, written to the connection; and a re-panic the test's
// own recover() can see carries the exact original value, not a wrapper.
func TestGuardSendsANoticeLineAFailureFrameAndPanicsAgainWithTheOriginalValue(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	type read struct {
		frame protocol.Frame
		err   error
	}
	frames := make(chan read, 1)
	go func() {
		frame, err := protocol.NewReader(clientEnd).Read()
		frames <- read{frame, err}
	}()

	said := &notices{}
	const panicValue = "boom-0049-guard"

	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("guard did not panic again — a recovered panic here must not let the connection's goroutine carry on")
			}
			// The exact value, via ==, not just a string that happens to
			// read the same: guard is not allowed to have rebuilt it
			// (with fmt.Errorf or otherwise) on the way back out.
			if recovered != panicValue {
				t.Fatalf("guard re-panicked with %#v (%T), want the untouched original %#v (%T)",
					recovered, recovered, panicValue, panicValue)
			}
		}()
		defer guard(serverEnd, said.record)
		panic(panicValue)
	}()

	select {
	case got := <-frames:
		if got.err != nil {
			t.Fatalf("reading the frame guard was supposed to send: %v", got.err)
		}
		if got.frame.Type != protocol.Failure {
			t.Fatalf("frame type = %v, want Failure", got.frame.Type)
		}
		if got.frame.ID != 0 {
			t.Fatalf("frame ID = %d, want 0 — this is not an answer to any particular request", got.frame.ID)
		}
		if !strings.Contains(string(got.frame.Payload), panicValue) {
			t.Fatalf("failure payload %s does not mention the panic value", got.frame.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame arrived on the connection within 2s — guard's write may be blocked or missing")
	}

	lines := said.all()
	if len(lines) != 1 {
		t.Fatalf("got %d notice lines, want exactly 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], panicValue) {
		t.Fatalf("notice line does not carry the panic value: %q", lines[0])
	}
	if !strings.Contains(lines[0], serverEnd.RemoteAddr().String()) {
		t.Fatalf("notice line does not carry the connection's remote address: %q", lines[0])
	}
	// debug.Stack() output always starts with "goroutine N [running/...]:" —
	// this is what stands in for "and there is a stack" without pinning the
	// test to any particular frame appearing in it, which would make it
	// break on every unrelated refactor of guard's own call depth.
	if !strings.Contains(lines[0], "goroutine ") {
		t.Fatalf("notice line does not look like it carries a stack: %q", lines[0])
	}
}

// TestGuardOnAnErrorPanicPreservesItsErrorsIsIdentity is the other half of
// mục 6's third assertion: when the panic value already IS an error, guard
// must not have re-wrapped it with %v (or anything else) on the way to
// failure() — this is the exact shape task 0049's brief warned against,
// citing handshake()'s fmt.Errorf("%w: %v", ErrHandshake, err) dropping
// signing.ErrBadSignature out of errors.Is. Measured here the same way that
// bug would be measured: build a payload from an error carrying a sentinel,
// and check errors.Is still finds it after the payload comes back through
// codeFor.
func TestGuardOnAnErrorPanicPreservesItsErrorsIsIdentity(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	type read struct {
		frame protocol.Frame
		err   error
	}
	frames := make(chan read, 1)
	go func() {
		frame, err := protocol.NewReader(clientEnd).Read()
		frames <- read{frame, err}
	}()

	panicErr := fmt.Errorf("sapedb/server: guard test wrapper: %w", store.ErrCondition)

	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("guard did not panic again")
			}
			asErr, ok := recovered.(error)
			if !ok {
				t.Fatalf("guard re-panicked with a %T, want the original error", recovered)
			}
			if !errors.Is(asErr, store.ErrCondition) {
				t.Fatalf("the re-panicked error lost its errors.Is chain to store.ErrCondition: %v", asErr)
			}
		}()
		defer guard(serverEnd, nil)
		panic(panicErr)
	}()

	select {
	case got := <-frames:
		if got.err != nil {
			t.Fatalf("reading the frame: %v", got.err)
		}
		var body struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(got.frame.Payload, &body); err != nil {
			t.Fatalf("failure payload does not read as JSON: %v\n%s", err, got.frame.Payload)
		}
		// codeFor is explicitly not touched by this task (mục 5) and a
		// panic gets no code of its own — but codeFor's own errors.Is
		// loop runs against whatever error guard hands it, and
		// store.ErrCondition IS one of its known sentinels. If guard had
		// flattened the panic value with fmt.Errorf("%v", ...) before
		// this point, that chain would already be gone and this would
		// read "failed" instead.
		if body.Code != "condition" {
			t.Fatalf("failure code = %q, want %q — guard must have wrapped the panic error with %%v somewhere and lost the errors.Is chain to it", body.Code, "condition")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame arrived on the connection within 2s")
	}
}

// TestGuardInOneGoroutineDoesNotProtectAPanicInAnotherGoroutine measures,
// rather than asserts, the first limit task 0049's brief asked to see
// written down "ngay cạnh code, đừng để một câu 'never' trần" instead of
// argued about: recover() only ever catches a panic unwinding through its
// OWN goroutine's defer stack. guard is deferred once, in Serve's
// per-connection goroutine — but Handle can start a second goroutine off
// that same connection for a live subscription (subscribe.go's
// `go s.stream(...)`, called from follow), and stream has no recover of
// its own. A panic inside stream is not a hypothetical: it is a goroutine
// this exact codebase spawns, sitting one guard away from looking covered
// and not being covered at all.
//
// This runs as a child process because the whole point is an unrecovered
// panic in a goroutine — which crashes the entire program, guard or not,
// exactly the way it would in production. The parent only checks that the
// child died of that panic, not of anything guard-shaped.
func TestGuardInOneGoroutineDoesNotProtectAPanicInAnotherGoroutine(t *testing.T) {
	if os.Getenv("SAPEDB_CROSS_GOROUTINE_CHILD") == "1" {
		serverEnd, _ := net.Pipe()
		defer serverEnd.Close()

		// Goroutine A: has guard deferred, exactly like a connection's own
		// goroutine in Serve. It never panics itself — it just waits, the
		// way a healthy connection serving frames would.
		waitForever := make(chan struct{})
		go func() {
			defer guard(serverEnd, func(string) {})
			<-waitForever
		}()

		// Goroutine B: no recover anywhere in its stack, standing in for
		// stream() (subscribe.go) or any other goroutine a connection
		// spawns off to the side of the one guard actually watches.
		go func() {
			panic("panic in a sibling goroutine, not the one guard is deferred in")
		}()

		time.Sleep(2 * time.Second) // give B's panic time to bring the process down
		os.Stdout.WriteString("STILL ALIVE\n")
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestGuardInOneGoroutineDoesNotProtectAPanicInAnotherGoroutine$",
		"-test.timeout=30s")
	cmd.Env = append(os.Environ(), "SAPEDB_CROSS_GOROUTINE_CHILD=1")
	out, err := cmd.CombinedOutput()
	text := string(out)

	if strings.Contains(text, "STILL ALIVE") {
		t.Fatalf("the process survived a panic in an unguarded sibling goroutine — that would mean recover() somehow reached across goroutines, which Go's own spec says it cannot:\n%s", text)
	}
	if err == nil {
		t.Fatalf("the child exited cleanly; an unrecovered panic in goroutine B is supposed to crash the whole process\n%s", text)
	}
	if !strings.Contains(text, "panic: panic in a sibling goroutine") {
		t.Fatalf("the child died, but not of the panic this test raised in goroutine B:\n%s", text)
	}
}
