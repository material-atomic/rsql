package keys

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// ascending is every kind of value, written in the order the encoding promises
// to put them in. Every test below is some statement about this list.
func ascending() []any {
	return []any{
		Absent, // with Missing: first
		nil,
		false,
		true,
		math.Inf(-1),
		-math.MaxFloat64,
		-1e308,
		-12345.678,
		-1,
		-math.SmallestNonzeroFloat64,
		0.0,
		math.SmallestNonzeroFloat64,
		1e-308,
		0.5,
		1,
		2,
		10,
		1 << 53,
		1e308,
		math.MaxFloat64,
		math.Inf(1),
		"",
		"\x00",
		"\x00\x00",
		"\x00a",
		"A",
		"a",
		"a\x00",
		"a\x00b",
		"ab",
		"b",
		"z",
		"\xff",
		"\xff\xff",
	}
}

func encodeOne(t *testing.T, value any, field Field) []byte {
	t.Helper()
	key, err := Encode(nil, value, field)
	if err != nil {
		t.Fatalf("encode %#v: %v", value, err)
	}
	return key
}

func describe(value any) string {
	if _, ok := value.(absent); ok {
		return "absent"
	}
	return fmt.Sprintf("%#v", value)
}

func TestTheEncodingSortsTheWayTheValuesDo(t *testing.T) {
	values := ascending()
	field := Field{Missing: MissingFirst}

	for i := 0; i < len(values); i++ {
		for j := 0; j < len(values); j++ {
			first, second := encodeOne(t, values[i], field), encodeOne(t, values[j], field)

			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := sign(bytes.Compare(first, second)); got != want {
				t.Errorf("%s vs %s: bytes compare %d, want %d", describe(values[i]), describe(values[j]), got, want)
			}
		}
	}
}

func TestADescendingFieldReversesTheOrderExactly(t *testing.T) {
	values := ascending()
	up := Field{Missing: MissingFirst}
	down := Field{Missing: MissingFirst, Descending: true}

	for i := 0; i+1 < len(values); i++ {
		first, second := encodeOne(t, values[i], down), encodeOne(t, values[i+1], down)
		if bytes.Compare(first, second) <= 0 {
			t.Errorf("descending: %s does not sort after %s", describe(values[i]), describe(values[i+1]))
		}

		// And the two directions are not the same bytes, or nothing was reversed.
		if bytes.Equal(first, encodeOne(t, values[i], up)) {
			t.Errorf("descending %s encodes the same as ascending", describe(values[i]))
		}
	}
}

func TestEveryValueComesBackAsItself(t *testing.T) {
	for _, descending := range []bool{false, true} {
		field := Field{Missing: MissingFirst, Descending: descending}

		for _, value := range ascending() {
			key := encodeOne(t, value, field)
			got, rest, err := Decode(key, field)
			if err != nil {
				t.Fatalf("decode %s: %v", describe(value), err)
			}
			if len(rest) != 0 {
				t.Errorf("decode %s left %d bytes", describe(value), len(rest))
			}
			if !same(got, value) {
				t.Errorf("%s came back as %s", describe(value), describe(got))
			}
		}
	}
}

// The ambiguity that a naive encoding has and that a key encoding may never
// have: two different keys must never be the same bytes.
func TestComponentsCannotBeConfusedWithOneAnother(t *testing.T) {
	fields := []Field{{Missing: MissingFirst}, {Missing: MissingFirst}}

	keys := map[string][]any{}
	for _, values := range [][]any{
		{"a", "b"},
		{"ab", ""},
		{"", "ab"},
		{"a\x00b", ""},
		{"a", "\x00b"},
		{"a\x00", "b"},
		{"a", ""},
		{"", "a"},
		{"", ""},
		{nil, "a"},
		{"a", nil},
		{Absent, "a"},
		{"a", Absent},
	} {
		key, err := EncodeKey(nil, values, fields)
		if err != nil {
			t.Fatal(err)
		}
		if seen, ok := keys[string(key)]; ok {
			t.Errorf("%v and %v encode to the same key", seen, values)
		}
		keys[string(key)] = values

		// And every one of them reads back as what went in.
		back, rest, err := DecodeKey(key, fields)
		if err != nil {
			t.Fatalf("%v: decode: %v", values, err)
		}
		if len(rest) != 0 {
			t.Errorf("%v: %d bytes left over", values, len(rest))
		}
		for i := range values {
			if !same(back[i], values[i]) {
				t.Errorf("%v came back as %v", values, back)
			}
		}
	}
}

