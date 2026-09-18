package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/sapedb/sapedb/internal/pager"
)

// underfullBytes is when a node is considered too empty to leave alone. A
// quarter of a page: low enough that ordinary deletions do not keep merging and
// splitting the same two nodes, high enough that a tree emptied from one end
// does not leave a chain of nearly empty pages behind.
const underfullBytes = capacityBytes / 4

// Tree is an ordered map stored in a file.
//
// One writer at a time: a Tree is a handle onto a root, and writing through it
// moves that root. Readers take their own handle at a committed root and are
// unaffected by anything a writer does afterwards.
type Tree struct {
	pages *pager.Pager
	root  uint64
}

// New opens the tree at the last committed root.
func New(pages *pager.Pager) *Tree {
	return &Tree{pages: pages, root: pages.Meta().Root}
}

// At opens the tree at a root of your choosing — an older one, for a reader
// that wants the database as it was.
func At(pages *pager.Pager, root uint64) *Tree {
	return &Tree{pages: pages, root: root}
}

// Root is the page the tree currently hangs from. Zero means empty.
func (t *Tree) Root() uint64 { return t.root }

// Commit makes every change since the last one durable, and the new root the
// one a restart will find.
func (t *Tree) Commit() error { return t.pages.Commit(t.root) }

// Get returns the value for a key.
func (t *Tree) Get(key []byte) ([]byte, bool, error) {
	if err := checkKey(key); err != nil {
		return nil, false, err
	}

	id := t.root
	for id != 0 {
		page, err := t.pages.Read(id)
		if err != nil {
			return nil, false, err
		}

		switch page.Kind {
		case pager.KindLeaf:
			entries, err := decodeLeaf(page)
			if err != nil {
				return nil, false, err
			}
			at, found := findKey(entries, key)
			if !found {
				return nil, false, nil
			}
			value, err := t.load(entries[at])
			return value, err == nil, err

		case pager.KindNode:
			node, err := decodeBranch(page)
			if err != nil {
				return nil, false, err
			}
			id = node.children[childFor(node.keys, key)]
			if id == 0 {
				// Zero is the empty tree, never a child. A branch that holds one
				// is damaged, and saying "no such key" would turn that into a
				// wrong answer rather than an error.
				return nil, false, fmt.Errorf("%w: branch page %d has a zero child", ErrMalformed, page.ID)
			}

		default:
			return nil, false, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, id, page.Kind)
		}
	}
	return nil, false, nil
}

// Put stores a value, replacing one already under that key.
func (t *Tree) Put(key, value []byte) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if len(value) > MaxValue {
		return ErrValueTooLarge
	}

	entry, err := t.store(key, value)
	if err != nil {
		return err
	}

	if t.root == 0 {
		id, err := t.writeLeaf(0, []kv{entry})
		if err != nil {
			return err
		}
		t.root = id
		return nil
	}

	left, separator, right, err := t.insert(t.root, entry)
	if err != nil {
		return err
	}
	if right == 0 {
		t.root = left
		return nil
	}

	// The root split, so the tree grows a level. This is the only way it gets
	// taller, which is what keeps every leaf at the same depth.
	id, err := t.writeBranch(0, branch{keys: [][]byte{separator}, children: []uint64{left, right}})
	if err != nil {
		return err
	}
	t.root = id
	return nil
}

