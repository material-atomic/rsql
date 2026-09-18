package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/vfs"
)

// sorted reports whether keys are strictly increasing.
func sorted(keys [][]byte) bool {
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			return false
		}
	}
	return true
}

func freshTree(t *testing.T, seed int64) (*vfs.SimDisk, *Tree) {
	t.Helper()
	disk := vfs.NewSim(seed, vfs.Faults{})
	pages, err := pager.Create(disk, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return disk, New(pages)
}

func put(t *testing.T, tree *Tree, key, value string) {
	t.Helper()
	if err := tree.Put([]byte(key), []byte(value)); err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
}

func get(t *testing.T, tree *Tree, key string) (string, bool) {
	t.Helper()
	value, found, err := tree.Get([]byte(key))
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	return string(value), found
}

// check walks the tree and returns its height, failing on anything that is not
// a B+tree: keys out of order, a key outside the range its subtree is supposed
// to hold, leaves at different depths, or a node that does not fit its page.
//
// The model tests below say the tree answers correctly; this says it is
// actually a tree, so that a bug shows up as a broken shape rather than as an
// answer that happens to still be right.
func check(t *testing.T, tree *Tree) int {
	t.Helper()
	if tree.root == 0 {
		return 0
	}
	depths := map[int]bool{}
	// Nothing the tree can reach may also be on the free list. A page handed
	// out twice is the one way a tree that looks right can still be wrong.
	free := tree.pages.FreeSet()
	checkNode(t, tree, tree.root, nil, nil, 1, depths, free)
	if len(depths) != 1 {
		t.Fatalf("leaves sit at %d different depths: %v", len(depths), depths)
	}
	for depth := range depths {
		return depth
	}
	return 0
}

func checkNode(t *testing.T, tree *Tree, id uint64, low, high []byte, depth int, depths map[int]bool, free map[uint64]bool) {
	t.Helper()

	if free[id] {
		t.Fatalf("page %d is part of the tree and on the free list", id)
	}

	page, err := tree.pages.Read(id)
	if err != nil {
		t.Fatalf("read page %d: %v", id, err)
	}

	switch page.Kind {
	case pager.KindLeaf:
		entries, err := decodeLeaf(page)
		if err != nil {
			t.Fatalf("decode leaf %d: %v", id, err)
		}
		keys := make([][]byte, len(entries))
		for i, entry := range entries {
			keys[i] = entry.key
		}
		if !sorted(keys) {
			t.Fatalf("leaf %d has keys out of order: %q", id, keys)
		}
		if leafBytes(entries) > capacityBytes {
			t.Fatalf("leaf %d wants %d bytes of a %d-byte page", id, leafBytes(entries), capacityBytes)
		}
		for _, key := range keys {
			if low != nil && bytes.Compare(key, low) < 0 {
				t.Fatalf("leaf %d holds %q, below its subtree's floor %q", id, key, low)
			}
			if high != nil && bytes.Compare(key, high) >= 0 {
				t.Fatalf("leaf %d holds %q, at or above its subtree's ceiling %q", id, key, high)
			}
		}
		for _, entry := range entries {
			for at := entry.head; at != 0; {
				if free[at] {
					t.Fatalf("page %d holds part of a value and is on the free list", at)
				}
				chunk, err := tree.pages.Read(at)
				if err != nil {
					t.Fatalf("read chain page %d: %v", at, err)
				}
				if chunk.Kind != pager.KindBlob {
					t.Fatalf("chain page %d is kind %d", at, chunk.Kind)
				}
				at = binary.BigEndian.Uint64(chunk.Payload()[overflowNext:])
			}
		}
		depths[depth] = true

	case pager.KindNode:
		node, err := decodeBranch(page)
		if err != nil {
			t.Fatalf("decode branch %d: %v", id, err)
		}
		if !sorted(node.keys) {
			t.Fatalf("branch %d has separators out of order: %q", id, node.keys)
		}
		if len(node.children) != len(node.keys)+1 {
			t.Fatalf("branch %d has %d separators and %d children", id, len(node.keys), len(node.children))
		}
		if branchBytes(node) > capacityBytes {
			t.Fatalf("branch %d wants %d bytes of a %d-byte page", id, branchBytes(node), capacityBytes)
		}
		for i, child := range node.children {
			childLow, childHigh := low, high
			if i > 0 {
				childLow = node.keys[i-1]
			}
			if i < len(node.keys) {
				childHigh = node.keys[i]
			}
			checkNode(t, tree, child, childLow, childHigh, depth+1, depths, free)
		}

	default:
		t.Fatalf("page %d is kind %d, which is not a node", id, page.Kind)
	}
}

// collect is every key and value in the tree, in the order it hands them back.
func collect(t *testing.T, tree *Tree, from []byte) ([]string, []string) {
	t.Helper()
	var keys, values []string
	if err := tree.Ascend(from, func(key, value []byte) bool {
		keys = append(keys, string(key))
		values = append(values, string(value))
		return true
	}); err != nil {
		t.Fatalf("ascend: %v", err)
	}
	return keys, values
}

func TestAnEmptyTreeAnswersNothing(t *testing.T) {
	_, tree := freshTree(t, 1)

	if _, found := get(t, tree, "nothing"); found {
		t.Error("an empty tree found a key")
	}
	if tree.Root() != 0 {
		t.Errorf("an empty tree has root %d", tree.Root())
	}

	removed, err := tree.Delete([]byte("nothing"))
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("an empty tree removed a key")
	}

	keys, _ := collect(t, tree, nil)
	if len(keys) != 0 {
		t.Errorf("an empty tree walked %d keys", len(keys))
	}
}

