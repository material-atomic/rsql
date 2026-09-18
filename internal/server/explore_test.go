package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/material-atomic/rsql/internal/protocol"
	"github.com/material-atomic/rsql/internal/signing"
	"github.com/material-atomic/rsql/internal/store"
)

// operate answers the challenge in a welcome, the way the shell does.
func (c *client) operate(greeting welcome, secret string) protocol.Frame {
	c.t.Helper()

	challenge, err := hex.DecodeString(greeting.Challenge)
	if err != nil {
		c.t.Fatalf("the challenge is not hex: %v", err)
	}
	proof, err := signing.Operating(secret, challenge)
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(protocol.Elevate, elevating{Proof: proof})
	return c.read()
}

func (c *client) explore(access store.Access) protocol.Frame {
	c.t.Helper()
	c.send(protocol.Explore, exploring{Access: access})
	return c.read()
}

// TestAnOperatorLooksAndGetsBackSomethingToDeclare is the shell over the wire:
// prove the secret, type an access, get the rows and the operation that would
// fetch them again.
func TestAnOperatorLooksAndGetsBackSomethingToDeclare(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if greeting.Challenge == "" {
		t.Fatal("the welcome carried no challenge, so nobody can ever operate this server")
	}

	if frame := client.invoke("articles.add", map[string]any{"title": "one", "author": "ann"}); frame.Type != protocol.Result {
		t.Fatalf("writing something to look at: %s %s", frame.Type, frame.Payload)
	}

	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	frame := client.explore(store.Access{Kind: "scan", Collection: "articles", Index: "by_author"})
	if frame.Type != protocol.Result {
		t.Fatalf("exploring: %s %s", frame.Type, frame.Payload)
	}
	answer := decode[explored](t, frame)
	if answer.Result.Count != 1 || answer.Result.Rows[0]["author"] != "ann" {
		t.Errorf("the scan came back as %+v", answer.Result)
	}
	if answer.Draft.Action != store.ActionScan || answer.Draft.Index != "by_author" || answer.Draft.Limit == 0 {
		t.Errorf("the draft is not the scan that ran: %+v", answer.Draft)
	}
}

// TestExploringNeedsMoreThanAConnectionString: a connection string says which
// database to reach, never what may be done once there. Everyone who holds one
// would otherwise be able to run accesses nobody declared, which is the whole
// thing this database is built not to allow.
func TestExploringNeedsMoreThanAConnectionString(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))

	// A properly signed, perfectly ordinary connection.
	frame := client.explore(store.Access{Kind: "scan", Collection: "articles"})
	if frame.Type != protocol.Failure {
		t.Fatalf("an ordinary connection explored: %s %s", frame.Type, frame.Payload)
	}
	if code := decode[struct {
		Code string `json:"code"`
	}](t, frame).Code; code != "not_operator" {
		t.Errorf("refused with code %q", code)
	}

	// A wrong proof does not elevate, and the connection stays what it was
	// rather than ending — so a typo is not an outage.
	client.send(protocol.Elevate, elevating{Proof: "00"})
	if frame := client.read(); frame.Type != protocol.Failure {
		t.Fatalf("a proof of nothing was accepted: %s", frame.Payload)
	}
	if frame := client.explore(store.Access{Kind: "scan", Collection: "articles"}); frame.Type != protocol.Failure {
		t.Error("a failed proof left the connection able to explore")
	}
	if frame := client.invoke("articles.by_author", map[string]any{"author": "ann"}); frame.Type != protocol.Result {
		t.Errorf("a failed proof ended an otherwise good connection: %s", frame.Payload)
	}

	// And a proof made for a different challenge. This is the one that matters:
	// a proof is not a password, so one lifted from a log or a process listing
	// is spent.
	elsewhere := dial(t, address)
	other := decode[welcome](t, elsewhere.open(server, "acme", "main"))
	challenge, err := hex.DecodeString(other.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	if other.Challenge == greeting.Challenge {
		t.Fatal("two connections were given the same challenge")
	}
	stolen, err := signing.Operating(secret, challenge)
	if err != nil {
		t.Fatal(err)
	}
	client.send(protocol.Elevate, elevating{Proof: stolen})
	if frame := client.read(); frame.Type != protocol.Failure {
		t.Errorf("a proof answered a challenge it was not made for: %s", frame.Payload)
	}
}

