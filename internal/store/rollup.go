package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/sapedb/sapedb/internal/btree"
	"github.com/sapedb/sapedb/internal/keys"
)

// Totals kept as the writes happen, rather than worked out when asked.
//
// "How many orders this month, and for how much" is the question every
// application ends up asking, and the usual answers are all bad in the same
// way: scan the collection and hope it stays small, or keep a counter in the
// application and hope nothing ever crashes between the write and the counter.
// The second is how totals silently stop matching the data, and it is the
// commonest data bug there is.
//
// A rollup is that counter, moved inside the transaction that causes it. The
// document, its index entries and the totals it changes all land together or
// none of them do, so the total cannot disagree with the data — not after a
// crash, not after a rollback, not after a retry.
//
// What it can hold is count and sum, and that is not a first version.
//
//   - Both are exactly reversible: a delete subtracts what the write added,
//     and that is all a delete has to do.
//   - Minimum and maximum are not. Deleting the current maximum means finding
//     the next one, which is a scan of the whole group, at write time, with no
//     bound anybody declared. This store refuses unbounded work everywhere
//     else and it refuses it here. An application that needs a maximum can
//     declare an index and read one row of it, which is bounded and which it
//     can see the cost of.
//
// Average is not here either, and does not need to be: a count and a sum is an
// average, computed by whoever wants one, with no rounding decided in advance
// by a database that cannot know what the number is for.

// spaceRollups is where the totals live.
const spaceRollups byte = 0x08 // 0x08 | cid | rollup id | group values -> totals

// Rollup is a declared total, kept up to date by every write.
type Rollup struct {
	Name string `json:"name"`

	// Group is what the totals are per: the fields whose distinct combinations
	// each get a row. No fields at all is one row for the whole collection,
	// which is a perfectly good thing to want.
	Group []Field `json:"group,omitempty"`

	// Count is how many documents are in the group.
	Count bool `json:"count,omitempty"`

	// Sum is the document paths to total. A document whose value at one of
	// them is not a number is refused, the same as one an index cannot
	// describe: a total that silently skips what it could not add is a total
	// nobody can trust.
	Sum []string `json:"sum,omitempty"`

	ID uint16 `json:"id"`
}

// Totals is one row of a rollup.
type Totals struct {
	// Group is the values this row is for, in the order they were declared.
	Group []any `json:"group,omitempty"`

	Count int                `json:"count"`
	Sum   map[string]float64 `json:"sum,omitempty"`
}

// validate refuses a rollup that cannot be kept.
func (r *Rollup) validate() error {
	if err := usableName(r.Name); err != nil {
		return fmt.Errorf("rollup name: %w", err)
	}
	if !r.Count && len(r.Sum) == 0 {
		return fmt.Errorf("%w: rollup %q totals nothing", ErrDeclaration, r.Name)
	}
	for _, field := range r.Group {
		if field.Path == "" {
			return fmt.Errorf("%w: rollup %q groups by a field with no path", ErrDeclaration, r.Name)
		}
		switch field.Missing {
		case MissingSkip, MissingFirst, MissingLast:
		default:
			return fmt.Errorf("%w: rollup %q must say what it does with a document that has no %q",
				ErrDeclaration, r.Name, field.Path)
		}
		switch field.Type {
		case TypeString, TypeNumber, TypeBool:
		default:
			return fmt.Errorf("%w: rollup %q groups by %q, which is %q", ErrDeclaration, r.Name, field.Path, field.Type)
		}
	}
	for _, path := range r.Sum {
		if path == "" {
			return fmt.Errorf("%w: rollup %q sums a field with no path", ErrDeclaration, r.Name)
		}
	}
	return nil
}

// sameRollup says whether a redeclaration asks for the same thing.
func sameRollup(before, now Rollup) bool {
	if before.Count != now.Count || len(before.Group) != len(now.Group) || len(before.Sum) != len(now.Sum) {
		return false
	}
	for i := range before.Group {
		if before.Group[i] != now.Group[i] {
			return false
		}
	}
	for i := range before.Sum {
		if before.Sum[i] != now.Sum[i] {
			return false
		}
	}
	return true
}

