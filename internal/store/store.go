package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/material-atomic/rsql/internal/btree"
	"github.com/material-atomic/rsql/internal/keys"
	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/ulid"
)

// What a key starts with says what it is. One tree holds all of them, so a
// document and the index entries that describe it are written in one
// transaction and can never disagree.
const (
	spaceMeta    byte = 0x00 // 0x00 | name           -> a number the store keeps
	spaceCatalog byte = 0x01 // 0x01 | name           -> a collection descriptor
	spaceDoc     byte = 0x02 // 0x02 | id | key       -> the document
	spaceIndex   byte = 0x03 // 0x03 | id | index | fields | key -> the covered fields
)

// nextCollection is where the counter of collection ids lives.
var nextCollection = []byte{spaceMeta, 'c', 'o', 'l', 'l'}

// Store is a database: its catalogue, its documents and its indexes.
//
// One writer at a time, which is what the tree underneath allows. Readers take
// a snapshot and are unaffected by anything written afterwards.
type Store struct {
	pages       *pager.Pager
	tree        *btree.Tree
	collections map[string]*Collection
	ids         *ulid.Source
	now         func() time.Time
	// retain is how many log entries to keep; zero keeps all of them.
	retain int

	// files, parts, dropped and key are partitions: where other files come
	// from, the ones open now, the ones this transaction dropped, and the key
	// a new one is made under. See parts.go.
	files Files
	parts map[string]*part
	// dropping is a partition whose row is gone but whose file is still here,
	// because the transaction that dropped it has not landed yet.
	dropping map[string]*part
	dropped  []string
	key      []byte
}

// Open reads the catalogue of an existing database, or starts an empty one.
func Open(pages *pager.Pager) (*Store, error) {
	store := &Store{
		pages:       pages,
		tree:        btree.New(pages),
		collections: map[string]*Collection{},
		parts:       map[string]*part{},
		dropping:    map[string]*part{},
		ids:         ulid.New(),
		now:         time.Now,
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// Identifiers lets a caller — a test, mostly — decide where generated primary
// keys come from.
func (s *Store) Identifiers(source *ulid.Source) { s.ids = source }

// Commit makes everything written since the last one durable.
//
// Partitions first and the leader last, always. Their pages have to be on the
// disk before anything says they are part of the database, and the leader's
// meta is the only thing that says so.
func (s *Store) Commit() error {
	if err := s.commitParts(); err != nil {
		return err
	}
	if err := s.tree.Commit(); err != nil {
		return err
	}

	// Only now: a file unlinked before the transaction that dropped it landed
	// is a file the database would still be pointing at after a crash.
	if len(s.dropped) > 0 {
		return s.dropFiles()
	}
	return nil
}

// Rollback throws away everything written since the last commit: the
// documents, the index entries, the log entries, the declarations.
//
// Every handle taken before this is refused afterwards rather than quietly
// writing into a collection that no longer exists in the shape it had. A
// caller that rolls back and carries on asks for what it needs again.
func (s *Store) Rollback() error {
	if err := s.pages.Rollback(); err != nil {
		return err
	}

	s.tree = btree.New(s.pages)
	s.collections = map[string]*Collection{}

	// After the leader, because where each partition belongs is written in the
	// leader's tree — asking before it was rolled back would be asking the
	// transaction being undone where to undo it to.
	if err := s.abandonParts(); err != nil {
		return err
	}
	return s.load()
}

func (s *Store) load() error {
	prefix := []byte{spaceCatalog}

	return s.tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		spec := &Spec{}
		if err := json.Unmarshal(value, spec); err != nil {
			return false
		}
		s.collections[spec.Name] = &Collection{store: s, spec: *spec}
		return true
	})
}

