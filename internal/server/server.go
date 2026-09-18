// Package server is the only way a client reaches a database.
//
// A connection carries a connection string that was issued by whoever holds
// the secret: account, password and database name, signed together. The server
// verifies that signature and nothing else — there is no user table here, and
// no password to look up. The connection string IS the authorisation, and the
// secret is the single root of trust. That is what lets a database be created
// on first use without any provisioning step: a name nobody has blessed cannot
// be reached, and a name that has been blessed needs no further record.
//
// Everything after the handshake is a declared operation invoked by name. The
// server does not parse a query language because there is not one; it looks up
// the operation the database already holds, and runs it.
package server

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/protocol"
	"github.com/material-atomic/rsql/internal/signing"
	"github.com/material-atomic/rsql/internal/store"
	"github.com/material-atomic/rsql/internal/vfs"
)

// dbKeyLabel separates the key a database file is encrypted with from every
// other use of the same secret.
const dbKeyLabel = "rsql/server:database:v1"

var (
	ErrHandshake  = errors.New("rsql/server: the connection did not open properly")
	ErrName       = errors.New("rsql/server: that is not a usable account or database name")
	ErrClosed     = errors.New("rsql/server: the server is closed")
	ErrNotAllowed = errors.New("rsql/server: this connection may not reach that database")
)

// Options are what a server needs to run.
type Options struct {
	// Dir is where database files live, one per account and name.
	Dir string
	// Secret is what connection strings are signed with. Without it nothing
	// can be verified, so a server will not start without one.
	Secret string
	// Label is the signing label; empty means the default.
	Label string
	// Encrypt stores every database encrypted under a key derived from the
	// secret. It protects a stolen disk, not a compromised server — the server
	// can read every database it serves, by construction.
	Encrypt bool
}

// Server holds the open databases and serves connections.
type Server struct {
	options Options

	mutex  sync.Mutex
	open   map[string]*database
	closed bool
	// held is the lock on the whole directory. One server per directory, found
	// out at startup rather than at whichever request first collided.
	held io.Closer
}

// database is one open file and the lock that keeps its single writer single.
type database struct {
	mutex sync.Mutex
	pages *pager.Pager
	store *store.Store
	file  vfs.File

	// changed is closed and replaced every time something is committed. A
	// subscriber waits on it instead of asking again and again: a feed that
	// polls is a feed that is either late or wasteful, and usually both.
	watch   sync.Mutex
	changed chan struct{}
}

// notify wakes everything waiting for a change.
func (d *database) notify() {
	d.watch.Lock()
	defer d.watch.Unlock()

	if d.changed != nil {
		close(d.changed)
	}
	d.changed = make(chan struct{})
}

// waiting is a channel that closes when the next change is committed.
//
// Taken BEFORE reading the log, or a change committed between the read and the
// wait is one nothing ever wakes for — a subscriber that stops at exactly the
// wrong moment and looks healthy.
func (d *database) waiting() <-chan struct{} {
	d.watch.Lock()
	defer d.watch.Unlock()

	if d.changed == nil {
		d.changed = make(chan struct{})
	}
	return d.changed
}

// New checks the options and returns a server that has opened nothing yet.
func New(options Options) (*Server, error) {
	if options.Secret == "" {
		return nil, errors.New("rsql/server: no secret, so no connection could be verified")
	}
	if options.Dir == "" {
		return nil, errors.New("rsql/server: no directory to keep databases in")
	}
	if err := os.MkdirAll(options.Dir, 0o700); err != nil {
		return nil, err
	}

	held, err := vfs.LockDir(options.Dir)
	if err != nil {
		return nil, err
	}
	return &Server{options: options, open: map[string]*database{}, held: held}, nil
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			_ = s.Handle(conn)
		}()
	}
}

// Close shuts every open database. Connections in flight finish on their own.
func (s *Server) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.closed = true
	var failed error
	for _, db := range s.open {
		db.mutex.Lock()
		if err := db.pages.Close(); err != nil && failed == nil {
			failed = err
		}
		db.mutex.Unlock()
	}
	s.open = map[string]*database{}

	if s.held != nil {
		if err := s.held.Close(); err != nil && failed == nil {
			failed = err
		}
		s.held = nil
	}
	return failed
}