// rollups is the stretch of the tree holding one rollup.
func (c *Collection) rollups(rollup Rollup) []byte {
	key := make([]byte, 7)
	key[0] = spaceRollups
	binary.BigEndian.PutUint32(key[1:], c.spec.ID)
	binary.BigEndian.PutUint16(key[5:], rollup.ID)
	return key
}

// rollup finds a declared rollup by name.
func (c *Collection) rollup(name string) (*Rollup, bool) {
	for i := range c.spec.Rollups {
		if c.spec.Rollups[i].Name == name {
			return &c.spec.Rollups[i], true
		}
	}
	return nil, false
}

// groupKey is the row a document belongs to, or nothing when the rollup was
// told not to hold documents like it.
func (c *Collection) groupKey(rollup *Rollup, document map[string]any) ([]byte, bool, error) {
	values := make([]any, len(rollup.Group))

	for i, field := range rollup.Group {
		value, found := at(document, field.Path)
		if !found {
			if field.Missing == MissingSkip {
				return nil, false, nil
			}
			values[i] = keys.Absent
			continue
		}
		if !matches(field.Type, value) {
			return nil, false, fmt.Errorf("%w: rollup %q of %q wants %s at %q, and this is %T",
				ErrType, rollup.Name, c.spec.Name, field.Type, field.Path, value)
		}
		values[i] = value
	}

	key, err := keys.EncodeKey(c.rollups(*rollup), values, encodings(rollup.Group))
	if err != nil {
		return nil, false, err
	}
	return key, true, nil
}

// contribute adds a document to every rollup, or takes it away again.
//
// One function for both directions because they have to be exact opposites: a
// delete that subtracts anything other than what the write added is a total
// that drifts, and drift is invisible until somebody reconciles by hand.
func (c *Collection) contribute(tree *btree.Tree, document map[string]any, sign int) error {
	for i := range c.spec.Rollups {
		rollup := &c.spec.Rollups[i]

		key, belongs, err := c.groupKey(rollup, document)
		if err != nil {
			return err
		}
		if !belongs {
			continue
		}

		// The sums are worked out before anything is written, so that a value
		// that cannot be added stops this rollup before it has been half
		// moved. It does not stop an earlier rollup of the same collection
		// from having been moved already — what makes that safe is that an
		// error here means the caller does not commit, and nothing uncommitted
		// is part of the database. The same is true of the index entries a few
		// lines away, and has been all along.
		added := map[string]float64{}
		for _, path := range rollup.Sum {
			value, found := at(document, path)
			if !found {
				continue
			}
			number, isNumber := value.(float64)
			if !isNumber {
				return fmt.Errorf("%w: rollup %q of %q sums %q, and this document has %T there",
					ErrType, rollup.Name, c.spec.Name, path, value)
			}
			added[path] = number
		}

		stored, found, err := tree.Get(key)
		if err != nil {
			return err
		}

		totals := Totals{}
		if found {
			if err := json.Unmarshal(stored, &totals); err != nil {
				return fmt.Errorf("%w: rollup %q of %q: %v", ErrDamaged, rollup.Name, c.spec.Name, err)
			}
		}

		totals.Count += sign
		for path, number := range added {
			if totals.Sum == nil {
				totals.Sum = map[string]float64{}
			}
			totals.Sum[path] += float64(sign) * number
		}

		// A group with nothing in it is a row that would otherwise sit there
		// saying zero for as long as the database lives, and be returned by
		// every read that walks past it.
		if totals.Count <= 0 {
			if _, err := tree.Delete(key); err != nil {
				return err
			}
			continue
		}

		encoded, err := json.Marshal(totals)
		if err != nil {
			return err
		}
		if err := tree.Put(key, encoded); err != nil {
			return err
		}
	}
	return nil
}

