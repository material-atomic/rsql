package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// read waits for one frame, so a test that is wrong fails instead of hanging.
func (c *client) readWithin(d time.Duration) protocol.Frame {
	c.t.Helper()

	type answer struct {
		frame protocol.Frame
		err   error
	}
	got := make(chan answer, 1)
	go func() {
		frame, err := c.reader.Read()
		got <- answer{frame, err}
	}()

	select {
	case a := <-got:
		if a.err != nil {
			c.t.Fatalf("read: %v", a.err)
		}
		return a.frame
	case <-time.After(d):
		c.t.Fatal("nothing arrived")
		return protocol.Frame{}
	}
}

func TestASubscriptionCatchesUpAndThenKeepsUp(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	// Something to catch up on.
	writer := dial(t, address)
	writer.open(server, "acme", "main")
	for i := 0; i < 3; i++ {
		if frame := writer.invoke("articles.add", map[string]any{
			"title": fmt.Sprintf("before %d", i), "author": "ann",
		}); frame.Type != protocol.Result {
			t.Fatalf("write %d: %s", i, frame.Payload)
		}
	}

	reader := dial(t, address)
	reader.open(server, "acme", "main")
	id := reader.send(protocol.Subscribe, subscribe{From: 1})

	frame := reader.readWithin(5 * time.Second)
	if frame.Type != protocol.Result || frame.ID != id {
		t.Fatalf("the subscription answered with %s#%d: %s", frame.Type, frame.ID, frame.Payload)
	}
	confirmed := decode[following](t, frame)
	if confirmed.From != 1 || confirmed.Latest < 3 {
		t.Errorf("the subscription reports %+v", confirmed)
	}

	// The catch-up: everything already written, in order, tagged with the
	// subscription that asked.
	seen := []uint64{}
	for len(seen) < int(confirmed.Latest) {
		event := reader.readWithin(5 * time.Second)
		if event.Type != protocol.Event {
			t.Fatalf("expected an event, got %s: %s", event.Type, event.Payload)
		}
		if event.ID != id {
			t.Errorf("an event is tagged %d, the subscription is %d", event.ID, id)
		}
		change := decode[store.Change](t, event)
		seen = append(seen, change.LSN)
	}
	for i, lsn := range seen {
		if lsn != uint64(i+1) {
			t.Fatalf("the entries arrived as %v, which is not in order from 1", seen)
		}
	}

	// And then what happens next, without asking again.
	if frame := writer.invoke("articles.add", map[string]any{"title": "after", "author": "bob"}); frame.Type != protocol.Result {
		t.Fatalf("the later write: %s", frame.Payload)
	}

	event := reader.readWithin(5 * time.Second)
	if event.Type != protocol.Event {
		t.Fatalf("expected the new change, got %s: %s", event.Type, event.Payload)
	}
	change := decode[store.Change](t, event)
	if change.Kind != store.ChangePut || change.Document["title"] != "after" {
		t.Errorf("the live change reads %+v", change)
	}
}

// A consumer that has fallen further behind than the log keeps must be told,
// not quietly started from wherever the log now begins. Skipping ahead would
// leave it believing it had seen everything.
func TestASubscriberTooFarBehindIsToldSo(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	db.Retain(5)
	release()

	writer := dial(t, address)
	writer.open(server, "acme", "main")
	for i := 0; i < 20; i++ {
		writer.invoke("articles.add", map[string]any{"title": fmt.Sprintf("n%d", i), "author": "ann"})
	}

	reader := dial(t, address)
	reader.open(server, "acme", "main")
	reader.send(protocol.Subscribe, subscribe{From: 1})

	frame := reader.readWithin(5 * time.Second)
	if frame.Type != protocol.Failure {
		t.Fatalf("a subscriber from before the window was answered with %s: %s", frame.Type, frame.Payload)
	}
	if !strings.Contains(string(frame.Payload), `"code":"too_far_behind"`) {
		t.Errorf("the failure reads %s", frame.Payload)
	}

	// And one inside the window is served.
	inside, err := func() (uint64, error) {
		db, release, err := server.Store("acme", "main")
		if err != nil {
			return 0, err
		}
		defer release()
		return db.OldestLSN()
	}()
	if err != nil {
		t.Fatal(err)
	}

	again := dial(t, address)
	again.open(server, "acme", "main")
	again.send(protocol.Subscribe, subscribe{From: inside})
	if frame := again.readWithin(5 * time.Second); frame.Type != protocol.Result {
		t.Fatalf("a subscriber inside the window: %s %s", frame.Type, frame.Payload)
	}
}

