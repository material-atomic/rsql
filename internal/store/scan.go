package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/sapedb/sapedb/internal/keys"
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

// Direction is which way along an index a walk runs.
//
// Reverse is the declared order read back to front, and that is the whole of
// what it means. An index declared (a ascending, b descending) read in reverse
// comes back (a descending, b ascending) — it is not "every field descending",
// which is a third order again and still needs an index of its own. The word
// is "reverse" rather than "descending" for exactly that reason: a field is
// descending, a walk is reversed, and the two are not the same thing.
//
// Ordering still comes from the index, so this buys no order nobody paid for:
// the reverse of a stored order costs the same walk over the same pages.
type Direction uint8

const (
	// Forward is the index's own order, the one its fields declared.
	Forward Direction = iota
	// Reverse is that same order, walked from the far end.
	Reverse
)

// directionOf turns what an operation wrote down, or what a caller passed, into
// a direction.
//
// Anything that is not one of the two words is refused rather than read as
// forward. A caller that misspells "reverse" is asking for an order, and
// quietly handing back the other one is the worst of the three answers
// available: it is wrong, it looks right, and nothing says so.
func directionOf(value any) (Direction, error) {
	switch value {
	case DirectionForward:
		return Forward, nil
	case DirectionReverse:
		return Reverse, nil
	}
	return Forward, fmt.Errorf("a scan runs %q or %q, and this is %v",
		DirectionForward, DirectionReverse, value)
}

