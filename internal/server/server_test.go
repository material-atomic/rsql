package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/protocol"
	"github.com/material-atomic/rsql/internal/store"
	"github.com/material-atomic/rsql/internal/vfs"
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
	from := filepath.Join(dir, "acme", "first.rsql")
	to := filepath.Join(dir, "acme", "second.rsql")
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
