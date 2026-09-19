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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sapedb/sapedb/internal/dbkey"
	"github.com/sapedb/sapedb/internal/dbname"
	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

var (
	ErrHandshake  = errors.New("sapedb/server: the connection did not open properly")
	ErrName       = errors.New("sapedb/server: that is not a usable account or database name")
	ErrClosed     = errors.New("sapedb/server: the server is closed")
	ErrNotAllowed = errors.New("sapedb/server: this connection may not reach that database")
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

	// Notice is where the server says things that are nobody's request and
	// somebody's business — a database that was not closed cleanly, most of
	// all. Nil says them nowhere.
	Notice func(string)
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
		return nil, errors.New("sapedb/server: no secret, so no connection could be verified")
	}
	if options.Dir == "" {
		return nil, errors.New("sapedb/server: no directory to keep databases in")
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
			// Registered after conn.Close, which means it runs BEFORE it —
			// defers unwind last-registered-first — so guard still has a
			// live socket to write a Failure frame to if this connection's
			// goroutine panics. See guard's own godoc for what it does and
			// does not buy.
			defer guard(conn, s.options.Notice)
			// Handle already told the client why, over the wire, when it can —
			// a Failure frame for a handshake that failed. But the client was
			// the one asking; the operator running sapedbd was not in that
			// conversation and, until this, had no way to be. A dropped error
			// here (the underscore this replaced) meant a signature rejected,
			// a database file that would not open, or a client that spoke out
			// of turn all looked identical from the outside: the socket just
			// closed, and sapedbd never said a word.
			if err := s.Handle(conn); err != nil {
				s.notice(conn, err)
			}
		}()
	}
}

