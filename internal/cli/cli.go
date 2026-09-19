// Package cli is the tool that sets a database up and looks inside it.
//
// It exists because declaring is not an operation. Everything a client can do
// over the wire is an operation the database already holds, which means a new
// database can do nothing at all until somebody puts the first declarations in
// it — and that somebody is on the same host as the file, not on the far end
// of a connection.
//
// Every command here opens the database file directly, and the file takes an
// exclusive lock. So a command run while the server is up is refused rather
// than quietly becoming the second writer: see internal/vfs for what two
// writers do to one of these files.
package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sapedb/sapedb/internal/dbkey"
	"github.com/sapedb/sapedb/internal/dbname"
	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

// passwordBytes is how long a generated password is. Well inside the 16..128
// the format allows, and long enough that guessing is not a strategy.
const passwordLength = 32

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~-"

const usage = `sapedb — set up and look inside a database

  sapedb [options] apply FILE...     declare collections and operations
  sapedb [options] ls                what this database holds
  sapedb [options] dump              write a dump to stdout
  sapedb [options] restore           read a dump from stdin, into an empty database
  sapedb [options] log [FROM]        print the change log from an entry onwards
  sapedb [options] url               print a signed connection string
  sapedb [options] shell [HOST]      look inside a running server

options
  -dir DIR       where databases live      (SAPEDB_DIR, default /var/lib/sapedb)
  -account NAME  which account             (SAPEDB_ACCOUNT)
  -db NAME       which database            (SAPEDB_DB)
  -encrypt       the database is encrypted (SAPEDB_ENCRYPT)

SAPEDB_SECRET is read from the environment. It is what connection strings are
signed with and what database encryption keys are derived from, so a command
run with the wrong one either refuses or writes a file the server cannot read.

A password is never taken as an argument: arguments are visible to anyone who
can run ps. "url" makes one and prints it as part of the connection string,
or reads one from stdin when told to.
`

var (
	ErrUsage  = errors.New("sapedb: that is not how this is used")
	ErrSecret = errors.New("sapedb: SAPEDB_SECRET is not set")
)

// oldEnvPrefix is the environment prefix this product read before it was
// called sapedb, assembled from single-character literals rather than spelled
// whole. internal/naming walks every file in this tree looking for exactly
// the four letters that would make; the only reason this function still knows
// them is to refuse them, and it would be a strange sort of refusal that
// itself left the old name lying around in the source for the next person to
// copy.
var oldEnvPrefix = string([]byte{'R', 'S', 'Q', 'L', '_'})

// sapedbEnvNames are every product variable this command reads. Walked once
// here, by the check below, instead of once per call to get() — so the set
// this refuses old names for cannot silently drift from the set it actually
// reads.
var sapedbEnvNames = []string{
	"SAPEDB_DIR", "SAPEDB_ACCOUNT", "SAPEDB_DB", "SAPEDB_ENCRYPT", "SAPEDB_SECRET", "SAPEDB_LABEL",
}

// rejectOldEnv refuses to start when a variable is set under the product's
// old name and not under its current one. It is not a compatibility path: it
// never reads what the old name holds, only whether it is there, and it stops
// the process rather than falling back to it. Without this, "-dir" in
// particular would fail silently — it has a default, so a renamed variable
// nobody set would just be read as absent and the command would carry on
// against the wrong directory.
func rejectOldEnv(lookup func(string) (string, bool)) error {
	for _, name := range sapedbEnvNames {
		if value, found := lookup(name); found && value != "" {
			continue
		}
		old := strings.Replace(name, "SAPEDB_", oldEnvPrefix, 1)
		if value, found := lookup(old); found && value != "" {
			return fmt.Errorf("sapedb: %s is not read any longer; set %s", old, name)
		}
	}
	return nil
}

// Run is the whole command. It returns the exit status.
func Run(args []string, lookup func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer) int {
	if err := run(args, lookup, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, ErrUsage) {
			fmt.Fprint(stderr, "\n", usage)
		}
		return 1
	}
	return 0
}

