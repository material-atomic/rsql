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
	"crypto/sha256"
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
	ErrHandshake = errors.New("rsql/server: the connection did not open properly")
	ErrName      = errors.New("rsql/server: that is not a usable account or database name")
	ErrClosed    = errors.New("rsql/server: the server is closed")
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

	db, opened, err := s.handshake(reader, conn)
	if err != nil {
		// The client is told why, then the connection ends: a handshake that
		// failed must not leave a socket that looks usable.
		_ = write(conn, protocol.Frame{Type: protocol.Failure, ID: 0}, failure(err))
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
			if err := write(conn, protocol.Frame{Type: protocol.Pong, ID: frame.ID}, nil); err != nil {
				return err
			}

		case protocol.Goodbye:
			return nil

		case protocol.Invoke:
			result, err := s.invoke(db, opened, frame.Payload)
			if err != nil {
				if err := write(conn, protocol.Frame{Type: protocol.Failure, ID: frame.ID}, failure(err)); err != nil {
					return err
				}
				continue
			}
			if err := write(conn, protocol.Frame{Type: protocol.Result, ID: frame.ID}, result); err != nil {
				return err
			}

		default:
			// A frame this version does not serve is refused by name rather
			// than ignored: a client waiting for an answer that never comes is
			// worse off than one that is told no.
			body := failure(fmt.Errorf("rsql/server: nothing here serves a %s frame", frame.Type))
			if err := write(conn, protocol.Frame{Type: protocol.Failure, ID: frame.ID}, body); err != nil {
				return err
			}
		}
	}
}

// hello is what a client opens with.
type hello struct {
	Account   string `json:"account"`
	Password  string `json:"password"`
	DBName    string `json:"dbname"`
	Signature string `json:"sig"`
}

// welcome is what it gets back.
type welcome struct {
	Version   uint8  `json:"version"`
	Account   string `json:"account"`
	DBName    string `json:"dbname"`
	LSN       uint64 `json:"lsn"`
	Encrypted bool   `json:"encrypted"`
}

func (s *Server) handshake(reader *protocol.Reader, conn io.Writer) (*database, hello, error) {
	frame, err := reader.Read()
	if err != nil {
		return nil, hello{}, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	if frame.Type != protocol.Hello {
		return nil, hello{}, fmt.Errorf("%w: it began with a %s frame", ErrHandshake, frame.Type)
	}

	opening := hello{}
	if err := json.Unmarshal(frame.Payload, &opening); err != nil {
		return nil, hello{}, fmt.Errorf("%w: %v", ErrHandshake, err)
	}

	// The signature is checked before anything is opened or created. A name
	// nobody signed for must not so much as cause a file to appear.
	parts := signing.Parts{
		AccountID: opening.Account,
		Password:  opening.Password,
		DBName:    opening.DBName,
	}
	if !signing.Verify(opening.Signature, parts, s.options.Secret, s.options.Label) {
		return nil, hello{}, fmt.Errorf("%w: %v", ErrHandshake, signing.ErrBadSignature)
	}

	db, err := s.database(opening.Account, opening.DBName)
	if err != nil {
		return nil, hello{}, err
	}

	db.mutex.Lock()
	lsn, err := db.store.LatestLSN()
	db.mutex.Unlock()
	if err != nil {
		return nil, hello{}, err
	}

	body, err := json.Marshal(welcome{
		Version: protocol.Version, Account: opening.Account, DBName: opening.DBName,
		LSN: lsn, Encrypted: s.options.Encrypt,
	})
	if err != nil {
		return nil, hello{}, err
	}
	if err := write(conn, protocol.Frame{Type: protocol.Welcome, ID: frame.ID}, body); err != nil {
		return nil, hello{}, err
	}
	return db, opening, nil
}

// call is one invocation on the wire.
type call struct {
	Operation string         `json:"op"`
	Version   int            `json:"version,omitempty"`
	Arguments map[string]any `json:"args,omitempty"`
	WriteID   string         `json:"write_id,omitempty"`
}

func (s *Server) invoke(db *database, opened hello, payload []byte) ([]byte, error) {
	asked := call{}
	if err := json.Unmarshal(payload, &asked); err != nil {
		return nil, fmt.Errorf("rsql/server: the call does not read as one: %w", err)
	}

	// One writer at a time per database, which is what the engine underneath
	// allows. Connections to one database queue here rather than racing.
	db.mutex.Lock()
	defer db.mutex.Unlock()

	// Scopes are not taken from the request: a caller that names its own
	// permissions has none. Until a token carries them, an operation that
	// declares a scope cannot be reached over the wire at all — which is the
	// safe direction to be incomplete in.
	caller := store.Caller{Actor: opened.Account, WriteID: asked.WriteID}

	result, err := db.store.Invoke(caller, asked.Operation, asked.Version, asked.Arguments)
	if err != nil {
		return nil, err
	}
	if result.Changed > 0 || result.Repeated > 0 {
		if err := db.store.Commit(); err != nil {
			return nil, err
		}
	}
	return json.Marshal(result)
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
	return &database{pages: pages, store: opened, file: file}, nil
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
func failure(err error) []byte {
	body, marshalled := json.Marshal(struct {
		Error string `json:"error"`
		At    int64  `json:"at"`
	}{Error: err.Error(), At: time.Now().UnixMilli()})
	if marshalled != nil {
		return []byte(`{"error":"rsql/server: the failure could not be described"}`)
	}
	return body
}
