package ulid

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

// still is a clock that does not move, so that what happens inside one
// millisecond can be looked at.
func still(at int64) func() time.Time {
	return func() time.Time { return time.UnixMilli(at) }
}

func TestAnIdentifierLooksLikeOne(t *testing.T) {
	source := With(still(1469918176385), bytes.NewReader(make([]byte, 10)))

	id, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != Size {
		t.Errorf("%q is %d characters, want %d", id, len(id), Size)
	}
	for i := 0; i < len(id); i++ {
		if !strings.ContainsRune(alphabet, rune(id[i])) {
			t.Errorf("%q holds %q, which is not in the alphabet", id, id[i])
		}
	}

	// The timestamp half, against a value worked out with a separate
	// implementation rather than with this one: 1469918176385ms is 01ARYZ6S41.
	if got := id[:10]; got != "01ARYZ6S41" {
		t.Errorf("the timestamp reads %q, want %q", got, "01ARYZ6S41")
	}
}

// The other direction, against an identifier written by somebody else: the
// example that appears in the ULID specification. Its timestamp is 1469922850259
// — not the 1469918176385 the two are often quoted together with, which is a
// different identifier altogether.
func TestAnIdentifierFromElsewhereReadsTheSame(t *testing.T) {
	when, err := Time("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	if got := when.UnixMilli(); got != 1469922850259 {
		t.Errorf("the example identifier reads as %d", got)
	}
}

func TestTheTimestampComesBack(t *testing.T) {
	for _, at := range []int64{0, 1, 1469918176385, time.Now().UnixMilli(), 1<<48 - 1} {
		source := With(still(at), bytes.NewReader(make([]byte, 10)))
		id, err := source.Next()
		if err != nil {
			t.Fatalf("%d: %v", at, err)
		}
		when, err := Time(id)
		if err != nil {
			t.Fatalf("%d: %v", at, err)
		}
		if got := when.UnixMilli(); got != at {
			t.Errorf("%d came back as %d (%q)", at, got, id)
		}
	}
}

// The property the default primary key rests on: later identifiers sort after
// earlier ones, as strings, including within a single millisecond.
func TestIdentifiersOnlyEverGoUp(t *testing.T) {
	at := int64(1700000000000)
	source := With(func() time.Time { return time.UnixMilli(at) }, bytes.NewReader(bytes.Repeat([]byte{0x7f}, 200)))

	ids := make([]string, 0, 3000)
	for i := 0; i < 3000; i++ {
		if i%500 == 499 {
			at++ // a new millisecond every so often
		}
		id, err := source.Next()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("%q does not sort after %q", ids[i], ids[i-1])
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Error("the identifiers are not in order")
	}

	// And they are all different, which the ordering above already implies but
	// is the property anyone actually relies on.
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("%q was handed out twice", id)
		}
		seen[id] = true
	}
}

func TestAClockThatGoesBackwardsStillGoesForwards(t *testing.T) {
	at := int64(1700000000000)
	source := With(func() time.Time { return time.UnixMilli(at) }, bytes.NewReader(make([]byte, 10)))

	first, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}

	// NTP steps the clock back, or a leap second, or a virtual machine
	// resuming. An identifier that sorted after another must not stop doing so.
	at -= 5000
	second, err := source.Next()
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Errorf("after the clock went back, %q does not sort after %q", second, first)
	}
}

func TestAMillisecondThatRunsOutSaysSo(t *testing.T) {
	source := With(still(1700000000000), bytes.NewReader(bytes.Repeat([]byte{0xff}, 10)))

	if _, err := source.Next(); err != nil {
		t.Fatal(err)
	}
	// The random part was all ones, so stepping it has nowhere to go. Wrapping
	// round would hand out an identifier that sorts before one already given.
	if _, err := source.Next(); !errors.Is(err, ErrExhausted) {
		t.Errorf("want ErrExhausted, got %v", err)
	}
}

func TestWhatIsNotAnIdentifierIsRefused(t *testing.T) {
	for _, id := range []string{"", "01ARZ3NDEK", strings.Repeat("0", 27), strings.Repeat("U", 26)} {
		if _, err := Time(id); err == nil {
			t.Errorf("%q was read as an identifier", id)
		}
	}
}

func TestRandomnessThatFailsIsNotPapered_Over(t *testing.T) {
	source := With(still(1700000000000), bytes.NewReader(nil))

	if _, err := source.Next(); err == nil {
		t.Error("an identifier was made without any randomness")
	}
}
