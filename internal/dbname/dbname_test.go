package dbname

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// moduleRoot finds the repository root from this file's own path, the same
// way internal/naming's tests do — so the structural test below does not
// depend on the working directory `go test` happens to be run from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find this file's own path")
	}
	// internal/dbname/dbname_test.go -> repo root is two levels up.
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// TestCheckTable is usableComponent's own table, before task 0053 moved the
// body here — copied rather than reduced, because the whole point of the
// move is "this changed nothing about what is accepted or refused". Each
// row is one axis: empty, the two directory names, one separator per
// character in the ContainsAny set, one character outside the a-zA-Z0-9-_.
// alphabet, and both sides of the 64-character boundary.
func TestCheckTable(t *testing.T) {
	cases := []struct {
		name    string
		usable  bool
		comment string
	}{
		{name: "acme", usable: true},
		{name: "a", usable: true},
		{name: strings.Repeat("x", 64), usable: true, comment: "exactly 64: the boundary itself must pass"},
		{name: "a-b_c.d", usable: true, comment: "every allowed punctuation character at once"},
		{name: "", usable: false, comment: "empty"},
		{name: strings.Repeat("x", 65), usable: false, comment: "one past the boundary"},
		{name: ".", usable: false},
		{name: "..", usable: false},
		{name: "a/b", usable: false, comment: "forward slash"},
		{name: `a\b`, usable: false, comment: "backslash"},
		{name: "a:b", usable: false, comment: "colon"},
		{name: "a\x00b", usable: false, comment: "NUL"},
		{name: "a;b", usable: false, comment: "semicolon: not a separator, but outside the character table"},
		{name: "m|n", usable: false, comment: "pipe: same story as semicolon"},
		{name: "a b", usable: false, comment: "space"},
		{name: "a!b", usable: false, comment: "punctuation outside the table"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Check(c.name)
			got := err == nil
			if got != c.usable {
				t.Errorf("Check(%q) usable=%v (err=%v), want usable=%v — %s", c.name, got, err, c.usable, c.comment)
			}
		})
	}
}

// TestCheckPairNamesTheFailingHalf pins CheckPair's own wrapping: which word
// — "account" or "database" — attaches to which value, with the negative
// half spelled out too. A `Contains` check on a bare word like "account"
// would pass even if the two branches were swapped, since the sentence for
// the OTHER half also happens to contain the same substring nowhere near as
// often as house-rules.md's "Contains with a short string" warns about for
// this exact shape of test — so this checks the quoted value together with
// its label, and asserts the other label's absence.
func TestCheckPairNamesTheFailingHalf(t *testing.T) {
	err := CheckPair("a/b", "main")
	if err == nil || !errors.Is(err, Err) {
		t.Fatalf("CheckPair(%q, %q) = %v, want an error wrapping Err", "a/b", "main", err)
	}
	if !strings.Contains(err.Error(), `account "a/b"`) {
		t.Errorf("account error does not name the account: %v", err)
	}
	if strings.Contains(err.Error(), `database "`) {
		t.Errorf("account error also names a database: %v", err)
	}

	err = CheckPair("acme", "a/b")
	if err == nil || !errors.Is(err, Err) {
		t.Fatalf("CheckPair(%q, %q) = %v, want an error wrapping Err", "acme", "a/b", err)
	}
	if !strings.Contains(err.Error(), `database "a/b"`) {
		t.Errorf("database error does not name the database: %v", err)
	}
	if strings.Contains(err.Error(), `account "`) {
		t.Errorf("database error also names an account: %v", err)
	}

	if err := CheckPair("acme", "main"); err != nil {
		t.Errorf("CheckPair(%q, %q) = %v, want nil", "acme", "main", err)
	}
}

