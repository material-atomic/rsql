// Package keys turns values into bytes that sort the way the values do.
//
// The tree knows nothing but byte order. An index over a number field is
// therefore only as correct as the encoding beneath it: if -1 does not sort
// before 2 as bytes, every range query over that index is quietly wrong, and
// nothing about the query says so. The same goes for a composite key, where
// ["a", "b"] must never encode to the same bytes as ["ab", ""] — an ambiguity
// there is two different documents sharing one index entry.
//
// So the rules are:
//
//   - Every value carries a tag, and the tags are ordered. Values of different
//     types therefore still have one definite order rather than whichever one
//     the comparison happened to produce.
//   - Numbers use IEEE-754 bits with the sign folded in, which puts negatives,
//     zero and positives in the order arithmetic gives them.
//   - Strings are terminated, and a zero byte inside one is escaped, so that
//     the end of a component can never be mistaken for a byte of its contents.
//   - A descending field is the same encoding with every byte complemented,
//     which reverses the order exactly and keeps it self-terminating.
//
// A missing field is a value in its own right: an index says whether documents
// without the field sort first, sort last, or are not indexed at all. Leaving
// that to chance is how "where x > 5" starts returning documents with no x.
package keys

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Tags, in the order they sort. The gaps are deliberate: a type added later
// can be given a tag that puts it where it belongs rather than at the end.
const (
	tagMissingFirst = 0x01
	tagNull         = 0x10
	tagFalse        = 0x20
	tagTrue         = 0x21
	tagNumber       = 0x30
	tagString       = 0x40
	tagMissingLast  = 0xfe
)

// String components end with these two bytes, and a zero byte inside the
// string is written as 0x00 0xff. The terminator is therefore smaller than any
// escaped byte, which is what makes "a" sort before "a\x00".
const (
	escape     = 0x00
	escaped    = 0xff
	terminator = 0x01
)

// Missing says what an index does with a document that has no value for the
// field. It has to be declared: a default here is a silent decision about
// which documents a range query returns.
type Missing uint8

const (
	// MissingSkip leaves the document out of the index entirely.
	MissingSkip Missing = iota
	// MissingFirst sorts documents without the field before every value.
	MissingFirst
	// MissingLast sorts them after every value.
	MissingLast
)

// Field is how one component of a key is encoded.
type Field struct {
	Descending bool
	Missing    Missing
}

// Absent is what to encode for a field a document does not have.
type absent struct{}

// Absent stands for a field that is not there, as opposed to one that is null.
var Absent any = absent{}

// exactIntegers is the largest integer magnitude a float64 holds exactly.
const exactIntegers = 1 << 53

var (
	ErrNotIndexable = errors.New("sapedb/keys: this type cannot be part of a key")
	ErrNotANumber   = errors.New("sapedb/keys: NaN cannot be part of a key")
	ErrTooLarge     = errors.New("sapedb/keys: this integer cannot be held exactly")
	ErrSkipped      = errors.New("sapedb/keys: this field skips documents that do not have it")
	ErrTruncated    = errors.New("sapedb/keys: the key ends in the middle of a value")
	ErrTag          = errors.New("sapedb/keys: the key holds a value this build does not know")
)

// Encode appends one value to a key.
func Encode(dst []byte, value any, field Field) ([]byte, error) {
	start := len(dst)

	dst, err := encode(dst, value, field.Missing)
	if err != nil {
		return nil, err
	}

	if field.Descending {
		// Complementing reverses the order of everything that follows in this
		// component, terminator included, so it stays self-describing.
		for i := start; i < len(dst); i++ {
			dst[i] = ^dst[i]
		}
	}
	return dst, nil
}

func encode(dst []byte, value any, missing Missing) ([]byte, error) {
	switch typed := value.(type) {
	case absent:
		switch missing {
		case MissingFirst:
			return append(dst, tagMissingFirst), nil
		case MissingLast:
			return append(dst, tagMissingLast), nil
		default:
			return nil, ErrSkipped
		}

	case nil:
		return append(dst, tagNull), nil

	case bool:
		if typed {
			return append(dst, tagTrue), nil
		}
		return append(dst, tagFalse), nil

	case float64:
		return encodeNumber(dst, typed)

	case float32:
		return encodeNumber(dst, float64(typed))

	case int:
		return encodeInteger(dst, int64(typed))

	case int64:
		return encodeInteger(dst, typed)

	case string:
		return encodeString(dst, typed), nil

	default:
		return nil, fmt.Errorf("%w: %T", ErrNotIndexable, value)
	}
}

