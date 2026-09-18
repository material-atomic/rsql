package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sapedb/sapedb/internal/btree"
	"github.com/sapedb/sapedb/internal/keys"
)

// Collection is a declared collection: documents in primary-key order, and the
// indexes declared on them.
type Collection struct {
	store *Store
	spec  Spec
}

// Spec is the declaration this collection is running.
func (c *Collection) Spec() Spec { return c.spec }

// into is the tree this key's document lives in.
//
// For a collection nobody divided that is the one tree there is. For a
// partitioned one it is decided by the key alone, which is why the key is the
// only thing a partition may be decided by: every read, write and delete
// already has it, and none of them has to go looking first.
func (c *Collection) into(key any) (*btree.Tree, error) {
	if c.spec.Partition == nil {
		return c.store.tree, nil
	}
	name, err := c.spec.Partition.name(c.spec.Name, key)
	if err != nil {
		return nil, err
	}

	tree, err := c.store.Part(name)
	if err != nil {
		return nil, err
	}

	// A new partition is what ages a collection. Checked here rather than on a
	// timer, because a database nobody has written to for a year should not
	// lose a year of history the moment somebody opens it — what makes data
	// old is new data arriving.
	if err := c.expire(); err != nil {
		return nil, err
	}
	return tree, nil
}

// expire drops the partitions that have fallen out of what is kept.
func (c *Collection) expire() error {
	if c.spec.Partition == nil || c.spec.Partition.Keep <= 0 {
		return nil
	}

	mine := []string{}
	for _, name := range c.store.Parts() {
		if strings.HasPrefix(name, c.spec.Name+"-") {
			mine = append(mine, name)
		}
	}

	for _, old := range c.spec.Partition.expired(mine) {
		if err := c.store.DropPart(old); err != nil {
			return err
		}
	}
	return nil
}

// across is every tree this collection lives in, oldest partition first.
//
// Partition names are written so that they sort in the order the periods
// happened, so this is also the order a scan walks them in — which is what
// makes concatenating the results of a scan across partitions come out in the
// right order. See the check in ops.go for when that is true.
func (c *Collection) across() ([]*btree.Tree, error) {
	if c.spec.Partition == nil {
		return []*btree.Tree{c.store.tree}, nil
	}

	trees := []*btree.Tree{}
	for _, name := range c.store.Parts() {
		if !strings.HasPrefix(name, c.spec.Name+"-") {
			continue
		}
		tree, err := c.store.Part(name)
		if err != nil {
			return nil, err
		}
		trees = append(trees, tree)
	}
	return trees, nil
}

// live refuses a handle to a collection that has been dropped. Writing through
// one would fill a keyspace nothing points at, and a later collection given the
// same id would inherit it.
func (c *Collection) live() error {
	if c.store.collections[c.spec.Name] != c {
		return fmt.Errorf("%w: %q was dropped", ErrNoCollection, c.spec.Name)
	}
	return nil
}

func (c *Collection) index(name string) (*Index, bool) {
	for i := range c.spec.Indexes {
		if c.spec.Indexes[i].Name == name {
			return &c.spec.Indexes[i], true
		}
	}
	return nil, false
}

// Put stores a document, replacing whatever was under the same primary key,
// and returns that key.
func (c *Collection) Put(document map[string]any) (any, error) {
	return c.write(Attribution{}, document, true)
}

// PutBy is Put with a record of who did it and why, which is what ends up in
// the change log.
func (c *Collection) PutBy(by Attribution, document map[string]any) (any, error) {
	return c.write(by, document, true)
}

// write stores a document, and — unless this is a replay of somebody else's
// log — records that it did.
//
// The document, every index entry that describes it and the log entry that
// announces it are one write. Any two of the three without the third is worse
// than none of them: an index entry with no document makes a query return
// something that is not there, and a log that missed a change makes every
// replica of this database quietly wrong.
func (c *Collection) write(by Attribution, document map[string]any, record bool) (any, error) {
	if err := c.live(); err != nil {
		return nil, err
	}

	key, found := document[c.spec.Key.Path]
	if !found || key == nil {
		if c.spec.Key.Auto != "ulid" {
			return nil, fmt.Errorf("%w: %q has no %q", ErrNoKey, c.spec.Name, c.spec.Key.Path)
		}
		generated, err := c.store.ids.Next()
		if err != nil {
			return nil, err
		}
		// Written into the document, not just used as its address: a document
		// that does not carry its own key is one a reader cannot refer back to.
		document[c.spec.Key.Path] = generated
		key = generated
	}

	stored, err := c.encodeKey(key)
	if err != nil {
		return nil, err
	}

	tree, err := c.into(key)
	if err != nil {
		return nil, err
	}

	previous, replaced, err := c.read(tree, stored)
	if err != nil {
		return nil, err
	}

	// Worked out before anything is written, so that a document the indexes
	// cannot describe is refused whole rather than half stored.
	additions, err := c.entriesFor(document, key)
	if err != nil {
		return nil, err
	}
	if err := c.checkUnique(tree, additions, stored); err != nil {
		return nil, err
	}

	if replaced {
		removals, err := c.entriesFor(previous, key)
		if err != nil {
			return nil, err
		}
		for _, entry := range removals {
			if _, err := tree.Delete(entry.key); err != nil {
				return nil, err
			}
		}
	}

	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("sapedb/store: this document cannot be stored: %w", err)
	}
	// The totals move in the same transaction as the document. A counter kept
	// anywhere else is a counter that stops matching the data the first time
	// anything goes wrong between the two, which is the commonest data bug
	// there is.
	if replaced {
		if err := c.contribute(tree, previous, -1); err != nil {
			return nil, err
		}
	}
	if err := c.contribute(tree, document, 1); err != nil {
		return nil, err
	}

	if err := tree.Put(stored, encoded); err != nil {
		return nil, err
	}
	for _, entry := range additions {
		if err := tree.Put(entry.key, entry.value); err != nil {
			return nil, err
		}
	}

	if record {
		if _, err := c.store.record(Change{
			Kind: ChangePut, Collection: c.spec.Name, Key: key, Document: document, By: by,
		}); err != nil {
			return nil, err
		}
	}

	return key, nil
}