// A composite key sorts by its first field, then its second, and a longer key
// sorts after the prefix it extends — which is what makes a range scan over
// the first field a contiguous stretch of the tree.
func TestACompositeKeySortsFieldByField(t *testing.T) {
	fields := []Field{{Missing: MissingFirst}, {Descending: true, Missing: MissingLast}}

	// The first field ascends. The second descends, so within one value of the
	// first field the larger strings come first — and because descending
	// reverses the whole component, its missing values come first too, even
	// though they were declared to sort last.
	ordered := [][]any{
		{"a", Absent},
		{"a", "z"},
		{"a", "b"},
		{"a", ""},
		{"b", Absent},
		{"b", "z"},
		{"c", 1.0},
	}

	var previous []byte
	for _, values := range ordered {
		key, err := EncodeKey(nil, values, fields)
		if err != nil {
			t.Fatal(err)
		}
		if previous != nil && bytes.Compare(previous, key) >= 0 {
			t.Errorf("%v does not sort after the key before it", values)
		}
		previous = key
	}
}

func TestWhereMissingValuesGoIsWhatWasDeclared(t *testing.T) {
	first := Field{Missing: MissingFirst}
	last := Field{Missing: MissingLast}

	for _, value := range ascending() {
		if _, ok := value.(absent); ok {
			continue
		}
		if bytes.Compare(encodeOne(t, Absent, first), encodeOne(t, value, first)) >= 0 {
			t.Errorf("missing does not sort before %s", describe(value))
		}
		if bytes.Compare(encodeOne(t, Absent, last), encodeOne(t, value, last)) <= 0 {
			t.Errorf("missing does not sort after %s", describe(value))
		}
	}

	// Descending flips where they go, like everything else in the component.
	down := Field{Missing: MissingFirst, Descending: true}
	if bytes.Compare(encodeOne(t, Absent, down), encodeOne(t, "a", down)) <= 0 {
		t.Error("a descending field did not reverse where missing values go")
	}

	// And a field that skips them refuses to encode one at all: the document
	// does not belong in the index, which is not something a key can say.
	if _, err := Encode(nil, Absent, Field{Missing: MissingSkip}); !errors.Is(err, ErrSkipped) {
		t.Errorf("want ErrSkipped, got %v", err)
	}
}

func TestZeroAndNegativeZeroAreTheSameKey(t *testing.T) {
	field := Field{}
	zero := encodeOne(t, 0.0, field)
	negative := encodeOne(t, math.Copysign(0, -1), field)

	if !bytes.Equal(zero, negative) {
		t.Error("-0 and 0 are the same number and must be the same key")
	}
}

func TestIntegersAndFloatsShareOneOrder(t *testing.T) {
	field := Field{}

	// The same number written either way is the same key, so an index built
	// from an integer is searchable with a float and the other way round.
	for _, pair := range []struct {
		integer int64
		number  float64
	}{{0, 0}, {1, 1}, {-1, -1}, {1 << 52, 1 << 52}, {-(1 << 52), -(1 << 52)}} {
		if !bytes.Equal(encodeOne(t, pair.integer, field), encodeOne(t, pair.number, field)) {
			t.Errorf("%d and %v are the same number but not the same key", pair.integer, pair.number)
		}
	}

	if !bytes.Equal(encodeOne(t, 7, field), encodeOne(t, int64(7), field)) {
		t.Error("int and int64 must encode the same")
	}
}

func TestWhatCannotBeAKeyIsRefused(t *testing.T) {
	field := Field{}

	if _, err := Encode(nil, math.NaN(), field); !errors.Is(err, ErrNotANumber) {
		t.Errorf("NaN: want ErrNotANumber, got %v", err)
	}
	// An integer that float64 cannot hold exactly would round, and two values
	// that round together would share one index entry.
	for _, value := range []int64{(1 << 53) + 1, -(1 << 53) - 1, math.MaxInt64, math.MinInt64} {
		if _, err := Encode(nil, value, field); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%d: want ErrTooLarge, got %v", value, err)
		}
	}
	for _, value := range []int64{1 << 53, -(1 << 53), 0, 1, -1} {
		if _, err := Encode(nil, value, field); err != nil {
			t.Errorf("%d is held exactly and must be allowed: %v", value, err)
		}
	}
	for _, value := range []any{[]byte("x"), []any{1}, map[string]any{}, struct{}{}, uint64(1)} {
		if _, err := Encode(nil, value, field); !errors.Is(err, ErrNotIndexable) {
			t.Errorf("%T: want ErrNotIndexable, got %v", value, err)
		}
	}
}

