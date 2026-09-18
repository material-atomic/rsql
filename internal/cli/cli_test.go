package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/material-atomic/rsql/internal/connection"
	"github.com/material-atomic/rsql/internal/vfs"
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
		"RSQL_SECRET":  secret,
		"RSQL_DIR":     s.dir,
		"RSQL_ACCOUNT": "acme",
		"RSQL_DB":      "main",
	}
	for name, value := range extra {
		vars[name] = value
	}
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

func TestACommandRunWhileTheServerHoldsTheFileIsRefused(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	// Somebody else — the server — has it open.
	held, err := vfs.OpenFile(filepath.Join(setup.dir, "acme", "main.rsql"), 0o600)
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
	if _, errs, status := setup.runWith(map[string]string{"RSQL_ENCRYPT": "1"}, "", "apply", file); status != 0 {
		t.Fatal(errs)
	}

	_, errs, status := setup.runWith(map[string]string{
		"RSQL_ENCRYPT": "1", "RSQL_SECRET": "a different secret entirely",
	}, "", "ls")
	if status == 0 {
		t.Fatal("it opened an encrypted database with the wrong secret")
	}
	if !strings.Contains(errs, "RSQL_SECRET") {
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
			if !strings.Contains(errs, "rsql [options]") {
				t.Errorf("it did not print how to use it: %q", errs)
			}
		})
	}

	// And the things a command cannot work without.
	if _, errs, _ := setup.runWith(map[string]string{"RSQL_SECRET": ""}, "", "ls"); !strings.Contains(errs, "RSQL_SECRET") {
		t.Errorf("no secret: %q", errs)
	}
	if _, errs, _ := setup.runWith(map[string]string{"RSQL_DB": ""}, "", "ls"); !strings.Contains(errs, "-db") {
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