// TestNoValidPairCollidesAndEveryConfusableSpellingIsRefused is the axis
// task 0053 exists to close, stated as a table rather than a paragraph:
// house-rules.md's own account of this task says the trigger was never the
// character '/' — it was filepath.Join normalizing several different
// strings onto one path. So this checks two things about the same list at
// once: every pair CheckPair accepts produces its OWN filepath.Join path
// (nothing here duplicates that as a way to decide accept/refuse — the
// decision comes only from CheckPair; filepath.Join is used only to compare
// the paths of pairs already known to be accepted), and every pair that
// would have joined onto an already-used path is one CheckPair refuses
// before that join ever mattered.
func TestNoValidPairCollidesAndEveryConfusableSpellingIsRefused(t *testing.T) {
	type pair struct{ account, db string }
	cases := []pair{
		{"a/b", "c"},          // the original bug report, half one
		{"a", "b/c"},          // the original bug report, half two
		{"acme", "main"},      // control: the plain spelling
		{"acme/", "main"},     // trailing slash normalizes away
		{"acme/x/..", "main"}, // an embedded parent reference normalizes away
		{"./acme", "main"},    // a leading "./" normalizes away
		{"acme", "./main"},    // same, on the db half
		{"bravo", "main"},     // control: a second, genuinely different valid pair
		{"acme", "other"},     // control: a third, genuinely different valid pair
	}

	seenPath := map[string]pair{}
	for _, c := range cases {
		if err := CheckPair(c.account, c.db); err != nil {
			continue // refused before a path is ever built — exactly the point
		}
		path := filepath.Join("dir", c.account, c.db+".sapedb")
		if prior, ok := seenPath[path]; ok {
			t.Errorf("valid pairs %+v and %+v both produce %s", prior, c, path)
		}
		seenPath[path] = c
	}

	// And separately: name the five confusable spellings and the two bug-
	// report pairs explicitly, so a change that accidentally starts
	// accepting one of them fails with a message that says which, not just
	// "a collision somewhere in the table above".
	mustRefuse := []pair{
		{"a/b", "c"}, {"a", "b/c"},
		{"acme/", "main"}, {"acme/x/..", "main"}, {"./acme", "main"}, {"acme", "./main"},
	}
	for _, c := range mustRefuse {
		if err := CheckPair(c.account, c.db); err == nil {
			t.Errorf("CheckPair(%q, %q) = nil, want refused (this spelling would join the same file as a differently-written pair)", c.account, c.db)
		}
	}

	// Control: the plain, unconfusable pairs used above as controls must
	// all still be accepted — this table is not allowed to close the axis
	// by refusing everything.
	for _, c := range []pair{{"acme", "main"}, {"bravo", "main"}, {"acme", "other"}} {
		if err := CheckPair(c.account, c.db); err != nil {
			t.Errorf("CheckPair(%q, %q) = %v, want nil", c.account, c.db, err)
		}
	}
}

// dbPathFiles is the file set task 0053's own notes name as the two places
// that build a database's on-disk path independently, with no shared layer
// between them before this package existed: internal/cli/cli.go and
// internal/server/server.go. This is written down here, once, so
// TestNoSecondCopyOfThePathRule and its own doc comment do not have to
// repeat the list.
var dbPathFiles = map[string]bool{
	"internal/cli/cli.go":       true,
	"internal/server/server.go": true,
}

// TestNoSecondCopyOfThePathRule is the structural test this package's own
// doc comment promises: a third place that builds a database's file path —
// which always means writing the literal ".sapedb" somewhere, since that is
// the one string every such path ends in — is a place that skipped calling
// this package and wrote its own rule instead. It greps every non-test .go
// file in the tree, not just the two known ones, so a new file introducing
// a third copy fails this test even if nobody remembers to update
// dbPathFiles.
//
// This is deliberately blunter than parsing for filepath.Join calls shaped
// like "account, db+\".sapedb\"": that would need to understand Go
// expressions, and a rule that only fires on one exact syntactic shape is
// one a differently-shaped duplicate slips past. Grepping for the literal
// extension catches any way of building the string, at the cost of also
// firing on a file that merely mentions ".sapedb" without building a path
// from it — which, measured against the tree as it stands today, does not
// happen: the two files below are the only two that mention it anywhere,
// including in comments.
func TestNoSecondCopyOfThePathRule(t *testing.T) {
	root := moduleRoot(t)
	found := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == ".git" || name == "node_modules" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(content), ".sapedb") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	for file := range found {
		if !dbPathFiles[file] {
			t.Errorf("%s builds a database path (mentions \".sapedb\") but is not in dbPathFiles — a second copy of the path rule, or this test's own list is stale", file)
		}
	}
	for file := range dbPathFiles {
		if !found[file] {
			t.Errorf("dbPathFiles lists %s but it no longer mentions \".sapedb\" — the list is stale, update it", file)
		}
	}
}
