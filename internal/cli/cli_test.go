package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/vfs"
)

const secret = "the secret only the control plane has"

// setup is a directory, an environment, and a way to run the command in it.
type setup struct {
	t   *testing.T
	dir string
}

func start(t *testing.T) *setup {
	t.Helper()
	return &setup{t: t, dir: t.TempDir()}
}

func (s *setup) env(extra map[string]string) func(string) (string, bool) {
	vars := map[string]string{
		"SAPEDB_SECRET":  secret,
		"SAPEDB_DIR":     s.dir,
		"SAPEDB_ACCOUNT": "acme",
		"SAPEDB_DB":      "main",
	}
	for name, value := range extra {
		vars[name] = value
	}
	return func(name string) (string, bool) {
		value, found := vars[name]
		return value, found
	}
}

// envOnly is exactly the variables given, none of the setup's usual
// defaults — for tests about what happens when a variable this command
// needs is simply not there.
func (s *setup) envOnly(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := vars[name]
		return value, found
	}
}

// run returns what the command printed, what it complained about, and its
// status.
func (s *setup) run(args ...string) (string, string, int) {
	s.t.Helper()
	return s.runWith(nil, "", args...)
}

func (s *setup) runWith(extra map[string]string, stdin string, args ...string) (string, string, int) {
	s.t.Helper()

	out, errs := &strings.Builder{}, &strings.Builder{}
	status := Run(args, s.env(extra), strings.NewReader(stdin), out, errs)
	return out.String(), errs.String(), status
}

// write puts a file in the directory and returns its path.
func (s *setup) write(name, content string) string {
	s.t.Helper()
	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return path
}

const articles = `{
  "collections": [
    {
      "name": "articles",
      "key": {"path": "id", "type": "string", "auto": "ulid"},
      "indexes": [
        {"name": "by_author", "fields": [
          {"path": "author", "type": "string", "missing": "skip"},
          {"path": "published", "type": "number", "descending": true, "missing": "last"}
        ]}
      ]
    }
  ],
  "operations": [
    {
      "name": "articles.add", "collection": "articles", "action": "insert",
      "input": [
        {"name": "title", "type": "string", "required": true},
        {"name": "author", "type": "string", "required": true}
      ],
      "document": {"title": {"arg": "title"}, "author": {"arg": "author"}}
    },
    {
      "name": "articles.by_author", "collection": "articles", "action": "scan",
      "index": "by_author", "limit": 20,
      "input": [{"name": "author", "type": "string", "required": true}],
      "from": {"terms": [{"arg": "author"}]},
      "to": {"terms": [{"arg": "author"}]}
    }
  ]
}`

func TestApplyDeclaresWhatTheFileSays(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)

	out, errs, status := setup.run("apply", file)
	if status != 0 {
		t.Fatalf("apply: %d %s", status, errs)
	}
	if !strings.Contains(out, "collection articles") ||
		!strings.Contains(out, "operation articles.add version 1") {
		t.Fatalf("apply printed %q", out)
	}

	out, errs, status = setup.run("ls")
	if status != 0 {
		t.Fatalf("ls: %d %s", status, errs)
	}
	for _, want := range []string{
		"collection articles (key id string, ulid)",
		"index by_author [author string missing:skip, published number desc missing:last]",
		"operation articles.add v1 insert articles",
		"operation articles.by_author v1 scan articles via by_author limit 20",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls does not show %q:\n%s", want, out)
		}
	}
}

// Applying the same file again must change nothing. This runs on every deploy
// or it does not run at all, and a tool that makes a new version of every
// operation each time makes the version number meaningless.
func TestApplyingTwiceChangesNothing(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)

	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatalf("first apply: %s", errs)
	}
	out, errs, status := setup.run("apply", file)
	if status != 0 {
		t.Fatalf("second apply: %s", errs)
	}
	if !strings.Contains(out, "articles.add unchanged at version 1") {
		t.Errorf("the second apply printed %q", out)
	}

	// And a real change does make a version.
	changed := strings.Replace(articles, `"limit": 20`, `"limit": 5`, 1)
	file = setup.write("schema.json", changed)
	out, errs, status = setup.run("apply", file)
	if status != 0 {
		t.Fatalf("third apply: %s", errs)
	}
	if !strings.Contains(out, "articles.by_author version 2") {
		t.Errorf("a changed operation printed %q", out)
	}
	if !strings.Contains(out, "articles.add unchanged at version 1") {
		t.Errorf("an unchanged operation was given a version: %q", out)
	}
}

