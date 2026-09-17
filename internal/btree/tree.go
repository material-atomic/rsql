package btree

import (
	"fmt"

	"github.com/material-atomic/rsql/internal/pager"
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
			return entries[at].value, true, nil

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

	if t.root == 0 {
		id, err := t.writeLeaf(0, []kv{{key: copyOf(key), value: copyOf(value)}})
		if err != nil {
			return err
		}
		t.root = id
		return nil
	}

	left, separator, right, err := t.insert(t.root, key, value)
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
func (t *Tree) insert(id uint64, key, value []byte) (uint64, []byte, uint64, error) {
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

		at, found := findKey(entries, key)
		if found {
			entries[at].value = copyOf(value)
		} else {
			entries = append(entries, kv{})
			copy(entries[at+1:], entries[at:])
			entries[at] = kv{key: copyOf(key), value: copyOf(value)}
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

		at := childFor(node.keys, key)
		child, separator, split, err := t.insert(node.children[at], key, value)
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
			t.root = node.children[0]

		case pager.KindLeaf:
			entries, err := decodeLeaf(page)
			if err != nil {
				return false, err
			}
			if len(entries) == 0 {
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
			if !visit(entry.key, entry.value) {
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

// place is where a rewritten node goes: back where it was if no reader can see
// it, and on a new page otherwise.
func (t *Tree) place(at uint64) (uint64, error) {
	if at != 0 && t.pages.Dirty(at) {
		return at, nil
	}
	return t.pages.Allocate()
}

func copyOf(b []byte) []byte { return append([]byte(nil), b...) }
