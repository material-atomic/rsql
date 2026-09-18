package naming

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Every fixture below is built from target rather than typed out, on
// purpose: this file lives in the tree that TestTheOldNameIsNowhereOutside-
// TheExceptionTable walks for real, so a literal old name here would be a
// hit in the very tree the patrol is supposed to call clean. target is the
// one place that word exists, and it exists there so this file does not
// have to repeat it.

// moduleRoot finds the repository root from this file's own path, so the
// test does not depend on the working directory `go test` happens to be
// run from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find this file's own path")
	}
	// internal/naming/naming_test.go -> repo root is two levels up.
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// TestTheOldNameIsNowhereOutsideTheExceptionTable is the promise this whole
// package exists to keep: every hit Walk finds in the real tree is one
// Allowed excuses, and nothing else survives the rename.
func TestTheOldNameIsNowhereOutsideTheExceptionTable(t *testing.T) {
	hits, err := Walk(moduleRoot(t))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	for _, hit := range hits {
		if !Allowed(hit) {
			t.Errorf("%s:%d carries the old name and is not in the exception table: %q",
				hit.File, hit.Line, hit.Text)
		}
	}
}

// TestExceptionTableNamesOnlyRealHits catches the opposite mistake: an
// exception whose Marker never matches anything Walk actually finds is a
// row nobody is reviewing against a real line, which is worse than not
// having it — it reads as documentation of something that no longer exists,
// or never did.
func TestExceptionTableNamesOnlyRealHits(t *testing.T) {
	hits, err := Walk(moduleRoot(t))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	matched := make(map[int]bool)
	for i, exception := range Exceptions {
		for _, hit := range hits {
			if hit.File == exception.File && Allowed(hit) {
				matched[i] = true
			}
		}
	}

	for i, exception := range Exceptions {
		// pager.go's Magic row is the one documented exception to this rule:
		// see its Why. Every other row must correspond to an actual hit.
		if exception.File == "internal/pager/pager.go" {
			continue
		}
		if !matched[i] {
			t.Errorf("exception %d (%s: %q) matched no hit Walk actually found",
				i, exception.File, exception.Marker)
		}
	}
}

// --- the patrol proving it patrols ---
//
// Everything below runs against a throwaway directory, never the real tree,
// so it says nothing about whether this repository is clean. It says
// whether Walk is capable of finding out.

// plant writes files under dir, creating parent directories as needed.
func plant(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWalkFindsTheOldNameAtTheBottomOfADeepTree(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join("a", "b", "c", "d", "e", "leftover.go")
	plant(t, dir, map[string]string{
		deep:         "package e // this still talks to " + target + " over the wire",
		"a/clean.go": `package a // nothing to see here`,
	})

	hits, err := Walk(dir)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].File != deep {
		t.Errorf("hit named the wrong file: %q", hits[0].File)
	}

	// This is the assertion that would fail if Walk stopped recursing after
	// the top level (or after the first non-empty directory): the offending
	// file is five directories deep, past every clean file at shallower
	// levels.
}

func TestWalkIsCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{
		"shout.go": "package shout // still says " + strings.ToUpper(target) + " in capitals",
		"title.go": "package title // and " + strings.ToUpper(target[:1]) + target[1:] + " in the middle of a sentence",
	})

	hits, err := Walk(dir)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("case-insensitive match should have caught both files, got %d hits: %+v", len(hits), hits)
	}
}

func TestWalkReportsEveryHitNotJustTheFirst(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{
		"one.go": "package one // " + target,
		"two.go": "package two // " + target,
		"multi.go": "package multi\n" +
			"// first mention of " + target + "\n" +
			"// second mention of " + target + "\n",
	})

	hits, err := Walk(dir)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// one.go, two.go: one hit each. multi.go: two hits, one per line.
	if len(hits) != 4 {
		t.Fatalf("want 4 hits across three files, got %d: %+v", len(hits), hits)
	}
}

// TestWalkDoesNotSkipCmd pins the boundary of skipDirs, not just Walk's
// reach: the self-proving tests above show Walk *can* find a hit — they say
// nothing about which directories it is allowed to stop entering. Adding
// "cmd" to skipDirs would pass every test above unchanged, because none of
// them plant a hit inside a directory literally named cmd. cmd/ is exactly
// where this rename's own mechanical pass renamed two binaries under the
// old name to their sapedb/sapedbd equivalents, and is exactly the kind of
// directory a future rename would most want the patrol still watching.
func TestWalkDoesNotSkipCmd(t *testing.T) {
	dir := t.TempDir()
	hit := filepath.Join("cmd", "sapedb", "leftover.go")
	plant(t, dir, map[string]string{
		hit:            "package main // still imports " + target + " directly",
		"cmd/clean.go": "package cmd // nothing to see here",
	})

	hits, err := Walk(dir)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(hits) != 1 || hits[0].File != hit {
		t.Fatalf("a hit inside cmd/ was not reported: %+v", hits)
	}
}

// TestAllowedIsScopedToTheExactLineNotTheWholeFile is the mutation this
// table exists to resist: widening an exception from "this line, in this
// file" to "anywhere in this file" would let a second, unrelated leftover
// hide behind a real exception's file name.
func TestAllowedIsScopedToTheExactLineNotTheWholeFile(t *testing.T) {
	sameFileDifferentLine := Hit{
		File: "internal/pager/crypt.go",
		Line: 999,
		Text: "// an unrelated leftover mentioning " + target + " that is not either derivation label",
	}
	if Allowed(sameFileDifferentLine) {
		t.Error("Allowed excused a line that only shares a file with a real exception, not its content")
	}
}

