// Package connection reads the string that says where a database is and
// proves who said so.
//
//	sapedb://account:password@host:port/dbname?sig=hex
//
// The signature covers the account, the password and the database name
// together, so the string is a whole thing: no part of it can be changed —
// pointed at another database, another account — without the signature
// failing. What it does not cover is the host, deliberately. The same
// credentials have to work against a primary, a replica and a laptop, and
// signing the host would mean reissuing every string to move a server.
//
// This is the Go counterpart of the TypeScript parser, and both are checked
// against fixtures/signing.json so that a string one side writes is one the
// other reads.
package connection

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/sapedb/sapedb/internal/signing"
)

// Scheme is what a connection string starts with, and DefaultPort is what it
// means when it names none.
const (
	Scheme      = "sapedb"
	DefaultPort = 7433
)

var (
	ErrScheme    = errors.New("sapedb/connection: that is not an sapedb:// string")
	ErrField     = errors.New("sapedb/connection: the connection string is missing something")
	ErrPort      = errors.New("sapedb/connection: that is not a port")
	ErrSignature = errors.New("sapedb/connection: the connection string is not signed for this")
)

// Connection is a connection string taken apart.
type Connection struct {
	Account   string
	Password  string
	Host      string
	Port      int
	DBName    string
	Signature string
}

// Parse takes a connection string apart, naming whichever field is at fault.
//
// Naming the field matters more than it looks: these strings are pasted into
// configuration by people who cannot see them — they are secrets — so "invalid
// connection string" leaves somebody staring at a line they dare not print.
func Parse(text string) (Connection, error) {
	parsed, err := url.Parse(text)
	if err != nil {
		return Connection{}, fmt.Errorf("%w: %v", ErrScheme, err)
	}
	if parsed.Scheme != Scheme {
		return Connection{}, fmt.Errorf("%w: it begins %q", ErrScheme, parsed.Scheme)
	}

	made := Connection{
		Host:      parsed.Hostname(),
		Port:      DefaultPort,
		DBName:    strings.TrimPrefix(parsed.Path, "/"),
		Signature: parsed.Query().Get("sig"),
	}

	if parsed.User != nil {
		made.Account = parsed.User.Username()
		made.Password, _ = parsed.User.Password()
	}

	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return Connection{}, fmt.Errorf("%w: %q", ErrPort, port)
		}
		made.Port = number
	}

	for name, value := range map[string]string{
		"account":  made.Account,
		"password": made.Password,
		"host":     made.Host,
		"dbname":   made.DBName,
		"sig":      made.Signature,
	} {
		if value == "" {
			return Connection{}, fmt.Errorf("%w: no %s", ErrField, name)
		}
	}
	if strings.Contains(made.DBName, "/") {
		return Connection{}, fmt.Errorf("%w: the database name holds a %q", ErrField, "/")
	}

	return made, nil
}

// String writes a connection string back out.
func (c Connection) String() string {
	return fmt.Sprintf("%s://%s:%s@%s:%d/%s?sig=%s",
		Scheme, c.Account, c.Password, c.Host, c.Port, c.DBName, c.Signature)
}

// Redact is the same string with the password and signature taken out, for
// anywhere it might be written down — a log, an error, a screen somebody else
// can see.
func (c Connection) Redact() string {
	return fmt.Sprintf("%s://%s:***@%s:%d/%s?sig=***", Scheme, c.Account, c.Host, c.Port, c.DBName)
}

// Verify says whether this connection string was signed with a secret.
func (c Connection) Verify(secret, label string) error {
	parts := signing.Parts{AccountID: c.Account, Password: c.Password, DBName: c.DBName}
	if !signing.Verify(c.Signature, parts, secret, label) {
		return fmt.Errorf("%w: %s", ErrSignature, c.Redact())
	}
	return nil
}

// VerifyString parses and verifies in one step, for a caller that only wants
// the answer.
func VerifyString(text, secret, label string) error {
	parsed, err := Parse(text)
	if err != nil {
		return err
	}
	return parsed.Verify(secret, label)
}
