// Package signing carries the one contract this store shares with the app:
// the signature in a sapedb:// connection string.
//
// The app side is @ecosy/sapedb/signer. Both read fixtures/signing.json, so a
// change on either side turns both test suites red at once instead of arriving
// as a user who cannot connect.
package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DefaultLabel derives the signing key from the secret. Both sides must agree
// on the mode: this label, or Direct.
const DefaultLabel = "ecosy/sapedb:connection:v1"

// Direct signs with the secret itself, the plainer form — the app passes
// label: null for it.
//
// Its value is deliberately not the empty string. A blank configuration field
// must never select this: the two forms produce different signatures, so a
// server that fell back to it would refuse every string a client signed the
// ordinary way, and every test on one side would still pass because both sides
// of that side agree. That is exactly what happened here before this was a
// sentinel.
const Direct = "\x00sapedb:direct"

// PasswordPattern is what a password may be.
//
// Narrow on purpose. Unicode has more than one way to write the same password —
// macOS composes, Windows decomposes, copy-paste converts between them — so
// without this the same password would be two byte strings and two signatures,
// with nothing in a log to explain it. This charset has one encoding per
// password, so neither side normalises and neither side can normalise
// differently. It also excludes ":", which is what makes
// user_id:password:project_id unambiguous without length prefixing.
var PasswordPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,128}$`)

// Parts are the three fields of a connection string that a signature covers.
type Parts struct {
	// AccountID is the registered account — the tenant boundary. A database
	// name only has to be unique within one.
	AccountID string
	Password  string
	DBName    string
}

var (
	ErrEmptySecret  = errors.New("sapedb: secret must not be empty")
	ErrPassword     = errors.New("sapedb: password must be 16-128 characters of A-Z a-z 0-9 . _ ~ -")
	ErrFieldDelim   = errors.New(`sapedb: account_id and dbname must not be empty or contain ":"`)
	ErrBadSignature = errors.New("sapedb: signature does not verify")
)

// ValidPassword reports whether the protocol can carry this password.
func ValidPassword(password string) bool {
	return PasswordPattern.MatchString(password)
}

func field(value string) error {
	if value == "" || strings.Contains(value, ":") {
		return ErrFieldDelim
	}
	return nil
}

// Message is the exact bytes signed: account_id ":" password ":" dbname,
// UTF-8, unnormalised.
func Message(parts Parts) (string, error) {
	if err := field(parts.AccountID); err != nil {
		return "", fmt.Errorf("account_id: %w", err)
	}
	if err := field(parts.DBName); err != nil {
		return "", fmt.Errorf("dbname: %w", err)
	}
	if !ValidPassword(parts.Password) {
		return "", ErrPassword
	}
	return parts.AccountID + ":" + parts.Password + ":" + parts.DBName, nil
}

// key is what the signature is made with: derived under a label, or the secret
// itself when label is Direct.
func key(secret, label string) []byte {
	switch label {
	case Direct:
		return []byte(secret)
	case "":
		// Nothing said means the ordinary form, which is what every client
		// does by default. The dangerous answer is never the silent one.
		label = DefaultLabel
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// Sign returns the sig of a connection string: lower-case hex, 64 characters.
func Sign(parts Parts, secret, label string) (string, error) {
	if secret == "" {
		return "", ErrEmptySecret
	}
	message, err := Message(parts)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key(secret, label))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Verify reports whether signature is the one these parts have under this
// secret. Comparison is constant time; anything malformed is simply false, so
// a forged string is input rather than an error path of its own.
func Verify(signature string, parts Parts, secret, label string) bool {
	want, err := Sign(parts, secret, label)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
}

// Operating a database is a different permission from using one, and nothing
// in a connection string says which you hold.
//
// The string signs account, password and database name — what to reach, not
// what you may do with it. Adding a field for it would mean every existing
// string is reissued and both sides changed, and it would still be a claim
// the client makes about itself.
//
// So an operator proves something instead: possession of the server's own
// secret, over a nonce the server just chose. That is not a new power being
// handed out. Whoever can read the secret can already read the database files
// and mint any connection string they like; the shell only makes it
// comfortable, and — because every access it runs goes into the change log —
// visible afterwards, which reading the files is not.
//
// The nonce is what stops the proof being a password. One is good for one
// connection, so it cannot be copied out of a log or a process listing and
// used again.
const OperatorLabel = "sapedb/operator:v1"

// ErrNonce is a challenge that is not one: empty, or not the length the server
// issues. A proof over a nonce the client chose proves nothing.
var ErrNonce = errors.New("sapedb: the challenge is not one the server issued")

// NonceBytes is how long a challenge is.
const NonceBytes = 32

// Operating answers a challenge with proof that the secret is held.
func Operating(secret string, nonce []byte) (string, error) {
	if secret == "" {
		return "", ErrEmptySecret
	}
	if len(nonce) != NonceBytes {
		return "", ErrNonce
	}
	mac := hmac.New(sha256.New, key(secret, OperatorLabel))
	mac.Write(nonce)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Operates reports whether this is the answer to that challenge under this
// secret. Constant time, and anything malformed is false rather than an error
// path of its own.
//
// No test covers the constant time, and one that claimed to would be worse
// than none: a mutation replacing hmac.Equal with == passes everything here,
// because the difference is a timing signal and not an answer. Measuring it in
// a unit test would be measuring the machine the test runs on. It is written
// down here instead, which is the honest form of a property you can review but
// not assert.
func Operates(proof string, secret string, nonce []byte) bool {
	want, err := Operating(secret, nonce)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(strings.ToLower(proof)), []byte(want))
}