func TestAValueComesBackAndAnOverwriteReplacesIt(t *testing.T) {
	_, tree := freshTree(t, 2)

	put(t, tree, "b", "one")
	put(t, tree, "a", "two")
	put(t, tree, "c", "three")

	if value, found := get(t, tree, "a"); !found || value != "two" {
		t.Errorf("a = %q, %v", value, found)
	}

	put(t, tree, "a", "replaced")
	if value, found := get(t, tree, "a"); !found || value != "replaced" {
		t.Errorf("after an overwrite a = %q, %v", value, found)
	}

	keys, _ := collect(t, tree, nil)
	if fmt.Sprint(keys) != "[a b c]" {
		t.Errorf("an overwrite changed the shape of the tree: %v", keys)
	}
}

func TestEnoughKeysToMakeATreeOfIt(t *testing.T) {
	_, tree := freshTree(t, 3)

	const count = 4000
	model := map[string]string{}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("key-%06d", i)
		value := fmt.Sprintf("value-%d", i*7)
		put(t, tree, key, value)
		model[key] = value
	}

	if height := check(t, tree); height < 2 {
		t.Fatalf("%d keys made a tree of height %d; this test is meant to exercise branch nodes", count, height)
	}

	for key, want := range model {
		if value, found := get(t, tree, key); !found || value != want {
			t.Fatalf("%s = %q, %v; want %q", key, value, found, want)
		}
	}

	keys, _ := collect(t, tree, nil)
	if len(keys) != count {
		t.Fatalf("walked %d keys of %d", len(keys), count)
	}
	if !sort.StringsAreSorted(keys) {
		t.Error("the walk is not in key order")
	}
}