// Declare creates a collection, or brings an existing one up to date with the
// declaration.
//
// What can change and what cannot follows from what is already written down:
// the primary key cannot, because every document key is made of it. Indexes
// can be added, and are built over the documents already there; they can be
// dropped, and their entries go with them. An index that keeps its name but
// changes its shape is refused — that is two different indexes, and silently
// replacing one with the other would leave every reader of the old one wrong.
func (s *Store) Declare(spec Spec) (*Collection, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	existing, found := s.collections[spec.Name]
	if !found {
		id, err := s.takeCollectionID()
		if err != nil {
			return nil, err
		}
		spec.ID = id
		for i := range spec.Indexes {
			spec.Indexes[i].ID = uint16(i)
		}
		spec.NextIndexID = uint16(len(spec.Indexes))

		if err := s.install(spec); err != nil {
			return nil, err
		}
		if _, err := s.record(Change{Kind: ChangeDeclare, Collection: spec.Name, Spec: &spec}); err != nil {
			return nil, err
		}
		return s.collections[spec.Name], nil
	}

	if existing.spec.Key != spec.Key {
		return nil, fmt.Errorf("%w: the primary key of %q was declared %+v", ErrIncompatible, spec.Name, existing.spec.Key)
	}
	// How a collection is divided decides which file every document is in, so
	// changing it would mean moving all of them — which is a migration
	// somebody runs, not something a redeclaration does quietly.
	if !samePartition(existing.spec.Partition, spec.Partition) {
		return nil, fmt.Errorf("%w: %q is already divided %s; moving every document is not something redeclaring does",
			ErrIncompatible, spec.Name, describePartition(existing.spec.Partition))
	}

	updated := existing.spec
	updated.Indexes = nil
	wanted := map[string]bool{}

	for _, index := range spec.Indexes {
		wanted[index.Name] = true

		if before, ok := existing.index(index.Name); ok {
			if !sameShape(*before, index) {
				return nil, fmt.Errorf("%w: index %q of %q is already declared differently", ErrIncompatible, index.Name, spec.Name)
			}
			updated.Indexes = append(updated.Indexes, *before)
			continue
		}

		index.ID = updated.NextIndexID
		updated.NextIndexID++
		updated.Indexes = append(updated.Indexes, index)
	}

	if err := s.install(updated); err != nil {
		return nil, err
	}
	if _, err := s.record(Change{Kind: ChangeDeclare, Collection: updated.Name, Spec: &updated}); err != nil {
		return nil, err
	}
	return s.collections[updated.Name], nil
}

// install puts a declaration into effect: indexes that are new are built from
// the documents already stored, indexes that are gone take their entries with
// them. It assigns nothing — the numbers in the spec are the ones it is given,
// which is what lets a replica install exactly what the primary did.
//
// The collection object is updated in place rather than replaced. A handle
// somebody is holding must see the declaration that is now in force: one
// pointing at the old declaration would keep writing documents without
// entries in an index that exists, and nothing would say so until a query
// came back short.
func (s *Store) install(spec Spec) error {
	collection, found := s.collections[spec.Name]
	if !found {
		collection = &Collection{store: s, spec: spec}
		s.collections[spec.Name] = collection
	}

	previous := collection.spec
	collection.spec = spec

	if found {
		for _, before := range previous.Indexes {
			if _, still := collection.index(before.Name); !still {
				if err := collection.dropIndex(before); err != nil {
					collection.spec = previous
					return err
				}
			}
		}
		for _, index := range spec.Indexes {
			if declaredBefore(previous, index.Name) {
				continue
			}
			if err := collection.buildIndex(index); err != nil {
				collection.spec = previous
				return err
			}
		}
	}

	if err := s.writeSpec(spec); err != nil {
		collection.spec = previous
		return err
	}
	return nil
}

func declaredBefore(spec Spec, name string) bool {
	for _, index := range spec.Indexes {
		if index.Name == name {
			return true
		}
	}
	return false
}

// Collection is a collection that has been declared.
func (s *Store) Collection(name string) (*Collection, error) {
	collection, found := s.collections[name]
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrNoCollection, name)
	}
	return collection, nil
}

