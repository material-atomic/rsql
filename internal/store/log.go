package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The change log is what makes a database more than its current contents.
//
// Every change is written into it in the same transaction as the change
// itself, which is the whole point: a log kept beside the data can disagree
// with it, and a log that disagrees with the data is worse than no log,
// because everything built on it — a replica, a restore, a feed of changes,
// an audit trail — is then confidently wrong. Here the two are one write.
//
// It is logical rather than physical: an entry says "this collection, this
// key, this document" and not "this page, these bytes". So it can be replayed
// into a database whose pages went a different way, which is what replication
// and point-in-time restore actually need.
//
// Entries are numbered in order and applying one twice is the same as applying
// it once — a replica that loses its place and rewinds is a normal event, not
// an emergency.
const (
	spaceLog    byte = 0x05 // 0x05 | lsn -> the entry
	spaceWrites byte = 0x06 // 0x06 | write id -> what that write did
)

// What an entry says happened.
const (
	ChangePut       = "put"
	ChangeDelete    = "delete"
	ChangeDeclare   = "declare"
	ChangeOperation = "operation"
	ChangeDrop      = "drop"
	// ChangeRead is an operator looking at something through the shell. It
	// changes nothing, which is why it is here: a read that leaves no trace is
	// a read nobody can be asked about afterwards.
	ChangeRead = "read"
)

var nextLSN = []byte{spaceMeta, 'l', 's', 'n'}

// ErrOutOfOrder is a replay that skipped or repeated the wrong entry.
var ErrOutOfOrder = errors.New("sapedb/store: this entry does not follow the one before it")

// Attribution is who made a change and why: which declared operation, which
// version of it, on whose behalf, and under which write id.
//
// It is recorded with the change rather than in a separate audit file, because
// an audit trail kept separately is one that can be missing an entry the data
// says must exist.
type Attribution struct {
	Operation string `json:"operation,omitempty"`
	Version   int    `json:"version,omitempty"`
	Actor     string `json:"actor,omitempty"`
	// WriteID is the caller's own id for this write. A write that arrives
	// twice under the same id is applied once.
	WriteID string `json:"write_id,omitempty"`
}

// Change is one entry of the log.
type Change struct {
	LSN  uint64 `json:"lsn"`
	At   int64  `json:"at,omitempty"`
	Kind string `json:"kind"`

	Collection string         `json:"collection,omitempty"`
	Key        any            `json:"key,omitempty"`
	Document   map[string]any `json:"document,omitempty"`

	Spec      *Spec       `json:"spec,omitempty"`
	Operation *Operation  `json:"operation,omitempty"`
	By        Attribution `json:"by,omitempty"`
}

// done is what a write id records, so that the same write arriving twice is
// answered rather than repeated.
type done struct {
	LSN     uint64 `json:"lsn"`
	Key     any    `json:"key,omitempty"`
	Changed int    `json:"changed,omitempty"`
}

// Clock sets where entry timestamps come from.
func (s *Store) Clock(now func() time.Time) { s.now = now }

// Retain caps the log at a number of entries, dropping the oldest as new ones
// arrive. Zero keeps everything.
//
// A cap is not optional in practice: a log nobody trims is a disk that fills
// up while everything looks healthy. What it costs is that a replica which
// falls further behind than the cap cannot catch up from the log and has to be
// rebuilt from a dump — which is why the cap is a number somebody chooses
// rather than one this code picks.
func (s *Store) Retain(entries int) { s.retain = entries }

// record appends an entry. Called from inside the write it describes, so the
// two are one transaction.
func (s *Store) record(change Change) (uint64, error) {
	lsn, err := s.takeLSN()
	if err != nil {
		return 0, err
	}
	change.LSN = lsn
	if change.At == 0 {
		change.At = s.now().UnixMilli()
	}

	encoded, err := json.Marshal(change)
	if err != nil {
		return 0, err
	}
	if err := s.tree.Put(logKey(lsn), encoded); err != nil {
		return 0, err
	}
	if change.By.WriteID != "" {
		record, err := json.Marshal(done{LSN: lsn, Key: change.Key, Changed: 1})
		if err != nil {
			return 0, err
		}
		if err := s.tree.Put(writeKey(change.By.WriteID), record); err != nil {
			return 0, err
		}
	}

	return lsn, s.trim()
}

// trim drops the oldest entries once there are more than the cap allows.
func (s *Store) trim() error {
	if s.retain <= 0 {
		return nil
	}

	latest, err := s.latestLSN()
	if err != nil {
		return err
	}
	if latest <= uint64(s.retain) {
		return nil
	}
	until := latest - uint64(s.retain)

	prefix := []byte{spaceLog}
	var doomed []Change

	err = s.tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		change := Change{}
		if err := json.Unmarshal(value, &change); err != nil {
			return false
		}
		if change.LSN > until {
			return false
		}
		doomed = append(doomed, change)
		return true
	})
	if err != nil {
		return err
	}

	for _, change := range doomed {
		if _, err := s.tree.Delete(logKey(change.LSN)); err != nil {
			return err
		}
		// The write id goes with the entry it belongs to. A retry that arrives
		// after its entry has aged out is applied again — which is why the
		// retention window has to outlast whatever the client retries for.
		if change.By.WriteID != "" {
			if _, err := s.tree.Delete(writeKey(change.By.WriteID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Changes walks the log from an entry onwards, in order.
func (s *Store) Changes(from uint64, visit func(Change) bool) error {
	prefix := []byte{spaceLog}

	return s.tree.Ascend(logKey(from), func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		change := Change{}
		if err := json.Unmarshal(value, &change); err != nil {
			return false
		}
		return visit(change)
	})
}

// LatestLSN is the last entry written, or zero for a database that has never
// been written to.
func (s *Store) LatestLSN() (uint64, error) { return s.latestLSN() }

// OldestLSN is the earliest entry still kept. A subscriber asking for anything
// before this has fallen too far behind to catch up from the log.
func (s *Store) OldestLSN() (uint64, error) {
	prefix := []byte{spaceLog}
	oldest := uint64(0)

	err := s.tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) || len(key) != 9 {
			return false
		}
		oldest = binary.BigEndian.Uint64(key[1:])
		return false
	})
	return oldest, err
}

