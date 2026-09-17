// Package signing carries the one contract this store shares with the app:
// the signature in an rsql:// connection string.
//
// The app side is @ecosy/rsql/signer. Both read fixtures/signing.json, so a
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
const DefaultLabel = "ecosy/rsql:connection:v1"

// Direct signs with the secret itself, the plainer form — the app passes
// label: null for it.
const Direct = ""

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
	ErrEmptySecret  = errors.New("rsql: secret must not be empty")
	ErrPassword     = errors.New("rsql: password must be 16-128 characters of A-Z a-z 0-9 . _ ~ -")
	ErrFieldDelim   = errors.New(`rsql: account_id and dbname must not be empty or contain ":"`)
	ErrBadSignature = errors.New("rsql: signature does not verify")
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
	if label == Direct {
		return []byte(secret)
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