// One connection, two subscriptions, and the ids are the only thing telling
// their events apart.
func TestTwoSubscriptionsOnOneConnectionStaySeparate(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	reader := dial(t, address)
	reader.open(server, "acme", "main")

	first := reader.send(protocol.Subscribe, subscribe{From: 1})
	if frame := reader.readWithin(5 * time.Second); frame.Type != protocol.Result || frame.ID != first {
		t.Fatalf("the first subscription: %s#%d", frame.Type, frame.ID)
	}
	second := reader.send(protocol.Subscribe, subscribe{From: 1})

	// The first feed is already sending, so its events and the second
	// subscription's answer arrive interleaved — which is the whole point of
	// tagging them. Read until both are accounted for rather than assuming an
	// order nothing promises.
	counts := map[uint32]int{}
	confirmed := false

	for !confirmed || counts[first] == 0 || counts[second] == 0 {
		frame := reader.readWithin(5 * time.Second)

		switch frame.Type {
		case protocol.Result:
			if frame.ID != second {
				t.Fatalf("an answer arrived tagged %d, want %d", frame.ID, second)
			}
			confirmed = true
		case protocol.Event:
			if frame.ID != first && frame.ID != second {
				t.Fatalf("an event arrived tagged %d, which is neither subscription", frame.ID)
			}
			counts[frame.ID]++
		default:
			t.Fatalf("got a %s: %s", frame.Type, frame.Payload)
		}
	}
	if counts[first] == 0 || counts[second] == 0 {
		t.Errorf("one subscription sent nothing: %v", counts)
	}
}

// A subscription reads everything that happens in a database, so on an
// account-wide connection it is verified exactly as a call is — it does not
// inherit a handshake that proved nothing.
func TestASubscriptionOnAnAccountConnectionIsVerified(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")
	declare(t, server, "acme", "other")

	reader := dial(t, address)
	reader.send(protocol.Hello, hello{Account: "acme", Password: password, Mode: ModeAccount})
	if frame := reader.readWithin(5 * time.Second); frame.Type != protocol.Welcome {
		t.Fatalf("the account handshake: %s %s", frame.Type, frame.Payload)
	}

	// No signature at all.
	reader.send(protocol.Subscribe, subscribe{From: 1, DBName: "main"})
	if frame := reader.readWithin(5 * time.Second); frame.Type != protocol.Failure {
		t.Fatalf("an unsigned subscription was served: %s", frame.Payload)
	}

	// A signature for another database.
	other, err := server.Sign("acme", password, "other")
	if err != nil {
		t.Fatal(err)
	}
	reader.send(protocol.Subscribe, subscribe{From: 1, DBName: "main", Signature: other})
	if frame := reader.readWithin(5 * time.Second); frame.Type != protocol.Failure {
		t.Fatalf("a subscription signed for another database was served: %s", frame.Payload)
	}

	// And the right one works.
	signature, err := server.Sign("acme", password, "main")
	if err != nil {
		t.Fatal(err)
	}
	id := reader.send(protocol.Subscribe, subscribe{From: 1, DBName: "main", Signature: signature})
	if frame := reader.readWithin(5 * time.Second); frame.Type != protocol.Result || frame.ID != id {
		t.Fatalf("a properly signed subscription: %s %s", frame.Type, frame.Payload)
	}
}

// A subscription must not hold a database open against everybody else: the
// events are collected under the lock and written outside it.
func TestASlowSubscriberDoesNotStopTheWriters(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	// A subscriber that asks and then never reads a single byte.
	idle := dial(t, address)
	idle.open(server, "acme", "main")
	idle.send(protocol.Subscribe, subscribe{From: 1})

	writer := dial(t, address)
	writer.open(server, "acme", "main")

	// Enough writes to fill whatever the socket buffers, so the streamer is
	// certainly blocked on a write by the end.
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			frame := writer.invoke("articles.add", map[string]any{
				"title": fmt.Sprintf("while blocked %d", i), "author": "ann",
			})
			if frame.Type != protocol.Result {
				done <- fmt.Errorf("write %d: %s", i, frame.Payload)
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a subscriber that stopped reading stopped the writers")
	}
}