// Totals reads a rollup over a stretch of its group.
//
// Partitions are added together rather than returned one after another: a
// rollup row is a total, and the total for a group is the sum of what each
// partition holds for it. Which is why a rollup read of a partitioned
// collection has to name one group — checked where the operation is declared,
// for the same reason a scan is.
func (c *Collection) Totals(name string, within Range, visit func(Totals) bool) error {
	rollup, found := c.rollup(name)
	if !found {
		return fmt.Errorf("%w: %q of %q", ErrNoRollup, name, c.spec.Name)
	}

	prefix := c.rollups(*rollup)
	fields := encodings(rollup.Group)

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

	// One tree is the ordinary case, and it is already in order: hand the rows
	// straight out. Collecting them first would hold every group in the range
	// in memory before returning one — memory bounded by the data rather than
	// by the operation, which is exactly what this store refuses elsewhere and
	// what the check in ops.go refuses for a partitioned collection. Doing it
	// here anyway would be that rule not applying to its own implementation.
	if len(trees) == 1 {
		return c.totalsIn(trees[0], prefix, fields, lower, upper, visit)
	}

	// Several partitions, and each holds part of every total, so they have to
	// be added up before any of them is an answer. That is why reading a
	// rollup of a partitioned collection must name one group — checked where
	// the operation is declared, so this only ever merges a handful of rows.
	order := [][]byte{}
	sums := map[string]*Totals{}

	for _, tree := range trees {
		err := c.totalsIn(tree, prefix, fields, lower, upper, func(row Totals) bool {
			at, err := keys.EncodeKey(prefix, row.Group, fields)
			if err != nil {
				return false
			}
			running, seen := sums[string(at)]
			if !seen {
				order = append(order, at)
				kept := row
				sums[string(at)] = &kept
				return true
			}
			running.Count += row.Count
			for path, number := range row.Sum {
				if running.Sum == nil {
					running.Sum = map[string]float64{}
				}
				running.Sum[path] += number
			}
			return true
		})
		if err != nil {
			return err
		}
	}

	sortKeys(order)
	for _, key := range order {
		if !visit(*sums[string(key)]) {
			return nil
		}
	}
	return nil
}

// totalsIn hands out the rows of one tree, in order.
func (c *Collection) totalsIn(tree *btree.Tree, prefix []byte, fields []keys.Field,
	lower, upper []byte, visit func(Totals) bool) error {

	return tree.Ascend(lower, func(key, value []byte) bool {
		// No test reaches this line and none can: bound never hands back an
		// unbounded end for a rollup, because the prefix begins with 0x08 and
		// every such prefix has a successor. So the walk already begins at or
		// after this rollup's rows and ends before the next one's. It stays
		// because it is what keeps the slice below in range if that ever stops
		// being true, and because a rollup read that wandered into another
		// rollup's rows would decode them perfectly happily and hand back
		// numbers that answer a different question.
		if len(key) < len(prefix) || string(key[:len(prefix)]) != string(prefix) {
			return false
		}
		if upper != nil && string(key) >= string(upper) {
			return false
		}

		row := Totals{}
		if err := json.Unmarshal(value, &row); err != nil {
			return false
		}
		values, rest, err := keys.DecodeKey(key[len(prefix):], fields)
		if err != nil || len(rest) != 0 {
			return false
		}
		row.Group = values

		// There is one tree here, so saying the caller stopped and saying this
		// walk ran out come to the same thing — unlike the scan in scan.go,
		// which has partitions left to visit and has to tell them apart.
		return visit(row)
	})
}

// sortKeys puts byte strings in order, which is the order the tree had them
// in. Needed because several partitions were merged.
func sortKeys(all [][]byte) {
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && string(all[j]) < string(all[j-1]); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
}

// buildRollup fills a new rollup from the documents already stored.
func (c *Collection) buildRollup(tree *btree.Tree, rollup Rollup) error {
	prefix := c.documents()
	var documents []map[string]any

	err := tree.Ascend(prefix, func(key, value []byte) bool {
		if len(key) < len(prefix) || string(key[:len(prefix)]) != string(prefix) {
			return false
		}
		document := map[string]any{}
		if err := json.Unmarshal(value, &document); err != nil {
			return false
		}
		documents = append(documents, document)
		return true
	})
	if err != nil {
		return err
	}

	// Built through the same function that keeps it, so a rollup that was
	// declared after the data cannot differ from one that was declared before.
	only := &Collection{store: c.store, spec: Spec{Name: c.spec.Name, ID: c.spec.ID, Rollups: []Rollup{rollup}}}
	for _, document := range documents {
		if err := only.contribute(tree, document, 1); err != nil {
			return err
		}
	}
	return nil
}