// insert writes a new version of the subtree at id. It returns the new node,
// or — when that node had to split — the two halves and the separator between
// them for the parent to take.
func (t *Tree) insert(id uint64, entry kv) (uint64, []byte, uint64, error) {
	page, err := t.pages.Read(id)
	if err != nil {
		return 0, nil, 0, err
	}

	switch page.Kind {
	case pager.KindLeaf:
		entries, err := decodeLeaf(page)
		if err != nil {
			return 0, nil, 0, err
		}

		at, found := findKey(entries, entry.key)
		if found {
			// Whatever the key held is now unreachable, chain and all.
			if err := t.release(entries[at]); err != nil {
				return 0, nil, 0, err
			}
			entries[at] = entry
		} else {
			entries = append(entries, kv{})
			copy(entries[at+1:], entries[at:])
			entries[at] = entry
		}

		if leafBytes(entries) <= capacityBytes {
			newID, err := t.writeLeaf(id, entries)
			return newID, nil, 0, err
		}

		cut, ok := splitLeaf(entries)
		if !ok {
			return 0, nil, 0, ErrCannotSplit
		}
		leftID, err := t.writeLeaf(id, entries[:cut])
		if err != nil {
			return 0, nil, 0, err
		}
		rightID, err := t.writeLeaf(0, entries[cut:])
		if err != nil {
			return 0, nil, 0, err
		}
		return leftID, entries[cut].key, rightID, nil

	case pager.KindNode:
		node, err := decodeBranch(page)
		if err != nil {
			return 0, nil, 0, err
		}

		at := childFor(node.keys, entry.key)
		child, separator, split, err := t.insert(node.children[at], entry)
		if err != nil {
			return 0, nil, 0, err
		}
		node.children[at] = child

		if split != 0 {
			node.keys = append(node.keys, nil)
			copy(node.keys[at+1:], node.keys[at:])
			node.keys[at] = separator

			node.children = append(node.children, 0)
			copy(node.children[at+2:], node.children[at+1:])
			node.children[at+1] = split
		}

		if branchBytes(node) <= capacityBytes {
			newID, err := t.writeBranch(id, node)
			return newID, nil, 0, err
		}

		cut, ok := splitBranch(node)
		if !ok {
			return 0, nil, 0, ErrCannotSplit
		}
		leftID, err := t.writeBranch(id, branch{keys: node.keys[:cut], children: node.children[:cut+1]})
		if err != nil {
			return 0, nil, 0, err
		}
		rightID, err := t.writeBranch(0, branch{keys: node.keys[cut+1:], children: node.children[cut+1:]})
		if err != nil {
			return 0, nil, 0, err
		}
		return leftID, node.keys[cut], rightID, nil

	default:
		return 0, nil, 0, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, id, page.Kind)
	}
}

// Delete removes a key, reporting whether it was there.
func (t *Tree) Delete(key []byte) (bool, error) {
	if err := checkKey(key); err != nil {
		return false, err
	}
	if t.root == 0 {
		return false, nil
	}

	newRoot, removed, err := t.remove(t.root, key)
	if err != nil || !removed {
		return false, err
	}
	t.root = newRoot

	// A root with one child is a level that no longer earns its page, and an
	// empty leaf root is an empty tree.
	for {
		page, err := t.pages.Read(t.root)
		if err != nil {
			return false, err
		}
		switch page.Kind {
		case pager.KindNode:
			node, err := decodeBranch(page)
			if err != nil {
				return false, err
			}
			if len(node.children) > 1 {
				return true, nil
			}
			t.pages.Free(t.root)
			t.root = node.children[0]

		case pager.KindLeaf:
			entries, err := decodeLeaf(page)
			if err != nil {
				return false, err
			}
			if len(entries) == 0 {
				t.pages.Free(t.root)
				t.root = 0
			}
			return true, nil

		default:
			return false, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, t.root, page.Kind)
		}
	}
}

// remove writes a new version of the subtree at id without the key. When the
// key was not there it returns the node untouched, so a delete that finds
// nothing costs nothing.
func (t *Tree) remove(id uint64, key []byte) (uint64, bool, error) {
	page, err := t.pages.Read(id)
	if err != nil {
		return 0, false, err
	}

	switch page.Kind {
	case pager.KindLeaf:
		entries, err := decodeLeaf(page)
		if err != nil {
			return 0, false, err
		}
		at, found := findKey(entries, key)
		if !found {
			return id, false, nil
		}
		if err := t.release(entries[at]); err != nil {
			return 0, false, err
		}
		entries = append(entries[:at], entries[at+1:]...)
		newID, err := t.writeLeaf(id, entries)
		return newID, true, err

	case pager.KindNode:
		node, err := decodeBranch(page)
		if err != nil {
			return 0, false, err
		}

		at := childFor(node.keys, key)
		child, removed, err := t.remove(node.children[at], key)
		if err != nil || !removed {
			return id, false, err
		}
		node.children[at] = child

		underfull, err := t.isUnderfull(child)
		if err != nil {
			return 0, false, err
		}
		if underfull {
			node, err = t.rebalance(node, at)
			if err != nil {
				return 0, false, err
			}
		}

		newID, err := t.writeBranch(id, node)
		return newID, true, err

	default:
		return 0, false, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, id, page.Kind)
	}
}

