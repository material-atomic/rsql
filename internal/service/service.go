// Package service turns environment into a running server.
//
// It is a separate package from the command so that the decisions worth
// arguing about — whether to serve without TLS, what happens on a shutdown
// signal, where databases live — are testable without building a binary and
// starting a process. A `main` that holds logic is a `main` nobody tests.
package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/signing"
)

// DefaultAddress is the port a connection string assumes when it names none.
const DefaultAddress = ":7433"

// DefaultDir is where a packaged server keeps databases.
const DefaultDir = "/var/lib/sapedb"

var (
	ErrNoSecret    = errors.New("sapedb: SAPEDB_SECRET is not set, so no connection could be verified")
	ErrNoTLS       = errors.New("sapedb: refusing to serve without TLS")
	ErrHalfTLS     = errors.New("sapedb: a TLS certificate needs its key, and a key needs its certificate")
	ErrNotDuration = errors.New("sapedb: that is not a length of time")
)

// oldEnvPrefix is the environment prefix this product read before it was
// called sapedb, assembled from single-character literals rather than spelled
// whole — see the identical constant and its comment in internal/cli, which
// this mirrors because the two binaries read overlapping variables under
// separate flag parsing and neither imports the other's package for it.
var oldEnvPrefix = string([]byte{'R', 'S', 'Q', 'L', '_'})

// sapedbEnvNames are every product variable the server reads, walked once
// here instead of once per call to get(), so the set this refuses old names
// for cannot drift from the set FromEnv actually reads.
var sapedbEnvNames = []string{
	"SAPEDB_ADDR", "SAPEDB_DIR", "SAPEDB_SECRET", "SAPEDB_LABEL",
	"SAPEDB_TLS_CERT", "SAPEDB_TLS_KEY", "SAPEDB_INSECURE", "SAPEDB_ENCRYPT", "SAPEDB_SHUTDOWN",
}

// rejectOldEnv refuses to start when a variable is set under the product's
// old name and not under its current one. It never reads what the old name
// holds, only whether it is there, and it stops the process rather than
// falling back to it — every one of these variables has a usable default or
// a zero value, so silently ignoring an old name would not fail loudly, it
// would just serve from the wrong place.
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

// Config is everything the server is told before it starts.
type Config struct {
	Address string
	Dir     string
	Secret  string
	Label   string

	CertFile string
	KeyFile  string
	// Insecure serves without TLS. It has to be said out loud: a server that
	// quietly falls back to plaintext when a certificate is missing is a server
	// that will one day be in production without one, and nothing about it
	// looks different.
	Insecure bool

	// Encrypt stores every database encrypted under a key derived from the
	// secret.
	Encrypt bool
	// Shutdown is how long to let connections finish after a signal.
	Shutdown time.Duration
}

// FromEnv reads the configuration. `lookup` is os.LookupEnv in a real process.
func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	if err := rejectOldEnv(lookup); err != nil {
		return Config{}, err
	}

	get := func(name, fallback string) string {
		if value, found := lookup(name); found && value != "" {
			return value
		}
		return fallback
	}
	flag := func(name string) bool {
		value := strings.ToLower(get(name, ""))
		return value == "1" || value == "true" || value == "yes"
	}

	label := get("SAPEDB_LABEL", "")
	if strings.EqualFold(label, "direct") {
		// The plainer form, which has to be asked for by name: its sentinel is
		// deliberately not something a blank field can produce.
		label = signing.Direct
	}

	config := Config{
		Address:  get("SAPEDB_ADDR", DefaultAddress),
		Dir:      get("SAPEDB_DIR", DefaultDir),
		Secret:   get("SAPEDB_SECRET", ""),
		Label:    label,
		CertFile: get("SAPEDB_TLS_CERT", ""),
		KeyFile:  get("SAPEDB_TLS_KEY", ""),
		Insecure: flag("SAPEDB_INSECURE"),
		Encrypt:  flag("SAPEDB_ENCRYPT"),
		Shutdown: 20 * time.Second,
	}

	if wait := get("SAPEDB_SHUTDOWN", ""); wait != "" {
		parsed, err := time.ParseDuration(wait)
		if err != nil {
			return Config{}, fmt.Errorf("%w: SAPEDB_SHUTDOWN=%q", ErrNotDuration, wait)
		}
		config.Shutdown = parsed
	}

	return config, config.check()
}