// TestAnOperatorStillHasToSayWhichDatabase: proving you can operate the server
// says what you may do, never what to. On an account-wide connection the
// database is named and signed for per call, and an explore is no different.
func TestAnOperatorStillHasToSayWhichDatabase(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")
	declare(t, server, "acme", "other")

	client := dial(t, address)
	client.send(protocol.Hello, hello{Account: "acme", Password: password, Mode: ModeAccount})
	greeting := decode[welcome](t, client.read())

	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	// Elevated, but naming no database.
	client.send(protocol.Explore, exploring{Access: store.Access{Kind: "scan", Collection: "articles"}})
	if frame := client.read(); frame.Type != protocol.Failure {
		t.Errorf("an operator reached a database it never named: %s", frame.Payload)
	}

	// Named, with the signature for it.
	signature, err := server.Sign("acme", password, "other")
	if err != nil {
		t.Fatal(err)
	}
	client.send(protocol.Explore, exploring{
		DBName: "other", Signature: signature,
		Access: store.Access{Kind: "scan", Collection: "articles"},
	})
	if frame := client.read(); frame.Type != protocol.Result {
		t.Errorf("an operator with a signature for it was refused: %s", frame.Payload)
	}

	// Naming one database with the signature for another. It has to be a
	// database this connection has not already been verified for: once it has,
	// the answer is remembered, which is deliberate — it cannot change while
	// the connection lives, and checking an HMAC per call on a hot connection
	// is work nobody asked for.
	client.send(protocol.Explore, exploring{
		DBName: "main", Signature: signature,
		Access: store.Access{Kind: "scan", Collection: "articles"},
	})
	if frame := client.read(); frame.Type != protocol.Failure {
		t.Errorf("a signature for another database opened this one: %s", frame.Payload)
	}
}

// TestWhatAnOperatorDidIsInTheFeed: the people who can explore are the people
// most worth being able to ask about afterwards, and the answer should reach
// whoever is following the database rather than only the file.
func TestWhatAnOperatorDidIsInTheFeed(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}
	if frame := client.explore(store.Access{Kind: "get", Collection: "articles", Key: "nothing"}); frame.Type != protocol.Result {
		t.Fatalf("exploring: %s", frame.Payload)
	}

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var last store.Change
	if err := db.Changes(1, func(change store.Change) bool {
		last = change
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if last.Kind != store.ChangeRead {
		t.Fatalf("the log ends with a %q", last.Kind)
	}
	if last.By.Actor != "acme (operator)" {
		t.Errorf("the log says %q looked", last.By.Actor)
	}
}

// TestAFollowerHearsWhatAnOperatorDid: the audit entry is in the same log
// everything else is in, so whoever is following this database learns that
// somebody looked at the same moment they learn anything else.
//
// Without it the entry is in the file and nowhere else, and a replica or an
// audit consumer would not hear about the one kind of access most worth
// hearing about until something else happened to wake the feed.
func TestAFollowerHearsWhatAnOperatorDid(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	reader := dial(t, address)
	reader.open(server, "acme", "main")
	reader.send(protocol.Subscribe, subscribe{From: 1})
	confirmed := decode[following](t, reader.readWithin(5*time.Second))
	for i := uint64(0); i < confirmed.Latest; i++ {
		reader.readWithin(5 * time.Second)
	}

	operator := dial(t, address)
	greeting := decode[welcome](t, operator.open(server, "acme", "main"))
	if frame := operator.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s", frame.Payload)
	}
	if frame := operator.explore(store.Access{Kind: "get", Collection: "articles", Key: "nothing"}); frame.Type != protocol.Result {
		t.Fatalf("exploring: %s", frame.Payload)
	}

	event := reader.readWithin(5 * time.Second)
	if event.Type != protocol.Event {
		t.Fatalf("the follower got a %s: %s", event.Type, event.Payload)
	}
	change := decode[store.Change](t, event)
	if change.Kind != store.ChangeRead || change.By.Actor != "acme (operator)" {
		t.Errorf("the follower was told: %+v", change)
	}
}
