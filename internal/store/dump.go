package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/material-atomic/rsql/internal/btree"
	"github.com/material-atomic/rsql/internal/pager"
)

// A dump is the database written out as itself: declarations, operations and
// documents, one JSON object to a line.
//
// It is taken from a snapshot, so nothing is locked and the writer carries on
// while it is written. Copy-on-write already keeps the pages of a committed
// state intact for as long as somebody is holding it, which is exactly what a
// consistent backup needs and is the reason a snapshot costs nothing to take.
//
// The line that matters most is the last one. A dump that was cut short — the
// disk filled, the pipe broke, the process was killed — looks exactly like a
// dump of a smaller database, and restoring one of those loses data in a way
// that nothing afterwards can detect. So a dump ends with a count and the
// number of the change it was taken at, and a restore that does not reach that
// line refuses the whole file.
//
// That number is also what makes a restore useful rather than merely a copy: a
// replica restored from a dump knows exactly where to pick the log up, so the
// two together cover a replica that has fallen further behind than the log
// keeps.
const dumpFormat = 1

// Line kinds, in the order they appear.
const (
	lineHeader     = "header"
	lineCollection = "collection"
	lineOperation  = "operation"
	lineDocument   = "document"
	lineEnd        = "end"
)

var (
	ErrDumpFormat     = errors.New("rsql/store: this is not a dump this build can read")
	ErrDumpIncomplete = errors.New("rsql/store: the dump ends before it says it does")
	ErrNotEmpty       = errors.New("rsql/store: a restore needs a database with nothing in it")
)

type line struct {
	Kind       string         `json:"kind"`
	Format     int            `json:"format,omitempty"`
	LSN        uint64         `json:"lsn,omitempty"`
	At         int64          `json:"at,omitempty"`
	Collection string         `json:"collection,omitempty"`
	Spec       *Spec          `json:"spec,omitempty"`
	Operation  *Operation     `json:"operation,omitempty"`
	Document   map[string]any `json:"document,omitempty"`
	Documents  int            `json:"documents,omitempty"`
}

// Snapshot is the database as one transaction left it, held open.
//
// While it is held, the pages that made up that state are not reused, so
// everything read through it agrees with everything else read through it
// however much the writer does in the meantime. Releasing it is not optional:
// an unreleased snapshot is a database that cannot reclaim anything.
type Snapshot struct {
	view *Store
	pin  *pager.Snapshot
	lsn  uint64
}

// Take holds the database as it stands at the last commit.
//
// Work that has not been committed is not in it. A snapshot of a half-finished
// transaction is not a state the database was ever in.
func (s *Store) Take() (*Snapshot, error) {
	pin := s.pages.Snapshot()

	view := &Store{
		tree:        btree.At(s.pages, pin.Root),
		collections: map[string]*Collection{},
		ids:         s.ids,
		now:         s.now,
	}
	if err := view.load(); err != nil {
		pin.Release()
		return nil, err
	}

	lsn, err := view.latestLSN()
	if err != nil {
		pin.Release()
		return nil, err
	}
	return &Snapshot{view: view, pin: pin, lsn: lsn}, nil
}

// LSN is the last change in this snapshot. A replica restored from it carries
// on from the next one.
func (n *Snapshot) LSN() uint64 { return n.lsn }

// Collections is what the snapshot holds.
func (n *Snapshot) Collections() []string { return n.view.Collections() }

// Walk reads a collection as it was, in primary-key order.
func (n *Snapshot) Walk(name string, visit func(key any, document map[string]any) bool) error {
	collection, err := n.view.Collection(name)
	if err != nil {
		return err
	}
	return collection.Walk(visit)
}

// Release lets the snapshot go.
func (n *Snapshot) Release() {
	if n.pin != nil {
		n.pin.Release()
		n.pin = nil
	}
}

// Dump writes the database out from a snapshot taken now.
func (s *Store) Dump(out io.Writer) (uint64, error) {
	snapshot, err := s.Take()
	if err != nil {
		return 0, err
	}
	defer snapshot.Release()

	return snapshot.lsn, snapshot.Dump(out)
}