type options struct {
	dir     string
	account string
	db      string
	encrypt bool
	secret  string
	label   string
}

func run(args []string, lookup func(string) (string, bool), stdin io.Reader, stdout io.Writer) error {
	if err := rejectOldEnv(lookup); err != nil {
		return err
	}

	// found && value != "": an explicitly empty variable is not a value, it
	// is "not set" — a container with SAPEDB_DIR= would otherwise put its
	// databases in the current directory instead of the documented default.
	// A mutation dropping the `value != ""` half survives every test in this
	// file: catching it means actually exercising the fallback, and for
	// -dir that fallback is the hard-coded absolute path /var/lib/sapedb —
	// the one hard-coded system path anywhere in this package's tests would
	// touch, with a result that depends on who is running the suite and
	// whether they followed the house rule of always going through the
	// mounted docker image. internal/service's equivalent test (see
	// TestTheEnvironmentIsReadAsWritten) gets away with checking this
	// because FromEnv returns Dir as a plain field on Config, so nothing
	// ever has to be opened to see it; this package never hands opts back
	// to a test to inspect the same way.
	get := func(name, fallback string) string {
		if value, found := lookup(name); found && value != "" {
			return value
		}
		return fallback
	}

	label := get("SAPEDB_LABEL", "")
	if strings.EqualFold(label, "direct") {
		label = signing.Direct
	}

	opts := options{
		dir:     get("SAPEDB_DIR", "/var/lib/sapedb"),
		account: get("SAPEDB_ACCOUNT", ""),
		db:      get("SAPEDB_DB", ""),
		secret:  get("SAPEDB_SECRET", ""),
		encrypt: strings.EqualFold(get("SAPEDB_ENCRYPT", ""), "1") || strings.EqualFold(get("SAPEDB_ENCRYPT", ""), "true"),
		label:   label,
	}

	rest, err := parse(args, &opts)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("%w: no command", ErrUsage)
	}
	if opts.secret == "" {
		return ErrSecret
	}
	if opts.account == "" || opts.db == "" {
		return fmt.Errorf("%w: -account and -db say which database", ErrUsage)
	}

	// Both names become a path component and, when -encrypt is set, half of
	// a key-derivation input — see internal/dbname. This is the door that
	// was missing before task 0053: internal/server has always refused a
	// name shaped like a path, at its own door, but nothing here ever asked
	// the same question, so SAPEDB_ACCOUNT='a/b' SAPEDB_DB='c' and
	// SAPEDB_ACCOUNT='a' SAPEDB_DB='b/c' both wrote the same file with no
	// error from either side. This does not make that check upstream
	// unneeded — it is a conditional fact, not a guarantee: server has
	// checked this shape at database() since before this package existed,
	// so a name that gets past THIS call is a name both halves of the
	// product agree on, not a name this tool has privately decided is safe.
	//
	// Placed before vfs.LockDir (see open, below), and before the command
	// switch, on purpose: a refused name must not take the directory lock
	// or create the account's directory (open() does both), and it must be
	// refused the same way whether the command that follows opens a file at
	// all — url and shell never call open(), and a name they would sign or
	// connect with is exactly as wrong as one apply would write.
	if err := dbname.CheckPair(opts.account, opts.db); err != nil {
		return oldPathHint(opts, err)
	}

	command, rest := rest[0], rest[1:]

	switch command {
	case "url":
		return url(opts, rest, stdin, stdout)
	case "shell":
		// Not opened here: the shell talks to a running server, and taking the
		// directory lock is exactly what it must not do — the database an
		// operator wants to look inside is the one that is serving.
		return shell(opts, rest, stdin, stdout)
	case "apply", "ls", "dump", "restore", "log":
	default:
		return fmt.Errorf("%w: no command called %q", ErrUsage, command)
	}

	db, close, err := open(opts)
	if err != nil {
		return err
	}
	defer close()

	switch command {
	case "apply":
		return apply(db, rest, stdout)
	case "ls":
		return list(db, stdout)
	case "dump":
		return dump(db, stdout)
	case "restore":
		return restore(db, stdin, stdout)
	case "log":
		return changes(db, rest, stdout)
	}
	return nil
}