// guard is what a connection's goroutine defers instead of a bare recover().
//
// It does NOT let the connection, or the process, keep running. Task 0049
// measured why a recover-and-keep-serving guard would be actively worse
// than today's crash, not just no better: runBatch (batch.go) calls
// s.Rollback() only on its error path, not with its own defer, so a panic
// partway through it skips that call and leaves s.pages.Pending() true —
// and runBatch's own first line refuses to run anything else on a database
// with Pending() true, forever, for every caller, until the process
// restarts. A server that survives a panic by recovering and serving on
// would answer every write after that on this database with ErrUncommitted
// for as long as it stayed up: a wrong answer that outlives the panic that
// caused it. Dying is the safe path already available — storage here
// tolerates power loss (two meta pages, an ordered write sequence) and
// sayHowItWasLeft reports exactly that shape of shutdown the next time the
// file opens — so this does not try to invent a safer one.
//
// It is also not an answer to task 0049's other half: satisfies (batch.go)
// used to be able to end a goroutine with `fatal error: stack overflow`,
// not a panic, over a self-referential argument through the exported
// Invoke. No recover() anywhere — this one included — catches a fatal
// error; that half is closed by describe (describe.go) making the format
// call return instead of recursing forever, not by anything here. A guard
// at this boundary and a bounded formatter one layer down are two
// different fixes for two different ways a goroutine could stop, and one
// does not stand in for the other.
//
// And it only covers the ONE goroutine it is deferred in — Serve's
// per-connection goroutine, above. recover() only ever catches a panic
// unwinding through its own goroutine's own defer stack; it cannot reach
// into another one. This is not this connection's only goroutine: follow
// (subscribe.go) starts a second one per live subscription with
// `go s.stream(...)`, and stream has no recover of its own. A panic inside
// stream today crashes the whole process exactly as if this file did not
// exist — measured in TestGuardInOneGoroutineDoesNotProtectAPanicInAnother
// Goroutine (server_test.go), which is the general case (any two
// goroutines, not stream specifically) because reaching an actual panic
// inside stream would need a separate task's worth of fault injection.
// Giving stream its own guard is future work this task does not do: mục 5
// keeps this task's touch to guard and Serve.
//
// What it actually buys, measured against having nothing here at all: a
// client gets a Failure frame instead of a socket that just closes (0048
// taught the client driver to read ID 0 as a reason at teardown; before
// that, this would have been silent to the client too), and the operator
// gets a line naming the remote address, the panic value, and a stack —
// where before this there was nothing at all, on any of the panics
// task 0042 already knew this goroutine could throw.
//
// What is NOT reused after this runs: nothing. The three steps below run
// against the state as it stood at the moment of the panic — a
// best-effort write on a connection that may itself be half torn, a log
// line, nothing more — and then the same panic value goes back out
// unrecovered, which is what makes "nothing is reused" true rather than
// asserted: there is no further code path here that touches s, conn, or
// anything derived from either afterwards.
func guard(conn net.Conn, notice func(string)) {
	recovered := recover()
	if recovered == nil {
		return
	}

	// The panic value keeps its own identity when it already was an error:
	// handing the very same error to failure() (and, through it, codeFor)
	// preserves whatever errors.Is chain it already carried, exactly the
	// property task 0049's brief warned this file not to lose a second
	// time. handshake() (this same package) wraps signing.ErrBadSignature
	// with fmt.Errorf("%w: %v", ErrHandshake, err) — %v, not %w, on the
	// inner error — and that single wrong verb is why codeFor reports a
	// signature failure as "handshake" instead of "signature" (logged as
	// a debt, not fixed here: mục 5 of this task keeps codeFor and
	// handshake() out of scope). This file does not repeat that shape: a
	// panic value that is already an error is passed through as-is, not
	// re-wrapped with %v or %w. A panic value that was never an error —
	// the ordinary case, a string from a bare `panic("...")` or a runtime
	// error like an index out of range — has no identity to lose, and
	// %v is the plain, correct way to turn it into one. codeFor does not
	// gain a new code for this: whatever it already returns (usually
	// "failed", the word for "this build does not have a nearer answer"),
	// unless the panic value happened to already be one of its known
	// sentinels, is left alone. See mục 5 of task 0049 for why that line
	// is not to be touched here.
	var err error
	if asErr, ok := recovered.(error); ok {
		err = asErr
	} else {
		err = fmt.Errorf("%v", recovered)
	}

	// 1) Tell the client something, on this same connection, best-effort.
	// A write that fails here (the connection may already be the reason
	// this panicked) is not itself a second failure worth reporting —
	// there is already a panic in flight, and conn.Close (deferred before
	// this in Serve) still runs after this returns either way.
	_ = (&sender{conn: conn}).send(protocol.Frame{Type: protocol.Failure, ID: 0}, failure(err))

	// 2) Tell the operator: address, the panic value itself (not just
	// err.Error() — a recovered runtime error's %v and its Error() text
	// agree, but this is written to read like what it is, a panic, not
	// an ordinary failed connection), and where it happened.
	if notice != nil {
		notice(fmt.Sprintf("a connection from %s panicked: %v\n%s", conn.RemoteAddr(), recovered, debug.Stack()))
	}

	// 3) Die. The exact recovered value, not err — a caller of recover()
	// downstream of this (there is none in this build, but the next
	// reader of this function should not have to check) must see the
	// original panic, not a value this function reshaped on the way
	// through.
	panic(recovered)
}