// Collections is every declared collection, by name.
func (s *Store) Collections() []string {
	names := make([]string, 0, len(s.collections))
	for name := range s.collections {
		names = append(names, name)
	}
	return names
}

// Drop removes a collection: its documents, its index entries and its
// declaration.
func (s *Store) Drop(name string) error { return s.drop(name, true) }

func (s *Store) drop(name string, record bool) error {
	collection, err := s.Collection(name)
	if err != nil {
		return err
	}

	for _, index := range collection.spec.Indexes {
		if err := collection.dropIndex(index); err != nil {
			return err
		}
	}
	if err := s.deleteRange(collection.documents()); err != nil {
		return err
	}
	if _, err := s.tree.Delete(catalogKey(name)); err != nil {
		return err
	}

	delete(s.collections, name)

	if record {
		if _, err := s.record(Change{Kind: ChangeDrop, Collection: name}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeSpec(spec Spec) error {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return s.tree.Put(catalogKey(spec.Name), encoded)
}

func (s *Store) takeCollectionID() (uint32, error) {
	next := uint32(1)
	if value, found, err := s.tree.Get(nextCollection); err != nil {
		return 0, err
	} else if found {
		if len(value) != 4 {
			return 0, fmt.Errorf("%w: the collection counter is %d bytes", ErrDamaged, len(value))
		}
		next = binary.BigEndian.Uint32(value)
	}

	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], next+1)
	if err := s.tree.Put(nextCollection, raw[:]); err != nil {
		return 0, err
	}
	return next, nil
}

// deleteRange removes every key that starts with a prefix.
//
// Collected first and deleted afterwards: deleting while walking would be
// changing the tree under the walk.
func (s *Store) deleteRange(prefix []byte) error {
	return deleteRange(s.tree, prefix)
}

// deleteRange removes everything under a prefix from one tree. Taken out of
// the Store because a partitioned collection has to do it once per partition,
// and a tree is what a partition is.
func deleteRange(tree *btree.Tree, prefix []byte) error {
	var doomed [][]byte

	err := tree.Ascend(prefix, func(key, _ []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		doomed = append(doomed, append([]byte(nil), key...))
		return true
	})
	if err != nil {
		return err
	}

	for _, key := range doomed {
		if _, err := tree.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func catalogKey(name string) []byte {
	return append([]byte{spaceCatalog}, name...)
}

// documents is the stretch of the tree holding this collection's documents.
func (c *Collection) documents() []byte {
	key := make([]byte, 5)
	key[0] = spaceDoc
	binary.BigEndian.PutUint32(key[1:], c.spec.ID)
	return key
}

// entries is the stretch holding one index.
func (c *Collection) entries(index Index) []byte {
	key := make([]byte, 7)
	key[0] = spaceIndex
	binary.BigEndian.PutUint32(key[1:], c.spec.ID)
	binary.BigEndian.PutUint16(key[5:], index.ID)
	return key
}

// encodeKey turns a primary key value into the bytes a document is stored at.
func (c *Collection) encodeKey(value any) ([]byte, error) {
	if !matches(c.spec.Key.Type, value) {
		return nil, fmt.Errorf("%w: the primary key of %q is %s, and this is %T", ErrType, c.spec.Name, c.spec.Key.Type, value)
	}
	return keys.Encode(c.documents(), value, keys.Field{})
}

// matches says whether a value is the type a field was declared as.
//
// Null is allowed everywhere: it is a value the document carries, as opposed
// to a field it does not have, and the two sort in different places. Refusing
// it would make "the field is explicitly nothing" unindexable.
func matches(declared string, value any) bool {
	if value == nil {
		return true
	}
	switch declared {
	case TypeAny:
		return true
	case TypeString:
		_, ok := value.(string)
		return ok
	case TypeNumber:
		switch value.(type) {
		case float64, float32, int, int64:
			return true
		}
		return false
	case TypeBool:
		_, ok := value.(bool)
		return ok
	}
	return false
}