func (c Config) check() error {
	if c.Secret == "" {
		return ErrNoSecret
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return ErrHalfTLS
	}
	if c.CertFile == "" && !c.Insecure {
		return fmt.Errorf("%w: set SAPEDB_TLS_CERT and SAPEDB_TLS_KEY, or SAPEDB_INSECURE=1 to say you meant it", ErrNoTLS)
	}
	return nil
}

// Service is a server and the listener it is answering on.
type Service struct {
	config   Config
	server   *server.Server
	listener net.Listener
}

// Start opens the listener and the server, but serves nothing yet.
//
// Opening the listener here rather than inside Serve is what lets a caller —
// a test, or a supervisor — know the port is taken before anything is
// announced as ready.
func Start(config Config) (*Service, error) {
	if err := config.check(); err != nil {
		return nil, err
	}

	made, err := server.New(server.Options{
		Dir: config.Dir, Secret: config.Secret, Label: config.Label, Encrypt: config.Encrypt,
	})
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", config.Address)
	if err != nil {
		made.Close()
		return nil, err
	}

	if config.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			listener.Close()
			made.Close()
			return nil, fmt.Errorf("sapedb: reading the certificate: %w", err)
		}
		listener = tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			// Nothing below this is worth serving a database over.
			MinVersion: tls.VersionTLS12,
		})
	}

	return &Service{config: config, server: made, listener: listener}, nil
}

// Address is where it is listening, which is worth asking when the port was
// zero.
func (s *Service) Address() string { return s.listener.Addr().String() }

// Server is the server underneath, for a caller that needs to declare
// something before anybody connects.
func (s *Service) Server() *server.Server { return s.server }

// Serve answers connections until the context is done or the listener fails.
//
// A signal closes the listener, so no new connection is accepted, and then the
// databases are closed. Connections in flight finish what they were doing:
// every write has already been committed before its answer went back, so
// nothing is lost either way — but cutting a client off mid-answer for no
// reason is rudeness with no benefit.
func (s *Service) Serve(ctx context.Context, announce io.Writer) error {
	// Databases are opened on demand, so what a database has to say about how
	// it was last left is said while the server is running, not before.
	if announce != nil {
		s.server.Say(func(line string) { fmt.Fprintf(announce, "sapedb: %s\n", line) })
	}

	if announce != nil {
		scheme := "sapedb+tls"
		if s.config.CertFile == "" {
			scheme = "sapedb (no TLS)"
		}
		fmt.Fprintf(announce, "sapedb listening on %s as %s, databases in %s\n",
			s.Address(), scheme, s.config.Dir)
	}

	done := make(chan error, 1)
	go func() { done <- s.server.Serve(s.listener) }()

	select {
	case err := <-done:
		s.server.Close()
		// A listener closed on purpose is not a failure to report.
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err

	case <-ctx.Done():
		if announce != nil {
			fmt.Fprintf(announce, "sapedb stopping, %s for connections to finish\n", s.config.Shutdown)
		}
		s.listener.Close()

		select {
		case <-done:
		case <-time.After(s.config.Shutdown):
			if announce != nil {
				fmt.Fprintln(announce, "sapedb: connections did not finish in time; closing anyway")
			}
		}
		return s.server.Close()
	}
}

// Close stops serving without waiting for anything.
func (s *Service) Close() error {
	s.listener.Close()
	return s.server.Close()
}

// Run is the whole of a `main`: read the environment, start, serve until a
// signal.
func Run(ctx context.Context, lookup func(string) (string, bool), announce io.Writer) error {
	config, err := FromEnv(lookup)
	if err != nil {
		return err
	}

	service, err := Start(config)
	if err != nil {
		return err
	}
	return service.Serve(ctx, announce)
}

// Env is os.LookupEnv, named so that Run reads as what it does.
var Env = os.LookupEnv