// TestAllowedRefusesAFileNotOnTheTable is the ordinary case the one above
// would otherwise stand alone as an edge of, and it closes the fourth cell
// of a 2x2 the two of them together are answering: same file/different file
// crossed with matching content/non-matching content. The test above
// (TestAllowedIsScopedToTheExactLineNotTheWholeFile) covers "same file,
// different content"; the old version of this one — a bare comment built
// from target alone, matching no Marker anywhere — only ever covered
// "different file, non-matching content". The cell neither one reached was
// "different file, matching content", and that is exactly the cell QA found
// a second copy of the key-derivation label waved through in round 1: the
// fixture below is now the real dbkey.Label line, verbatim, sitting in a
// file that is not on the table at all. Dropping the
// `hit.File == exception.File` check from Allowed is invisible to every
// other test in this file — Text-only matching already agrees with them —
// and is only caught here.
func TestAllowedRefusesAFileNotOnTheTable(t *testing.T) {
	hit := Hit{
		File: "internal/store/ops.go",
		Line: 1,
		Text: "const Label = \"" + target + "/server:database:v1\"",
	}
	if Allowed(hit) {
		t.Error("Allowed excused a file that is not in the exception table at all, even though its content matches a real exception's Marker verbatim")
	}
}

// TestAllowedRequiresTheDbKeyLabelValueToMatchNotJustTheKeyword pins the gap
// QA measured directly, and task 0047 changed what closing it means. Before
// 0047, internal/cli and internal/server each carried their own copy of this
// label, and the risk was the two copies drifting apart from each other —
// hence a Marker that pins the value, not just the keyword, on each of two
// rows. 0047 collapsed both callers onto internal/dbkey.Key, so there is now
// exactly one place in the tree this label's value can live. The risk this
// test now pins is narrower but not gone: a prefix-only marker
// ("const Label = ", value not included) would keep waving that one line
// through even after somebody edited its value to a "v2" — silently rotating
// the key of every encrypted database that already exists, the same failure
// the pre-0047 two-copy version of this test was written against, just with
// one copy left to make it happen to instead of two.
func TestAllowedRequiresTheDbKeyLabelValueToMatchNotJustTheKeyword(t *testing.T) {
	matching := Hit{
		File: "internal/dbkey/dbkey.go",
		Line: 27,
		Text: "const Label = \"" + target + "/server:database:v1\"",
	}
	if !Allowed(matching) {
		t.Fatal("Allowed refused the exact line internal/dbkey/dbkey.go's row names")
	}

	drifted := Hit{
		File: "internal/dbkey/dbkey.go",
		Line: 27,
		Text: "const Label = \"" + target + "/server:database:v2\"",
	}
	if Allowed(drifted) {
		t.Error("Allowed excused dbkey.Label after its value changed to v2 — a prefix-only marker would miss exactly this, and it is the only copy left, so nothing else in the tree would catch the rotation either")
	}
}

// TestWalkScansFilesWithNoRecognisedExtensionOrADotPrefix closes three gaps
// QA measured, in one test, because they share the same cause: every
// fixture above plants its hit inside a *.go file, so a mutant that
// filtered Walk down to files ending in .go/.mod/.json would pass every
// test above unchanged. That is exactly the filter that would have missed
// this rename's own real work: this tree has seven non-.go files —
// Dockerfile, Makefile, docker-compose.yml, README.md,
// examples/ledger/run.mjs, .gitignore, .dockerignore — and it is precisely
// those files where the task changed the image tag, ENTRYPOINT,
// /var/lib/sapedb, and the volume name. Along the way, a second axis:
// nothing above plants a hit in a dotfile either, so a mutant that skipped
// any file whose name starts with "." would also pass unchanged, and would
// have missed .gitignore and .dockerignore for a second, unrelated reason.
//
// Round 4 added the third axis, still the same underlying cause read one
// level up: the fixtures above close the dot-prefixed *file* case, but
// nothing here planted a hit inside a dot-prefixed *directory*, and
// .github/workflows/*.yml is exactly the file this rename's own image tag
// (built from target, the same one Dockerfile's fixture below uses) would
// have kept living in unnoticed if a mutant taught Walk to SkipDir on any
// directory name starting with ".". skipDirs today only
// skips .git, node_modules, and dist by exact name, so that mutant is not
// live against the real source — but nothing before round 4 proved it
// wasn't, and the exact same fixture shape (a dot-prefixed file, this time
// nested inside a dot-prefixed directory) closes it the same way .env
// closed the file case.
func TestWalkScansFilesWithNoRecognisedExtensionOrADotPrefix(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, map[string]string{
		"Dockerfile":               "FROM golang:1.24-alpine\n# still builds " + target + ":latest\n",
		".env":                     "SECRET=" + target + "_fixture\n",
		".github/workflows/ci.yml": "# still builds " + target + ":latest on every push\n",
		"clean.go":                 "package clean // nothing to see here",
	})

	hits, err := Walk(dir)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("want exactly three hits (Dockerfile, .env, .github/workflows/ci.yml), got %d: %+v", len(hits), hits)
	}
	found := map[string]bool{}
	for _, hit := range hits {
		found[hit.File] = true
	}
	if !found["Dockerfile"] {
		t.Error("a hit in a file with no recognised extension (Dockerfile) was not reported")
	}
	if !found[".env"] {
		t.Error("a hit in a hidden, dot-prefixed file (.env) was not reported")
	}
	if !found[filepath.Join(".github", "workflows", "ci.yml")] {
		t.Error("a hit inside a hidden, dot-prefixed directory (.github/workflows) was not reported")
	}
}