// rebalance puts an underfull child back in shape against a neighbour: the two
// are poured together and either stay one node or are split evenly again. A
// node that is too empty is never left to shrink to nothing.
func (t *Tree) rebalance(node branch, at int) (branch, error) {
	// Always work on a pair, taking the left neighbour when there is one.
	left := at
	if at > 0 {
		left = at - 1
	}

	firstPage, err := t.pages.Read(node.children[left])
	if err != nil {
		return branch{}, err
	}
	secondPage, err := t.pages.Read(node.children[left+1])
	if err != nil {
		return branch{}, err
	}
	if firstPage.Kind != secondPage.Kind {
		return branch{}, fmt.Errorf("%w: siblings are kinds %d and %d", ErrMalformed, firstPage.Kind, secondPage.Kind)
	}

	switch firstPage.Kind {
	case pager.KindLeaf:
		first, err := decodeLeaf(firstPage)
		if err != nil {
			return branch{}, err
		}
		second, err := decodeLeaf(secondPage)
		if err != nil {
			return branch{}, err
		}
		merged := append(first, second...)

		if leafBytes(merged) <= capacityBytes {
			id, err := t.writeLeaf(node.children[left], merged)
			if err != nil {
				return branch{}, err
			}
			t.pages.Free(node.children[left+1])
			return collapse(node, left, id), nil
		}

		cut, ok := splitLeaf(merged)
		if !ok {
			return branch{}, ErrCannotSplit
		}
		firstID, err := t.writeLeaf(node.children[left], merged[:cut])
		if err != nil {
			return branch{}, err
		}
		secondID, err := t.writeLeaf(node.children[left+1], merged[cut:])
		if err != nil {
			return branch{}, err
		}
		node.keys[left] = merged[cut].key
		node.children[left], node.children[left+1] = firstID, secondID
		return node, nil

	case pager.KindNode:
		first, err := decodeBranch(firstPage)
		if err != nil {
			return branch{}, err
		}
		second, err := decodeBranch(secondPage)
		if err != nil {
			return branch{}, err
		}

		// The separator in the parent is the key that was missing between the
		// two nodes; pouring them together puts it back.
		merged := branch{
			keys:     append(append(append([][]byte{}, first.keys...), node.keys[left]), second.keys...),
			children: append(append([]uint64{}, first.children...), second.children...),
		}

		if branchBytes(merged) <= capacityBytes {
			id, err := t.writeBranch(node.children[left], merged)
			if err != nil {
				return branch{}, err
			}
			t.pages.Free(node.children[left+1])
			return collapse(node, left, id), nil
		}

		cut, ok := splitBranch(merged)
		if !ok {
			return branch{}, ErrCannotSplit
		}
		firstID, err := t.writeBranch(node.children[left], branch{keys: merged.keys[:cut], children: merged.children[:cut+1]})
		if err != nil {
			return branch{}, err
		}
		secondID, err := t.writeBranch(node.children[left+1], branch{keys: merged.keys[cut+1:], children: merged.children[cut+1:]})
		if err != nil {
			return branch{}, err
		}
		node.keys[left] = merged.keys[cut]
		node.children[left], node.children[left+1] = firstID, secondID
		return node, nil

	default:
		return branch{}, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, node.children[left], firstPage.Kind)
	}
}

// collapse replaces the pair of children at `left` with the single node they
// merged into, and drops the separator that used to stand between them.
func collapse(node branch, left int, merged uint64) branch {
	node.keys = append(node.keys[:left], node.keys[left+1:]...)
	node.children[left] = merged
	node.children = append(node.children[:left+1], node.children[left+2:]...)
	return node
}

func (t *Tree) isUnderfull(id uint64) (bool, error) {
	page, err := t.pages.Read(id)
	if err != nil {
		return false, err
	}
	switch page.Kind {
	case pager.KindLeaf:
		entries, err := decodeLeaf(page)
		if err != nil {
			return false, err
		}
		return leafBytes(entries) < underfullBytes, nil
	case pager.KindNode:
		node, err := decodeBranch(page)
		if err != nil {
			return false, err
		}
		return branchBytes(node) < underfullBytes, nil
	default:
		return false, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, id, page.Kind)
	}
}

// Ascend walks the tree in key order from `from` (nil for the beginning),
// stopping early if the callback says so.
func (t *Tree) Ascend(from []byte, visit func(key, value []byte) bool) error {
	if t.root == 0 {
		return nil
	}
	_, err := t.ascend(t.root, from, visit)
	return err
}

