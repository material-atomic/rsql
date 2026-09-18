package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/material-atomic/rsql/internal/protocol"
	"github.com/material-atomic/rsql/internal/store"
)

const secret = "the secret only the control plane has"

func env(pairs map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := pairs[name]
		return value, found
	}
}

// A server that will not verify anything, or will quietly serve a database in
// the clear, is a server that should not start.
func TestAServerRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	for name, want := range map[string]struct {
		vars map[string]string
		err  error
	}{
		"no secret": {
			vars: map[string]string{"RSQL_INSECURE": "1"},
			err:  ErrNoSecret,
		},
		"no TLS and nobody said so": {
			vars: map[string]string{"RSQL_SECRET": secret},
			err:  ErrNoTLS,
		},
		"a certificate with no key": {
			vars: map[string]string{"RSQL_SECRET": secret, "RSQL_TLS_CERT": "/tmp/cert.pem"},
			err:  ErrHalfTLS,
		},
		"a key with no certificate": {
			vars: map[string]string{"RSQL_SECRET": secret, "RSQL_TLS_KEY": "/tmp/key.pem"},
			err:  ErrHalfTLS,
		},
		"a shutdown wait that is not a time": {
			vars: map[string]string{"RSQL_SECRET": secret, "RSQL_INSECURE": "1", "RSQL_SHUTDOWN": "soonish"},
			err:  ErrNotDuration,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := FromEnv(env(want.vars)); !errors.Is(err, want.err) {
				t.Errorf("want %v, got %v", want.err, err)
			}
		})
	}

	// And serving in the clear is possible — it just has to be said out loud.
	config, err := FromEnv(env(map[string]string{"RSQL_SECRET": secret, "RSQL_INSECURE": "1"}))
	if err != nil {
		t.Fatalf("a server told to serve without TLS: %v", err)
	}
	if config.Address != DefaultAddress || config.Dir != DefaultDir {
		t.Errorf("the defaults read %+v", config)
	}
	if config.Shutdown != 20*time.Second {
		t.Errorf("the shutdown wait is %s", config.Shutdown)
	}
}

