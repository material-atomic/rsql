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

	"github.com/material-atomic/rsql/internal/server"
	"github.com/material-atomic/rsql/internal/signing"
)

// DefaultAddress is the port a connection string assumes when it names none.
const DefaultAddress = ":7433"

// DefaultDir is where a packaged server keeps databases.
const DefaultDir = "/var/lib/rsql"

var (
	ErrNoSecret    = errors.New("rsql: RSQL_SECRET is not set, so no connection could be verified")
	ErrNoTLS       = errors.New("rsql: refusing to serve without TLS")
	ErrHalfTLS     = errors.New("rsql: a TLS certificate needs its key, and a key needs its certificate")
	ErrNotDuration = errors.New("rsql: that is not a length of time")
)

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

	label := get("RSQL_LABEL", "")
	if strings.EqualFold(label, "direct") {
		// The plainer form, which has to be asked for by name: its sentinel is
		// deliberately not something a blank field can produce.
		label = signing.Direct
	}

	config := Config{
		Address:  get("RSQL_ADDR", DefaultAddress),
		Dir:      get("RSQL_DIR", DefaultDir),
		Secret:   get("RSQL_SECRET", ""),
		Label:    label,
		CertFile: get("RSQL_TLS_CERT", ""),
		KeyFile:  get("RSQL_TLS_KEY", ""),
		Insecure: flag("RSQL_INSECURE"),
		Encrypt:  flag("RSQL_ENCRYPT"),
		Shutdown: 20 * time.Second,
	}

	if wait := get("RSQL_SHUTDOWN", ""); wait != "" {
		parsed, err := time.ParseDuration(wait)
		if err != nil {
			return Config{}, fmt.Errorf("%w: RSQL_SHUTDOWN=%q", ErrNotDuration, wait)
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
		return fmt.Errorf("%w: set RSQL_TLS_CERT and RSQL_TLS_KEY, or RSQL_INSECURE=1 to say you meant it", ErrNoTLS)
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
			return nil, fmt.Errorf("rsql: reading the certificate: %w", err)
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
		s.server.Say(func(line string) { fmt.Fprintf(announce, "rsql: %s\n", line) })
	}

	if announce != nil {
		scheme := "rsql+tls"
		if s.config.CertFile == "" {
			scheme = "rsql (no TLS)"
		}
		fmt.Fprintf(announce, "rsql listening on %s as %s, databases in %s\n",
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
			fmt.Fprintf(announce, "rsql stopping, %s for connections to finish\n", s.config.Shutdown)
		}
		s.listener.Close()

		select {
		case <-done:
		case <-time.After(s.config.Shutdown):
			if announce != nil {
				fmt.Fprintln(announce, "rsql: connections did not finish in time; closing anyway")
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