// A tree more than two levels deep, so that a split has to travel through a
// branch node and not only through the root. Long keys get there on a few
// hundred entries: seven of them fill a page.
func TestATallTreeKeepsItsShape(t *testing.T) {
	_, tree := freshTree(t, 12)

	const count = 600
	pad := bytes.Repeat([]byte("p"), MaxKey-8)
	key := func(i int) []byte {
		return append(append([]byte(nil), []byte(fmt.Sprintf("%08d", i))...), pad...)
	}

	for i := 0; i < count; i++ {
		if err := tree.Put(key(i), []byte(fmt.Sprint(i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	if height := check(t, tree); height < 3 {
		t.Fatalf("%d long keys made a tree of height %d, want at least 3", count, height)
	}

	for i := 0; i < count; i++ {
		value, found, err := tree.Get(key(i))
		if err != nil || !found || string(value) != fmt.Sprint(i) {
			t.Fatalf("key %d = %q, %v, %v", i, value, found, err)
		}
	}

	// Emptied from the middle outwards, so rebalancing runs through the branch
	// level and not only the leaves. With keys this long two sibling branches
	// rarely fit in one page, which is the path where they are poured together
	// and split again — and where a separator one place out would leave a key
	// that is still in the tree unfindable.
	live := map[int]bool{}
	for i := 0; i < count; i++ {
		live[i] = true
	}

	for i := count/2 - 1; i >= 0; i-- {
		for _, at := range []int{i, count - 1 - i} {
			if removed, err := tree.Delete(key(at)); err != nil || !removed {
				t.Fatalf("delete %d: %v, %v", at, removed, err)
			}
			delete(live, at)

			check(t, tree)
			for remaining := range live {
				if _, found, err := tree.Get(key(remaining)); err != nil || !found {
					t.Fatalf("after deleting %d, searching for %d found nothing (%v)", at, remaining, err)
				}
			}
		}
	}
	if tree.Root() != 0 {
		t.Errorf("the emptied tree has root %d", tree.Root())
	}
}

func TestAscendStartsWhereItIsTold(t *testing.T) {
	_, tree := freshTree(t, 4)

	for i := 0; i < 500; i++ {
		put(t, tree, fmt.Sprintf("k%04d", i*2), fmt.Sprint(i))
	}

	// Only the even keys are there. A walk starts at the first key at or after
	// where it was told to: on the key itself when it exists, and on the next
	// one when it does not.
	for _, want := range []struct {
		from  string
		first string
		count int
	}{
		{from: "k0500", first: "k0500", count: 250},
		{from: "k0501", first: "k0502", count: 249},
		{from: "k0000", first: "k0000", count: 500},
	} {
		keys, _ := collect(t, tree, []byte(want.from))
		if len(keys) == 0 {
			t.Fatalf("from %q: nothing", want.from)
		}
		if keys[0] != want.first {
			t.Errorf("from %q the walk starts at %q, want %q", want.from, keys[0], want.first)
		}
		if len(keys) != want.count {
			t.Errorf("from %q the walk has %d keys, want %d", want.from, len(keys), want.count)
		}
	}

	keys, _ := collect(t, tree, []byte("zzz"))
	if len(keys) != 0 {
		t.Errorf("a walk from past the end returned %d keys", len(keys))
	}
}

func TestDeletingEverythingLeavesAnEmptyTree(t *testing.T) {
	_, tree := freshTree(t, 5)

	const count = 2000
	keys := make([]string, count)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%05d", i)
		put(t, tree, keys[i], fmt.Sprintf("value-%d", i))
	}
	if check(t, tree) < 2 {
		t.Fatal("this test wants a tree with branch nodes")
	}

	// Deleted in an order unrelated to the order they were inserted, so merges
	// happen in the middle of the tree and not only at one end.
	random := rand.New(rand.NewSource(5))
	random.Shuffle(count, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	for i, key := range keys {
		removed, err := tree.Delete([]byte(key))
		if err != nil {
			t.Fatalf("delete %q: %v", key, err)
		}
		if !removed {
			t.Fatalf("delete %q said there was nothing there", key)
		}
		if i%137 == 0 {
			check(t, tree)
		}
	}

	if tree.Root() != 0 {
		t.Errorf("a tree with everything deleted has root %d, want 0", tree.Root())
	}
	if walked, _ := collect(t, tree, nil); len(walked) != 0 {
		t.Errorf("an emptied tree still walks %d keys", len(walked))
	}
}

// The model test: thousands of random operations against a map, with the tree
// checked, committed and reopened along the way. Anything the hand-written
// tests did not think of has to show up here.
func TestTheTreeAgreesWithAMapThroughRandomWork(t *testing.T) {
	disk := vfs.NewSim(6, vfs.Faults{})
	pages, err := pager.Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}
	tree := New(pages)

	model := map[string]string{}
	random := rand.New(rand.NewSource(6))

	for step := 0; step < 20000; step++ {
		// Keys from a small enough space that overwrites and deletes of keys
		// that are really there happen often.
		key := fmt.Sprintf("k%04d", random.Intn(3000))

		switch {
		case random.Intn(100) < 60:
			value := fmt.Sprintf("v%d-%d", step, random.Intn(1000))
			if err := tree.Put([]byte(key), []byte(value)); err != nil {
				t.Fatalf("step %d: put: %v", step, err)
			}
			model[key] = value

		default:
			removed, err := tree.Delete([]byte(key))
			if err != nil {
				t.Fatalf("step %d: delete: %v", step, err)
			}
			_, inModel := model[key]
			if removed != inModel {
				t.Fatalf("step %d: delete %q said %v, the model says %v", step, key, removed, inModel)
			}
			delete(model, key)
		}

		if step%2000 != 0 {
			continue
		}

		check(t, tree)
		if err := tree.Commit(); err != nil {
			t.Fatalf("step %d: commit: %v", step, err)
		}

		// Reopened from the durable image, it must be the same tree. The live
		// disk is left alone so the run can carry on from here.
		restart := vfs.NewSim(6, vfs.Faults{})
		restart.Restore(disk.Durable())
		reopened, err := pager.Open(restart, 0)
		if err != nil {
			t.Fatalf("step %d: reopen: %v", step, err)
		}
		compare(t, At(reopened, reopened.Meta().Root), model, step)
	}

	compare(t, tree, model, -1)
}

func compare(t *testing.T, tree *Tree, model map[string]string, step int) {
	t.Helper()

	keys, values := collect(t, tree, nil)
	if len(keys) != len(model) {
		t.Fatalf("step %d: the tree holds %d keys, the model %d", step, len(keys), len(model))
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("step %d: the walk is out of order", step)
	}
	for i, key := range keys {
		if values[i] != model[key] {
			t.Fatalf("step %d: %s = %q, the model says %q", step, key, values[i], model[key])
		}
	}
}

func TestAKeyOrValueThatDoesNotFitIsRefused(t *testing.T) {
	_, tree := freshTree(t, 7)

	if err := tree.Put(nil, []byte("x")); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("empty key: want ErrEmptyKey, got %v", err)
	}
	if err := tree.Put(bytes.Repeat([]byte("k"), MaxKey+1), []byte("x")); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("long key: want ErrKeyTooLarge, got %v", err)
	}
	if err := tree.Put([]byte("k"), bytes.Repeat([]byte("v"), MaxValue+1)); !errors.Is(err, ErrValueTooLarge) {
		t.Errorf("long value: want ErrValueTooLarge, got %v", err)
	}

	// The largest key and the largest value that stays in the leaf must fit
	// twice over: two of them have to share a page for a split to be possible.
	big := bytes.Repeat([]byte("v"), MaxInline)
	for i := 0; i < 50; i++ {
		key := append(bytes.Repeat([]byte("k"), MaxKey-4), []byte(fmt.Sprintf("%04d", i))...)
		if err := tree.Put(key, big); err != nil {
			t.Fatalf("the largest entry the tree allows did not fit: %v", err)
		}
	}
	check(t, tree)
}

func TestOnlyCommittedWorkSurvivesACrash(t *testing.T) {
	disk := vfs.NewSim(8, vfs.Faults{})
	pages, err := pager.Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}
	tree := New(pages)

	for i := 0; i < 800; i++ {
		put(t, tree, fmt.Sprintf("committed-%04d", i), "yes")
	}
	if err := tree.Commit(); err != nil {
		t.Fatal(err)
	}

	// Work that never commits, enough of it to split pages and move the root.
	for i := 0; i < 800; i++ {
		put(t, tree, fmt.Sprintf("uncommitted-%04d", i), "no")
	}

	after, err := pager.Open(disk.Crash(), 0)
	if err != nil {
		t.Fatalf("open after the crash: %v", err)
	}
	restored := New(after)
	check(t, restored)

	keys, _ := collect(t, restored, nil)
	if len(keys) != 800 {
		t.Fatalf("the restored tree holds %d keys, want the 800 that were committed", len(keys))
	}
	for _, key := range keys {
		if !bytes.HasPrefix([]byte(key), []byte("committed-")) {
			t.Fatalf("work that was never committed survived: %q", key)
		}
	}
}

func TestAReaderKeepsTheTreeItOpened(t *testing.T) {
	// Copy-on-write in one test: a reader holding an old root sees the database
	// as it was, however much the writer changes afterwards.
	_, tree := freshTree(t, 9)

	for i := 0; i < 1000; i++ {
		put(t, tree, fmt.Sprintf("key-%04d", i), "first")
	}
	if err := tree.Commit(); err != nil {
		t.Fatal(err)
	}
	reader := At(tree.pages, tree.Root())

	for i := 0; i < 1000; i++ {
		put(t, tree, fmt.Sprintf("key-%04d", i), "second")
	}
	for i := 0; i < 500; i++ {
		if _, err := tree.Delete([]byte(fmt.Sprintf("key-%04d", i))); err != nil {
			t.Fatal(err)
		}
	}

	keys, values := collect(t, reader, nil)
	if len(keys) != 1000 {
		t.Fatalf("the reader lost keys: %d of 1000", len(keys))
	}
	for i, value := range values {
		if value != "first" {
			t.Fatalf("the reader saw %q at %s: a writer changed a page under it", value, keys[i])
		}
	}
}

