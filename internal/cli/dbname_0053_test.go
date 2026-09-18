package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listFiles walks root and returns every regular file under it, relative to
// root. Used below to prove a refused command left nothing behind at all —
// not the colliding file, not a .lock, not an account directory one level
// up from where it belongs.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// TestTheOriginalTwoUsersOneFileCollisionIsNowRefused reproduces task
// 0053's headline bug report exactly: two SAPEDB_ACCOUNT/SAPEDB_DB pairs
// that, before this task, wrote to the identical file
// (dir/a/b/c.sapedb) with both commands exiting 0 and neither side
// warning. Both must now be refused, each naming the half of the pair that
// is actually wrong, and neither may leave a file anywhere behind.
func TestTheOriginalTwoUsersOneFileCollisionIsNowRefused(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)

	_, errs, status := setup.runWith(map[string]string{"SAPEDB_ACCOUNT": "a/b", "SAPEDB_DB": "c"}, "", "apply", file)
	if status == 0 {
		t.Fatalf("SAPEDB_ACCOUNT='a/b' SAPEDB_DB='c' ran instead of being refused")
	}
	if !strings.Contains(errs, `account "a/b"`) {
		t.Errorf("refusal does not name the account: %q", errs)
	}
	if strings.Contains(errs, `database "`) {
		t.Errorf("refusal about the account also names a database: %q", errs)
	}

	_, errs, status = setup.runWith(map[string]string{"SAPEDB_ACCOUNT": "a", "SAPEDB_DB": "b/c"}, "", "apply", file)
	if status == 0 {
		t.Fatalf("SAPEDB_ACCOUNT='a' SAPEDB_DB='b/c' ran instead of being refused")
	}
	if !strings.Contains(errs, `database "b/c"`) {
		t.Errorf("refusal does not name the database: %q", errs)
	}
	if strings.Contains(errs, `account "`) {
		t.Errorf("refusal about the database also names an account: %q", errs)
	}

	// Nothing at all was created under dir except the schema file the test
	// itself wrote — not the colliding file, not any account directory.
	if got := listFiles(t, setup.dir); len(got) != 1 || got[0] != "schema.json" {
		t.Errorf("files under dir after two refused commands: %v, want only [schema.json]", got)
	}
}

// TestNoComponentEscapesSAPEDB_DIR is the axis the original bug report
// under-stated: SAPEDB_DB escapes SAPEDB_DIR just as easily as
// SAPEDB_ACCOUNT does, and by more directories at once
// ("../../pwned" vs "../escape"). This runs a small table of escaping
// values through BOTH -account and -db, and checks the disk, not just the
// exit code: a name that fails open() late (say, only once vfs.LockDir has
// already run) can still be "refused" while having created a directory or
// a lock file outside where it belongs.
func TestNoComponentEscapesSAPEDB_DIR(t *testing.T) {
	badValues := []string{"../escape", "../../escape", ".", ".."}

	for _, bad := range badValues {
		t.Run("account="+bad, func(t *testing.T) {
			lab := t.TempDir()
			// dir is never created ahead of time — on purpose. If the fix
			// works, nothing below ever calls vfs.LockDir(dir), so dir
			// itself must not exist afterwards either.
			dir := filepath.Join(lab, "dir")
			schema := filepath.Join(lab, "schema.json")
			if err := os.WriteFile(schema, []byte(articles), 0o600); err != nil {
				t.Fatal(err)
			}
			s := &setup{t: t, dir: dir}

			_, errs, status := s.runWith(map[string]string{"SAPEDB_ACCOUNT": bad, "SAPEDB_DB": "main"}, "", "apply", schema)
			if status == 0 {
				t.Fatalf("account %q ran instead of being refused", bad)
			}
			if !strings.Contains(errs, `account "`+bad+`"`) {
				t.Errorf("refusal does not name the account: %q", errs)
			}
			if got := listFiles(t, lab); len(got) != 1 || got[0] != "schema.json" {
				t.Errorf("files under lab after a refused account %q: %v, want only [schema.json] (dir itself must not have been created)", bad, got)
			}
		})

		t.Run("db="+bad, func(t *testing.T) {
			lab := t.TempDir()
			dir := filepath.Join(lab, "dir")
			schema := filepath.Join(lab, "schema.json")
			if err := os.WriteFile(schema, []byte(articles), 0o600); err != nil {
				t.Fatal(err)
			}
			s := &setup{t: t, dir: dir}

			_, errs, status := s.runWith(map[string]string{"SAPEDB_ACCOUNT": "acme", "SAPEDB_DB": bad}, "", "apply", schema)
			if status == 0 {
				t.Fatalf("db %q ran instead of being refused", bad)
			}
			if !strings.Contains(errs, `database "`+bad+`"`) {
				t.Errorf("refusal does not name the database: %q", errs)
			}
			if got := listFiles(t, lab); len(got) != 1 || got[0] != "schema.json" {
				t.Errorf("files under lab after a refused db %q: %v, want only [schema.json] (dir itself must not have been created)", bad, got)
			}
		})
	}
}

