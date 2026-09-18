// This file lives in package server_test rather than package server because
// it is the one place in the suite that imports both internal/cli and
// internal/server, and verifies the thing neither package's own test file
// can: that a database internal/cli writes is one internal/server can
// actually open. cli_test.go runs the CLI on the CLI's own files;
// server_test.go runs the server on the server's own files. Each side has
// only ever agreed with itself — the exact shape that let two copies of a
// key-derivation label drift silently apart (see internal/dbkey), and the
// exact shape house-rules.md's "Chạy thật" section names as having let three
// real contract mismatches through unit tests before.
//
// It is also the only thing in the suite that catches that mine being laid
// again, and internal/naming's patrol is not a substitute for it. A second
// copy of the label that keeps the old product name and merely bumps the
// suffix to v2 turns itself in, because the old name is what the patrol
// watches for. The honest shape of the mistake does not: a well-meaning
// cleanup writes the label out under the current product name, still at v1,
// the patrol goes green because there is no old name left for it to hit, and
// the only thing that notices is a database one side wrote and the other
// cannot open. Measured: that honest second copy, plus deleting this file,
// leaves all 14 packages green. So do not delete it, do not t.Skip it, and
// do not argue that the patrol still covers this — against the shape a real
// person would actually type, it does not.
package server_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/cli"
	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/server"
)

const handoffSecret = "the secret only the control plane has"

// handoffSchema is the smallest apply file that gives the server something
// to see: one collection, no operations, since this test's assertion is
// "the server can open the file at all and read what is in it", not
// anything about operations.
const handoffSchema = `{
  "collections": [
    {"name": "articles", "key": {"path": "id", "type": "string", "auto": "ulid"}}
  ]
}`

// handoffEnv builds the lookup func cli.Run takes in place of the real
// environment, with the four variables every row needs and whatever the
// caller wants to add or override on top.
func handoffEnv(dir string, extra map[string]string) func(string) (string, bool) {
	vars := map[string]string{
		"SAPEDB_DIR":     dir,
		"SAPEDB_ACCOUNT": "acme",
		"SAPEDB_DB":      "main",
		"SAPEDB_SECRET":  handoffSecret,
	}
	for name, value := range extra {
		vars[name] = value
	}
	return func(name string) (string, bool) {
		value, found := vars[name]
		return value, found
	}
}

// writeSchema puts handoffSchema in dir and returns its path.
func writeSchema(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(path, []byte(handoffSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// applyWithCLI runs `sapedb apply` the way an operator would, through the
// real cli.Run entry point rather than anything internal to internal/cli.
//
// It must return before the caller starts a server on the same dir: cli.Run
// releases the directory lock inside run()'s deferred close (see
// internal/cli/cli.go's open()), and that defer has already fired by the
// time this function returns, because Go runs deferred calls before the
// function they are inside returns to its own caller. A server.New on the
// same dir while the CLI still held the lock would fail with
// vfs.ErrLocked, not with anything about keys — a test that saw ErrLocked
// here would be a broken test, not a finding about dbkey.
func applyWithCLI(t *testing.T, dir string, extraEnv map[string]string) {
	t.Helper()
	file := writeSchema(t, dir)
	out, errBuf := &strings.Builder{}, &strings.Builder{}
	if status := cli.Run([]string{"apply", file}, handoffEnv(dir, extraEnv), strings.NewReader(""), out, errBuf); status != 0 {
		t.Fatalf("cli apply failed: %s", errBuf.String())
	}
}

// TestServerOpensWhatCLIWrote is the test the task exists to add: nothing
// before it, anywhere in this repo, wrote a database with the CLI and then
// opened that same file with the server. Four rows, one shared shape,
// because the claim under test is not just "they agree today" (row 1 alone
// would only show that) but "they agree, and disagree for the right reason
// when they should disagree" (rows 2-4).
func TestServerOpensWhatCLIWrote(t *testing.T) {
	t.Run("same secret, both encrypted: opens and sees what the CLI declared", func(t *testing.T) {
		dir := t.TempDir()
		applyWithCLI(t, dir, map[string]string{"SAPEDB_ENCRYPT": "1"})

		srv, err := server.New(server.Options{Dir: dir, Secret: handoffSecret, Encrypt: true})
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()

		store, release, err := srv.Store("acme", "main")
		if err != nil {
			t.Fatalf("server could not open the database the CLI just wrote: %v", err)
		}
		defer release()

		found := false
		for _, name := range store.Collections() {
			if name == "articles" {
				found = true
			}
		}
		if !found {
			t.Errorf("server opened the file but does not see the collection the CLI declared; Collections() = %v", store.Collections())
		}
	})

	t.Run("different secret: the one case where \"wrong secret\" is the true story", func(t *testing.T) {
		dir := t.TempDir()
		applyWithCLI(t, dir, map[string]string{"SAPEDB_ENCRYPT": "1"})

		srv, err := server.New(server.Options{Dir: dir, Secret: "a different secret entirely", Encrypt: true})
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()

		if _, _, err := srv.Store("acme", "main"); !errors.Is(err, pager.ErrKey) {
			t.Errorf("got %v, want pager.ErrKey", err)
		}
	})

	t.Run("CLI wrote unencrypted, server asks for encryption: ErrNotEncrypted, not a key mismatch", func(t *testing.T) {
		dir := t.TempDir()
		applyWithCLI(t, dir, nil) // no SAPEDB_ENCRYPT

		srv, err := server.New(server.Options{Dir: dir, Secret: handoffSecret, Encrypt: true})
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()

		_, _, err = srv.Store("acme", "main")
		if !errors.Is(err, pager.ErrNotEncrypted) {
			t.Errorf("got %v, want pager.ErrNotEncrypted", err)
		}
		// This is the row TestServerOpensWhatCLIWrote's first subtest cannot
		// stand in for: a database that matches on every axis except "does it
		// have a key at all" must not be confused with the secret-mismatch
		// case above, and must not just happen to also fail — same class of
		// error either way would let a regression collapse the two rows into
		// one code path and still pass a test that only checked err != nil.
		if errors.Is(err, pager.ErrKey) {
			t.Errorf("got pager.ErrKey, want pager.ErrNotEncrypted — these are different mistakes with different fixes")
		}
	})

	t.Run("CLI wrote encrypted, server asks for none: refused, not a silently empty database", func(t *testing.T) {
		dir := t.TempDir()
		applyWithCLI(t, dir, map[string]string{"SAPEDB_ENCRYPT": "1"})

		srv, err := server.New(server.Options{Dir: dir, Secret: handoffSecret, Encrypt: false})
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()

		store, _, err := srv.Store("acme", "main")
		if err == nil {
			// The failure mode this row exists to catch: pager.OpenWith would
			// otherwise have no key to check against an encrypted file's meta
			// page and could read on regardless. It must not — silently
			// reading past encryption it was never given the key for is worse
			// than any error message.
			t.Fatalf("server opened an encrypted database with no key at all; Collections() = %v", store.Collections())
		}
		if !errors.Is(err, pager.ErrKey) {
			t.Errorf("got %v, want pager.ErrKey", err)
		}
	})
}