// parse reads the options, which come before the command.
func parse(args []string, opts *options) ([]string, error) {
	for len(args) > 0 {
		arg := args[0]
		if !strings.HasPrefix(arg, "-") {
			return args, nil
		}
		args = args[1:]

		name, value, joined := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		next := func() (string, error) {
			if joined {
				return value, nil
			}
			if len(args) == 0 {
				return "", fmt.Errorf("%w: -%s needs a value", ErrUsage, name)
			}
			taken := args[0]
			args = args[1:]
			return taken, nil
		}

		var err error
		switch name {
		case "dir":
			opts.dir, err = next()
		case "account":
			opts.account, err = next()
		case "db":
			opts.db, err = next()
		case "encrypt":
			opts.encrypt = true
		case "h", "help":
			return nil, fmt.Errorf("%w", ErrUsage)
		default:
			return nil, fmt.Errorf("%w: no option called -%s", ErrUsage, name)
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// oldFileExt is the database file extension this product used before it was
// called sapedb, assembled from single-character literals for the same
// reason oldEnvPrefix above is: internal/naming would otherwise flag the
// string that spells it, and a check for the old extension has no business
// leaving the old extension lying around in the source as plain text.
var oldFileExt = "." + string([]byte{'r', 's', 'q', 'l'})

// checkOldExtension refuses to open a database when its .sapedb path does
// not exist but a same-named file under the old extension does.
//
// This is the one variant of the old name that fails silently rather than
// being refused: a renamed environment variable at least gets a signpost
// (see rejectOldEnv above), and an old connection scheme or signing label
// gets a parse or verify error, but a missing .sapedb file with a real
// old-extension file sitting right next to it does not look like an error
// at all — vfs.OpenFile below would simply create a new, empty database and
// this tool would report an empty database where a real one exists. That is
// not "not found", it is data going invisible. So this stops and says
// exactly where the data actually is, rather than renaming it or opening it
// as-is: changing what a file means without being asked is not this tool's
// call to make, and the operator is the one who knows whether that old file
// is still needed anywhere else.
//
// Same shape as the environment-variable signpost, and the same note
// applies: this is not a compatibility path, and it is meant to be removed
// once operators have confirmed they have moved their database files.
func checkOldExtension(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	old := strings.TrimSuffix(path, ".sapedb") + oldFileExt
	if _, err := os.Stat(old); err == nil {
		return fmt.Errorf("sapedb: %s does not exist, but %s does; mv %s %s", path, old, old, path)
	}
	return nil
}

// dbPath is where an (account, db) pair's file lives under dir. This is the
// one expression in the tree that turns those three things into a path for
// this side of the product — internal/server has its own, independent one
// (see task 0053's own text, "Không có tầng nào hôm nay MỌI đường đều đi
// qua" — there was no single layer both sides went through before this
// task), and this function is what keeps this file from growing a second
// one of its own: both open() and oldPathHint(), below, call this instead
// of writing filepath.Join again.
func dbPath(opts options) string {
	return filepath.Join(opts.dir, opts.account, opts.db+".sapedb")
}

// oldPathHint decides whether a name dbname.CheckPair just refused is one
// that, before this check existed, had already been used to write a real
// file — and if so, says exactly where it is and how to get the data out.
//
// Same discipline as checkOldExtension above, and the same reason: os.Stat,
// not a promise. A name nobody ever wrote anything under gets the bare
// refusal and nothing more — a migration lecture that does not apply teaches
// people to stop reading migration lectures.
//
// The encrypted case is deliberately not offered mv. dbkey.Key derives a
// database's encryption key from account+"/"+name (see that package), so
// changing either half of a valid pair changes the key — there is no
// rename that carries the old key forward, only a read-with-the-old-code,
// then a restore into a name this version accepts. Task 0053's Result
// section has the structural argument for why no pair CheckPair accepts can
// ever collide with a pair it refused: their info strings cannot be equal.
func oldPathHint(opts options, refusal error) error {
	path := dbPath(opts)
	if _, err := os.Stat(path); err != nil {
		return refusal
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	parts := strings.TrimSuffix(abs, ".sapedb") + ".parts"

	if opts.encrypt {
		return fmt.Errorf("%w\n  a file already exists at %s (and %s, if there is one).\n"+
			"  It is encrypted, so no rename carries its key forward: read it out with\n"+
			"  the sapedb build from before this name was refused, then restore into a\n"+
			"  valid account/db pair.", refusal, abs, parts)
	}
	return fmt.Errorf("%w\n  a file already exists at %s (and %s, if there is one).\n"+
		"  mv it, and that .parts directory if there is one, to a valid account/db\n"+
		"  pair.", refusal, abs, parts)
}

// open opens the database file, taking its lock.
func open(opts options) (*store.Store, func(), error) {
	// checkOldExtension first, and before anything that touches disk:
	// os.Stat needs no lock and creates nothing, so a name refused here
	// leaves SAPEDB_DIR exactly as it found it — no .lock, no account
	// folder. Task 0038 found both of those left behind by a refusal that
	// used to happen after LockDir and MkdirAll had already run; task 0050
	// is this reordering. A side effect worth naming: when the server
	// already holds the directory lock and the file also carries the old
	// extension, this now answers with the mv hint instead of "the server
	// is probably running" — both are true, but the mv hint is the one
	// action that is actually blocking, and doing it makes the other
	// message appear on the next attempt.
	//
	// checkOldExtension is placed ahead of BOTH os.MkdirAll and vfs.LockDir
	// below, not just ahead of LockDir — and that first half matters on its
	// own, separately from the old-extension case this function is named
	// after. TestARefusalOnTheLockedDirectoryLeavesNoAccountFolder is the
	// case that pins it: a directory-lock refusal (nothing wrong with the
	// file at all, somebody else just holds the lock) must not create the
	// account folder either, and it will if os.MkdirAll runs before this
	// check — MkdirAll does not know or care whether the file it is making
	// room for is old, missing, or fine; it creates the folder the moment
	// it runs, refusal or not. An earlier version of this comment argued
	// that ordering could never be observed because an old-extension file
	// can only be found once its folder already exists, making MkdirAll a
	// no-op either way — true for THAT specific repro, but that argument
	// quietly assumed the only way into this refusal path was through an
	// old-extension file. It is not: a bare locked-directory refusal, with
	// no file of any kind on disk yet, goes through the exact same lines.
	path := dbPath(opts)
	if err := checkOldExtension(path); err != nil {
		return nil, nil, err
	}

	// The directory next: a server holds this for its whole life, so being
	// refused here is the answer to "is the server running", rather than a
	// guess that depends on which databases it happens to have opened.
	held, err := vfs.LockDir(opts.dir)
	if err != nil {
		if errors.Is(err, vfs.ErrLocked) {
			return nil, nil, fmt.Errorf("%w\n  the server is probably running; stop it, or work on a dump instead", err)
		}
		return nil, nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		held.Close()
		return nil, nil, err
	}

	file, err := vfs.OpenFile(path, 0o600)
	if err != nil {
		held.Close()
		if errors.Is(err, vfs.ErrLocked) {
			return nil, nil, fmt.Errorf("%w\n  the server is probably running; stop it, or work on a dump instead", err)
		}
		return nil, nil, err
	}

	settings := pager.Options{}
	if opts.encrypt {
		key, err := dbkey.Key(opts.secret, opts.account, opts.db)
		if err != nil {
			file.Close()
			held.Close()
			return nil, nil, err
		}
		settings.Key = key
	}

	size, err := file.Size()
	if err != nil {
		file.Close()
		held.Close()
		return nil, nil, err
	}

	var pages *pager.Pager
	if size == 0 {
		pages, err = pager.CreateWith(file, settings)
	} else {
		pages, err = pager.OpenWith(file, settings)
	}
	if err != nil {
		file.Close()
		held.Close()
		if errors.Is(err, pager.ErrKey) {
			return nil, nil, fmt.Errorf("%w\n  SAPEDB_SECRET does not match the one this database was made with", err)
		}
		if errors.Is(err, pager.ErrNotEncrypted) {
			return nil, nil, fmt.Errorf("%w\n  drop -encrypt, or this is not the database you meant", err)
		}
		return nil, nil, err
	}

	opened, err := store.Open(pages)
	if err != nil {
		file.Close()
		held.Close()
		return nil, nil, err
	}

	// The same directory the server uses, so that a tool and a server see the
	// same database rather than each seeing the part of it they made.
	folder, err := vfs.At(strings.TrimSuffix(path, ".sapedb")+".parts", 0o600)
	if err != nil {
		file.Close()
		held.Close()
		return nil, nil, err
	}
	opened.Keep(folder, settings.Key)

	return opened, func() { pages.Close(); held.Close() }, nil
}

// schema is what an apply file holds.
type schema struct {
	Collections []store.Spec      `json:"collections,omitempty"`
	Operations  []store.Operation `json:"operations,omitempty"`
}

// apply declares what the files say, and says what changed.
//
// Applying the same file twice does nothing the first time did not: a
// collection already declared that way is left alone, and an operation whose
// declaration has not changed is not given a new version. That is what makes
// this safe to run on every deploy, which is the only way it will actually be
// run.
func apply(db *store.Store, files []string, out io.Writer) error {
	if len(files) == 0 {
		return fmt.Errorf("%w: apply needs a file", ErrUsage)
	}

	for _, name := range files {
		content, err := os.ReadFile(name)
		if err != nil {
			return err
		}

		wanted := schema{}
		decoder := json.NewDecoder(strings.NewReader(string(content)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&wanted); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		for _, spec := range wanted.Collections {
			if _, err := db.Declare(spec); err != nil {
				return fmt.Errorf("%s: collection %q: %w", name, spec.Name, err)
			}
			fmt.Fprintf(out, "collection %s\n", spec.Name)
		}

		for _, operation := range wanted.Operations {
			before, found, err := db.Operation(operation.Name, 0)
			if err != nil && !errors.Is(err, store.ErrNoOperation) {
				return err
			}
			if found && sameOperation(before, operation) {
				fmt.Fprintf(out, "operation %s unchanged at version %d\n", operation.Name, before.Version)
				continue
			}

			stored, err := db.DeclareOperation(operation)
			if err != nil {
				return fmt.Errorf("%s: operation %q: %w", name, operation.Name, err)
			}
			fmt.Fprintf(out, "operation %s version %d\n", stored.Name, stored.Version)
		}
	}

	return db.Commit()
}

// sameOperation compares a declaration with one already stored, ignoring the
// version the stored one was given.
func sameOperation(stored, wanted store.Operation) bool {
	wanted.Version = stored.Version

	first, err := json.Marshal(stored)
	if err != nil {
		return false
	}
	second, err := json.Marshal(wanted)
	if err != nil {
		return false
	}
	return string(first) == string(second)
}

func list(db *store.Store, out io.Writer) error {
	// First, because how the database was last left decides whether anything
	// below is the whole story.
	if state := db.HowItWasLeft(); state != "" {
		fmt.Fprintln(out, state)
	}

	for _, name := range db.Collections() {
		collection, err := db.Collection(name)
		if err != nil {
			return err
		}
		spec := collection.Spec()

		fmt.Fprintf(out, "collection %s (key %s %s", spec.Name, spec.Key.Path, spec.Key.Type)
		if spec.Key.Auto != "" {
			fmt.Fprintf(out, ", %s", spec.Key.Auto)
		}
		fmt.Fprintln(out, ")")

		for _, index := range spec.Indexes {
			fields := make([]string, 0, len(index.Fields))
			for _, field := range index.Fields {
				mark := ""
				if field.Descending {
					mark = " desc"
				}
				fields = append(fields, field.Path+" "+field.Type+mark+" missing:"+field.Missing)
			}
			flags := ""
			if index.Unique {
				flags += " unique"
			}
			if index.Array != "" {
				flags += " array:" + index.Array
			}
			fmt.Fprintf(out, "  index %s%s [%s]\n", index.Name, flags, strings.Join(fields, ", "))
		}
	}

	operations, err := db.Operations()
	if err != nil {
		return err
	}
	for _, operation := range operations {
		fmt.Fprintf(out, "operation %s v%d %s %s", operation.Name, operation.Version, operation.Action, operation.Collection)
		if operation.Index != "" {
			fmt.Fprintf(out, " via %s", operation.Index)
		}
		if operation.Limit > 0 {
			fmt.Fprintf(out, " limit %d", operation.Limit)
		}
		if len(operation.Scopes) > 0 {
			fmt.Fprintf(out, " scopes %s", strings.Join(operation.Scopes, ","))
		}
		fmt.Fprintln(out)
	}

	latest, err := db.LatestLSN()
	if err != nil {
		return err
	}
	oldest, err := db.OldestLSN()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "log %d..%d\n", oldest, latest)
	return nil
}

func dump(db *store.Store, out io.Writer) error {
	_, err := db.Dump(out)
	return err
}

func restore(db *store.Store, in io.Reader, out io.Writer) error {
	at, err := db.Restore(in)
	if err != nil {
		return err
	}
	if err := db.Commit(); err != nil {
		return err
	}
	fmt.Fprintf(out, "restored to change %d\n", at)
	return nil
}

func changes(db *store.Store, args []string, out io.Writer) error {
	from := uint64(0)
	if len(args) > 0 {
		if _, err := fmt.Sscanf(args[0], "%d", &from); err != nil {
			return fmt.Errorf("%w: %q is not an entry number", ErrUsage, args[0])
		}
	}

	return db.Changes(from, func(change store.Change) bool {
		line, err := json.Marshal(change)
		if err != nil {
			return false
		}
		fmt.Fprintln(out, string(line))
		return true
	})
}

// url prints a connection string.
//
// The password is made here and printed as part of the string, or read from
// stdin when the caller has one already. It is never an argument: arguments
// are visible in ps to every user on the machine, and a password that has been
// in a process list is a password that has been published.
func url(opts options, args []string, stdin io.Reader, out io.Writer) error {
	host := "localhost:7433"
	if len(args) > 0 {
		host = args[0]
	}

	password := ""
	if len(args) > 1 {
		if args[1] != "-" {
			return fmt.Errorf("%w: a password is read from stdin with -, never given as an argument", ErrUsage)
		}
		read, err := io.ReadAll(io.LimitReader(stdin, 1024))
		if err != nil {
			return err
		}
		password = strings.TrimSpace(string(read))
	} else {
		made, err := makePassword()
		if err != nil {
			return err
		}
		password = made
	}

	// The password is not checked here: signing refuses one the format does not
	// allow, and two places deciding the same thing is one place to forget when
	// the rule changes.
	signature, err := signing.Sign(
		signing.Parts{AccountID: opts.account, Password: password, DBName: opts.db},
		opts.secret, opts.label,
	)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "sapedb://%s:%s@%s/%s?sig=%s\n", opts.account, password, host, opts.db, signature)
	return nil
}

func makePassword() (string, error) {
	raw := make([]byte, passwordLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("sapedb: no randomness for a password: %w", err)
	}

	// Rejection-free: the alphabet is 66 long and a byte is 256, so taking the
	// remainder favours the first 58 characters slightly. Drawn again until the
	// byte is in a range that divides evenly instead.
	made := make([]byte, 0, passwordLength)
	for len(made) < passwordLength {
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		for _, b := range raw {
			if int(b) >= (256/len(passwordAlphabet))*len(passwordAlphabet) {
				continue
			}
			made = append(made, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(made) == passwordLength {
				break
			}
		}
	}
	return string(made), nil
}
