package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/material-atomic/rsql/internal/protocol"
	"github.com/material-atomic/rsql/internal/store"
)

// A subscription is the change log, read out loud.
//
// It is the same log a replica replays and an audit reads, so a consumer that
// keeps up gets every change in order, once, with nothing invented for its
// benefit. What it does not get is a promise that the log waits: entries are
// trimmed to whatever retention was set, and a consumer that falls behind that
// is told so and must start again from a dump. Skipping ahead quietly would
// leave it believing it had seen everything.
//
// Events carry the id of the subscription that asked for them, so one
// connection can carry several and each knows which is which.

// ErrTooFarBehind is a subscriber asking for entries that have been trimmed.
var ErrTooFarBehind = errors.New("rsql/server: those changes are no longer kept; start again from a dump")

// subscribe is what a client asks for.
type subscribe struct {
	From uint64 `json:"from"`

	// DBName and Signature are how an account-wide connection says which
	// database it wants the changes of, exactly as a call does. A subscription
	// reads everything that happens in a database, so it is verified the same
	// way rather than inheriting a connection that proved nothing.
	DBName    string `json:"dbname,omitempty"`
	Signature string `json:"sig,omitempty"`
}

// following is the answer: where the feed starts and where it has reached.
type following struct {
	From   uint64 `json:"from"`
	Latest uint64 `json:"latest"`
	Oldest uint64 `json:"oldest"`
}

// sender serialises writes to one connection.
//
// Until now everything wrote from the read loop, so nothing could interleave.
// A subscription writes from somewhere else, and two goroutines each writing a
// frame produce bytes that are correct one at a time and together are not a
// frame at all.
//
// Over TCP this would be hard to notice, because Go serialises writes on a
// network file descriptor itself. But Handle takes any io.ReadWriter, and a
// buffered writer, a pipe or a test double promises nothing of the sort. The
// lock is for those — not decoration on a socket that already has one.
type sender struct {
	mutex sync.Mutex
	conn  io.Writer
}

func (s *sender) send(frame protocol.Frame, payload []byte) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return write(s.conn, frame, payload)
}

// follow starts a subscription and streams it until the connection ends.
func (s *Server) follow(live *session, out *sender, id uint32, payload []byte, done <-chan struct{}) error {
	asked := subscribe{}
	if err := json.Unmarshal(payload, &asked); err != nil {
		return fmt.Errorf("rsql/server: the subscription does not read as one: %w", err)
	}

	db, err := s.reach(live, call{DBName: asked.DBName, Signature: asked.Signature})
	if err != nil {
		return err
	}

	db.mutex.Lock()
	oldest, err := db.store.OldestLSN()
	if err == nil {
		var follow bool
		follow, err = db.store.CanFollow(asked.From)
		if err == nil && !follow {
			err = fmt.Errorf("%w: the log starts at %d and you asked for %d", ErrTooFarBehind, oldest, asked.From)
		}
	}
	latest, latestErr := db.store.LatestLSN()
	db.mutex.Unlock()

	if err != nil {
		return err
	}
	if latestErr != nil {
		return latestErr
	}

	// The answer goes back before any event does, so a client knows the
	// subscription exists rather than inferring it from the first change —
	// which on a quiet database might be tomorrow.
	body, err := json.Marshal(following{From: asked.From, Latest: latest, Oldest: oldest})
	if err != nil {
		return err
	}
	if err := out.send(protocol.Frame{Type: protocol.Result, ID: id}, body); err != nil {
		return err
	}

	go s.stream(db, out, id, asked.From, done)
	return nil
}

// stream sends every entry from `from` onwards, and then whatever arrives.
func (s *Server) stream(db *database, out *sender, id uint32, from uint64, done <-chan struct{}) {
	cursor := from

	for {
		// Taken before reading, not after. A change committed in between closes
		// a channel nobody is waiting on yet; a feed that then waits on a fresh
		// one stops with that change undelivered, and looks perfectly healthy
		// doing it.
		//
		// No test covers this. The window is the few instructions between the
		// read returning and the wait beginning, and reaching it from outside
		// would take a hook in this function — which would be a worse thing to
		// ship than an untested ordering with a reason written beside it.
		next := db.waiting()

		sent, more, err := s.drain(db, out, id, cursor)
		if err != nil {
			body := failure(err)
			_ = out.send(protocol.Frame{Type: protocol.Failure, ID: id}, body)
			return
		}
		if sent > cursor {
			cursor = sent
		}

		// A full batch means there is more of it. Waiting for the next commit
		// here would stall a subscriber catching up on a long log until
		// somebody happened to write again — which on a database being read
		// from and no longer written to is never.
		if more {
			select {
			case <-done:
				return
			default:
				continue
			}
		}

		select {
		case <-done:
			return
		case <-next:
		}
	}
}

// drain sends what is there now, and returns the entry to carry on from.
func (s *Server) drain(db *database, out *sender, id uint32, from uint64) (uint64, bool, error) {
	const batchSize = 512

	db.mutex.Lock()
	follow, err := db.store.CanFollow(from)
	if err != nil {
		db.mutex.Unlock()
		return from, false, err
	}
	if !follow {
		oldest, _ := db.store.OldestLSN()
		db.mutex.Unlock()
		return from, false, fmt.Errorf("%w: the log starts at %d and you are at %d", ErrTooFarBehind, oldest, from)
	}

	// Collected under the lock and sent outside it. Writing to a socket while
	// holding the database would let one slow reader stop every writer.
	var batch []store.Change
	err = db.store.Changes(from, func(change store.Change) bool {
		batch = append(batch, change)
		return len(batch) < batchSize
	})
	db.mutex.Unlock()
	if err != nil {
		return from, false, err
	}

	cursor := from
	for _, change := range batch {
		body, err := json.Marshal(change)
		if err != nil {
			return cursor, false, err
		}
		if err := out.send(protocol.Frame{Type: protocol.Event, ID: id}, body); err != nil {
			return cursor, false, err
		}
		cursor = change.LSN + 1
	}
	return cursor, len(batch) == batchSize, nil
}
