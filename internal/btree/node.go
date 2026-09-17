// Package btree is the ordered map the engine is built on: a copy-on-write
// B+tree over the pager.
//
// Copy-on-write means no page is ever changed in place. Touching a key rewrites
// the leaf it lives in and every node above it, all to freshly allocated pages,
// and the transaction ends by pointing the meta page at the new root. A reader
// that started before all that is still looking at the old root, which is still
// intact — so readers never block the writer and never see half a change.
//
// The price is that the old pages are garbage once the new root is committed.
// Reclaiming them is the freelist, which the meta page already has a slot for.
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/material-atomic/rsql/internal/pager"
)

// capacityBytes is how much of a page a node may use.
const capacityBytes = pager.PageBytes - pager.HeaderBytes

const (
	leafHeader   = 2     // count
	branchHeader = 2 + 8 // count, then the leftmost child
	slotBytes    = 2     // one offset per entry
	// A leaf entry is a key and a value; a branch entry is a key and the child
	// to the right of it.
	leafEntry   = 2 + 4
	branchEntry = 2 + 8
)

// MaxKey is the longest key the tree takes.
//
// Keys are identifiers and index keys, not documents. Capping them keeps a
// branch node fanned out — a node full of 512-byte keys still holds seven
// children, so the tree stays shallow.
const MaxKey = 512

// MaxValue is the largest value that fits beside a key in a leaf.
//
// Two entries of the largest size must share a page, so that a leaf which
// overflows can always be split into two that fit. Anything bigger than this
// belongs in overflow pages, which the tree does not have yet and which is why
// this is an error rather than a silent slow path.
const MaxValue = capacityBytes/2 - leafEntry - slotBytes - MaxKey

var (
	ErrKeyTooLarge   = fmt.Errorf("rsql/btree: a key may be at most %d bytes", MaxKey)
	ErrValueTooLarge = fmt.Errorf("rsql/btree: a value may be at most %d bytes", MaxValue)
	ErrEmptyKey      = errors.New("rsql/btree: a key may not be empty")
	ErrMalformed     = errors.New("rsql/btree: the page is not a node this build can read")
	ErrNotANode      = errors.New("rsql/btree: the page is not a tree node")
	// ErrCannotSplit cannot happen while MaxKey and MaxValue hold: two entries
	// of the largest size fit in one page, so an overflowing node always has a
	// place to be cut. It is here so that a future change to those numbers
	// fails loudly instead of writing a node that cannot be read back.
	ErrCannotSplit = errors.New("rsql/btree: a node overflowed with nowhere to split it")
)

// kv is one leaf entry.
type kv struct {
	key   []byte
	value []byte
}

// branch is an internal node: n separator keys and n+1 children.
//
// keys[i] is the smallest key in children[i+1]'s subtree, so a search goes
// right at keys[i] when the key it is looking for is not smaller.
type branch struct {
	keys     [][]byte
	children []uint64
}

func checkKey(key []byte) error {
	switch {
	case len(key) == 0:
		return ErrEmptyKey
	case len(key) > MaxKey:
		return ErrKeyTooLarge
	}
	return nil
}

// leafBytes is what these entries take up in a page.
func leafBytes(entries []kv) int {
	total := leafHeader
	for _, entry := range entries {
		total += slotBytes + leafEntry + len(entry.key) + len(entry.value)
	}
	return total
}

func branchBytes(node branch) int {
	total := branchHeader
	for _, key := range node.keys {
		total += slotBytes + branchEntry + len(key)
	}
	return total
}

func encodeLeaf(page *pager.Page, entries []kv) error {
	if leafBytes(entries) > capacityBytes {
		return ErrCannotSplit
	}

	payload := page.Payload()
	binary.BigEndian.PutUint16(payload, uint16(len(entries)))

	// Slots grow up from the front, entries down from the back; they must not
	// meet, which leafBytes has already established.
	heap := len(payload)
	for i, entry := range entries {
		heap -= leafEntry + len(entry.key) + len(entry.value)
		binary.BigEndian.PutUint16(payload[leafHeader+i*slotBytes:], uint16(heap))
		binary.BigEndian.PutUint16(payload[heap:], uint16(len(entry.key)))
		binary.BigEndian.PutUint32(payload[heap+2:], uint32(len(entry.value)))
		copy(payload[heap+leafEntry:], entry.key)
		copy(payload[heap+leafEntry+len(entry.key):], entry.value)
	}
	return nil
}

