// Package ulid makes identifiers that sort by the time they were made.
//
// A primary key decides the order documents are stored in, so a random key
// scatters every insert across the file while a time-ordered one appends. That
// is the whole reason this is the default: 48 bits of millisecond timestamp
// followed by 80 bits of randomness, written in Crockford's base32, which
// keeps the byte order and the time order the same.
//
// Two identifiers made in the same millisecond still have to be ordered, so
// within a millisecond the random part is incremented rather than redrawn.
// Without that, the order of two documents written in the same millisecond
// would be whatever the random draw happened to be.
package ulid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Size is the length of a ULID written out.
const Size = 26

// alphabet is Crockford's base32: no I, L, O or U, so an identifier read aloud
// or typed back in cannot turn into a different one.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrExhausted is the once-in-a-lifetime case where a single millisecond used
// up all 2^80 identifiers. Reported rather than wrapped around: wrapping would
// hand out an identifier that sorts before one already given away.
var ErrExhausted = errors.New("rsql/ulid: this millisecond has no identifiers left")

// Source hands out identifiers. The zero value is not usable; use New.
type Source struct {
	// Now and Random are here so that a test can hold the clock still and see
	// what happens inside one millisecond, which is where the interesting
	// behaviour is.
	now    func() time.Time
	random io.Reader

	mutex sync.Mutex
	last  uint64
	seed  [10]byte
}

// New is a source that reads the real clock and the system's randomness.
func New() *Source {
	return &Source{now: time.Now, random: rand.Reader}
}

// With is a source driven by a clock and a reader of your choosing.
func With(now func() time.Time, random io.Reader) *Source {
	return &Source{now: now, random: random}
}

// Next is the next identifier.
func (s *Source) Next() (string, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	stamp := uint64(s.now().UnixMilli())
	if stamp>>48 != 0 {
		return "", fmt.Errorf("rsql/ulid: the clock is past what 48 bits hold: %d", stamp)
	}

	switch {
	case stamp > s.last:
		if _, err := io.ReadFull(s.random, s.seed[:]); err != nil {
			return "", fmt.Errorf("rsql/ulid: no randomness: %w", err)
		}
		s.last = stamp

	default:
		// The same millisecond, or a clock that went backwards. Either way the
		// identifier must still sort after the one before it, so the previous
		// timestamp is kept and the random part is stepped.
		if err := increment(&s.seed); err != nil {
			return "", err
		}
		stamp = s.last
	}

	return write(stamp, s.seed), nil
}

// MustNext is Next for callers that have nowhere to put an error. It panics
// only if randomness itself has failed, which is not a condition to continue
// writing a database through.
func (s *Source) MustNext() string {
	id, err := s.Next()
	if err != nil {
		panic(err)
	}
	return id
}

func increment(seed *[10]byte) error {
	for i := len(seed) - 1; i >= 0; i-- {
		seed[i]++
		if seed[i] != 0 {
			return nil
		}
	}
	return ErrExhausted
}

// write lays the 128 bits out as 26 base32 characters, most significant first.
func write(stamp uint64, seed [10]byte) string {
	var raw [16]byte
	for i := 0; i < 6; i++ {
		raw[i] = byte(stamp >> (40 - 8*i))
	}
	copy(raw[6:], seed[:])

	out := make([]byte, Size)
	// The first character carries the top 2 bits; the remaining 25 carry 5
	// bits each, which is 2 + 125 = 127... so the top character holds 3 bits
	// and the value can never use its highest two, exactly as the format says.
	bits := uint16(0)
	held := 0
	at := Size - 1
	for i := len(raw) - 1; i >= 0; i-- {
		bits |= uint16(raw[i]) << held
		held += 8
		for held >= 5 {
			out[at] = alphabet[bits&0x1f]
			at--
			bits >>= 5
			held -= 5
		}
	}
	if at >= 0 {
		out[at] = alphabet[bits&0x1f]
		at--
	}
	for ; at >= 0; at-- {
		out[at] = alphabet[0]
	}
	return string(out)
}

// Time is when an identifier was made, for a caller that wants to know without
// keeping a separate field for it.
func Time(id string) (time.Time, error) {
	if len(id) != Size {
		return time.Time{}, fmt.Errorf("rsql/ulid: %q is %d characters, want %d", id, len(id), Size)
	}

	// Twenty-six characters hold 130 bits and a ULID is 128, so the stream
	// starts with two bits that are always zero. The first ten characters are
	// those two bits followed by the whole 48-bit timestamp.
	stamp := uint64(0)
	for i := 0; i < 10; i++ {
		value := index(id[i])
		if value < 0 {
			return time.Time{}, fmt.Errorf("rsql/ulid: %q is not base32 at %d", id, i)
		}
		stamp = stamp<<5 | uint64(value)
	}
	return time.UnixMilli(int64(stamp & (1<<48 - 1))).UTC(), nil
}

func index(character byte) int {
	for i := 0; i < len(alphabet); i++ {
		if alphabet[i] == character {
			return i
		}
	}
	return -1
}