// A node whose bytes are intact but whose contents make no sense — a page from
// another version of this format, or one a wild pointer led to — must be an
// error. Never a panic, and never a slice of whatever lay next to it.
func TestANodeThatMakesNoSenseIsRefused(t *testing.T) {
	damage := map[string]func(payload []byte){
		"a slot pointing past the page": func(payload []byte) {
			binary.BigEndian.PutUint16(payload[leafHeader:], 0xfff0)
		},
		"more entries than could fit": func(payload []byte) {
			binary.BigEndian.PutUint16(payload, 0xffff)
		},
		"a slot pointing into the slot array": func(payload []byte) {
			binary.BigEndian.PutUint16(payload[leafHeader:], 1)
		},
	}

	for name, break_ := range damage {
		t.Run(name, func(t *testing.T) {
			_, tree := freshTree(t, 13)
			for i := 0; i < 8; i++ {
				put(t, tree, fmt.Sprintf("k%d", i), "v")
			}

			page, err := tree.pages.Read(tree.Root())
			if err != nil {
				t.Fatal(err)
			}
			break_(page.Payload())
			if err := tree.pages.Write(page); err != nil { // reseals the checksum
				t.Fatal(err)
			}

			if _, _, err := tree.Get([]byte("k1")); !errors.Is(err, ErrMalformed) {
				t.Errorf("get: want ErrMalformed, got %v", err)
			}
			if err := tree.Ascend(nil, func(_, _ []byte) bool { return true }); !errors.Is(err, ErrMalformed) {
				t.Errorf("ascend: want ErrMalformed, got %v", err)
			}
		})
	}
}

func TestABranchWithAZeroChildIsRefused(t *testing.T) {
	_, tree := freshTree(t, 14)
	for i := 0; i < 400; i++ {
		put(t, tree, fmt.Sprintf("key-%04d", i), "x")
	}
	if check(t, tree) < 2 {
		t.Fatal("this test needs a branch node")
	}

	page, err := tree.pages.Read(tree.Root())
	if err != nil {
		t.Fatal(err)
	}
	node, err := decodeBranch(page)
	if err != nil {
		t.Fatal(err)
	}
	node.children[0] = 0
	if err := encodeBranch(page, node); err != nil {
		t.Fatal(err)
	}
	if err := tree.pages.Write(page); err != nil {
		t.Fatal(err)
	}

	if _, _, err := tree.Get([]byte("key-0000")); !errors.Is(err, ErrMalformed) {
		t.Errorf("want ErrMalformed, got %v", err)
	}
}