// decodeLeaf reads a leaf back.
//
// Every offset and length is checked against the page. The checksum already
// says the bytes are the ones that were written, so this is about a page from
// another version or another kind — it must be an error, never a panic and
// never a slice of whatever came next.
func decodeLeaf(page *pager.Page) ([]kv, error) {
	payload := page.Payload()
	if len(payload) < leafHeader {
		return nil, ErrMalformed
	}

	count := int(binary.BigEndian.Uint16(payload))
	slotsEnd := leafHeader + count*slotBytes
	if slotsEnd > len(payload) {
		return nil, ErrMalformed
	}

	entries := make([]kv, 0, count)
	for i := 0; i < count; i++ {
		at := int(binary.BigEndian.Uint16(payload[leafHeader+i*slotBytes:]))
		if at < slotsEnd || at+leafEntry > len(payload) {
			return nil, ErrMalformed
		}
		keyLen := int(binary.BigEndian.Uint16(payload[at:]))
		valueLen := int(binary.BigEndian.Uint32(payload[at+2:]))
		start := at + leafEntry
		if keyLen > MaxKey || valueLen > capacityBytes || start+keyLen+valueLen > len(payload) {
			return nil, ErrMalformed
		}
		entries = append(entries, kv{
			key:   append([]byte(nil), payload[start:start+keyLen]...),
			value: append([]byte(nil), payload[start+keyLen:start+keyLen+valueLen]...),
		})
	}
	return entries, nil
}

func encodeBranch(page *pager.Page, node branch) error {
	if len(node.children) != len(node.keys)+1 {
		return fmt.Errorf("rsql/btree: %d keys with %d children", len(node.keys), len(node.children))
	}
	if branchBytes(node) > capacityBytes {
		return ErrCannotSplit
	}

	payload := page.Payload()
	binary.BigEndian.PutUint16(payload, uint16(len(node.keys)))
	binary.BigEndian.PutUint64(payload[2:], node.children[0])

	heap := len(payload)
	for i, key := range node.keys {
		heap -= branchEntry + len(key)
		binary.BigEndian.PutUint16(payload[branchHeader+i*slotBytes:], uint16(heap))
		binary.BigEndian.PutUint16(payload[heap:], uint16(len(key)))
		binary.BigEndian.PutUint64(payload[heap+2:], node.children[i+1])
		copy(payload[heap+branchEntry:], key)
	}
	return nil
}

func decodeBranch(page *pager.Page) (branch, error) {
	payload := page.Payload()
	if len(payload) < branchHeader {
		return branch{}, ErrMalformed
	}

	count := int(binary.BigEndian.Uint16(payload))
	slotsEnd := branchHeader + count*slotBytes
	if slotsEnd > len(payload) {
		return branch{}, ErrMalformed
	}

	node := branch{
		keys:     make([][]byte, 0, count),
		children: make([]uint64, 1, count+1),
	}
	node.children[0] = binary.BigEndian.Uint64(payload[2:])

	for i := 0; i < count; i++ {
		at := int(binary.BigEndian.Uint16(payload[branchHeader+i*slotBytes:]))
		if at < slotsEnd || at+branchEntry > len(payload) {
			return branch{}, ErrMalformed
		}
		keyLen := int(binary.BigEndian.Uint16(payload[at:]))
		start := at + branchEntry
		if keyLen > MaxKey || start+keyLen > len(payload) {
			return branch{}, ErrMalformed
		}
		node.keys = append(node.keys, append([]byte(nil), payload[start:start+keyLen]...))
		node.children = append(node.children, binary.BigEndian.Uint64(payload[at+2:]))
	}
	return node, nil
}

// childFor is the child whose subtree a key belongs in.
func childFor(keys [][]byte, key []byte) int {
	return sort.Search(len(keys), func(i int) bool { return bytes.Compare(key, keys[i]) < 0 })
}

// findKey is where a key is, or would go, among sorted entries.
func findKey(entries []kv, key []byte) (int, bool) {
	at := sort.Search(len(entries), func(i int) bool { return bytes.Compare(entries[i].key, key) >= 0 })
	return at, at < len(entries) && bytes.Equal(entries[at].key, key)
}

// splitLeaf cuts entries into two pages that both fit, as evenly as the entry
// sizes allow.
func splitLeaf(entries []kv) (int, bool) {
	prefix := make([]int, len(entries)+1)
	for i, entry := range entries {
		prefix[i+1] = prefix[i] + slotBytes + leafEntry + len(entry.key) + len(entry.value)
	}

	best, bestSkew := 0, 0
	for cut := 1; cut < len(entries); cut++ {
		left := leafHeader + prefix[cut]
		right := leafHeader + prefix[len(entries)] - prefix[cut]
		if left > capacityBytes || right > capacityBytes {
			continue
		}
		skew := left - right
		if skew < 0 {
			skew = -skew
		}
		if best == 0 || skew < bestSkew {
			best, bestSkew = cut, skew
		}
	}
	return best, best != 0
}

// splitBranch is the same for an internal node, except that the key at the cut
// does not go to either side: it is the separator handed to the parent.
func splitBranch(node branch) (int, bool) {
	prefix := make([]int, len(node.keys)+1)
	for i, key := range node.keys {
		prefix[i+1] = prefix[i] + slotBytes + branchEntry + len(key)
	}

	best, bestSkew := -1, 0
	for cut := 0; cut < len(node.keys); cut++ {
		left := branchHeader + prefix[cut]
		right := branchHeader + prefix[len(node.keys)] - prefix[cut+1]
		if left > capacityBytes || right > capacityBytes {
			continue
		}
		skew := left - right
		if skew < 0 {
			skew = -skew
		}
		if best < 0 || skew < bestSkew {
			best, bestSkew = cut, skew
		}
	}
	return best, best >= 0
}