// Handle runs one connection: the handshake, then frames until it ends.
func (s *Server) Handle(conn io.ReadWriter) error {
	reader := protocol.NewReader(conn).Accept(protocol.Version)

	// Subscriptions write from goroutines of their own, so every write goes
	// through one place. Two writers on one socket make bytes that are each
	// correct and together are not a frame.
	out := &sender{conn: conn}

	// Closed when this connection ends, which is how a subscription learns to
	// stop rather than streaming into a socket nobody is reading.
	done := make(chan struct{})
	defer close(done)

	live, err := s.handshake(reader, out)
	if err != nil {
		// The client is told why, then the connection ends: a handshake that
		// failed must not leave a socket that looks usable.
		_ = out.send(protocol.Frame{Type: protocol.Failure, ID: 0}, failure(err))
		return err
	}

	for {
		frame, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		switch frame.Type {
		case protocol.Ping:
			if err := out.send(protocol.Frame{Type: protocol.Pong, ID: frame.ID}, nil); err != nil {
				return err
			}

		case protocol.Goodbye:
			return nil

		case protocol.Elevate:
			if err := s.elevate(live, frame.Payload); err != nil {
				if err := out.send(protocol.Frame{Type: protocol.Failure, ID: frame.ID}, failure(err)); err != nil {
					return err
				}
				continue
			}
			if err := out.send(protocol.Frame{Type: protocol.Result, ID: frame.ID}, []byte(`{"operator":true}`)); err != nil {
				return err
			}

		case protocol.Explore:
			body, err := s.explore(live, frame.Payload)
			if err != nil {
				if err := out.send(protocol.Frame{Type: protocol.Failure, ID: frame.ID}, failure(err)); err != nil {
					return err
				}
				continue
			}
			if err := out.send(protocol.Frame{Type: protocol.Result, ID: frame.ID}, body); err != nil {
				return err
			}

		case protocol.Invoke:
			result, err := s.invoke(live, frame.Payload)
			if err != nil {
				if err := out.send(protocol.Frame{Type: protocol.Failure, ID: frame.ID}, failure(err)); err != nil {
					return err
				}
				continue
			}
			if err := out.send(protocol.Frame{Type: protocol.Result, ID: frame.ID}, result); err != nil {
				return err
			}

		case protocol.Subscribe:
			if err := s.follow(live, out, frame.ID, frame.Payload, done); err != nil {
				if err := out.send(protocol.Frame{Type: protocol.Failure, ID: frame.ID}, failure(err)); err != nil {
					return err
				}
			}

		default:
			// A frame this version does not serve is refused by name rather
			// than ignored: a client waiting for an answer that never comes is
			// worse off than one that is told no.
			body := failure(fmt.Errorf("rsql/server: nothing here serves a %s frame", frame.Type))
			if err := out.send(protocol.Frame{Type: protocol.Failure, ID: frame.ID}, body); err != nil {
				return err
			}
		}
	}
}

// hello is what a client opens with.
//
// In "bound" mode the database is named here and fixed for the connection. In
// "account" mode it is not: one connection serves every database of an
// account, and each call names its own and carries its own signature.
//
// That means an account-mode handshake proves nothing, and is not treated as
// if it did. The connection grants no access at all; every call is verified on
// its way through. Anything else would make the handshake a credential for
// databases it never named.
type hello struct {
	Account   string `json:"account"`
	Password  string `json:"password"`
	DBName    string `json:"dbname"`
	Signature string `json:"sig"`
	Mode      string `json:"mode"`
}

// session is what one connection knows.
type session struct {
	opening hello
	// bound is the database of a bound connection, and nil for an account one.
	bound *database
	// verified is the databases this connection has already shown a signature
	// for. Checking a signature is an HMAC, which is cheap, but doing it per
	// call on a hot connection is work nobody asked for — and the answer cannot
	// change while the connection lives.
	verified map[string]*database

	// challenge is what this connection must answer to become an operator, and
	// operator is whether it has. One challenge per connection, so a proof
	// that leaks out of a log or a process listing is already spent.
	challenge []byte
	operator  bool
}

// welcome is what it gets back.
type welcome struct {
	Version   uint8  `json:"version"`
	Account   string `json:"account"`
	DBName    string `json:"dbname,omitempty"`
	Mode      string `json:"mode"`
	LSN       uint64 `json:"lsn,omitempty"`
	Encrypted bool   `json:"encrypted"`
	// Challenge is what an operator would have to answer. Sent to everyone,
	// because a challenge is not a secret and deciding who gets one would mean
	// the server knew who was asking before they had proved anything.
	Challenge string `json:"challenge,omitempty"`
}

// How a connection is scoped.
const (
	ModeBound   = "bound"
	ModeAccount = "account"
)