// Deleting from a tree whose leaves are too full to merge sends the rebalance
// down its other path: the pair is poured together and split again, and the
// separator between them has to be the first key of the new right-hand leaf.
// One place out and a key that is still in the tree stops being findable.
func TestLeavesTooFullToMergeAreSplitAgainCorrectly(t *testing.T) {
	_, tree := freshTree(t, 15)

	// Sized so that four entries fill a leaf and one entry is underfull: every
	// delete near a leaf boundary has to rebalance, and the pair rarely fits in
	// a single page.
	value := bytes.Repeat([]byte("v"), 900)
	model := map[string]bool{}
	for i := 0; i < 400; i++ {
		key := fmt.Sprintf("key-%04d", i)
		put(t, tree, key, string(value))
		model[key] = true
	}
	if check(t, tree) < 2 {
		t.Fatal("this test needs branch nodes")
	}

	random := rand.New(rand.NewSource(15))
	keys := make([]string, 0, len(model))
	for key := range model {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	random.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	for _, key := range keys[:len(keys)/2] {
		if removed, err := tree.Delete([]byte(key)); err != nil || !removed {
			t.Fatalf("delete %q: %v, %v", key, removed, err)
		}
		delete(model, key)
		check(t, tree)

		// Every key still in the model must still be findable by search — the
		// walk would find it either way, so this has to go through Get.
		for remaining := range model {
			if _, found := get(t, tree, remaining); !found {
				t.Fatalf("after deleting %q, searching for %q found nothing", key, remaining)
			}
		}
	}
}

func TestANodeTooBigForItsPageIsRefused(t *testing.T) {
	// The guard behind every split: a node that does not fit is an error, not a
	// page written over its own slot array.
	entries := make([]kv, 0, 64)
	for i := 0; len(entries)*100 < capacityBytes*2; i++ {
		entries = append(entries, kv{key: []byte(fmt.Sprintf("k%04d", i)), value: bytes.Repeat([]byte("v"), 100)})
	}

	page := &pager.Page{ID: 7, Kind: pager.KindLeaf, Data: make([]byte, pager.PageBytes)}
	if err := encodeLeaf(page, entries); !errors.Is(err, ErrCannotSplit) {
		t.Errorf("a leaf of %d bytes: want ErrCannotSplit, got %v", leafBytes(entries), err)
	}

	node := branch{children: []uint64{1}}
	for i := 0; branchBytes(node) < capacityBytes*2; i++ {
		node.keys = append(node.keys, bytes.Repeat([]byte("k"), MaxKey))
		node.children = append(node.children, uint64(i+2))
	}
	if err := encodeBranch(page, node); !errors.Is(err, ErrCannotSplit) {
		t.Errorf("a branch of %d bytes: want ErrCannotSplit, got %v", branchBytes(node), err)
	}
}

// The branch-level twin of the leaf case above: a branch that has emptied,
// next to one that is nearly full. They are too big to merge, so they are
// poured together and split again, and the separator that ends up in the
// parent decides whether the keys on the right are still reachable.
//
// This shape is built by hand rather than hoped for. Random work does not
// reach it: a branch is only ever this full while it is being filled, and by
// the time its neighbour has emptied it has usually been split in half.
func TestBranchesTooFullToMergeAreSplitAgainCorrectly(t *testing.T) {
	_, tree := freshTree(t, 16)

	pad := bytes.Repeat([]byte("p"), MaxKey-8)
	key := func(i int) []byte {
		return append(append([]byte(nil), []byte(fmt.Sprintf("%08d", i))...), pad...)
	}

	leaf := func(keys ...int) uint64 {
		entries := make([]kv, 0, len(keys))
		for _, at := range keys {
			entries = append(entries, kv{key: key(at), value: []byte(fmt.Sprint(at))})
		}
		id, err := tree.writeLeaf(0, entries)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	node := func(children []uint64, separators []int) uint64 {
		made := branch{children: children}
		for _, at := range separators {
			made.keys = append(made.keys, key(at))
		}
		id, err := tree.writeBranch(0, made)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Left: two small leaves, so one delete empties the branch above them.
	left := node([]uint64{leaf(0, 1), leaf(2, 3)}, []int{2})

	// Right: as many children as a branch of these keys can hold.
	var children []uint64
	var separators []int
	for i := 0; i < 8; i++ {
		children = append(children, leaf(10+i))
		if i > 0 {
			separators = append(separators, 10+i)
		}
	}
	right := node(children, separators)

	tree.root = node([]uint64{left, right}, []int{10})
	check(t, tree)

	// One delete: the two left leaves merge, the branch above them is left with
	// nothing, and the parent has to rebalance it against a neighbour that will
	// not fit beside it.
	if removed, err := tree.Delete(key(0)); err != nil || !removed {
		t.Fatalf("delete: %v, %v", removed, err)
	}
	check(t, tree)

	for _, at := range []int{1, 2, 3, 10, 11, 12, 13, 14, 15, 16, 17} {
		value, found, err := tree.Get(key(at))
		if err != nil || !found {
			t.Fatalf("key %d: found=%v, err=%v", at, found, err)
		}
		if string(value) != fmt.Sprint(at) {
			t.Errorf("key %d reads as %q", at, value)
		}
	}
	if _, found, _ := tree.Get(key(0)); found {
		t.Error("the deleted key is still there")
	}

	walked, _ := collect(t, tree, nil)
	if len(walked) != 11 {
		t.Errorf("the walk has %d keys, want 11", len(walked))
	}
}

// A value too big for a leaf goes to pages of its own, and has to come back
// exactly — including at the sizes where it lands on a page boundary.
func TestLargeValuesGoOutOfLineAndComeBackWhole(t *testing.T) {
	sizes := []int{
		MaxInline,     // the last size that stays in the leaf
		MaxInline + 1, // the first that does not
		overflowChunk - 1, overflowChunk, overflowChunk + 1,
		2*overflowChunk - 1, 2 * overflowChunk, 2*overflowChunk + 1,
		300 * 1024,
	}

	_, tree := freshTree(t, 17)
	model := map[string][]byte{}

	for i, size := range sizes {
		key := fmt.Sprintf("value-%02d", i)
		value := make([]byte, size)
		for at := range value {
			// Not a repeated byte: a chain stitched together in the wrong order
			// would still look right if every byte were the same.
			value[at] = byte(at*7 + i)
		}
		if err := tree.Put([]byte(key), value); err != nil {
			t.Fatalf("put %d bytes: %v", size, err)
		}
		model[key] = value
	}
	check(t, tree)

	// The boundary is where it says it is: one more byte and the leaf holds a
	// reference instead of the value.
	page, err := tree.pages.Read(tree.Root())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := decodeLeaf(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		size := len(model[string(entry.key)])
		if entry.overflows() != (size > MaxInline) {
			t.Errorf("a %d-byte value: overflows=%v", size, entry.overflows())
		}
		if entry.overflows() && int(entry.length) != size {
			t.Errorf("a %d-byte value says it is %d bytes", size, entry.length)
		}
	}

	for key, want := range model {
		got, found, err := tree.Get([]byte(key))
		if err != nil || !found {
			t.Fatalf("%s: found=%v, err=%v", key, found, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes back of %d, and not the same ones", key, len(got), len(want))
		}
	}
}

// Large values must survive everything the tree does to its leaves: splits,
// merges, overwrites, and a restart.
func TestLargeValuesSurviveSplitsAndARestart(t *testing.T) {
	disk := vfs.NewSim(18, vfs.Faults{})
	pages, err := pager.Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}
	tree := New(pages)

	// With the value out of line a leaf entry is only a key and a page number,
	// so it takes rather more documents than it used to before a leaf splits —
	// which is the point of moving them out.
	body := func(i int) []byte {
		value := make([]byte, 5000+i%97*13)
		for at := range value {
			value[at] = byte(at + i)
		}
		return value
	}

	const count = 1200
	for i := 0; i < count; i++ {
		if err := tree.Put([]byte(fmt.Sprintf("doc-%04d", i)), body(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if check(t, tree) < 2 {
		t.Fatal("this test wants the leaves to have split")
	}

	// Overwritten with something of a different shape, so the entry changes
	// from one chain to another and, for some, back into the leaf.
	for i := 0; i < count; i += 3 {
		small := []byte(fmt.Sprintf("small-%d", i))
		if err := tree.Put([]byte(fmt.Sprintf("doc-%04d", i)), small); err != nil {
			t.Fatal(err)
		}
	}
	if err := tree.Commit(); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(18, vfs.Faults{})
	restart.Restore(disk.Durable())
	reopened, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	after := New(reopened)
	check(t, after)

	for i := 0; i < count; i++ {
		want := body(i)
		if i%3 == 0 {
			want = []byte(fmt.Sprintf("small-%d", i))
		}
		got, found, err := after.Get([]byte(fmt.Sprintf("doc-%04d", i)))
		if err != nil || !found {
			t.Fatalf("doc %d: found=%v, err=%v", i, found, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("doc %d came back as %d bytes, want %d", i, len(got), len(want))
		}
	}

	// And the walk hands back the same values the search does.
	keys, values := collect(t, after, nil)
	if len(keys) != count {
		t.Fatalf("the walk has %d keys of %d", len(keys), count)
	}
	for i, key := range keys {
		got, _, err := after.Get([]byte(key))
		if err != nil {
			t.Fatal(err)
		}
		if values[i] != string(got) {
			t.Fatalf("%s reads differently through the walk than through a search", key)
		}
	}
}

// A chain that does not hold together is an error, never a value with a hole
// in it.
func TestABrokenChainIsRefused(t *testing.T) {
	damage := map[string]func(t *testing.T, tree *Tree, head uint64){
		"a chunk that claims more than a page holds": func(t *testing.T, tree *Tree, head uint64) {
			page, err := tree.pages.Read(head)
			if err != nil {
				t.Fatal(err)
			}
			binary.BigEndian.PutUint32(page.Payload()[overflowLength:], uint32(capacityBytes+1))
			if err := tree.pages.Write(page); err != nil {
				t.Fatal(err)
			}
		},
		"a chain that ends early": func(t *testing.T, tree *Tree, head uint64) {
			page, err := tree.pages.Read(head)
			if err != nil {
				t.Fatal(err)
			}
			binary.BigEndian.PutUint64(page.Payload()[overflowNext:], 0)
			if err := tree.pages.Write(page); err != nil {
				t.Fatal(err)
			}
		},
		"a chunk that is not a chunk at all": func(t *testing.T, tree *Tree, head uint64) {
			page, err := tree.pages.Read(head)
			if err != nil {
				t.Fatal(err)
			}
			page.Kind = pager.KindLeaf
			if err := tree.pages.Write(page); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, break_ := range damage {
		t.Run(name, func(t *testing.T) {
			_, tree := freshTree(t, 19)
			if err := tree.Put([]byte("big"), bytes.Repeat([]byte("v"), 3*overflowChunk)); err != nil {
				t.Fatal(err)
			}

			page, err := tree.pages.Read(tree.Root())
			if err != nil {
				t.Fatal(err)
			}
			entries, err := decodeLeaf(page)
			if err != nil {
				t.Fatal(err)
			}
			if !entries[0].overflows() {
				t.Fatal("this test needs a value that overflowed")
			}

			break_(t, tree, entries[0].head)

			if _, _, err := tree.Get([]byte("big")); !errors.Is(err, ErrBrokenChain) {
				t.Errorf("want ErrBrokenChain, got %v", err)
			}
		})
	}
}

// A leaf whose reference points at a meta page is damage, not a value: a chain
// may never start at page 0 or 1.
//
// Page 0 has to be written into the page by hand. In memory a head of zero is
// how an entry says it holds its value inline, so only the bytes on disk can
// say "out of line, starting at page zero" — which is exactly the damaged
// leaf this guards against.
func TestAChainThatStartsAtAMetaPageIsRefused(t *testing.T) {
	for _, head := range []uint64{0, 1} {
		t.Run(fmt.Sprintf("head %d", head), func(t *testing.T) {
			_, tree := freshTree(t, 20)
			if err := tree.Put([]byte("big"), bytes.Repeat([]byte("v"), 2*overflowChunk)); err != nil {
				t.Fatal(err)
			}

			page, err := tree.pages.Read(tree.Root())
			if err != nil {
				t.Fatal(err)
			}
			payload := page.Payload()
			at := int(binary.BigEndian.Uint16(payload[leafHeader:]))
			keyLen := int(binary.BigEndian.Uint16(payload[at:]))
			if binary.BigEndian.Uint32(payload[at+2:])&overflowFlag == 0 {
				t.Fatal("this test needs a value that overflowed")
			}
			binary.BigEndian.PutUint64(payload[at+leafEntry+keyLen:], head)
			if err := tree.pages.Write(page); err != nil {
				t.Fatal(err)
			}

			if _, _, err := tree.Get([]byte("big")); !errors.Is(err, ErrMalformed) {
				t.Errorf("want ErrMalformed, got %v", err)
			}
		})
	}
}

// A database that is only ever updated must stop growing. Without a free list
// every overwrite costs pages forever; with one the file settles at the size of
// what is actually stored.
func TestOverwritingTheSameKeysStopsGrowingTheFile(t *testing.T) {
	_, tree := freshTree(t, 21)

	const keys = 400
	write := func(round int) {
		for i := 0; i < keys; i++ {
			put(t, tree, fmt.Sprintf("key-%04d", i), fmt.Sprintf("round-%d-value-%d", round, i))
		}
		if err := tree.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	write(0)
	settled := tree.pages.Meta().PageCount

	var sizes []uint64
	for round := 1; round <= 12; round++ {
		write(round)
		check(t, tree)
		sizes = append(sizes, tree.pages.Meta().PageCount)
	}

	// The last rounds must all be the same size: freed pages come back one
	// transaction later, so the first round or two may still grow.
	last := sizes[len(sizes)-1]
	for _, size := range sizes[len(sizes)-5:] {
		if size != last {
			t.Fatalf("the file is still growing: %v", sizes)
		}
	}
	if last > settled*3 {
		t.Errorf("the file settled at %d pages, %d were needed to store the data", last, settled)
	}

	// And it still holds what it should.
	for i := 0; i < keys; i++ {
		want := fmt.Sprintf("round-%d-value-%d", 12, i)
		if value, found := get(t, tree, fmt.Sprintf("key-%04d", i)); !found || value != want {
			t.Fatalf("key %d = %q, want %q", i, value, want)
		}
	}
}

// The promise copy-on-write makes, now that pages are reused: a reader that
// holds a snapshot sees the database as it was, however many times the writer
// goes round and whatever it hands back out.
func TestASnapshotReaderIsSafeWhilePagesAreReused(t *testing.T) {
	_, tree := freshTree(t, 22)

	const keys = 300
	for i := 0; i < keys; i++ {
		put(t, tree, fmt.Sprintf("key-%04d", i), "first")
	}
	if err := tree.Commit(); err != nil {
		t.Fatal(err)
	}

	held := tree.pages.Snapshot()
	reader := At(tree.pages, held.Root)

	// Enough rounds that, without the hold, the reader's pages would have been
	// handed out several times over.
	for round := 0; round < 10; round++ {
		for i := 0; i < keys; i++ {
			put(t, tree, fmt.Sprintf("key-%04d", i), fmt.Sprintf("round-%d", round))
		}
		if err := tree.Commit(); err != nil {
			t.Fatal(err)
		}

		walked, values := collect(t, reader, nil)
		if len(walked) != keys {
			t.Fatalf("round %d: the reader sees %d keys of %d", round, len(walked), keys)
		}
		for i, value := range values {
			if value != "first" {
				t.Fatalf("round %d: the reader sees %q at %s", round, value, walked[i])
			}
		}
	}

	// Once it lets go, those pages are ordinary free space again.
	before := tree.pages.FreePages()
	held.Release()
	if _, err := tree.pages.Allocate(); err != nil {
		t.Fatal(err)
	}
	if after := tree.pages.FreePages(); after >= before {
		t.Errorf("after the snapshot was released the list still holds %d of %d pages", after, before)
	}
}

// Reuse and the crash fallback have to agree. A transaction may write into
// pages the one before it gave up, because a crash lands on the last committed
// state and that state does not point at them. It may never write into pages
// it is giving up itself: those are exactly what the state a crash lands on is
// made of.
func TestACrashWhileReusingPagesLeavesTheCommittedTreeIntact(t *testing.T) {
	const keys = 400

	for attempt := int64(0); attempt < 8; attempt++ {
		disk := vfs.NewSim(23+attempt, vfs.Faults{})
		pages, err := pager.Create(disk, 0)
		if err != nil {
			t.Fatal(err)
		}
		tree := New(pages)

		// Several rounds, so that by the last one the writer is working almost
		// entirely in pages earlier rounds gave back.
		round := 0
		for ; round < 4; round++ {
			for i := 0; i < keys; i++ {
				put(t, tree, fmt.Sprintf("key-%04d", i), fmt.Sprintf("round-%d", round))
			}
			if err := tree.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		if tree.pages.FreePages() == 0 {
			t.Fatal("this test is only worth running once pages are being reused")
		}
		committed := fmt.Sprintf("round-%d", round-1)

		// One more round that never commits, cut off part way through.
		stop := int(attempt+1) * keys / 8
		for i := 0; i < stop; i++ {
			put(t, tree, fmt.Sprintf("key-%04d", i), "never committed")
		}

		after, err := pager.Open(disk.Crash(), 0)
		if err != nil {
			t.Fatalf("attempt %d: open after the crash: %v", attempt, err)
		}
		restored := New(after)
		check(t, restored)

		walked, values := collect(t, restored, nil)
		if len(walked) != keys {
			t.Fatalf("attempt %d: the restored tree has %d keys of %d", attempt, len(walked), keys)
		}
		for i, value := range values {
			if value != committed {
				t.Fatalf("attempt %d: %s reads as %q, want %q", attempt, walked[i], value, committed)
			}
		}
	}
}

// The same, for values that live in pages of their own: overwriting one has to
// give its chain back, or a database of documents grows without bound however
// small it really is.
func TestOverwritingLargeValuesStopsGrowingTheFile(t *testing.T) {
	_, tree := freshTree(t, 40)

	const keys = 60
	body := func(round, i int) []byte {
		value := make([]byte, 3*overflowChunk+i)
		for at := range value {
			value[at] = byte(at + round)
		}
		return value
	}
	write := func(round int) {
		for i := 0; i < keys; i++ {
			if err := tree.Put([]byte(fmt.Sprintf("doc-%03d", i)), body(round, i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tree.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	write(0)
	settled := tree.pages.Meta().PageCount

	var sizes []uint64
	for round := 1; round <= 8; round++ {
		write(round)
		sizes = append(sizes, tree.pages.Meta().PageCount)
	}
	check(t, tree)

	last := sizes[len(sizes)-1]
	for _, size := range sizes[len(sizes)-3:] {
		if size != last {
			t.Fatalf("the file is still growing: %v", sizes)
		}
	}
	if last > settled*3 {
		t.Errorf("the file settled at %d pages; %d hold the data", last, settled)
	}

	for i := 0; i < keys; i++ {
		want := body(8, i)
		got, found, err := tree.Get([]byte(fmt.Sprintf("doc-%03d", i)))
		if err != nil || !found || !bytes.Equal(got, want) {
			t.Fatalf("doc %d: %d bytes, found=%v, err=%v", i, len(got), found, err)
		}
	}
}

// Space that deletion frees has to come back — including the pages of nodes
// that were merged away, which is most of them when a tree empties.
func TestTheSpaceDeletionFreesComesBack(t *testing.T) {
	_, tree := freshTree(t, 41)

	const keys = 1500
	refill := func() {
		for i := 0; i < keys; i++ {
			put(t, tree, fmt.Sprintf("key-%05d", i), fmt.Sprintf("value-%d", i))
		}
		if err := tree.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	refill()
	full := tree.pages.Meta().PageCount

	for i := 0; i < keys; i++ {
		if removed, err := tree.Delete([]byte(fmt.Sprintf("key-%05d", i))); err != nil || !removed {
			t.Fatalf("delete %d: %v, %v", i, removed, err)
		}
	}
	if err := tree.Commit(); err != nil {
		t.Fatal(err)
	}
	// A second commit, so that what the first one freed is past the crash
	// fallback and available again.
	if err := tree.Commit(); err != nil {
		t.Fatal(err)
	}
	if freed := tree.pages.FreePages(); freed < int(full)/2 {
		t.Fatalf("emptying a %d-page tree gave back only %d pages", full, freed)
	}

	refill()
	check(t, tree)

	if grown := tree.pages.Meta().PageCount; grown > full*3/2 {
		t.Errorf("refilling the same data grew the file from %d to %d pages", full, grown)
	}
}

// collectDown is every key and value the tree hands back walking downward.
func collectDown(t *testing.T, tree *Tree, from []byte) ([]string, []string) {
	t.Helper()
	var keys, values []string
	if err := tree.Descend(from, func(key, value []byte) bool {
		keys = append(keys, string(key))
		values = append(values, string(value))
		return true
	}); err != nil {
		t.Fatalf("descend: %v", err)
	}
	return keys, values
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, one := range in {
		out[len(in)-1-i] = one
	}
	return out
}

// TestDescendHandsBackWhatAscendDoesBackwards: the two walks see the same tree,
// so they must see the same entries — every key, with its own value still
// attached, in the opposite order. A walk that quietly dropped the first or the
// last entry would still look sorted, which is why this compares the whole list
// rather than the ends of it.
func TestDescendHandsBackWhatAscendDoesBackwards(t *testing.T) {
	_, tree := freshTree(t, 11)

	for i := 0; i < 700; i++ {
		put(t, tree, fmt.Sprintf("k%04d", i), fmt.Sprintf("v%04d", i))
	}
	if height := check(t, tree); height < 2 {
		t.Fatalf("the tree is %d levels deep, so this says nothing about branches", height)
	}

	up, upValues := collect(t, tree, nil)
	down, downValues := collectDown(t, tree, nil)

	if fmt.Sprint(down) != fmt.Sprint(reversed(up)) {
		t.Errorf("walking down gave %d keys that are not the %d from walking up, reversed", len(down), len(up))
	}
	if fmt.Sprint(downValues) != fmt.Sprint(reversed(upValues)) {
		t.Error("the values did not come back attached to the same keys")
	}
	for i := 1; i < len(down); i++ {
		if down[i-1] <= down[i] {
			t.Fatalf("walking down, %q came before %q", down[i-1], down[i])
		}
	}
}

// TestDescendStopsJustBelowWhereItIsTold: Descend's bound is exclusive where
// Ascend's is inclusive, so that Ascend(lo) and Descend(hi) cover exactly the
// same half-open stretch [lo, hi). The key sitting exactly on the bound is the
// only one that can tell the two conventions apart, so it is what this checks.
func TestDescendStopsJustBelowWhereItIsTold(t *testing.T) {
	_, tree := freshTree(t, 12)

	for i := 0; i < 500; i++ {
		put(t, tree, fmt.Sprintf("k%04d", i*2), fmt.Sprint(i))
	}

	for _, want := range []struct {
		from  string
		first string
		count int
	}{
		// k0500 exists, and is left out: the walk starts below it.
		{from: "k0500", first: "k0498", count: 250},
		// k0501 does not exist, so the largest key below it is k0500.
		{from: "k0501", first: "k0500", count: 251},
		{from: "zzz", first: "k0998", count: 500},
	} {
		keys, _ := collectDown(t, tree, []byte(want.from))
		if len(keys) == 0 {
			t.Fatalf("from %q: nothing", want.from)
		}
		if keys[0] != want.first {
			t.Errorf("from %q the walk starts at %q, want %q", want.from, keys[0], want.first)
		}
		if len(keys) != want.count {
			t.Errorf("from %q the walk has %d keys, want %d", want.from, len(keys), want.count)
		}
	}

	// The smallest key in the tree is k0000, and nothing is below it.
	if keys, _ := collectDown(t, tree, []byte("k0000")); len(keys) != 0 {
		t.Errorf("a walk down from the smallest key returned %d keys", len(keys))
	}
	if keys, _ := collectDown(t, tree, []byte("a")); len(keys) != 0 {
		t.Errorf("a walk down from before the beginning returned %d keys", len(keys))
	}
}

// TestDescendStopsWhenTheCallbackSaysSo: stopping early has to be honoured at
// every level, not only within one leaf, or a limited read would keep paying
// for pages after it had all it asked for.
func TestDescendStopsWhenTheCallbackSaysSo(t *testing.T) {
	_, tree := freshTree(t, 13)

	for i := 0; i < 700; i++ {
		put(t, tree, fmt.Sprintf("k%04d", i), fmt.Sprint(i))
	}
	if height := check(t, tree); height < 2 {
		t.Fatalf("the tree is %d levels deep, so this says nothing about branches", height)
	}

	var seen []string
	if err := tree.Descend(nil, func(key, _ []byte) bool {
		seen = append(seen, string(key))
		return len(seen) < 5
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 5 {
		t.Fatalf("the walk visited %d keys after being stopped at 5", len(seen))
	}
	if seen[0] != "k0699" || seen[4] != "k0695" {
		t.Errorf("the first five keys walking down are %v", seen)
	}
}

func TestDescendOnAnEmptyTreeAnswersNothing(t *testing.T) {
	_, tree := freshTree(t, 14)

	if keys, _ := collectDown(t, tree, nil); len(keys) != 0 {
		t.Errorf("an empty tree handed back %d keys", len(keys))
	}
	if keys, _ := collectDown(t, tree, []byte("k")); len(keys) != 0 {
		t.Errorf("an empty tree handed back %d keys from a bound", len(keys))
	}
}