// Range is what part of an index to walk. A nil end is unbounded.
//
// From is the low end of the stretch and To is the high end, in both
// directions. Direction changes only the order entries come back in, not
// which entries they are: a reversed walk is the same stretch read from its
// high end down to its low end, never a different stretch.
//
// It was not always this way. From and To used to swap which one was the
// upper end depending on Direction — "From is where the walk starts" — and
// three rounds of review each found a different declaration where that swap
// let a caller-chosen direction reach rows no forward call of the same
// declaration could: a constant written at one end only, the two ends
// disagreeing about Exclusive, and the two ends written at different widths.
// All three were the same defect wearing different clothes — a declaration
// whose meaning depended on an argument the person who wrote it never sees —
// so the swap was removed rather than patched a fourth time. See task 0012,
// round 4.
//
// Direction is the order a Scan (or Walk) reads its rows in. A rollup read
// (Collection.Totals) has no such order to reverse — it hands back a sum per
// group, not a sequence of rows a caller could read backwards — so it refuses
// a Range whose Direction is anything but Forward rather than accept a word
// it would then have nowhere to spend. See Totals's own doc.
type Range struct {
	From      *Bound
	To        *Bound
	Direction Direction
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

// Scan walks an index and hands back the entries in it, in index order — or in
// the reverse of it when the range says so — stopping early if the callback
// says so.
func (c *Collection) Scan(name string, within Range, visit func(Found) bool) error {
	index, found := c.index(name)
	if !found {
		return fmt.Errorf("%w: %q of %q", ErrNoIndex, name, c.spec.Name)
	}

	prefix := c.entries(*index)
	fields := index.Fields

	return c.walk(within, prefix, fields, func(key, value []byte) bool {
		rest := key[len(prefix):]
		values, rest, err := keys.DecodeKey(rest, encodings(fields))
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
		return visit(entry)
	})
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
	fields := []Field{{Path: c.spec.Key.Path, Type: c.spec.Key.Type, Missing: MissingSkip}}

	return c.walk(within, prefix, fields, func(key, value []byte) bool {
		primary, rest, err := keys.Decode(key[len(prefix):], keys.Field{})
		if err != nil || len(rest) != 0 {
			return false
		}
		document := map[string]any{}
		if err := json.Unmarshal(value, &document); err != nil {
			return false
		}
		return visit(primary, document)
	})
}

// stretch turns a Range into the two byte positions that bound the walk:
// lower always comes from From, upper always comes from To, and neither reads
// within.Direction. That is the whole of what this round changed and the
// whole of why it is pulled out of walk into a function of its own — it is
// the one thing that has to be true for "a direction widens nothing" to hold,
// and it has to be provable by calling it twice with Direction flipped and
// comparing the two byte strings, not by reading rows through a fixture.
func (c *Collection) stretch(within Range, prefix []byte, fields []Field) (lower, upper []byte, err error) {
	lower, err = c.bound(prefix, fields, within.From, false)
	if err != nil {
		return nil, nil, err
	}
	upper, err = c.bound(prefix, fields, within.To, true)
	if err != nil {
		return nil, nil, err
	}
	return lower, upper, nil
}

// walk is the one walk both Scan and walkRange are: a stretch of one keyspace,
// across every partition, in whichever direction was asked for. Only what to
// do with each entry differs, so only that is passed in.
func (c *Collection) walk(within Range, prefix []byte, fields []Field,
	visit func(key, value []byte) bool) error {

	lower, upper, err := c.stretch(within, prefix, fields)
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

	// A reversed walk reverses the list of partitions as well as each tree in
	// it: the reverse of a sorted concatenation is each piece reversed, last
	// piece first. Reversing only the trees, or only the list, gives an order
	// that is locally right and globally wrong, which no single-partition test
	// sees.
	//
	// That the concatenation is sorted at all is what scanAcross decides, when
	// the operation is declared. Reversing something in key order leaves it in
	// key order unconditionally, so the direction adds nothing for it to check
	// — but it has a known hole of its own, for bounds that fall away at call
	// time, and that hole is the same in both directions. See task 0016.
	if within.Direction == Reverse {
		slices.Reverse(trees)
	}

	for _, tree := range trees {
		stop := false
		// There is no prefix test here on purpose. It was one, and it was dead
		// in both directions: forward starts at `lower`, which is at or after
		// the prefix, and stops at `upper`, which is the first key after
		// everything carrying it; reversed, Descend starts below `upper` and
		// the `lower` test below stops it, and `lower` is never nil. A
		// defensive clause nobody can reach is worse than no clause: the next
		// person to change bound() cannot tell what it was covering for, and
		// every mutation of the two bounds hides behind it. Removed for the
		// same reason the dead branch in pager.Dirty was.
		bounded := func(key, value []byte) bool {
			if within.Direction == Reverse {
				// Descend already started below `upper`; `lower` is where it ends.
				if bytes.Compare(key, lower) < 0 {
					return false
				}
			} else if upper != nil && bytes.Compare(key, upper) >= 0 {
				return false
			}
			if !visit(key, value) {
				// The caller has had enough, and it has had enough of the
				// whole scan rather than of this partition.
				stop = true
				return false
			}
			return true
		}

		if within.Direction == Reverse {
			// A nil upper means the stretch runs to the very end of the
			// keyspace, and Descend takes nil for the end of the tree. That
			// only happens when the prefix is every byte 0xff, and then every
			// key above the prefix begins with it, so nothing outside the
			// stretch is walked on the way in.
			err = tree.Descend(upper, bounded)
		} else {
			err = tree.Ascend(lower, bounded)
		}
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
//
// The values here are never a constant written into an operation's
// declaration — constantIsEncodable (ops.go) already refused any of those
// that keys cannot turn into bytes, at the point the operation was declared.
// So whatever bound() cannot encode was one of the two things that only get a
// value at call time: an argument, or what an earlier step of a batch
// produced. Either way it is the caller's mistake, discovered here because
// this is the first place anything tries to encode it — which is why it is
// wrapped in ErrArgument rather than returned bare. A bare error from keys
// has no entry in codeFor's table, and a client that sees the resulting
// code=failed learns nothing it can act on for a mistake that is, in fact,
// entirely knowable: the argument does not fit an index built for something
// else.
//
// The loop encodes one value at a time, rather than handing the whole slice to
// keys.EncodeKey at once, so that a failure names the field it happened at —
// fields[i].Path — and not just the index as a whole.
func (c *Collection) bound(prefix []byte, fields []Field, at *Bound, upper bool) ([]byte, error) {
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

	side := "from"
	if upper {
		side = "to"
	}

	key := append([]byte(nil), prefix...)
	for i, value := range at.Values {
		var err error
		if key, err = keys.Encode(key, value, fields[i].encoding()); err != nil {
			return nil, fmt.Errorf("%w: the %s bound holds a value for %q that keys cannot encode: %v",
				ErrArgument, side, fields[i].Path, err)
		}
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
