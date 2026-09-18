// Package naming is the patrol that keeps the old product name from coming
// back. A rename is not "done" because a sed command ran once; it is done
// when nothing in the tree can reintroduce the old name without this package
// noticing. So this walks every file, not a sample of the ones the rename
// touched.
//
// It intentionally does not know the old name as a whole word anywhere in
// its own source — see target below — which is what lets it also patrol
// itself.
package naming

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// errNotText marks a file readFile declined to scan because it is not valid
// UTF-8. Never returned across a package boundary as anything other than
// "skip this file" — Walk does not distinguish it from any other read
// failure, on purpose: either way there is nothing to check.
var errNotText = errors.New("naming: not text")

// target is the word this patrol is looking for, spelled apart. Not for
// disguise: it is so that adding this package to the tree does not itself
// become a hit, and so that nobody can "fix" a red patrol by deleting the
// one place that still names what it is watching for.
var target = string([]byte{'r', 's', 'q', 'l'})

// skipDirs are trees this patrol does not enter. .git is not source; the
// other two only exist in the TypeScript sibling, but staying the same list
// shape in both repos is worth more than the two entries being dead weight
// here.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
}

// readFile returns a file's content as text, or an error if it plainly is
// not text. It exists so Walk has one place to decide that, instead of the
// decision being implicit in whatever os.ReadFile happens to return.
func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", errNotText
	}
	return string(raw), nil
}

// Hit is one line, in one file, that contains the old name.
type Hit struct {
	File string // relative to the root Walk was given
	Line int    // 1-based
	Text string
}

// Walk reads every regular file under root and reports every line that
// contains target, case-insensitively, regardless of how deep it is nested.
//
// A directory is not a signal either way; only file content is. A file this
// patrol cannot decode as text (there are none in either repo today) is
// skipped rather than failing the walk, because a patrol that cannot finish
// is a patrol nobody can trust the silence of.
func Walk(root string) ([]Hit, error) {
	var hits []Hit

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		content, err := readFile(path)
		if err != nil {
			// Not text, or not readable. Either way there is nothing this
			// patrol can check, and that is not the same claim as "clean".
			return nil
		}

		lower := strings.ToLower(content)
		if !strings.Contains(lower, target) {
			return nil
		}

		lines := strings.Split(content, "\n")
		lowerLines := strings.Split(lower, "\n")
		for i, lowerLine := range lowerLines {
			if strings.Contains(lowerLine, target) {
				hits = append(hits, Hit{File: rel, Line: i + 1, Text: lines[i]})
			}
		}
		return nil
	})

	return hits, err
}

// Exception is one line this patrol lets through: a file, and a fragment of
// that exact line that does not itself contain the old name, so this table
// can name what it excuses without becoming one more hit.
type Exception struct {
	File   string
	Marker string
	Why    string
}

// Exceptions is every place the old name is allowed to remain, and the whole
// point of writing it down: a new entry shows up in a diff, and a reviewer
// either agrees with Why or does not.
//
// pager.go's Magic is listed for the record even though the scan above never
// actually matches it — the tag is written as eight separate byte literals,
// with no four of them adjacent as text. It is a format tag, not a name, and
// this row says that on purpose rather than by the accident of how Go
// literals happen to print.
var Exceptions = []Exception{
	{
		File:   "internal/pager/pager.go",
		Marker: "Magic = [8]byte{'R', 'S', 'Q', 'L'",
		Why:    "the on-disk format tag; changing it makes every existing file unreadable",
	},
	{
		File:   "internal/pager/crypt.go",
		Marker: "pageLabel  = ",
		Why:    "a key-derivation label baked into every encrypted page",
	},
	{
		File:   "internal/pager/crypt.go",
		Marker: "checkLabel = ",
		Why:    "a key-derivation label baked into the encryption key-check",
	},
	{
		// The Marker is the whole line, value included, not just the
		// declaration keyword — on purpose. A prefix-only marker like
		// "const Label = " would excuse whatever value sits after it, and
		// this is the one label internal/cli and internal/server both derive
		// their database key from — see internal/dbkey's own comment. Built
		// by concatenation, not typed whole, for the same reason target is:
		// this file's own source must not spell the old name out
		// contiguously.
		//
		// There used to be two rows here, one for internal/cli/cli.go and one
		// for internal/server/server.go, because each package carried its own
		// copy of this label and its own hkdf.Key call. Task 0047 collapsed
		// both onto internal/dbkey.Key, so there is now exactly one line in
		// the whole tree that can carry this value, and exactly one row here
		// to excuse it. A single source can still be edited to a "v2" by
		// mistake — naming_test.go's
		// TestAllowedRequiresTheDbKeyLabelValueToMatchNotJustTheKeyword pins
		// that a marker this specific still refuses it — but it can no longer
		// drift from a sibling copy, because
		// there is no longer a sibling copy to drift from.
		File:   "internal/dbkey/dbkey.go",
		Marker: "const Label = \"" + target + "/server:database:v1\"",
		Why:    "the single key-derivation label internal/cli and internal/server both call dbkey.Key with; changing its value silently rotates the key of every encrypted database that already exists",
	},
}

// Allowed reports whether a hit is one this patrol excuses.
func Allowed(hit Hit) bool {
	for _, exception := range Exceptions {
		if hit.File == exception.File && strings.Contains(hit.Text, exception.Marker) {
			return true
		}
	}
	return false
}