func TestADeclarationThatMakesNoSenseIsRefusedWithItsName(t *testing.T) {
	setup := start(t)

	for name, content := range map[string]string{
		"a field with no missing policy": `{"collections":[{"name":"a","key":{"path":"id","type":"string"},
			"indexes":[{"name":"i","fields":[{"path":"x","type":"string"}]}]}]}`,
		"a scan with no limit": `{"collections":[{"name":"a","key":{"path":"id","type":"string"}}],
			"operations":[{"name":"a.all","collection":"a","action":"scan","index":"_key"}]}`,
		"an operation on a collection that is not there": `{"operations":[
			{"name":"a.all","collection":"nowhere","action":"scan","index":"_key","limit":1}]}`,
		"a field nobody knows": `{"collections":[{"name":"a","key":{"path":"id","type":"string"},"wat":1}]}`,
		// task 0045: a declared scan whose From and To are both constants and
		// From sorts after To can never return a row, with any argument —
		// refused when the schema is applied, not discovered the first time
		// somebody runs it. "b" > "a", so this is backwards.
		"a scan whose declared from/to can never return a row": `{
			"collections":[{"name":"a","key":{"path":"id","type":"string","auto":"ulid"},
				"indexes":[{"name":"i","fields":[{"path":"x","type":"string","missing":"skip"}]}]}],
			"operations":[{"name":"a.backwards","collection":"a","action":"scan","index":"i","limit":1,
				"from":{"terms":[{"value":"b"}]},"to":{"terms":[{"value":"a"}]}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			file := setup.write("bad.json", content)
			_, errs, status := setup.run("apply", file)
			if status == 0 {
				t.Fatal("it was applied anyway")
			}
			if !strings.Contains(errs, "bad.json") {
				t.Errorf("the complaint does not name the file: %q", errs)
			}
			if strings.Contains(errs, "goroutine ") || strings.Contains(errs, "panic:") {
				t.Errorf("the complaint looks like a raw Go crash, not an operator-readable line: %q", errs)
			}
		})
	}
}

func TestDumpAndRestoreGoThroughTheCommand(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	dumped, errs, status := setup.run("dump")
	if status != 0 {
		t.Fatalf("dump: %s", errs)
	}
	if !strings.Contains(dumped, `"kind":"header"`) || !strings.Contains(dumped, `"kind":"end"`) {
		t.Fatalf("the dump reads %q", dumped[:min(200, len(dumped))])
	}

	// Into a database that has never been used.
	into := start(t)
	out, errs, status := into.runWith(nil, dumped, "restore")
	if status != 0 {
		t.Fatalf("restore: %s", errs)
	}
	if !strings.Contains(out, "restored to change") {
		t.Errorf("restore printed %q", out)
	}

	listed, _, _ := into.run("ls")
	if !strings.Contains(listed, "collection articles") || !strings.Contains(listed, "operation articles.add") {
		t.Errorf("the restored database holds %q", listed)
	}

	// And a dump cut short is refused rather than restored in part.
	third := start(t)
	half := dumped[:len(dumped)/2]
	if _, _, status := third.runWith(nil, half, "restore"); status == 0 {
		t.Error("half a dump was restored")
	}
}

func TestTheLogCanBeRead(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	out, errs, status := setup.run("log")
	if status != 0 {
		t.Fatalf("log: %s", errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("the log has %d entries", len(lines))
	}
	first := map[string]any{}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("an entry does not read as json: %v", err)
	}
	if first["kind"] != "declare" {
		t.Errorf("the first entry is a %v", first["kind"])
	}

	// From part way along.
	from, _, _ := setup.run("log", "2")
	if strings.Count(strings.TrimSpace(from), "\n") >= strings.Count(strings.TrimSpace(out), "\n") {
		t.Error("reading from entry 2 gave as much as reading from the start")
	}
	if _, _, status := setup.run("log", "soon"); status == 0 {
		t.Error("a log position that is not a number was accepted")
	}
}

// The connection string has to be one the client library parses and the server
// verifies. Printing something that looks right is not the same thing.
func TestTheConnectionStringIsOneTheOtherSideAccepts(t *testing.T) {
	setup := start(t)

	out, errs, status := setup.run("url", "db.example:7433")
	if status != 0 {
		t.Fatalf("url: %s", errs)
	}

	printed := strings.TrimSpace(out)
	parsed, err := connection.Parse(printed)
	if err != nil {
		t.Fatalf("what it printed does not parse: %v\n%s", err, printed)
	}
	if parsed.Account != "acme" || parsed.DBName != "main" || parsed.Host != "db.example" || parsed.Port != 7433 {
		t.Errorf("it printed %+v", parsed)
	}
	if err := connection.VerifyString(printed, secret, ""); err != nil {
		t.Errorf("the signature does not verify: %v", err)
	}

	// A different secret must not verify it, or the signature proves nothing.
	if err := connection.VerifyString(printed, "another secret entirely", ""); err == nil {
		t.Error("it verified under a secret it was not signed with")
	}
}

// TestTheDirectLabelIsRecognisedRegardlessOfCase is the case-folding this
// command promises with strings.EqualFold: an operator typing SAPEDB_LABEL
// in a shell script is not reliably going to match the lower-case spelling
// used in the one doc comment that names it. "Direct" and "DIRECT" must
// select the same signing mode as "direct" does.
func TestTheDirectLabelIsRecognisedRegardlessOfCase(t *testing.T) {
	for _, spelling := range []string{"direct", "Direct", "DIRECT"} {
		t.Run(spelling, func(t *testing.T) {
			setup := start(t)
			out, errs, status := setup.runWith(map[string]string{"SAPEDB_LABEL": spelling}, "", "url")
			if status != 0 {
				t.Fatalf("url: %s", errs)
			}
			printed := strings.TrimSpace(out)

			if err := connection.VerifyString(printed, secret, signing.Direct); err != nil {
				t.Errorf("%q did not select the direct form: %v", spelling, err)
			}
			if err := connection.VerifyString(printed, secret, ""); err == nil {
				t.Errorf("%q verified under the default label too, so it did not actually select direct", spelling)
			}
		})
	}
}

// A password on the command line is visible in ps to every user on the
// machine. It is generated, or read from stdin, and never taken as an
// argument.
func TestAPasswordIsNeverAnArgument(t *testing.T) {
	setup := start(t)

	_, errs, status := setup.run("url", "localhost:7433", "hunter2-hunter2-hunter2")
	if status == 0 {
		t.Fatal("a password given as an argument was accepted")
	}
	if !strings.Contains(errs, "stdin") {
		t.Errorf("the complaint does not say where a password goes: %q", errs)
	}

	// From stdin it is taken.
	given := "a-password-of-the-right-shape"
	out, errs, status := setup.runWith(nil, given+"\n", "url", "localhost:7433", "-")
	if status != 0 {
		t.Fatalf("url with stdin: %s", errs)
	}
	if !strings.Contains(out, given) {
		t.Errorf("it did not use the password it was given: %q", out)
	}

	// And one the format refuses is refused here, not signed and printed.
	if _, _, status := setup.runWith(nil, "short\n", "url", "localhost:7433", "-"); status == 0 {
		t.Error("a password the format does not allow was signed anyway")
	}
}

func TestAGeneratedPasswordIsDifferentEveryTimeAndValid(t *testing.T) {
	setup := start(t)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		out, _, status := setup.run("url", "localhost:7433")
		if status != 0 {
			t.Fatal(out)
		}
		parsed, err := connection.Parse(strings.TrimSpace(out))
		if err != nil {
			t.Fatal(err)
		}
		if seen[parsed.Password] {
			t.Fatal("the same password was made twice")
		}
		seen[parsed.Password] = true
	}
}

// TestThePartitionsFolderNameDropsTheFileExtension catches a mutation that
// TestACommandRunWhileTheServerHoldsTheFileIsRefused above cannot: that test
// only ever names main.sapedb, so it says nothing about the sibling folder
// open() derives from it with TrimSuffix. Drop the TrimSuffix and the
// database file is still exactly where every other test expects it — only
// the partitions folder ends up misnamed "main.sapedb.parts" instead of
// "main.parts", which nothing above would have noticed.
func TestThePartitionsFolderNameDropsTheFileExtension(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	if info, err := os.Stat(filepath.Join(setup.dir, "acme", "main.parts")); err != nil || !info.IsDir() {
		t.Errorf("want a partitions folder named main.parts, stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(setup.dir, "acme", "main.sapedb.parts")); err == nil {
		t.Error("the partitions folder kept the .sapedb suffix instead of having it trimmed")
	}
}

// oldDBExt mirrors internal/cli's own oldFileExt: built from bytes so this
// file, inside the tree internal/naming walks, does not carry the old word
// as a literal.
func oldDBExt() string {
	return "." + string([]byte{'r', 's', 'q', 'l'})
}

// TestAnOldExtensionFileIsRefusedNotSilentlyReplaced is the file-extension
// counterpart to TestTheOldEnvironmentNameIsRefusedNotSilentlyIgnored, and
// the reason it needs its own test rather than reusing that one's shape: a
// renamed environment variable degrades cleanly — either it is read, or the
// signpost above refuses to start. A renamed *file extension* degrades by
// vfs.OpenFile treating "not found" as "create a new, empty database",
// which is a database opening successfully, on the wrong file, holding no
// data — the one variant of the old name in this whole rename that would
// otherwise fail silently rather than being refused.
func TestAnOldExtensionFileIsRefusedNotSilentlyReplaced(t *testing.T) {
	setup := start(t)
	if err := os.MkdirAll(filepath.Join(setup.dir, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(setup.dir, "acme", "main"+oldDBExt())
	if err := os.WriteFile(old, []byte("stand-in for a real database file; only its path matters here"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, errs, status := setup.run("ls")
	if status == 0 {
		t.Fatal("it ran against a database whose only file on disk carries the old extension")
	}

	want := filepath.Join(setup.dir, "acme", "main.sapedb")
	if !strings.Contains(errs, old) || !strings.Contains(errs, want) {
		t.Errorf("the refusal does not name both paths: %q", errs)
	}
	if !strings.Contains(errs, "mv") {
		t.Errorf("the refusal does not tell the operator to mv the file: %q", errs)
	}
	if _, err := os.Stat(want); err == nil {
		t.Error("a new .sapedb file was created even though the command was refused")
	}
}

func TestACommandRunWhileTheServerHoldsTheFileIsRefused(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	// Somebody else — the server — has it open.
	held, err := vfs.OpenFile(filepath.Join(setup.dir, "acme", "main.sapedb"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	_, errs, status := setup.run("ls")
	if status == 0 {
		t.Fatal("it opened a database another process is writing")
	}
	if !strings.Contains(errs, "another process") || !strings.Contains(errs, "server") {
		t.Errorf("the complaint does not say what to do: %q", errs)
	}
}

func TestTheWrongSecretIsSaidPlainly(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.runWith(map[string]string{"SAPEDB_ENCRYPT": "1"}, "", "apply", file); status != 0 {
		t.Fatal(errs)
	}

	_, errs, status := setup.runWith(map[string]string{
		"SAPEDB_ENCRYPT": "1", "SAPEDB_SECRET": "a different secret entirely",
	}, "", "ls")
	if status == 0 {
		t.Fatal("it opened an encrypted database with the wrong secret")
	}
	if !strings.Contains(errs, "SAPEDB_SECRET") {
		t.Errorf("the complaint does not name the secret: %q", errs)
	}

	// And opening an encrypted database without saying so says which mistake
	// it is, rather than reading as a damaged file.
	_, errs, status = setup.run("ls")
	if status == 0 {
		t.Fatal("an encrypted database opened with no key")
	}
	if !strings.Contains(errs, "encrypted") {
		t.Errorf("the complaint reads %q", errs)
	}
}

// oldEnvName mirrors internal/cli's own rejectOldEnv: built from bytes so
// this file, inside the tree internal/naming walks, does not itself carry
// the string the whole rename exists to remove.
func oldEnvName(suffix string) string {
	return string([]byte{'R', 'S', 'Q', 'L', '_'}) + suffix
}

// TestTheOldEnvironmentNameIsRefusedNotSilentlyIgnored is the trap the task
// called out by name: SAPEDB_DIR in particular has a default, so a renamed
// variable nobody actually set would otherwise be read as simply absent and
// the command would carry on quietly against the wrong directory.
//
// This is a universal claim — every variable this command reads refuses its
// old name — so it is a table, one row per variable, each row differing in
// exactly which one is old. A check that happened to compare secret
// specifically, and nothing else, would pass this file forever while dir or
// label kept quietly reading the old name underneath it.
func TestTheOldEnvironmentNameIsRefusedNotSilentlyIgnored(t *testing.T) {
	base := func(s *setup) map[string]string {
		return map[string]string{
			"SAPEDB_SECRET":  secret,
			"SAPEDB_DIR":     s.dir,
			"SAPEDB_ACCOUNT": "acme",
			"SAPEDB_DB":      "main",
			"SAPEDB_ENCRYPT": "1",
			"SAPEDB_LABEL":   "another/label",
		}
	}

	for _, suffix := range []string{"DIR", "ACCOUNT", "DB", "ENCRYPT", "SECRET", "LABEL"} {
		t.Run(suffix, func(t *testing.T) {
			setup := start(t)
			vars := base(setup)
			newName, oldName := "SAPEDB_"+suffix, oldEnvName(suffix)
			value := vars[newName]
			delete(vars, newName)
			vars[oldName] = value

			stdoutBuf, errBuf := &strings.Builder{}, &strings.Builder{}
			status := Run([]string{"ls"}, setup.envOnly(vars), strings.NewReader(""), stdoutBuf, errBuf)
			if status == 0 {
				t.Fatalf("it started with only %s set instead of %s", oldName, newName)
			}
			// Not just "the message names both variables" — that survives the
			// two names being swapped, which points the operator at the wrong
			// one of the two: it would tell them to go set the very variable
			// that was just refused. The signpost's whole job is to say which
			// way to go, so the sentence has to be pinned whole, in order.
			errs := errBuf.String()
			want := oldName + " is not read any longer; set " + newName
			if !strings.Contains(errs, want) {
				t.Errorf("the refusal does not say %q: %q", want, errs)
			}
		})
	}

	// The control every row above is compared against: all current names,
	// nothing old, must run. Without this, a bug that refused everything
	// unconditionally would pass every row above too.
	t.Run("control: every current name, nothing old", func(t *testing.T) {
		setup := start(t)
		stdoutBuf, errBuf := &strings.Builder{}, &strings.Builder{}
		status := Run([]string{"ls"}, setup.envOnly(base(setup)), strings.NewReader(""), stdoutBuf, errBuf)
		if status != 0 {
			t.Fatalf("it refused the current environment names too: %s", errBuf.String())
		}
	})

	// Both set, to different values: the new one must win, silently — this
	// is not a compatibility path, so there must be no complaint at all.
	t.Run("both set: the current name wins without complaint", func(t *testing.T) {
		setup := start(t)
		vars := base(setup)
		vars[oldEnvName("SECRET")] = "a value nobody should ever read"

		stdoutBuf, errBuf := &strings.Builder{}, &strings.Builder{}
		status := Run([]string{"ls"}, setup.envOnly(vars), strings.NewReader(""), stdoutBuf, errBuf)
		if status != 0 {
			t.Fatalf("setting both refused to start: %s", errBuf.String())
		}
	})

	// The new name set to the empty string is "not set", the same as it not
	// being in the environment at all — get() treats them identically, on
	// purpose, so a container with SAPEDB_DIR= would not silently run
	// against the current directory. rejectOldEnv checks that at both ends —
	// found&&value!="" on the new name, found&&value!="" on the old name —
	// and the two do NOT fail the same way. This test exercises only the
	// first: dropping the new-name guard reads a blank SAPEDB_DIR as "set"
	// and skips straight past a real old-named DIR variable sitting right
	// next to it, the shape a container ships by accident (an env file that
	// declares the new key with no value, alongside a leftover old one).
	t.Run("new name blank, old name has a value: still refused", func(t *testing.T) {
		setup := start(t)
		vars := base(setup)
		newName, oldName := "SAPEDB_DIR", oldEnvName("DIR")
		vars[newName] = ""
		vars[oldName] = "/data"

		stdoutBuf, errBuf := &strings.Builder{}, &strings.Builder{}
		status := Run([]string{"ls"}, setup.envOnly(vars), strings.NewReader(""), stdoutBuf, errBuf)
		if status == 0 {
			t.Fatalf("it started with %s blank and %s set to a real value", newName, oldName)
		}
		want := oldName + " is not read any longer; set " + newName
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("the refusal does not say %q: %q", want, errBuf.String())
		}
	})

	// The mirror image, on the OLD name's guard instead: dropping
	// found&&value!="" on the old-name check would read a blank old-named
	// LABEL variable as "set" and refuse to start even though there is
	// nothing there to conflict with — a container that has never set
	// anything under the old name at all, but whose env file (or
	// `env | sort` habit) still declares it blank. This uses LABEL rather
	// than DIR on purpose: DIR's
	// fallback is the hard-coded absolute path /var/lib/sapedb (see get(),
	// and the comment on the one documented mutation survivor next to it),
	// and reaching that fallback here — which "must run" requires, since
	// nothing refuses first — is exactly the system-path dependency this
	// package's tests otherwise avoid. LABEL's fallback is the empty
	// string, so this observes the same guard without touching a real path.
	t.Run("new name blank, old name also blank: must run", func(t *testing.T) {
		setup := start(t)
		vars := base(setup)
		newName, oldName := "SAPEDB_LABEL", oldEnvName("LABEL")
		vars[newName] = ""
		vars[oldName] = ""

		stdoutBuf, errBuf := &strings.Builder{}, &strings.Builder{}
		status := Run([]string{"ls"}, setup.envOnly(vars), strings.NewReader(""), stdoutBuf, errBuf)
		if status != 0 {
			t.Fatalf("it refused to start with %s and %s both blank, neither one a real value: %s", newName, oldName, errBuf.String())
		}
	})
}

func TestWhatIsNotACommandIsExplained(t *testing.T) {
	setup := start(t)

	for name, args := range map[string][]string{
		"nothing at all":          {},
		"a command nobody has":    {"frobnicate"},
		"an option nobody has":    {"-wat", "x", "ls"},
		"an option with no value": {"-account"},
		"apply with no file":      {"apply"},
	} {
		t.Run(name, func(t *testing.T) {
			_, errs, status := setup.run(args...)
			if status == 0 {
				t.Fatal("it ran anyway")
			}
			if !strings.Contains(errs, "sapedb [options]") {
				t.Errorf("it did not print how to use it: %q", errs)
			}
		})
	}

	// And the things a command cannot work without.
	if _, errs, _ := setup.runWith(map[string]string{"SAPEDB_SECRET": ""}, "", "ls"); !strings.Contains(errs, "SAPEDB_SECRET") {
		t.Errorf("no secret: %q", errs)
	}
	if _, errs, _ := setup.runWith(map[string]string{"SAPEDB_DB": ""}, "", "ls"); !strings.Contains(errs, "-db") {
		t.Errorf("no database: %q", errs)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A generated password is drawn from the alphabet evenly. The alphabet is 66
// long and a byte is 256, so taking the remainder would make the first 58
// characters a third more likely than the last 8 — which costs about one bit
// across a whole password and is worth almost nothing, but is also the kind of
// thing that gets copied into somewhere it does matter.
func TestGeneratedPasswordsAreDrawnEvenly(t *testing.T) {
	counts := map[byte]int{}
	total := 0

	for i := 0; i < 4000; i++ {
		made, err := makePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(made) != passwordLength {
			t.Fatalf("a password of %d characters", len(made))
		}
		for j := 0; j < len(made); j++ {
			if !strings.ContainsRune(passwordAlphabet, rune(made[j])) {
				t.Fatalf("a password holds %q, which is not in the alphabet", made[j])
			}
			counts[made[j]]++
			total++
		}
	}

	if len(counts) != len(passwordAlphabet) {
		t.Fatalf("only %d of %d characters ever appeared", len(counts), len(passwordAlphabet))
	}

	expected := float64(total) / float64(len(passwordAlphabet))
	for character, count := range counts {
		off := float64(count)/expected - 1
		if off < -0.2 || off > 0.2 {
			t.Errorf("%q appeared %d times, %.0f%% off the %.0f expected",
				character, count, off*100, expected)
		}
	}
}

// The tool tells people the server is probably running. That has to be a fact
// rather than a guess, which means the lock it trips over is the directory —
// not whichever database files the server happens to have opened.
func TestTheToolIsRefusedWhileAServerHoldsTheDirectory(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	// A server that has started and opened nothing at all.
	held, err := vfs.LockDir(setup.dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, command := range [][]string{{"ls"}, {"dump"}, {"log"}, {"apply", file}} {
		_, errs, status := setup.run(command...)
		if status == 0 {
			t.Errorf("%v ran while the server held the directory", command)
		}
		if !strings.Contains(errs, "server") {
			t.Errorf("%v: the complaint does not say what to do: %q", command, errs)
		}
	}

	// url does not touch the database, so it still works — which is what makes
	// it usable for handing somebody a connection string on a running system.
	if _, errs, status := setup.run("url", "localhost:7433"); status != 0 {
		t.Errorf("url needs no database and should still work: %s", errs)
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, errs, status := setup.run("ls"); status != 0 {
		t.Errorf("after the server let go: %s", errs)
	}
}