// Dump writes this snapshot out.
func (n *Snapshot) Dump(out io.Writer) error {
	buffered := bufio.NewWriter(out)
	encoder := json.NewEncoder(buffered)

	write := func(one line) error { return encoder.Encode(one) }

	if err := write(line{
		Kind: lineHeader, Format: dumpFormat, LSN: n.lsn, At: n.view.now().UnixMilli(),
	}); err != nil {
		return err
	}

	// Declarations first: a document cannot be restored into a collection that
	// has not been declared, and an operation names a collection.
	names := n.view.Collections()
	for _, name := range names {
		collection, err := n.view.Collection(name)
		if err != nil {
			return err
		}
		spec := collection.Spec()
		if err := write(line{Kind: lineCollection, Collection: name, Spec: &spec}); err != nil {
			return err
		}
	}

	operations, err := n.view.everyOperation()
	if err != nil {
		return err
	}
	for i := range operations {
		if err := write(line{Kind: lineOperation, Operation: &operations[i]}); err != nil {
			return err
		}
	}

	documents := 0
	for _, name := range names {
		collection, err := n.view.Collection(name)
		if err != nil {
			return err
		}
		var failed error
		if err := collection.Walk(func(_ any, document map[string]any) bool {
			if err := write(line{Kind: lineDocument, Collection: name, Document: document}); err != nil {
				failed = err
				return false
			}
			documents++
			return true
		}); err != nil {
			return err
		}
		if failed != nil {
			return failed
		}
	}

	if err := write(line{Kind: lineEnd, LSN: n.lsn, Documents: documents}); err != nil {
		return err
	}
	return buffered.Flush()
}

// Restore reads a dump into a database with nothing in it, and returns the
// change it was taken at.
//
// It must be empty: restoring over a database that already holds something
// would leave a mixture of two, which is neither of them and looks like a
// working database.
func (s *Store) Restore(in io.Reader) (uint64, error) {
	// The log is what says a database has been used: declaring a collection is
	// itself a change, so a database with anything in it — or with anything
	// that has been in it — is at a change past zero.
	latest, err := s.latestLSN()
	if err != nil {
		return 0, err
	}
	if latest != 0 {
		return 0, fmt.Errorf("%w: it is at change %d and holds %v", ErrNotEmpty, latest, s.Collections())
	}

	reader := bufio.NewReaderSize(in, 1<<16)
	decoder := json.NewDecoder(reader)

	first := line{}
	if err := decoder.Decode(&first); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrDumpFormat, err)
	}
	if first.Kind != lineHeader || first.Format != dumpFormat {
		return 0, fmt.Errorf("%w: it starts %q at format %d", ErrDumpFormat, first.Kind, first.Format)
	}

	documents := 0
	finished := false
	lsn := first.LSN

	for {
		one := line{}
		err := decoder.Decode(&one)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrDumpFormat, err)
		}
		if finished {
			return 0, fmt.Errorf("%w: there is more after the end", ErrDumpFormat)
		}

		switch one.Kind {
		case lineCollection:
			if one.Spec == nil {
				return 0, fmt.Errorf("%w: a collection with no declaration", ErrDumpFormat)
			}
			if err := s.install(*one.Spec); err != nil {
				return 0, err
			}

		case lineOperation:
			if one.Operation == nil {
				return 0, fmt.Errorf("%w: an operation with nothing in it", ErrDumpFormat)
			}
			encoded, err := json.Marshal(*one.Operation)
			if err != nil {
				return 0, err
			}
			if err := s.tree.Put(operationKey(one.Operation.Name, one.Operation.Version), encoded); err != nil {
				return 0, err
			}

		case lineDocument:
			collection, err := s.Collection(one.Collection)
			if err != nil {
				return 0, err
			}
			if _, err := collection.write(Attribution{}, one.Document, false); err != nil {
				return 0, err
			}
			documents++

		case lineEnd:
			if one.Documents != documents {
				return 0, fmt.Errorf("%w: it says %d documents and holds %d", ErrDumpIncomplete, one.Documents, documents)
			}
			if one.LSN != lsn {
				return 0, fmt.Errorf("%w: it starts at change %d and ends at %d", ErrDumpFormat, lsn, one.LSN)
			}
			finished = true

		default:
			return 0, fmt.Errorf("%w: a line of kind %q", ErrDumpFormat, one.Kind)
		}
	}

	if !finished {
		return 0, fmt.Errorf("%w: %d documents read and no end", ErrDumpIncomplete, documents)
	}

	// The log carries on from where the dump was taken, so following it from
	// here neither repeats nor skips anything.
	if err := s.setLSN(lsn); err != nil {
		return 0, err
	}
	return lsn, nil
}

// everyOperation is all versions of every operation, oldest first.
func (s *Store) everyOperation() ([]Operation, error) {
	prefix := []byte{spaceOps}
	var all []Operation

	err := s.tree.Ascend(prefix, func(key, value []byte) bool {
		if len(key) == 0 || key[0] != spaceOps {
			return false
		}
		operation := Operation{}
		if err := json.Unmarshal(value, &operation); err != nil {
			return false
		}
		all = append(all, operation)
		return true
	})
	return all, err
}
