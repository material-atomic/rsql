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
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/signing"
	"github.com/material-atomic/rsql/internal/store"
	"github.com/material-atomic/rsql/internal/vfs"
)

// dbKeyLabel must match what the server derives with, or a database written by
// one is unreadable by the other.
const dbKeyLabel = "rsql/server:database:v1"

// passwordBytes is how long a generated password is. Well inside the 16..128
// the format allows, and long enough that guessing is not a strategy.
const passwordLength = 32

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~-"

const usage = `rsql — set up and look inside a database

  rsql [options] apply FILE...     declare collections and operations
  rsql [options] ls                what this database holds
  rsql [options] dump              write a dump to stdout
  rsql [options] restore           read a dump from stdin, into an empty database
  rsql [options] log [FROM]        print the change log from an entry onwards
  rsql [options] url               print a signed connection string
  rsql [options] shell [HOST]      look inside a running server

options
  -dir DIR       where databases live      (RSQL_DIR, default /var/lib/rsql)
  -account NAME  which account             (RSQL_ACCOUNT)
  -db NAME       which database            (RSQL_DB)
  -encrypt       the database is encrypted (RSQL_ENCRYPT)

RSQL_SECRET is read from the environment. It is what connection strings are
signed with and what database encryption keys are derived from, so a command
run with the wrong one either refuses or writes a file the server cannot read.

A password is never taken as an argument: arguments are visible to anyone who
can run ps. "url" makes one and prints it as part of the connection string,
or reads one from stdin when told to.
`

var (
	ErrUsage  = errors.New("rsql: that is not how this is used")
	ErrSecret = errors.New("rsql: RSQL_SECRET is not set")
)

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
	get := func(name, fallback string) string {
		if value, found := lookup(name); found && value != "" {
			return value
		}
		return fallback
	}

	label := get("RSQL_LABEL", "")
	if strings.EqualFold(label, "direct") {
		label = signing.Direct
	}

	opts := options{
		dir:     get("RSQL_DIR", "/var/lib/rsql"),
		account: get("RSQL_ACCOUNT", ""),
		db:      get("RSQL_DB", ""),
		secret:  get("RSQL_SECRET", ""),
		encrypt: strings.EqualFold(get("RSQL_ENCRYPT", ""), "1") || strings.EqualFold(get("RSQL_ENCRYPT", ""), "true"),
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

// open opens the database file, taking its lock.
func open(opts options) (*store.Store, func(), error) {
	// The directory first: a server holds this for its whole life, so being
	// refused here is the answer to "is the server running", rather than a
	// guess that depends on which databases it happens to have opened.
	held, err := vfs.LockDir(opts.dir)
	if err != nil {
		if errors.Is(err, vfs.ErrLocked) {
			return nil, nil, fmt.Errorf("%w\n  the server is probably running; stop it, or work on a dump instead", err)
		}
		return nil, nil, err
	}

	path := filepath.Join(opts.dir, opts.account, opts.db+".rsql")
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
		key, err := hkdf.Key(sha256.New, []byte(opts.secret), []byte(opts.account+"/"+opts.db), dbKeyLabel, pager.KeyBytes)
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
			return nil, nil, fmt.Errorf("%w\n  RSQL_SECRET does not match the one this database was made with", err)
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
	folder, err := vfs.At(strings.TrimSuffix(path, ".rsql")+".parts", 0o600)
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

	fmt.Fprintf(out, "rsql://%s:%s@%s/%s?sig=%s\n", opts.account, password, host, opts.db, signature)
	return nil
}

func makePassword() (string, error) {
	raw := make([]byte, passwordLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("rsql: no randomness for a password: %w", err)
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