func TestASubscriptionThatMakesNoSenseIsRefused(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	reader := dial(t, address)
	reader.open(server, "acme", "main")

	frame, err := protocol.Encode(protocol.Frame{
		Version: protocol.Version, Type: protocol.Subscribe, ID: 99, Payload: []byte("not json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.conn.Write(frame); err != nil {
		t.Fatal(err)
	}

	answer := reader.readWithin(5 * time.Second)
	if answer.Type != protocol.Failure || answer.ID != 99 {
		t.Fatalf("got %s#%d: %s", answer.Type, answer.ID, answer.Payload)
	}

	// And the connection survives it.
	if frame := reader.invoke("articles.by_author", map[string]any{"author": "ann"}); frame.Type != protocol.Result {
		t.Errorf("the connection did not survive a bad subscription: %s", frame.Payload)
	}
	_ = errors.Is
}

// fill writes n changes through the wire, so the log has something in it.
func fill(t *testing.T, writer *client, n int, prefix string) {
	t.Helper()
	for i := 0; i < n; i++ {
		frame := writer.invoke("articles.add", map[string]any{
			"title": fmt.Sprintf("%s%d", prefix, i), "author": "ann",
		})
		if frame.Type != protocol.Result {
			t.Fatalf("write %d: %s", i, frame.Payload)
		}
	}
}

// A backlog longer than one batch must arrive in full, without waiting for
// somebody to write again. A feed that stops at the batch size and waits looks
// exactly like a feed that has caught up — and on a database being read from
// and no longer written to, it waits forever.
func TestABacklogLongerThanOneBatchArrivesInFull(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	writer := dial(t, address)
	writer.open(server, "acme", "main")
	fill(t, writer, 600, "backlog ")

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := db.LatestLSN()
	release()
	if err != nil {
		t.Fatal(err)
	}
	if latest <= 512 {
		t.Fatalf("this test needs more than one batch; the log has %d", latest)
	}

	reader := dial(t, address)
	reader.open(server, "acme", "main")
	id := reader.send(protocol.Subscribe, subscribe{From: 1})
	if frame := reader.readWithin(10 * time.Second); frame.Type != protocol.Result || frame.ID != id {
		t.Fatalf("the subscription: %s %s", frame.Type, frame.Payload)
	}

	// Nothing else is written from here on, so every entry has to come from
	// the catch-up alone.
	for want := uint64(1); want <= latest; want++ {
		event := reader.readWithin(10 * time.Second)
		if event.Type != protocol.Event {
			t.Fatalf("at entry %d of %d: got a %s: %s", want, latest, event.Type, event.Payload)
		}
		if change := decode[store.Change](t, event); change.LSN != want {
			t.Fatalf("entry %d arrived as %d", want, change.LSN)
		}
	}
}

// A change committed while the feed is busy sending must still arrive. The
// wakeup is taken before the log is read for exactly this: a change that lands
// in between closes a channel nobody is waiting on yet, and a feed that then
// waits on a fresh one stops with that change undelivered — looking perfectly
// healthy.
func TestAChangeCommittedWhileTheFeedIsBusyStillArrives(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	writer := dial(t, address)
	writer.open(server, "acme", "main")
	fill(t, writer, 300, "busy ")

	reader := dial(t, address)
	reader.open(server, "acme", "main")
	id := reader.send(protocol.Subscribe, subscribe{From: 1})
	if frame := reader.readWithin(10 * time.Second); frame.Type != protocol.Result || frame.ID != id {
		t.Fatalf("the subscription: %s %s", frame.Type, frame.Payload)
	}

	// While the feed is pushing the backlog into a socket nobody is draining,
	// one more change is committed.
	time.Sleep(200 * time.Millisecond)
	if frame := writer.invoke("articles.add", map[string]any{"title": "the last one", "author": "bob"}); frame.Type != protocol.Result {
		t.Fatalf("the late write: %s", frame.Payload)
	}

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := db.LatestLSN()
	release()
	if err != nil {
		t.Fatal(err)
	}

	for want := uint64(1); want <= latest; want++ {
		event := reader.readWithin(10 * time.Second)
		if event.Type != protocol.Event {
			t.Fatalf("at entry %d of %d: got a %s: %s", want, latest, event.Type, event.Payload)
		}
		if change := decode[store.Change](t, event); change.LSN != want {
			t.Fatalf("entry %d arrived as %d", want, change.LSN)
		}
	}
}

// A feed whose cursor falls out of the retention window must be told, not left
// quietly skipping whatever was trimmed.
//
// Checked where the decision is made rather than through a socket. Getting a
// real feed to fall behind takes filling the kernel's buffer and then writing
// past the window while it is stuck — thousands of round trips, and a test
// that passes or fails on how fast the machine is. What is worth knowing is
// that the check is there and says the right thing.
func TestAFeedIsRefusedWhenItsCursorLeavesTheWindow(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	db.Retain(10)
	release()

	writer := dial(t, address)
	writer.open(server, "acme", "main")
	fill(t, writer, 40, "trimmed ")

	opened, found := server.open["acme/main"]
	if !found {
		t.Fatal("the database is not open")
	}

	// Entry 1 is long gone.
	if _, _, err := server.drain(opened, &sender{conn: io.Discard}, 7, 1); !errors.Is(err, ErrTooFarBehind) {
		t.Errorf("a cursor before the window: want ErrTooFarBehind, got %v", err)
	}

	// And one inside it is served.
	opened.mutex.Lock()
	oldest, err := opened.store.OldestLSN()
	opened.mutex.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.drain(opened, &sender{conn: io.Discard}, 7, oldest); err != nil {
		t.Errorf("a cursor inside the window: %v", err)
	}
}

// Two busy subscriptions on one connection write from two goroutines. Without
// one place for writes, the bytes each produces are correct and what arrives
// is not a frame at all.
func TestTwoBusyFeedsProduceAStreamThatStillDecodes(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	writer := dial(t, address)
	writer.open(server, "acme", "main")
	fill(t, writer, 400, "both ")

	reader := dial(t, address)
	reader.open(server, "acme", "main")
	first := reader.send(protocol.Subscribe, subscribe{From: 1})
	second := reader.send(protocol.Subscribe, subscribe{From: 1})

	// Every frame must decode, and each feed's entries must arrive in order.
	next := map[uint32]uint64{first: 1, second: 1}
	answers := 0

	for next[first] <= 400 || next[second] <= 400 {
		frame := reader.readWithin(15 * time.Second)

		switch frame.Type {
		case protocol.Result:
			answers++
		case protocol.Event:
			want, known := next[frame.ID]
			if !known {
				t.Fatalf("an event arrived tagged %d, which is neither feed", frame.ID)
			}
			change := decode[store.Change](t, frame)
			if change.LSN != want {
				t.Fatalf("feed %d: entry %d arrived as %d — the streams are interleaved", frame.ID, want, change.LSN)
			}
			next[frame.ID] = want + 1
		default:
			t.Fatalf("got a %s: %s", frame.Type, frame.Payload)
		}
	}

	if answers != 2 {
		t.Errorf("%d subscriptions were confirmed, want 2", answers)
	}
}

// chunky writes a little at a time and gives the scheduler a chance in
// between, which is what an io.Writer that is not a socket does.
type chunky struct {
	mutex sync.Mutex
	out   []byte
}

func (c *chunky) Write(p []byte) (int, error) {
	for i := 0; i < len(p); i += 7 {
		end := min(i+7, len(p))

		c.mutex.Lock()
		c.out = append(c.out, p[i:end]...)
		c.mutex.Unlock()

		runtime.Gosched()
	}
	return len(p), nil
}

// Two feeds write from two goroutines, so every write goes through one place.
//
// Over a TCP socket this would be hard to see: Go serialises writes on a
// network file descriptor itself, so whole frames do not interleave there
// whatever this code does. But Handle takes any io.ReadWriter — a buffered
// writer, a pipe, something in a test — and none of those promise it. The lock
// is for those, and this is what it saves them from.
func TestFramesFromTwoGoroutinesDoNotInterleave(t *testing.T) {
	writer := &chunky{}
	out := &sender{conn: writer}

	const each = 60
	var running sync.WaitGroup

	for _, id := range []uint32{1, 2} {
		running.Add(1)
		go func(id uint32) {
			defer running.Done()
			for i := 0; i < each; i++ {
				body := []byte(fmt.Sprintf(`{"feed":%d,"n":%d,"padding":"%s"}`, id, i, strings.Repeat("x", 40)))
				if err := out.send(protocol.Frame{Type: protocol.Event, ID: id}, body); err != nil {
					t.Error(err)
					return
				}
			}
		}(id)
	}
	running.Wait()

	// Every frame must come out whole, and each feed's own must be in order.
	decoder := protocol.NewReader(bytes.NewReader(writer.out)).Accept(protocol.Version)
	next := map[uint32]int{1: 0, 2: 0}

	for i := 0; i < each*2; i++ {
		frame, err := decoder.Read()
		if err != nil {
			t.Fatalf("frame %d of %d: %v — the writes interleaved", i, each*2, err)
		}
		body := struct {
			Feed uint32 `json:"feed"`
			N    int    `json:"n"`
		}{}
		if err := json.Unmarshal(frame.Payload, &body); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if body.Feed != frame.ID {
			t.Fatalf("a frame tagged %d carries feed %d", frame.ID, body.Feed)
		}
		if body.N != next[frame.ID] {
			t.Fatalf("feed %d: expected %d, got %d", frame.ID, next[frame.ID], body.N)
		}
		next[frame.ID]++
	}
}