// CanFollow says whether a consumer that has seen everything up to `from`-1
// can carry on from the log, or has fallen so far behind that the entries it
// needs have been trimmed and it must be rebuilt from a dump.
//
// This is a question worth one call rather than a comparison everyone repeats:
// to follow from an entry, that entry must still be there, and getting the
// off-by-one wrong means a replica that silently skips a change.
func (s *Store) CanFollow(from uint64) (bool, error) {
	oldest, err := s.OldestLSN()
	if err != nil {
		return false, err
	}
	if oldest == 0 {
		// Nothing is kept, so there is nothing to miss: a consumer is up to
		// date exactly when it has seen everything written.
		latest, err := s.latestLSN()
		return from > latest, err
	}
	return from >= oldest, nil
}

// Wrote reports what a write id already did, for a caller retrying a write it
// is not sure landed.
func (s *Store) Wrote(id string) (uint64, any, bool, error) {
	if id == "" {
		return 0, nil, false, nil
	}
	value, found, err := s.tree.Get(writeKey(id))
	if err != nil || !found {
		return 0, nil, false, err
	}

	record := done{}
	if err := json.Unmarshal(value, &record); err != nil {
		return 0, nil, false, fmt.Errorf("%w: the record of write %q: %v", ErrDamaged, id, err)
	}
	return record.LSN, record.Key, true, nil
}

// Apply puts an entry from another database into this one, keeping its number
// so that this copy's log is the same log.
//
// Applying the same entry twice does nothing the first time did not already
// do, and an entry out of order is refused rather than left as a hole: a
// replica with a gap in it is a replica that is wrong in a way nothing will
// notice later.
func (s *Store) Apply(change Change) error {
	latest, err := s.latestLSN()
	if err != nil {
		return err
	}
	if change.LSN <= latest {
		return nil // already here
	}
	if change.LSN != latest+1 {
		return fmt.Errorf("%w: entry %d after %d", ErrOutOfOrder, change.LSN, latest)
	}

	switch change.Kind {
	case ChangePut:
		collection, err := s.Collection(change.Collection)
		if err != nil {
			return err
		}
		if _, err := collection.write(change.By, change.Document, false); err != nil {
			return err
		}

	case ChangeDelete:
		collection, err := s.Collection(change.Collection)
		if err != nil {
			return err
		}
		if _, err := collection.remove(change.By, change.Key, false); err != nil {
			return err
		}

	case ChangeDeclare:
		if change.Spec == nil {
			return fmt.Errorf("%w: entry %d declares nothing", ErrDamaged, change.LSN)
		}
		if err := s.install(*change.Spec); err != nil {
			return err
		}

	case ChangeOperation:
		if change.Operation == nil {
			return fmt.Errorf("%w: entry %d declares no operation", ErrDamaged, change.LSN)
		}
		encoded, err := json.Marshal(*change.Operation)
		if err != nil {
			return err
		}
		if err := s.tree.Put(operationKey(change.Operation.Name, change.Operation.Version), encoded); err != nil {
			return err
		}

	case ChangeDrop:
		if err := s.drop(change.Collection, false); err != nil && !errors.Is(err, ErrNoCollection) {
			return err
		}

	case ChangeRead:
		// Nothing to apply — somebody looked. The entry is still written into
		// the replica below, because an audit trail that stops at the primary
		// is one you cannot read from anywhere the primary is not.

	default:
		return fmt.Errorf("%w: entry %d is a %q", ErrDamaged, change.LSN, change.Kind)
	}

	// The entry keeps its own number and time, so that a replica can serve the
	// same log to whatever reads from it.
	encoded, err := json.Marshal(change)
	if err != nil {
		return err
	}
	if err := s.tree.Put(logKey(change.LSN), encoded); err != nil {
		return err
	}
	if err := s.setLSN(change.LSN); err != nil {
		return err
	}
	return s.trim()
}

func (s *Store) takeLSN() (uint64, error) {
	latest, err := s.latestLSN()
	if err != nil {
		return 0, err
	}
	next := latest + 1
	return next, s.setLSN(next)
}

func (s *Store) latestLSN() (uint64, error) {
	value, found, err := s.tree.Get(nextLSN)
	if err != nil || !found {
		return 0, err
	}
	if len(value) != 8 {
		return 0, fmt.Errorf("%w: the log counter is %d bytes", ErrDamaged, len(value))
	}
	return binary.BigEndian.Uint64(value), nil
}

func (s *Store) setLSN(lsn uint64) error {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], lsn)
	return s.tree.Put(nextLSN, raw[:])
}

func logKey(lsn uint64) []byte {
	key := make([]byte, 9)
	key[0] = spaceLog
	binary.BigEndian.PutUint64(key[1:], lsn)
	return key
}

func writeKey(id string) []byte {
	return append([]byte{spaceWrites}, id...)
}