func (t *Tree) ascend(id uint64, from []byte, visit func(key, value []byte) bool) (bool, error) {
	page, err := t.pages.Read(id)
	if err != nil {
		return false, err
	}

	switch page.Kind {
	case pager.KindLeaf:
		entries, err := decodeLeaf(page)
		if err != nil {
			return false, err
		}
		at := 0
		if from != nil {
			at, _ = findKey(entries, from)
		}
		for _, entry := range entries[at:] {
			value, err := t.load(entry)
			if err != nil {
				return false, err
			}
			if !visit(entry.key, value) {
				return false, nil
			}
		}
		return true, nil

	case pager.KindNode:
		node, err := decodeBranch(page)
		if err != nil {
			return false, err
		}
		start := 0
		if from != nil {
			start = childFor(node.keys, from)
		}
		for i := start; i < len(node.children); i++ {
			// Only the first subtree needs to skip anything: everything after it
			// is entirely above `from`.
			cursor := from
			if i > start {
				cursor = nil
			}
			more, err := t.ascend(node.children[i], cursor, visit)
			if err != nil || !more {
				return more, err
			}
		}
		return true, nil

	default:
		return false, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, id, page.Kind)
	}
}

// Descend walks the tree in reverse key order, starting just below `from` (nil
// for the end), stopping early if the callback says so.
//
// `from` is exclusive where Ascend's is inclusive, and the asymmetry is the
// point rather than an oversight: a stretch of the tree is half-open, [lo, hi),
// and Ascend(lo) walks it upward while Descend(hi) walks that same stretch
// downward. Were both ends inclusive the two walks would disagree about the
// endpoints, and a range read forward would hold one more entry than the same
// range read backward — which is the kind of difference that only shows up on
// the one key that sits exactly on the bound.
func (t *Tree) Descend(from []byte, visit func(key, value []byte) bool) error {
	if t.root == 0 {
		return nil
	}
	_, err := t.descend(t.root, from, visit)
	return err
}

func (t *Tree) descend(id uint64, from []byte, visit func(key, value []byte) bool) (bool, error) {
	page, err := t.pages.Read(id)
	if err != nil {
		return false, err
	}

	switch page.Kind {
	case pager.KindLeaf:
		entries, err := decodeLeaf(page)
		if err != nil {
			return false, err
		}
		end := len(entries)
		if from != nil {
			// findKey is the first entry at or above `from`, so everything
			// strictly below it is exactly what is left to walk.
			end, _ = findKey(entries, from)
		}
		for i := end - 1; i >= 0; i-- {
			value, err := t.load(entries[i])
			if err != nil {
				return false, err
			}
			if !visit(entries[i].key, value) {
				return false, nil
			}
		}
		return true, nil

	case pager.KindNode:
		node, err := decodeBranch(page)
		if err != nil {
			return false, err
		}
		start := len(node.children) - 1
		if from != nil {
			// Every key below `from` lives in this child or to the left of it,
			// so the subtrees after it hold nothing this walk wants.
			start = childFor(node.keys, from)
		}
		for i := start; i >= 0; i-- {
			// Only the first subtree needs to stop anywhere: everything after
			// it is entirely below `from` and is walked whole.
			//
			// Dropping the cursor here is an economy, not a correctness
			// condition, and no test can see it: a subtree left of `start`
			// holds only keys below the separator, which is at or below
			// `from`, so handing it `from` would make it search for a place it
			// would reach anyway. A mutant that keeps the cursor survives on
			// purpose — it costs a binary search per page and changes no
			// answer.
			cursor := from
			if i < start {
				cursor = nil
			}
			more, err := t.descend(node.children[i], cursor, visit)
			if err != nil || !more {
				return more, err
			}
		}
		return true, nil

	default:
		return false, fmt.Errorf("%w: page %d is kind %d", ErrNotANode, id, page.Kind)
	}
}

// writeLeaf stores entries, reusing `at` when that page belongs to the
// transaction in progress and allocating a new one otherwise. Pass zero to
// always allocate.
func (t *Tree) writeLeaf(at uint64, entries []kv) (uint64, error) {
	id, err := t.place(at)
	if err != nil {
		return 0, err
	}
	page := t.pages.NewPage(id, pager.KindLeaf)
	if err := encodeLeaf(page, entries); err != nil {
		return 0, err
	}
	return id, t.pages.Write(page)
}