// Get is the document under a primary key.
func (c *Collection) Get(key any) (map[string]any, bool, error) {
	stored, err := c.encodeKey(key)
	if err != nil {
		return nil, false, err
	}
	tree, err := c.into(key)
	if err != nil {
		return nil, false, err
	}
	return c.read(tree, stored)
}

// Delete removes a document and everything the indexes say about it.
func (c *Collection) Delete(key any) (bool, error) {
	return c.remove(Attribution{}, key, true)
}

// DeleteBy is Delete with a record of who did it and why.
func (c *Collection) DeleteBy(by Attribution, key any) (bool, error) {
	return c.remove(by, key, true)
}

func (c *Collection) remove(by Attribution, key any, record bool) (bool, error) {
	if err := c.live(); err != nil {
		return false, err
	}

	stored, err := c.encodeKey(key)
	if err != nil {
		return false, err
	}

	tree, err := c.into(key)
	if err != nil {
		return false, err
	}

	document, found, err := c.read(tree, stored)
	if err != nil || !found {
		return false, err
	}

	removals, err := c.entriesFor(document, key)
	if err != nil {
		return false, err
	}
	for _, entry := range removals {
		if _, err := tree.Delete(entry.key); err != nil {
			return false, err
		}
	}

	if err := c.contribute(tree, document, -1); err != nil {
		return false, err
	}

	// The document was read above, so this removes it: a key that was not there
	// returned before any of this.
	if _, err := tree.Delete(stored); err != nil {
		return false, err
	}

	if record {
		if _, err := c.store.record(Change{
			Kind: ChangeDelete, Collection: c.spec.Name, Key: key, By: by,
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (c *Collection) read(tree *btree.Tree, stored []byte) (map[string]any, bool, error) {
	value, found, err := tree.Get(stored)
	if err != nil || !found {
		return nil, false, err
	}

	document := map[string]any{}
	if err := json.Unmarshal(value, &document); err != nil {
		return nil, false, fmt.Errorf("%w: a document of %q: %v", ErrDamaged, c.spec.Name, err)
	}
	return document, true, nil
}

// entry is one index entry: where it goes and what it carries.
type entry struct {
	key   []byte
	value []byte
	// index and prefix are kept for the uniqueness check, which has to look at
	// the field values without the primary key on the end.
	index  *Index
	prefix int
}

// entriesFor is every index entry that describes a document.
func (c *Collection) entriesFor(document map[string]any, key any) ([]entry, error) {
	var all []entry

	for i := range c.spec.Indexes {
		index := &c.spec.Indexes[i]
		made, err := c.entriesForIndex(document, key, index)
		if err != nil {
			return nil, err
		}
		all = append(all, made...)
	}
	return all, nil
}

func (c *Collection) entriesForIndex(document map[string]any, key any, index *Index) ([]entry, error) {
	values := make([]any, len(index.Fields))
	spread := -1

	for i, field := range index.Fields {
		value, found := at(document, field.Path)
		if !found {
			if field.Missing == MissingSkip {
				// The index was told not to hold documents without this field.
				return nil, nil
			}
			values[i] = keys.Absent
			continue
		}
		if index.Array == field.Path {
			spread = i
			values[i] = value
			continue
		}
		if !matches(field.Type, value) {
			return nil, fmt.Errorf("%w: index %q of %q wants %s at %q, and this is %T",
				ErrType, index.Name, c.spec.Name, field.Type, field.Path, value)
		}
		values[i] = value
	}

	// One entry per element of the spread field. A document with no elements
	// is in no entry at all: there is nothing to look it up by.
	elements := []any{nil}
	if spread >= 0 {
		elements = spreadOf(values[spread])
		field := index.Fields[spread]
		for _, element := range elements {
			if !matches(field.Type, element) {
				return nil, fmt.Errorf("%w: index %q of %q wants %s in %q, and this is %T",
					ErrType, index.Name, c.spec.Name, field.Type, field.Path, element)
			}
		}
	}

	carried, err := c.include(document, index)
	if err != nil {
		return nil, err
	}

	made := make([]entry, 0, len(elements))
	seen := map[string]bool{}

	for _, element := range elements {
		if spread >= 0 {
			values[spread] = element
		}

		prefix := c.entries(*index)
		encoded, err := keys.EncodeKey(prefix, values, encodings(index.Fields))
		if err != nil {
			return nil, fmt.Errorf("index %q of %q: %w", index.Name, c.spec.Name, err)
		}
		length := len(encoded)

		encoded, err = keys.Encode(encoded, key, keys.Field{})
		if err != nil {
			return nil, err
		}

		// The same element twice in one array encodes to the same key, so the
		// tree would hold it once whatever happened here. Skipping it saves the
		// write rather than preventing a duplicate — the key doing that.
		if seen[string(encoded)] {
			continue
		}
		seen[string(encoded)] = true

		made = append(made, entry{key: encoded, value: carried, index: index, prefix: length})
	}
	return made, nil
}

// checkUnique refuses a write that would put a second document under a value a
// unique index already holds.
func (c *Collection) checkUnique(tree *btree.Tree, additions []entry, stored []byte) error {
	for _, addition := range additions {
		if !addition.index.Unique {
			continue
		}

		prefix := addition.key[:addition.prefix]
		var clash []byte

		err := tree.Ascend(prefix, func(key, _ []byte) bool {
			if !bytes.HasPrefix(key, prefix) {
				return false
			}
			// The document being replaced is allowed to hold its own value.
			if bytes.Equal(key[addition.prefix:], stored[len(c.documents()):]) {
				return true
			}
			clash = append([]byte(nil), key...)
			return false
		})
		if err != nil {
			return err
		}

		if clash != nil {
			values, _, err := keys.DecodeKey(clash[len(c.entries(*addition.index)):], encodings(addition.index.Fields))
			if err != nil {
				return fmt.Errorf("%w: index %q of %q", ErrDuplicate, addition.index.Name, c.spec.Name)
			}
			return fmt.Errorf("%w: index %q of %q already holds %v", ErrDuplicate, addition.index.Name, c.spec.Name, values)
		}
	}
	return nil
}

func (c *Collection) include(document map[string]any, index *Index) ([]byte, error) {
	if len(index.Include) == 0 {
		return nil, nil
	}

	carried := map[string]any{}
	for _, path := range index.Include {
		if value, found := at(document, path); found {
			carried[path] = value
		}
	}
	return json.Marshal(carried)
}

// buildIndex fills a new index from the documents already stored.
func (c *Collection) buildIndex(index Index) error {
	trees, err := c.across()
	if err != nil {
		return err
	}
	for _, tree := range trees {
		if err := c.buildIndexIn(tree, index); err != nil {
			return err
		}
	}
	return nil
}

// buildIndexIn fills a new index from the documents in one tree. An index is
// local to its partition, so building one is this, once per partition.
func (c *Collection) buildIndexIn(tree *btree.Tree, index Index) error {
	prefix := c.documents()
	var made []entry

	err := tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		document := map[string]any{}
		if err := json.Unmarshal(value, &document); err != nil {
			return false
		}
		entries, err := c.entriesForIndex(document, document[c.spec.Key.Path], &index)
		if err != nil {
			return false
		}
		made = append(made, entries...)
		return true
	})
	if err != nil {
		return err
	}

	for _, one := range made {
		if index.Unique {
			// The key of the document this entry describes, as a document key,
			// so that the check can tell "already there" from "this one".
			own := append(c.documents(), one.key[one.prefix:]...)
			if err := c.checkUnique(tree, []entry{one}, own); err != nil {
				return err
			}
		}
		if err := tree.Put(one.key, one.value); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collection) dropIndex(index Index) error {
	trees, err := c.across()
	if err != nil {
		return err
	}
	for _, tree := range trees {
		if err := deleteRange(tree, c.entries(index)); err != nil {
			return err
		}
	}
	return nil
}

func encodings(fields []Field) []keys.Field {
	encoded := make([]keys.Field, len(fields))
	for i, field := range fields {
		encoded[i] = field.encoding()
	}
	return encoded
}

// at reads a dotted path out of a document.
func at(document map[string]any, path string) (any, bool) {
	current := any(document)

	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, found := object[part]
		if !found {
			return nil, false
		}
		current = value
	}
	return current, true
}

// spreadOf is the elements an array field contributes.
//
// A value that is not an array contributes itself, so a field that is
// sometimes a list and sometimes a single value is still findable either way —
// which is how documents actually arrive.
func spreadOf(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return []any{value}
}
