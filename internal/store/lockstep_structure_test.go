package store

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestEveryIndexedFieldListReadIsOnTheList is the structural half of task
// 0054 — the bảng ca above (lockstep_test.go) only proves the lockstep
// property holds at the sites that exist TODAY. It says nothing about a
// SEVENTEENTH place that starts indexing into Fields/Terms/Group next
// month (the list below has exactly 16 entries — Reviewer 0054 measured
// that an earlier draft of this comment said "twelfth"/"thirteenth" here,
// which was simply wrong arithmetic against the list's own length, not a
// claim about the code).
//
// This greps every non-test .go source file in internal/store and
// internal/keys for an expression that reads a named element out of one of
// the lists this task is about (Fields, Terms, Group, fields, Include, Sum,
// Steps, Indexes, Rollups) by a loop-shaped index name (i, at, spread, idx),
// records which function each match falls inside, and requires that set of
// functions to equal EXACTLY the list below — no fewer, no more. A
// SEVENTEENTH reader appearing without a matching line added here fails
// this test; a reader disappearing (say, because a function was deleted)
// also fails it, so the list cannot go stale in either direction without
// someone noticing.
//
// The "which function" tracking is deliberately the simplest thing that
// works: scan the file top to bottom, and every line beginning with "func "
// updates "the current function". A match on a line that is still part of
// the PREVIOUS function's doc comment — because the doc comment sits above
// the func line it documents — is attributed to that previous function, not
// the one it is actually documenting. This is not a bug in the test: it is
// why store.bound is on the list below even though bound() itself does not
// index anything. Its doc comment ends right where boundAt's begins
// (scan.go), and boundAt's own doc comment says "fields[i].Path" while
// explaining why the loop names the field a failure happened at — a comment
// line, not code, and the naive scan attributes it to whichever func line
// came before it lexically, which is bound's. Measured, not asserted: an
// earlier draft of this list, built by reasoning about the code rather than
// running this exact scan, had fifteen entries and was missing this one.
//
// What this test does NOT catch, on purpose, said in words because a green
// test here buys more confidence than it should: endpointBound (ops.go)
// walks its terms with "for _, term := range endpoint.Terms" — a range over
// values, not an index — so a rewrite of it that silently stops after the
// first term (the exact shape of mutant M3 in the task doc, which the
// bảng ca in lockstep_test.go catches and this structural test does not)
// matches none of these patterns and is invisible here. Only the case table
// catches that shape; this test only catches a NEW index-shaped reader
// showing up unnoticed.
var indexedListRead = regexp.MustCompile(
	`(Fields|Terms|Group|fields|Include|Sum|Steps|Indexes|Rollups)\[(i|at|spread|idx)\]`)

var funcDeclaration = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?([A-Za-z_][A-Za-z0-9_]*)`)

// knownIndexedListReaders is task 0054 mục 2.3's list, measured on this
// repo at HEAD by the scan below — one line per function, each with the
// reason it belongs. package.Function is store's or keys's exported name,
// not the receiver type, since the point of this list is which FUNCTION
// reads a list by index, not which type it hangs off.
var knownIndexedListReaders = map[string]string{
	"keys.EncodeKey": "encodes one key component per field, in order: Encode(dst, value, fields[i])",

	"store.Declare": "assigns IDs to a spec's own Indexes and Rollups by position when installing it",
	"store.bound":   "attributed here by the naive scan above, not by its own body — see this test's doc comment",

	"store.boundAt":           "the loop constantIsEncodable's sibling: encodes each bound value with fields[i].encoding() and names fields[i].Path on failure",
	"store.contribute":        "walks c.spec.Rollups by index to reach each rollup's own ID and Group",
	"store.entriesFor":        "walks c.spec.Indexes by index to build every index entry for a document",
	"store.entriesForIndex":   "encodes one index entry field by field: index.Fields[i].encoding(), and names index.Fields[i].Path on failure; also reads the spread field by its own index",
	"store.index":             "finds a declared index by name, walking c.spec.Indexes by index",
	"store.rollup":            "finds a declared rollup by name, walking c.spec.Rollups by index",
	"store.runBatch":          "reaches each batch step's own address by index: &operation.Steps[i]",
	"store.sameRollup":        "compares two rollups' Group and Sum element by element, by index",
	"store.sameShape":         "compares two indexes' Fields and Include element by element, by index",
	"store.scanAcross":        "the mutant this task's debt names directly: Terms[i]/Fields[i].Path per field of a partitioned index",
	"store.totalsAcross":      "totalsAcross's own copy of the same rule, over rollup.Group",
	"store.validate":          "Spec.validate walks its own Rollups and Indexes by index to name one that fails",
	"store.validateOperation": "check()/constantIsEncodable() against fields[i], and &operation.Steps[i] for a batch",
}

func moduleRootForLockstepTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find this file's own path")
	}
	// this file lives at internal/store/lockstep_structure_test.go
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func scanFileForIndexedListReads(t *testing.T, path, pkg string, found map[string][]string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	current := ""
	lineNo := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if m := funcDeclaration.FindStringSubmatch(line); m != nil {
			current = m[1]
		}
		if indexedListRead.MatchString(line) && current != "" {
			key := pkg + "." + current
			found[key] = append(found[key], filepath.Base(path)+":"+itoa(lineNo))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func TestEveryIndexedFieldListReadIsOnTheList(t *testing.T) {
	root := moduleRootForLockstepTest(t)
	found := map[string][]string{}

	for _, dir := range []struct {
		path, pkg string
	}{
		{filepath.Join(root, "internal", "store"), "store"},
		{filepath.Join(root, "internal", "keys"), "keys"},
	} {
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			scanFileForIndexedListReads(t, filepath.Join(dir.path, name), dir.pkg, found)
		}
	}

	for name, reason := range knownIndexedListReaders {
		if _, ok := found[name]; !ok {
			t.Errorf("%s is on the known list (%s) but no longer reads a list by index — the list is stale, or the code moved", name, reason)
		}
	}
	for name, sites := range found {
		if _, ok := knownIndexedListReaders[name]; !ok {
			t.Errorf("%s reads a list by index at %v and is NOT on the known list — a new reader of Fields/Terms/Group/etc, or a rename of one already there; add it with a reason, after checking it does what the other 16 do", name, sites)
		}
	}
}