func (s *Server) handshake(reader *protocol.Reader, out *sender) (*session, error) {
	frame, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	if frame.Type != protocol.Hello {
		return nil, fmt.Errorf("%w: it began with a %s frame", ErrHandshake, frame.Type)
	}

	opening := hello{}
	if err := json.Unmarshal(frame.Payload, &opening); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	if opening.Mode == "" {
		opening.Mode = ModeBound
	}

	live := &session{opening: opening, verified: map[string]*database{}}

	live.challenge = make([]byte, signing.NonceBytes)
	if _, err := rand.Read(live.challenge); err != nil {
		return nil, err
	}

	greeting := welcome{
		Version: protocol.Version, Account: opening.Account, Mode: opening.Mode,
		Encrypted: s.options.Encrypt, Challenge: hex.EncodeToString(live.challenge),
	}

	switch opening.Mode {
	case ModeBound:
		// The signature is checked before anything is opened or created. A name
		// nobody signed for must not so much as cause a file to appear.
		db, err := s.verify(opening.Account, opening.DBName, opening.Password, opening.Signature)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
		live.bound = db
		live.verified[opening.DBName] = db

		db.mutex.Lock()
		lsn, err := db.store.LatestLSN()
		db.mutex.Unlock()
		if err != nil {
			return nil, err
		}
		greeting.DBName, greeting.LSN = opening.DBName, lsn

	case ModeAccount:
		// Nothing is opened and nothing is verified here, because there is
		// nothing yet to verify against: the signature a client holds is for a
		// database this handshake does not name. Every call brings its own.

	default:
		return nil, fmt.Errorf("%w: no mode called %q", ErrHandshake, opening.Mode)
	}

	body, err := json.Marshal(greeting)
	if err != nil {
		return nil, err
	}
	if err := out.send(protocol.Frame{Type: protocol.Welcome, ID: frame.ID}, body); err != nil {
		return nil, err
	}
	return live, nil
}

// verify checks a signature and opens the database it is for.
func (s *Server) verify(account, name, password, signature string) (*database, error) {
	parts := signing.Parts{AccountID: account, Password: password, DBName: name}
	if !signing.Verify(signature, parts, s.options.Secret, s.options.Label) {
		return nil, signing.ErrBadSignature
	}
	return s.database(account, name)
}

// call is one invocation on the wire.
//
// The field names are the client's, not this server's preference. They are the
// contract, and a server that renames them is a server the published driver
// cannot talk to — which is worth rather more than a tidier spelling.
type call struct {
	Command   string         `json:"command"`
	Version   int            `json:"version,omitempty"`
	Arguments map[string]any `json:"args,omitempty"`
	WriteID   string         `json:"writeId,omitempty"`

	// DBName and Signature are how an account-wide connection says which
	// database this call is for, and proves it may.
	DBName    string `json:"dbname,omitempty"`
	Signature string `json:"sig,omitempty"`
}

func (s *Server) invoke(live *session, payload []byte) ([]byte, error) {
	asked := call{}
	if err := json.Unmarshal(payload, &asked); err != nil {
		return nil, fmt.Errorf("rsql/server: the call does not read as one: %w", err)
	}

	db, err := s.reach(live, asked)
	if err != nil {
		return nil, err
	}
	opened := live.opening

	// One writer at a time per database, which is what the engine underneath
	// allows. Connections to one database queue here rather than racing.
	db.mutex.Lock()
	defer db.mutex.Unlock()

	// Scopes are not taken from the request: a caller that names its own
	// permissions has none. Until a token carries them, an operation that
	// declares a scope cannot be reached over the wire at all — which is the
	// safe direction to be incomplete in.
	caller := store.Caller{Actor: opened.Account, WriteID: asked.WriteID}

	result, err := db.store.Invoke(caller, asked.Command, asked.Version, asked.Arguments)
	if err != nil {
		return nil, err
	}
	// Invoke commits what it changed, so there is nothing to do here but tell
	// whoever is watching. Committing again would cost two more syncs and
	// change nothing.
	if result.Changed > 0 || result.Repeated > 0 {
		db.notify()
	}
	return json.Marshal(result)
}

// reach is the database a call is for, and the check that it may be.
//
// A bound connection has one and calls name none. An account connection names
// one per call and signs for it — and the answer is remembered, because an
// HMAC per call on a hot connection is work nobody asked for and the answer
// cannot change while the connection lives.
func (s *Server) reach(live *session, asked call) (*database, error) {
	if live.bound != nil {
		if asked.DBName != "" && asked.DBName != live.opening.DBName {
			return nil, fmt.Errorf("%w: this connection is bound to %q", ErrNotAllowed, live.opening.DBName)
		}
		return live.bound, nil
	}

	if asked.DBName == "" {
		return nil, fmt.Errorf("%w: an account-wide connection needs the database on every call", ErrNotAllowed)
	}
	if db, found := live.verified[asked.DBName]; found {
		return db, nil
	}

	db, err := s.verify(live.opening.Account, asked.DBName, live.opening.Password, asked.Signature)
	if err != nil {
		return nil, err
	}
	live.verified[asked.DBName] = db
	return db, nil
}