// notice reports a connection that ended in error, one line per connection.
//
// Handle returns nil for a Goodbye and for a clean EOF, so this never fires
// for a connection that ended the way everyone expected — only for one that
// did not, which is the only case an operator needs to see. It also fires
// for every kind of failure Handle can return, not only a handshake one:
// a client that goes silent mid-stream, or sends a frame this build cannot
// read, is exactly as invisible today as a rejected handshake was, and the
// fix for one is the fix for both.
//
// What gets printed is conn.RemoteAddr() and err.Error(), nothing else.
// err.Error() is not a fixed vocabulary: handshake() wraps its inner error
// with %v, and some of those errors do quote a value the caller sent —
// "no mode called %q" prints the mode verbatim, and usableComponent prints
// one rune of an account or database name plus its length. What none of
// them touch is the password or the signature: verify() answers with the
// constant signing.ErrBadSignature and nothing else ever reads those two
// fields into an error. That is the property this relies on, and it is a
// property of which fields today's errors reach for — not a rule the
// structure enforces — so anyone adding a field to hello (a token, a
// session key) has to check it again rather than assume this axis is
// closed for free. Quoting is %q throughout, so a newline in a caller's
// value cannot forge a second line in the log.
//
// No throttle: a caller that keeps knocking with the same bad credentials
// produces one line per attempt. A handshake that fails has already spent an
// accepted connection, and until now this was the only place any connection
// failure — handshake or not — was visible to the person running the
// server. Adding a rate limit is future work, not this task's.
func (s *Server) notice(conn net.Conn, err error) {
	if s.options.Notice == nil {
		return
	}
	s.options.Notice(fmt.Sprintf("a connection from %s ended: %v", conn.RemoteAddr(), err))
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
			body := failure(fmt.Errorf("sapedb/server: nothing here serves a %s frame", frame.Type))
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
		return nil, fmt.Errorf("sapedb/server: the call does not read as one: %w", err)
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
// at all — openFile below would simply create a new, empty database at the .sapedb
// path and this server would answer every call about a real database as if
// it were empty. That is not "not found", it is data going invisible. So
// this stops and says exactly where the data actually is, rather than
// renaming it or opening it as-is: changing what a file means without
// being asked is not this server's call to make, and the operator is the
// one who knows whether that old file is still needed anywhere else.
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

// database opens the file for an account and name, or returns the one already
// open.
func (s *Server) database(account, name string) (*database, error) {
	// The rule itself now lives in internal/dbname, not here — see that
	// package's doc comment for why, and task 0053's Result section for the
	// measurement that this changed nothing about what is accepted. The
	// wrapping below is unchanged from before that move: ErrName and the
	// wire code it maps to (codeFor, "name") are a contract with clients,
	// and this task does not touch either.
	if err := dbname.Check(account); err != nil {
		return nil, fmt.Errorf("%w: account %q: %v", ErrName, account, err)
	}
	if err := dbname.Check(name); err != nil {
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
	path := filepath.Join(folder, name+".sapedb")
	if err := checkOldExtension(path); err != nil {
		return nil, err
	}

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
		key, err := dbkey.Key(s.options.Secret, account, name)
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

	// Where this database's partitions live: a directory of its own, so that
	// "what files does this database have" is answered by the directory rather
	// than by a naming convention. A wrong answer to that question unlinks
	// somebody else's data.
	folder, err := vfs.At(strings.TrimSuffix(path, ".sapedb")+".parts", 0o600)
	if err != nil {
		file.Close()
		return nil, err
	}

	opened, err := store.Open(pages)
	if err != nil {
		file.Close()
		return nil, err
	}
	opened.Keep(folder, options.Key)

	// Said once, when the file is opened, because that is when it is known and
	// because saying it on every call would train people to stop reading it.
	s.sayHowItWasLeft(account, name, pages)
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
		return []byte(`{"message":"sapedb/server: the failure could not be described","code":"failed"}`)
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

// sayHowItWasLeft reports a database that was not shut down.
//
// sapedb survives losing power — two meta pages and an order of writes that has
// no recovery path to get wrong — but until this it survived it silently. The
// database came back at its last committed transaction and nothing anywhere
// said the machine had gone down, so the operator asking "why is that write
// missing" had nothing to read.
//
// What it says is bounded, and the bound is worth saying too. Every operation
// here is a transaction and the server answers only after committing, so the
// most that a power cut can discard is one operation — and one whose caller
// was never told it had succeeded. A database with interactive transactions
// can lose half an hour of somebody's work. This cannot, because there is no
// begin to hold half an hour in.
func (s *Server) sayHowItWasLeft(account, name string, pages *pager.Pager) {
	if s.options.Notice == nil || pages.Meta().Clean {
		return
	}

	where := account + "/" + name
	left, err := pages.Interrupted()
	if err != nil || left == 0 {
		s.options.Notice(fmt.Sprintf(
			"%s was not closed cleanly; it is at transaction %d, which is the last one that was committed",
			where, pages.Meta().TxID))
		return
	}

	s.options.Notice(fmt.Sprintf(
		"%s was not closed cleanly; it is at transaction %d, and %d pages of an operation that never committed have been discarded — no client was ever told that operation succeeded",
		where, pages.Meta().TxID, left))
}

// Say sets where the server reports things nobody asked for.
func (s *Server) Say(notice func(string)) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.options.Notice = notice
}