func (t *Tree) writeBranch(at uint64, node branch) (uint64, error) {
	id, err := t.place(at)
	if err != nil {
		return 0, err
	}
	page := t.pages.NewPage(id, pager.KindNode)
	if err := encodeBranch(page, node); err != nil {
		return 0, err
	}
	return id, t.pages.Write(page)
}

// store puts a value where it belongs: beside its key when it is small enough
// to share a page, and in a chain of its own pages when it is not.
func (t *Tree) store(key, value []byte) (kv, error) {
	if len(value) <= MaxInline {
		return kv{key: copyOf(key), value: copyOf(value)}, nil
	}
	head, err := t.writeOverflow(value)
	if err != nil {
		return kv{}, err
	}
	return kv{key: copyOf(key), head: head, length: uint32(len(value))}, nil
}

// writeOverflow lays a value out across pages, back to front, so that each one
// is written already knowing where the rest of it continues.
func (t *Tree) writeOverflow(value []byte) (uint64, error) {
	next := uint64(0)

	for start := (len(value) - 1) / overflowChunk * overflowChunk; start >= 0; start -= overflowChunk {
		chunk := value[start:min(start+overflowChunk, len(value))]

		id, err := t.pages.Allocate()
		if err != nil {
			return 0, err
		}
		page := t.pages.NewPage(id, pager.KindBlob)
		payload := page.Payload()
		binary.BigEndian.PutUint64(payload[overflowNext:], next)
		binary.BigEndian.PutUint32(payload[overflowLength:], uint32(len(chunk)))
		copy(payload[overflowHeader:], chunk)
		if err := t.pages.Write(page); err != nil {
			return 0, err
		}
		next = id
	}

	return next, nil
}

// load is the value an entry stands for, following its chain when it has one.
//
// Every step is checked against what the entry said to expect: a chain that is
// too short, too long, or made of pages that are not chunks is an error rather
// than a value with a hole in it.
func (t *Tree) load(entry kv) ([]byte, error) {
	if !entry.overflows() {
		return entry.value, nil
	}

	value := make([]byte, 0, entry.length)
	for id := entry.head; id != 0; {
		page, err := t.pages.Read(id)
		if err != nil {
			return nil, err
		}
		if page.Kind != pager.KindBlob {
			return nil, fmt.Errorf("%w: page %d is kind %d", ErrBrokenChain, id, page.Kind)
		}

		payload := page.Payload()
		length := int(binary.BigEndian.Uint32(payload[overflowLength:]))
		if length == 0 || length > overflowChunk || len(value)+length > int(entry.length) {
			return nil, fmt.Errorf("%w: page %d carries %d bytes of a %d-byte value", ErrBrokenChain, id, length, entry.length)
		}

		value = append(value, payload[overflowHeader:overflowHeader+length]...)
		id = binary.BigEndian.Uint64(payload[overflowNext:])
	}

	if len(value) != int(entry.length) {
		return nil, fmt.Errorf("%w: the chain held %d bytes of a %d-byte value", ErrBrokenChain, len(value), entry.length)
	}
	return value, nil
}

// place is where a rewritten node goes: back where it was if no reader can see
// it, and on a new page otherwise — in which case the version that was there
// is now nobody's, and goes on the free list.
func (t *Tree) place(at uint64) (uint64, error) {
	if at != 0 {
		if t.pages.Dirty(at) {
			return at, nil
		}
		t.pages.Free(at)
	}
	return t.pages.Allocate()
}

// release gives back the pages a value was living in. A value kept beside its
// key has none of its own; one in a chain has the whole chain.
func (t *Tree) release(entry kv) error {
	if !entry.overflows() {
		return nil
	}

	for id := entry.head; id != 0; {
		page, err := t.pages.Read(id)
		if err != nil {
			return err
		}
		if page.Kind != pager.KindBlob {
			return fmt.Errorf("%w: page %d is kind %d", ErrBrokenChain, id, page.Kind)
		}
		next := binary.BigEndian.Uint64(page.Payload()[overflowNext:])
		t.pages.Free(id)
		id = next
	}
	return nil
}

func copyOf(b []byte) []byte { return append([]byte(nil), b...) }
