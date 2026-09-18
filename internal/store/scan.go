package store

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/material-atomic/rsql/internal/keys"
)

// Bound is one end of a scan: values for the first fields of the index, and
// whether that point is part of the range.
//
// Fewer values than the index has fields is a prefix, which is the ordinary
// case: an index on (author, published) is scanned for one author by giving
// one value.
type Bound struct {
	Values    []any
	Exclusive bool
}

// Range is what part of an index to walk. A nil end is unbounded.
//
// The order is the index's own: a field declared descending runs from large to
// small, and "from" is where the walk starts rather than the smaller value.
// There is no reverse walk on purpose — ordering comes from the index, so an
// order nothing declared is an order nobody paid for.
type Range struct {
	From *Bound
	To   *Bound
}

// Found is one index entry.
type Found struct {
	// Values are the index fields, in the order they were declared.
	Values []any
	// Key is the primary key of the document the entry describes.
	Key any
	// Include is the fields the index carries, when it was declared to carry
	// any — enough for a read that never touches the document.
	Include map[string]any
}

// Scan walks an index and hands back the entries in it, in index order,
// stopping early if the callback says so.
func (c *Collection) Scan(name string, within Range, visit func(Found) bool) error {
	index, found := c.index(name)
	if !found {
		return fmt.Errorf("%w: %q of %q", ErrNoIndex, name, c.spec.Name)
	}

	prefix := c.entries(*index)
	fields := encodings(index.Fields)

	lower, err := c.bound(prefix, fields, within.From, false)
	if err != nil {
		return err
	}
	upper, err := c.bound(prefix, fields, within.To, true)
	if err != nil {
		return err
	}

	// Every partition, oldest first. An index is local to its partition, so
	// this is the only way to see all of them — and the order is right because
	// what an operation may declare on a partitioned collection is checked
	// where it is declared. See ops.go.
	trees, err := c.across()
	if err != nil {
		return err
	}

	for _, tree := range trees {
		stop := false
		err := tree.Ascend(lower, func(key, value []byte) bool {
			if !bytes.HasPrefix(key, prefix) {
				return false
			}
			if upper != nil && bytes.Compare(key, upper) >= 0 {
				return false
			}

			rest := key[len(prefix):]
			values, rest, err := keys.DecodeKey(rest, fields)
			if err != nil {
				return false
			}
			primary, rest, err := keys.Decode(rest, keys.Field{})
			if err != nil || len(rest) != 0 {
				return false
			}

			entry := Found{Values: values, Key: primary}
			if len(value) > 0 {
				entry.Include = map[string]any{}
				if err := json.Unmarshal(value, &entry.Include); err != nil {
					return false
				}
			}
			if !visit(entry) {
				// The caller has had enough, and it has had enough of the
				// whole scan rather than of this partition.
				stop = true
				return false
			}
			return true
		})
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// Walk hands back every document in primary-key order, which is the order they
// are stored in.
func (c *Collection) Walk(visit func(key any, document map[string]any) bool) error {
	return c.walkRange(Range{}, visit)
}

// walkRange is Walk over a stretch of the primary key rather than all of it.
// The primary key is an index like any other — the clustered one — so a scan
// along it takes the same bounds.
func (c *Collection) walkRange(within Range, visit func(key any, document map[string]any) bool) error {
	prefix := c.documents()
	fields := []keys.Field{{}}

	lower, err := c.bound(prefix, fields, within.From, false)
	if err != nil {
		return err
	}
	upper, err := c.bound(prefix, fields, within.To, true)
	if err != nil {
		return err
	}

	trees, err := c.across()
	if err != nil {
		return err
	}

	for _, tree := range trees {
		stop := false
		err := tree.Ascend(lower, func(key, value []byte) bool {
			if !bytes.HasPrefix(key, prefix) {
				return false
			}
			if upper != nil && bytes.Compare(key, upper) >= 0 {
				return false
			}

			primary, rest, err := keys.Decode(key[len(prefix):], keys.Field{})
			if err != nil || len(rest) != 0 {
				return false
			}
			document := map[string]any{}
			if err := json.Unmarshal(value, &document); err != nil {
				return false
			}
			if !visit(primary, document) {
				stop = true
				return false
			}
			return true
		})
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// bound turns one end of a range into the byte position to start or stop at.
func (c *Collection) bound(prefix []byte, fields []keys.Field, at *Bound, upper bool) ([]byte, error) {
	if at == nil {
		if upper {
			// Everything in this index, and nothing after it.
			return successor(prefix), nil
		}
		return append([]byte(nil), prefix...), nil
	}
	if len(at.Values) > len(fields) {
		return nil, fmt.Errorf("%w: %d values for an index of %d fields", ErrDeclaration, len(at.Values), len(fields))
	}

	key, err := keys.EncodeKey(append([]byte(nil), prefix...), at.Values, fields[:len(at.Values)])
	if err != nil {
		return nil, err
	}

	// A bound is a prefix, and every key that extends it is inside it. So an
	// inclusive lower bound starts at the prefix, and an inclusive upper bound
	// stops after everything that extends it — which is what successor is.
	if upper != at.Exclusive {
		return successor(key), nil
	}
	return key, nil
}

// successor is the smallest key that sorts after every key beginning with this
// prefix. Nil when there is none, meaning the walk runs to the end.
func successor(prefix []byte) []byte {
	out := append([]byte(nil), prefix...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}