// A key that was cut short, or that holds something this build does not know,
// has to be an error. Never a panic, and never half a value read as a whole
// one.
func TestADamagedKeyIsRefusedRatherThanGuessed(t *testing.T) {
	for _, descending := range []bool{false, true} {
		field := Field{Missing: MissingFirst, Descending: descending}

		for _, value := range ascending() {
			key := encodeOne(t, value, field)
			for cut := 0; cut < len(key); cut++ {
				_, _, err := Decode(key[:cut], field)
				if err == nil {
					t.Errorf("%s cut to %d of %d bytes decoded anyway", describe(value), cut, len(key))
				}
			}
		}

		for _, tag := range []byte{0x00, 0x02, 0x50, 0x7f, 0xfd} {
			src := []byte{tag}
			if descending {
				src[0] = ^src[0]
			}
			if _, _, err := Decode(src, field); !errors.Is(err, ErrTag) {
				t.Errorf("tag 0x%02x: want ErrTag, got %v", tag, err)
			}
		}
	}

	// A zero byte inside a string is followed by one of two bytes and nothing
	// else; anything else is a key from somewhere this build does not know.
	broken := []byte{tagString, 'a', escape, 0x7f, escape, terminator}
	if _, _, err := Decode(broken, Field{}); !errors.Is(err, ErrTag) {
		t.Errorf("want ErrTag, got %v", err)
	}
}

// The property the whole package exists for, over values it never saw written
// down: sorting the encodings is sorting the values.
func TestSortingKeysIsSortingValues(t *testing.T) {
	random := rand.New(rand.NewSource(7))

	for round := 0; round < 200; round++ {
		values := make([]any, 60)
		for i := range values {
			values[i] = randomValue(random)
		}

		field := Field{Missing: MissingFirst}
		byValue := append([]any(nil), values...)
		sort.SliceStable(byValue, func(i, j int) bool { return before(byValue[i], byValue[j]) })

		byKey := append([]any(nil), values...)
		sort.SliceStable(byKey, func(i, j int) bool {
			return bytes.Compare(encodeOne(t, byKey[i], field), encodeOne(t, byKey[j], field)) < 0
		})

		for i := range byValue {
			if !same(byValue[i], byKey[i]) {
				t.Fatalf("round %d: sorting by key gives a different order at %d: %s vs %s",
					round, i, describe(byKey[i]), describe(byValue[i]))
			}
		}
	}
}

func randomValue(random *rand.Rand) any {
	switch random.Intn(6) {
	case 0:
		return Absent
	case 1:
		return nil
	case 2:
		return random.Intn(2) == 1
	case 3:
		return float64(random.Intn(2001)-1000) / float64(random.Intn(9)+1)
	case 4:
		return int64(random.Intn(1 << 20))
	default:
		length := random.Intn(6)
		letters := []byte("ab\x00z\xff")
		value := make([]byte, length)
		for i := range value {
			value[i] = letters[random.Intn(len(letters))]
		}
		return string(value)
	}
}

// before is the order the package promises, written out independently of the
// encoding so that the property test has something to check against.
func before(a, b any) bool { return rank(a) < rank(b) || (rank(a) == rank(b) && sameRankBefore(a, b)) }

func rank(value any) int {
	switch typed := value.(type) {
	case absent:
		return 0
	case nil:
		return 1
	case bool:
		if typed {
			return 3
		}
		return 2
	case float64, int, int64:
		return 4
	case string:
		return 5
	}
	return 9
}

func sameRankBefore(a, b any) bool {
	switch first := a.(type) {
	case float64:
		return first < asFloat(b)
	case int, int64:
		return asFloat(a) < asFloat(b)
	case string:
		return first < b.(string)
	}
	return false
}

func asFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	}
	return 0
}

func same(a, b any) bool {
	_, first := a.(absent)
	_, second := b.(absent)
	if first || second {
		return first && second
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	switch typed := a.(type) {
	case float64:
		return typed == asFloat(b)
	case int, int64:
		return asFloat(a) == asFloat(b)
	}
	return a == b
}

func sign(value int) int {
	switch {
	case value < 0:
		return -1
	case value > 0:
		return 1
	}
	return 0
}