func TestTheEnvironmentIsReadAsWritten(t *testing.T) {
	config, err := FromEnv(env(map[string]string{
		"RSQL_SECRET":   secret,
		"RSQL_ADDR":     "127.0.0.1:9999",
		"RSQL_DIR":      "/data/here",
		"RSQL_LABEL":    "another/label",
		"RSQL_INSECURE": "yes",
		"RSQL_ENCRYPT":  "true",
		"RSQL_SHUTDOWN": "90s",
	}))
	if err != nil {
		t.Fatal(err)
	}

	want := Config{
		Address: "127.0.0.1:9999", Dir: "/data/here", Secret: secret, Label: "another/label",
		Insecure: true, Encrypt: true, Shutdown: 90 * time.Second,
	}
	if config != want {
		t.Errorf("read %+v, want %+v", config, want)
	}

	// An empty variable is not a value: it means "not set", or a container with
	// RSQL_DIR= would put its databases in the current directory.
	blank, err := FromEnv(env(map[string]string{"RSQL_SECRET": secret, "RSQL_INSECURE": "1", "RSQL_DIR": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if blank.Dir != DefaultDir {
		t.Errorf("an empty RSQL_DIR became %q", blank.Dir)
	}
}

// selfSigned writes a certificate and key for localhost, so the TLS path is
// exercised rather than assumed.
func selfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certFile)
	if err != nil {
		t.Fatal(err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}

	marshalled, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: marshalled}); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// The whole thing, over TLS, the way it runs in a container: start, declare,
// connect, invoke, stop on a signal.
func TestItServesOverTLSAndStopsWhenTold(t *testing.T) {
	certFile, keyFile := selfSigned(t)

	service, err := Start(Config{
		Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret,
		CertFile: certFile, KeyFile: keyFile, Shutdown: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Declared from inside the process, the way a CLI on the same host would.
	db, release, err := service.Server().Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Spec{
		Name: "notes", Key: store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input:    []store.Parameter{{Name: "body", Type: store.TypeString, Required: true}},
		Document: map[string]store.Term{"body": {Arg: "body"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
	release()

	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	announced := &strings.Builder{}
	go func() { served <- service.Serve(ctx, announced) }()

	// A client that trusts the certificate we just made and nothing else.
	pool := x509.NewCertPool()
	pem, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("the certificate did not parse")
	}

	conn, err := tls.Dial("tcp", service.Address(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	signature, err := service.Server().Sign("acme", "a-password-of-the-right-shape", "main")
	if err != nil {
		t.Fatal(err)
	}
	send := func(id uint32, kind protocol.Type, body any) {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := protocol.Encode(protocol.Frame{Version: protocol.Version, Type: kind, ID: id, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	reader := protocol.NewReader(conn).Accept(protocol.Version)

	send(1, protocol.Hello, map[string]any{
		"account": "acme", "password": "a-password-of-the-right-shape", "dbname": "main", "sig": signature,
	})
	if frame, err := reader.Read(); err != nil || frame.Type != protocol.Welcome {
		t.Fatalf("handshake: %s %v", frame.Type, err)
	}

	send(2, protocol.Invoke, map[string]any{"command": "notes.add", "args": map[string]any{"body": "over tls"}})
	frame, err := reader.Read()
	if err != nil || frame.Type != protocol.Result {
		t.Fatalf("invoke: %s %v %s", frame.Type, err, frame.Payload)
	}

	if !strings.Contains(announced.String(), "rsql+tls") {
		t.Errorf("it announced %q", announced.String())
	}

	// A signal stops it, and Serve returns rather than hanging.
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("stopping: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("it did not stop when told")
	}

	// And the port is free again, which is what a supervisor restarting it
	// depends on.
	listener, err := net.Listen("tcp", service.Address())
	if err != nil {
		t.Errorf("the port is still held: %v", err)
	} else {
		listener.Close()
	}
}

// A plaintext client talking to a TLS listener must fail rather than half work.
func TestPlainTextIsNotServedWhenTLSIsOn(t *testing.T) {
	certFile, keyFile := selfSigned(t)

	service, err := Start(Config{
		Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret,
		CertFile: certFile, KeyFile: keyFile, Shutdown: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = service.Serve(ctx, nil) }()

	conn, err := net.Dial("tcp", service.Address())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	frame, err := protocol.Encode(protocol.Frame{
		Version: protocol.Version, Type: protocol.Hello, ID: 1, Payload: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(frame); err != nil {
		return // refused outright, which is an answer
	}

	reader := protocol.NewReader(conn).Accept(protocol.Version)
	if _, err := reader.Read(); err == nil {
		t.Error("a plaintext client was served by a TLS listener")
	}
}

func TestAPortAlreadyTakenIsReportedRatherThanIgnored(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if _, err := Start(Config{
		Address: held.Addr().String(), Dir: t.TempDir(), Secret: secret, Insecure: true,
	}); err == nil {
		t.Error("it started on a port somebody else holds")
	}
}

func TestRunReadsTheEnvironmentAndServes(t *testing.T) {
	dir := t.TempDir()
	ctx, stop := context.WithCancel(context.Background())

	ready := make(chan error, 1)
	go func() {
		ready <- Run(ctx, env(map[string]string{
			"RSQL_SECRET":   secret,
			"RSQL_ADDR":     "127.0.0.1:0",
			"RSQL_DIR":      dir,
			"RSQL_INSECURE": "1",
			"RSQL_SHUTDOWN": "1s",
		}), nil)
	}()

	// Give it a moment to bind, then stop it: what is being checked is that Run
	// gets as far as serving and comes back when the context ends.
	time.Sleep(200 * time.Millisecond)
	stop()

	select {
	case err := <-ready:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not come back")
	}

	// And a configuration it refuses comes back as an error rather than a
	// process that sits there doing nothing.
	if err := Run(context.Background(), env(map[string]string{}), nil); !errors.Is(err, ErrNoSecret) {
		t.Errorf("want ErrNoSecret, got %v", err)
	}
}

// FromEnv is not the only door. A Config built by hand — by a test, by an
// embedder, by a future command with flags — goes through Start, and the guard
// that refuses to serve a database in the clear has to be on that path too.
func TestStartRefusesAConfigItWouldNotHaveRead(t *testing.T) {
	for name, config := range map[string]Config{
		"no TLS and nobody said so": {Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret},
		"no secret":                 {Address: "127.0.0.1:0", Dir: t.TempDir(), Insecure: true},
		"half a TLS setup": {
			Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret, CertFile: "/tmp/nothing.pem",
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, err := Start(config)
			if err == nil {
				service.Close()
				t.Error("it started on a configuration FromEnv would have refused")
			}
		})
	}
}
