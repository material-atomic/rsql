package pager

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Encryption at rest, one key per database.
//
// What is encrypted is the payload of every page that holds data: the leaves,
// the branches, the chains of large values, the free list. What is not is the
// meta page, and that is a deliberate, stated trade rather than an oversight.
//
// Something has to be readable without the key, or "this is not our file" and
// "this is our file and your key is wrong" become the same answer, and anybody
// holding a database they cannot open has no way to tell which mistake they
// made. So the meta page keeps its magic, its format, the salt the key was
// derived with, and a value that can only be opened by the right key. Opening
// a database therefore says immediately whether the key is right, before a
// single document is touched.
//
// The cost of that is what the meta page leaks: how many pages the file has,
// how many transactions it has seen, and where its root is. Not documents, not
// keys, not the names of anything — but it is a real leak and it is written
// down here rather than discovered later.
//
// Each page carries its own nonce, drawn fresh on every write. The alternative
// — deriving a nonce from the page number and the transaction — looks tidier
// and is wrong here: this engine writes the same page more than once inside a
// transaction, and a repeated nonce under one key is the failure that takes
// AES-GCM apart completely.
//
// The page number and kind go in as additional data rather than as plaintext
// to be trusted. A whole page moved to another position is then refused by the
// cipher itself, not merely by a checksum somebody could recompute.
const (
	// KeyBytes is the size of the key derived for pages.
	KeyBytes = 32
	// NonceBytes and TagBytes are what AES-GCM uses.
	NonceBytes = 12
	TagBytes   = 16
	// SaltBytes is the per-database salt the page key is derived with, so that
	// one passphrase used for two databases does not give them one key.
	SaltBytes = 16
)

// The labels that separate one derived key from another. A key derived for one
// purpose must never be usable for a different one.
const (
	pageLabel  = "rsql/pager:page:v1"
	checkLabel = "rsql/pager:key-check:v1"
)

var (
	// ErrKey is a database that is encrypted and was opened with the wrong key,
	// or with none.
	ErrKey = errors.New("rsql/pager: this database is encrypted and the key does not open it")
	// ErrNotEncrypted is a key offered to a database that has none.
	ErrNotEncrypted = errors.New("rsql/pager: this database is not encrypted")
	// ErrDecrypt is a page that did not authenticate, on a database whose key
	// is known to be right — so the page was changed, not the key.
	ErrDecrypt = errors.New("rsql/pager: a page did not decrypt")
)

// Options are how a database is created or opened.
type Options struct {
	// MaxPages caps the file. Zero is no limit.
	MaxPages uint64
	// Key is the secret this database is encrypted with. Empty means no
	// encryption; the secret may be any length, and the key actually used on
	// pages is derived from it and the database's own salt.
	Key []byte
}

// crypt is the cipher of an open database, or nil when there is none.
type crypt struct {
	aead cipher.AEAD
	salt [SaltBytes]byte
}

// newCrypt derives the page key from a secret and a salt.
func newCrypt(secret, salt []byte) (*crypt, error) {
	derived, err := hkdf.Key(sha256.New, secret, salt, pageLabel, KeyBytes)
	if err != nil {
		return nil, fmt.Errorf("rsql/pager: deriving the page key: %w", err)
	}

	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	made := &crypt{aead: aead}
	copy(made.salt[:], salt)
	return made, nil
}

// encrypt turns the payload of a sealed page into ciphertext in place.
//
// Called after seal, so the checksum is over the plaintext and the nonce and
// tag are still zero while it is computed. Reading undoes both in the opposite
// order, which is what keeps one checksum true in both directions.
func (c *crypt) encrypt(data []byte) error {
	nonce := data[offNonce : offNonce+NonceBytes]
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("rsql/pager: no randomness for a page nonce: %w", err)
	}

	payload := data[HeaderBytes:]

	// Sealed into a buffer of its own rather than over the payload. Seal wants
	// room for the tag as well as the ciphertext, and a page has none: writing
	// into `payload[:0]` makes Go grow the slice into a NEW array, leaving the
	// page itself in plaintext and the ciphertext somewhere nobody writes to
	// disk. That is a bug with no symptom except the file being readable.
	sealed := c.aead.Seal(nil, nonce, payload, data[:offNonce])

	copy(payload, sealed[:len(payload)])
	copy(data[offTag:offTag+TagBytes], sealed[len(payload):])
	return nil
}

// decrypt puts the plaintext back, and clears the nonce and tag so that the
// checksum can be checked over exactly what was hashed before encryption.
func (c *crypt) decrypt(data []byte, id uint64) error {
	nonce := data[offNonce : offNonce+NonceBytes]
	payload := data[HeaderBytes:]

	sealed := make([]byte, 0, len(payload)+TagBytes)
	sealed = append(sealed, payload...)
	sealed = append(sealed, data[offTag:offTag+TagBytes]...)

	plain, err := c.aead.Open(nil, nonce, sealed, data[:offNonce])
	if err != nil {
		return fmt.Errorf("%w: page %d", ErrDecrypt, id)
	}
	copy(payload, plain)

	for i := offNonce; i < offTag+TagBytes; i++ {
		data[i] = 0
	}
	return nil
}

// makeCheck writes the value that says whether a key is the right one.
//
// It is a sealed empty message: opening it needs the key and proves nothing
// else. Without it, a wrong key would first be noticed when a page failed to
// decrypt, which is both later and indistinguishable from a damaged file.
func makeCheck(secret, salt []byte, into []byte) error {
	aead, err := checkCipher(secret, salt)
	if err != nil {
		return err
	}

	nonce := into[:NonceBytes]
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("rsql/pager: no randomness for the key check: %w", err)
	}

	tag := aead.Seal(nil, nonce, nil, salt)
	copy(into[NonceBytes:NonceBytes+TagBytes], tag)
	return nil
}

// openCheck says whether a secret is the one a database was made with.
func openCheck(secret, salt []byte, stored []byte) error {
	aead, err := checkCipher(secret, salt)
	if err != nil {
		return err
	}

	nonce := stored[:NonceBytes]
	tag := stored[NonceBytes : NonceBytes+TagBytes]
	if _, err := aead.Open(nil, nonce, tag, salt); err != nil {
		return ErrKey
	}
	return nil
}

func checkCipher(secret, salt []byte) (cipher.AEAD, error) {
	derived, err := hkdf.Key(sha256.New, secret, salt, checkLabel, KeyBytes)
	if err != nil {
		return nil, fmt.Errorf("rsql/pager: deriving the check key: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