func encodeInteger(dst []byte, value int64) ([]byte, error) {
	// Numbers live in one order, and that order is the one JSON has: float64.
	// Beyond 2^53 a float64 no longer holds every integer, so two different
	// values would round to one key and share an index entry. Refused rather
	// than rounded; identifiers that large belong in a string, which is what
	// ULIDs are.
	//
	// Tested against 2^53 rather than by converting back: a float too big for
	// an int64 is undefined behaviour in Go, so the check itself must never
	// perform that conversion.
	if value > exactIntegers || value < -exactIntegers {
		return nil, fmt.Errorf("%w: %d is beyond 2^53", ErrTooLarge, value)
	}
	return encodeNumber(dst, float64(value))
}

func encodeNumber(dst []byte, value float64) ([]byte, error) {
	if math.IsNaN(value) {
		return nil, ErrNotANumber
	}
	if value == 0 {
		// Negative zero is the same number as zero and must be the same key.
		value = 0
	}

	bits := math.Float64bits(value)
	if bits&(1<<63) != 0 {
		bits = ^bits // negative: reverse, so the larger magnitude sorts lower
	} else {
		bits |= 1 << 63 // positive: above every negative
	}

	dst = append(dst, tagNumber)
	return binary.BigEndian.AppendUint64(dst, bits), nil
}

func encodeString(dst []byte, value string) []byte {
	dst = append(dst, tagString)
	for i := 0; i < len(value); i++ {
		if value[i] == escape {
			dst = append(dst, escape, escaped)
			continue
		}
		dst = append(dst, value[i])
	}
	return append(dst, escape, terminator)
}

// EncodeKey appends a whole key: one component per field, in order.
func EncodeKey(dst []byte, values []any, fields []Field) ([]byte, error) {
	if len(values) != len(fields) {
		return nil, fmt.Errorf("sapedb/keys: %d values for %d fields", len(values), len(fields))
	}
	for i, value := range values {
		var err error
		if dst, err = Encode(dst, value, fields[i]); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// Decode reads one value back, and returns what is left of the key.
func Decode(src []byte, field Field) (any, []byte, error) {
	if len(src) == 0 {
		return nil, nil, ErrTruncated
	}

	at := func(i int) byte {
		if field.Descending {
			return ^src[i]
		}
		return src[i]
	}

	switch tag := at(0); tag {
	case tagMissingFirst, tagMissingLast:
		return Absent, src[1:], nil

	case tagNull:
		return nil, src[1:], nil

	case tagFalse:
		return false, src[1:], nil

	case tagTrue:
		return true, src[1:], nil

	case tagNumber:
		if len(src) < 9 {
			return nil, nil, ErrTruncated
		}
		var raw [8]byte
		for i := range raw {
			raw[i] = at(1 + i)
		}
		bits := binary.BigEndian.Uint64(raw[:])
		if bits&(1<<63) != 0 {
			bits &^= 1 << 63
		} else {
			bits = ^bits
		}
		return math.Float64frombits(bits), src[9:], nil

	case tagString:
		value := make([]byte, 0, 16)
		for i := 1; i < len(src); i++ {
			if at(i) != escape {
				value = append(value, at(i))
				continue
			}
			if i+1 >= len(src) {
				return nil, nil, ErrTruncated
			}
			switch at(i + 1) {
			case terminator:
				return string(value), src[i+2:], nil
			case escaped:
				value = append(value, escape)
				i++
			default:
				return nil, nil, fmt.Errorf("%w: a string holds 0x00 0x%02x", ErrTag, at(i+1))
			}
		}
		return nil, nil, ErrTruncated

	default:
		return nil, nil, fmt.Errorf("%w: tag 0x%02x", ErrTag, tag)
	}
}

// DecodeKey reads a whole key back.
func DecodeKey(src []byte, fields []Field) ([]any, []byte, error) {
	values := make([]any, 0, len(fields))
	for _, field := range fields {
		value, rest, err := Decode(src, field)
		if err != nil {
			return nil, nil, err
		}
		values = append(values, value)
		src = rest
	}
	return values, src, nil
}