// TestEmptyNameUsageMessageIsUnchanged pins that the pre-existing usage
// message for a missing -account/-db still fires, unreplaced by
// dbname.CheckPair's own "it is empty" wording — the two checks answer
// different questions (this one is about the command being usable at all;
// dbname's is about a name being a safe path component), and only the
// first one should ever fire for an empty name, since the empty check runs
// first in run().
func TestEmptyNameUsageMessageIsUnchanged(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)

	_, errs, status := setup.runWith(map[string]string{"SAPEDB_ACCOUNT": ""}, "", "apply", file)
	if status == 0 {
		t.Fatal("an empty account ran")
	}
	if !strings.Contains(errs, "-account and -db say which database") {
		t.Errorf("the old usage message is gone: %q", errs)
	}
	if strings.Contains(errs, "sapedb/dbname") || strings.Contains(errs, "it is empty") {
		t.Errorf("dbname's own wording leaked into the empty-name case: %q", errs)
	}
}

// TestOldPathHintOnlyAppearsWhenARealFileExists is mục 3's "TỪ CHỐI, KÈM
// CHỈ ĐƯỜNG" decision, checked from both directions: no hint when there is
// nothing to point at, and a hint with both paths — absolute, .sapedb and
// .parts — when there is. Table because the encrypted and unencrypted
// cases give operators two different, incompatible instructions (restore
// vs mv), and a test with only one of them would not catch the other
// silently disappearing or the two swapping.
func TestOldPathHintOnlyAppearsWhenARealFileExists(t *testing.T) {
	t.Run("nothing on disk: no hint at all", func(t *testing.T) {
		setup := start(t)
		file := setup.write("schema.json", articles)

		_, errs, status := setup.runWith(map[string]string{"SAPEDB_ACCOUNT": "a/b", "SAPEDB_DB": "c"}, "", "apply", file)
		if status == 0 {
			t.Fatal("ran instead of being refused")
		}
		if strings.Contains(errs, setup.dir) {
			t.Errorf("a path appeared in the refusal even though nothing was ever written there: %q", errs)
		}
		if strings.Contains(errs, "mv it") || strings.Contains(errs, "restore") {
			t.Errorf("migration wording appeared with nothing to migrate: %q", errs)
		}
	})

	t.Run("unencrypted: names both paths absolutely", func(t *testing.T) {
		setup := start(t)
		file := setup.write("schema.json", articles)

		old := filepath.Join(setup.dir, "a", "b", "c.sapedb")
		oldParts := filepath.Join(setup.dir, "a", "b", "c.parts")
		if err := os.MkdirAll(oldParts, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(old, []byte("stand-in for a real database file; only its path matters here"), 0o600); err != nil {
			t.Fatal(err)
		}

		wantAbs, err := filepath.Abs(old)
		if err != nil {
			t.Fatal(err)
		}
		wantAbsParts, err := filepath.Abs(oldParts)
		if err != nil {
			t.Fatal(err)
		}

		_, errs, status := setup.runWith(map[string]string{"SAPEDB_ACCOUNT": "a/b", "SAPEDB_DB": "c"}, "", "apply", file)
		if status == 0 {
			t.Fatal("ran instead of being refused")
		}
		if !strings.Contains(errs, wantAbs) {
			t.Errorf("refusal does not name the old .sapedb file: %q (want %s)", errs, wantAbs)
		}
		if !strings.Contains(errs, wantAbsParts) {
			t.Errorf("refusal does not name the old .parts directory: %q (want %s)", errs, wantAbsParts)
		}
		// "mv it" rather than a bare "mv": setup.dir is a t.TempDir() path
		// that, on some platforms, embeds this subtest's own name — and a
		// bare "mv" is a short-string Contains check that house-rules.md
		// warns can be true for reasons that have nothing to do with what
		// it is supposed to be checking. wantAbs above already proves the
		// path is in there; this only needs to confirm the mv instruction
		// specifically.
		if !strings.Contains(errs, "mv it") {
			t.Errorf("refusal for an unencrypted database does not offer to mv it: %q", errs)
		}
	})

	t.Run("encrypted: says restore, not rename", func(t *testing.T) {
		setup := start(t)
		file := setup.write("schema.json", articles)

		old := filepath.Join(setup.dir, "a", "b", "c.sapedb")
		if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(old, []byte("stand-in for a real, encrypted database file"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, errs, status := setup.runWith(map[string]string{
			"SAPEDB_ACCOUNT": "a/b", "SAPEDB_DB": "c", "SAPEDB_ENCRYPT": "1",
		}, "", "apply", file)
		if status == 0 {
			t.Fatal("ran instead of being refused")
		}
		// "restore into a", not a bare "restore": this subtest's own name
		// ("encrypted: says restore, not rename") is folded into the
		// t.TempDir() path that setup.dir sits under, and that path is
		// exactly what appears inside errs (via oldPathHint's absolute
		// paths) — so a bare Contains(errs, "restore") is true whether or
		// not the PRODUCT ever says the word, because the subtest's own
		// name says it first. Measured: dropping "restore" from
		// oldPathHint's encrypted message while keeping this subtest's name
		// unchanged still passes. The multi-word phrase below has a literal
		// space in it, which t.TempDir() never leaves in a sanitized name
		// (spaces become underscores), so it cannot be satisfied by the
		// directory name the way the bare word could.
		if !strings.Contains(errs, "restore into a") {
			t.Errorf("the encrypted refusal does not mention restore: %q", errs)
		}
		// "mv it", not a bare "mv" — see the sibling subtest's comment on
		// why a bare short substring is the wrong check here.
		if strings.Contains(errs, "mv it") {
			t.Errorf("the encrypted refusal promises to mv it, which cannot carry the key forward: %q", errs)
		}
	})
}