// database opens the file for an account and name, or returns the one already
// open.
func (s *Server) database(account, name string) (*database, error) {
	if err := usableComponent(account); err != nil {
		return nil, fmt.Errorf("%w: account %q: %v", ErrName, account, err)
	}
	if err := usableComponent(name); err != nil {
		return nil, fmt.Errorf("%w: database %q: %v", ErrName, name, err)
	}

	at := account + "/" + name

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed {
		return nil, ErrClosed
	}
	if db, found := s.open[at]; found {
		return db, nil
	}

	folder := filepath.Join(s.options.Dir, account)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(folder, name+".rsql")

	db, err := s.openFile(path, account, name)
	if err != nil {
		return nil, err
	}
	s.open[at] = db
	return db, nil
}

func (s *Server) openFile(path, account, name string) (*database, error) {
	file, err := vfs.OpenFile(path, 0o600)
	if err != nil {
		return nil, err
	}

	options := pager.Options{}
	if s.options.Encrypt {
		key, err := hkdf.Key(sha256.New, []byte(s.options.Secret), []byte(account+"/"+name), dbKeyLabel, pager.KeyBytes)
		if err != nil {
			file.Close()
			return nil, err
		}
		options.Key = key
	}

	size, err := file.Size()
	if err != nil {
		file.Close()
		return nil, err
	}

	var pages *pager.Pager
	if size == 0 {
		pages, err = pager.CreateWith(file, options)
	} else {
		pages, err = pager.OpenWith(file, options)
	}
	if err != nil {
		file.Close()
		return nil, err
	}

	opened, err := store.Open(pages)
	if err != nil {
		file.Close()
		return nil, err
	}
	return &database{pages: pages, store: opened, file: file, changed: make(chan struct{})}, nil
}

// Store hands back the store for an account and database, for a caller inside
// this process — the CLI declaring a collection, a test setting one up.
func (s *Server) Store(account, name string) (*store.Store, func(), error) {
	db, err := s.database(account, name)
	if err != nil {
		return nil, nil, err
	}
	db.mutex.Lock()
	return db.store, db.mutex.Unlock, nil
}

// Sign makes the signature a connection string for this database needs, for
// whoever is issuing one.
func (s *Server) Sign(account, password, name string) (string, error) {
	return signing.Sign(signing.Parts{AccountID: account, Password: password, DBName: name},
		s.options.Secret, s.options.Label)
}

// usableComponent keeps a name from being a path.
//
// An account or database name becomes a directory and a file name, so a name
// holding a separator or a parent reference would put a database somewhere
// nobody meant it to be — and a signed name is only as safe as what it is
// allowed to mean.
func usableComponent(name string) error {
	switch {
	case name == "":
		return errors.New("it is empty")
	case len(name) > 64:
		return fmt.Errorf("%d characters is more than 64", len(name))
	case name == "." || name == "..":
		return errors.New("it names a directory")
	case strings.ContainsAny(name, `/\:`+"\x00"):
		return errors.New("it holds a path separator")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("it holds %q, and names are letters, digits, dot, dash and underscore", r)
		}
	}
	return nil
}

func write(conn io.Writer, frame protocol.Frame, payload []byte) error {
	frame.Version = protocol.Version
	frame.Payload = payload

	encoded, err := protocol.Encode(frame)
	if err != nil {
		return err
	}
	_, err = conn.Write(encoded)
	return err
}

// failure is what a client is told when something did not work.
//
// A message and a code, because a client that only receives prose cannot act
// on it: retry, ask for credentials again, or give up are different answers,
// and telling them apart by matching strings is how a driver breaks when a
// server improves its wording.
func failure(err error) []byte {
	body, marshalled := json.Marshal(struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		At      int64  `json:"at"`
	}{Message: err.Error(), Code: codeFor(err), At: time.Now().UnixMilli()})
	if marshalled != nil {
		return []byte(`{"message":"rsql/server: the failure could not be described","code":"failed"}`)
	}
	return body
}

// codeFor names what kind of failure this is, in a word a client can switch on.
func codeFor(err error) string {
	for _, known := range []struct {
		err  error
		code string
	}{
		{signing.ErrBadSignature, "signature"},
		{ErrHandshake, "handshake"},
		{ErrName, "name"},
		{ErrClosed, "closed"},
		{ErrTooFarBehind, "too_far_behind"},
		{store.ErrNoOperation, "no_operation"},
		{store.ErrArgument, "argument"},
		{store.ErrNotAllowed, "not_allowed"},
		{ErrNotOperator, "not_operator"},
		{store.ErrExists, "exists"},
		{store.ErrMissing, "missing"},
		{store.ErrCondition, "condition"},
		{store.ErrUncommitted, "uncommitted"},
		{store.ErrDuplicate, "duplicate"},
		{store.ErrType, "type"},
		{store.ErrNoCollection, "no_collection"},
		{store.ErrNoIndex, "no_index"},
		{store.ErrNoKey, "no_key"},
		{store.ErrDeclaration, "declaration"},
		{store.ErrDamaged, "damaged"},
	} {
		if errors.Is(err, known.err) {
			return known.code
		}
	}
	return "failed"
}
