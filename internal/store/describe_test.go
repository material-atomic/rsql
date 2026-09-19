package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// nestedTree builds a value nested depth levels deep: depth 0 is the leaf
// itself, depth 1 is []any{leaf}, depth 2 is []any{[]any{leaf}}, and so on.
// This is the same shape fits/renderCapped count depth against, so a tree
// built with depth==describeMaxDepth is the exact case the ceiling is
// supposed to still let through, and depth==describeMaxDepth+1 is the exact
// case it is supposed to start cutting.
func nestedTree(depth int, leaf any) any {
	v := leaf
	for i := 0; i < depth; i++ {
		v = []any{v}
	}
	return v
}

// buildCycle (also used by cycle_test.go) is a []any holding itself at
// index 0 — the simplest value that (directly or through something it
// contains) contains itself.

// runWithDeadline runs fn and fails the test if it has not returned within
// limit. go test's own -timeout only says something hung somewhere in the
// whole binary; this says which call, which is the point of cases 6/7/8 —
// they exist to measure "this returns", not what it returns.
func runWithDeadline(t *testing.T, name string, limit time.Duration, fn func() string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- fn() }()
	select {
	case result := <-done:
		return result
	case <-time.After(limit):
		t.Fatalf("%s: did not return within %s", name, limit)
		return ""
	}
}

// --- Case 1: a scalar is untouched -----------------------------------------
//
// Task 0049 §6 case 1: every one of these must come back as EXACTLY what
// fmt.Sprintf("%v", v) itself produces — compared against the real fmt call
// in this test, not against a string copied out of the task file by hand
// (copying it by hand is exactly the mistake the house rules call out:
// "chạy chương trình lấy chuỗi thật rồi so, đừng chép tay từ file task
// sang").
func TestDescribeOfAnOrdinaryScalarIsByteIdenticalToFmtSprintfV(t *testing.T) {
	for _, v := range []any{
		nil, true, false, 1.0, "x", "", []any{}, map[string]any{},
	} {
		t.Run(fmt.Sprintf("%#v", v), func(t *testing.T) {
			want := fmt.Sprintf("%v", v)
			got := describe(v)
			if got != want {
				t.Fatalf("describe(%#v) = %q, want %q (byte-identical to fmt.Sprintf)", v, got, want)
			}
		})
	}
}

// describeProbe is a plain struct used only to build a pointer value —
// *describeProbe implements no String()/GoString() method, so its %v text
// is whatever fmt's default machinery produces for a pointer, unmediated.
type describeProbe struct{ N int }

// TestDescribeOfAValueContainingAPointerIsByteIdenticalToFmtSprintfV is a
// counterexample QA 0049 found and this suite did not: fmt's own printer
// (fmt.printValue) dereferences a non-nil pointer to a struct, slice, map,
// or array and writes "&{...}"/"&[...]" only at depth 0 of a single
// fmt.Sprintf call. One level of nesting inside a compound value and it
// falls back to fmtPointer — a bare hex address — instead. renderCapped's
// leaf case calls fmt.Sprintf("%v", leaf) fresh for every leaf, which is
// depth 0 as far as THAT call is concerned regardless of how deep the
// surrounding structure actually is, so a pointer leaf renders as "&{...}"
// there even when the real, single top-level fmt.Sprintf on the whole
// value would have printed a hex address for the exact same leaf.
//
// This never shows on describe's own "fits" path (the common one): when a
// value fits, describe hands the WHOLE value to one real fmt.Sprintf call,
// so a pointer at any depth is formatted exactly the way fmt always
// formats it — there is only one fmt.Sprintf call in that path, at one
// depth, and it is real. The divergence is a property of renderCapped
// alone, so it can only be observed when renderCapped runs on a value that
// would otherwise have fit — which is exactly what task 0049 §8's D7
// (dropping the "fits -> fmt.Sprintf" branch so renderCapped always runs)
// does. Every case in this table below fits comfortably (a slice or map of
// depth 1 or 2, well under the 6-level ceiling), so today's code takes the
// fits path and this is byte-identical, same as case 1 — that identity is
// what D7 breaks.
func TestDescribeOfAValueContainingAPointerIsByteIdenticalToFmtSprintfV(t *testing.T) {
	p := &describeProbe{N: 7}
	nested := &[]any{1, 2}

	cases := []struct {
		name string
		v    any
	}{
		{"slice holding a pointer to a struct", []any{p}},
		{"map holding a pointer to a struct", map[string]any{"k": p}},
		{"slice holding a pointer to a slice", []any{nested}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := fmt.Sprintf("%v", c.v)
			got := describe(c.v)
			if got != want {
				t.Fatalf("describe(%s) = %q, want %q (byte-identical to fmt.Sprintf)", c.name, got, want)
			}
		})
	}
}

// --- Case 2: a tree well inside the depth boundary --------------------------
func TestDescribeOfADepth5TreeIsByteIdenticalToFmtSprintfV(t *testing.T) {
	// A richer shape than a bare chain of singleton slices, deliberately:
	// D7 (task 0049 §8 — drop the "fits → fmt.Sprintf" branch, always hand-
	// build) can only be caught here if the hand-built renderer's bracket
	// text actually differs from fmt's own somewhere in the space this
	// tests. A lone []any{[]any{...}} chain gives the hand-built renderer
	// the least chance to diverge; mixing maps, slices, numbers, and
	// negative numbers gives it more.
	// nestedTree(3, ...) wraps three more levels of []any around a leaf
	// that is itself two containers deep (a map holding a slice), for a
	// total nesting depth of 5 — inside the 6-level boundary, but only
	// just, and mixing map/slice/number/bool shapes the whole way down.
	v := nestedTree(3, map[string]any{
		"a": []any{1, -2, "three"},
		"b": true,
	})
	want := fmt.Sprintf("%v", v)
	got := describe(v)
	if got != want {
		t.Fatalf("describe(depth-5 tree) = %q, want %q", got, want)
	}
}

// TestDescribeOfADepth6TreeIsStillByteIdenticalToFmtSprintfV closes a gap
// task 0049 §6's own table leaves: its two depth cases are 5 (inside) and 7
// (outside), which never actually exercises the boundary value itself, 6.
// House rule "tự kiểm một bảng ca: phải NỚI, không chỉ THU HẸP" is about
// exactly this shape of gap — D2 (§8: max depth 6 → 5) would survive a
// suite that only ever tried 5 and 7, because both of those give the same
// answer whether the ceiling is 5 or 6. A depth-6 case does not.
func TestDescribeOfADepth6TreeIsStillByteIdenticalToFmtSprintfV(t *testing.T) {
	// A LITERAL 6, not describeMaxDepth: a boundary test that reads the
	// ceiling back out of the constant it is supposed to be pinning down
	// cannot ever catch a mutation OF that constant — it would silently
	// keep re-deriving "the boundary" as "whatever the code currently
	// says," which is the one thing under test. Measured, not assumed:
	// this exact mistake, made with describeMaxDepth here and
	// describeMaxElementsPerLevel in cases 4/5 below, was tried first and
	// let D1, D2 and D4 (task 0049 §8) all survive — see this task's
	// Kết quả for the mutation run that caught it and the fixed run after.
	v := nestedTree(6, "leaf")
	want := fmt.Sprintf("%v", v)
	got := describe(v)
	if got != want {
		t.Fatalf("describe(depth-6 tree) = %q, want %q (depth 6 is IN bounds)", got, want)
	}
}

// --- Case 3: a tree just outside the depth boundary --------------------------
func TestDescribeOfADepth7TreeIsCutJustOutsideTheDepthBoundary(t *testing.T) {
	v := nestedTree(7, "leaf") // literal, see the comment above
	got := describe(v)

	if !strings.Contains(got, "…") {
		t.Fatalf("describe(depth-7 tree) = %q, want a cut marker (…) — depth 7 is one past the boundary", got)
	}
	if len(got) > 512 {
		t.Fatalf("describe(depth-7 tree) is %d bytes, want <= 512", len(got))
	}
	if raw := fmt.Sprintf("%v", v); got == raw {
		t.Fatalf("describe(depth-7 tree) equals the raw %%v text; it should have been cut")
	}
}

// --- Case 4: element-count boundary, slice -----------------------------------
func TestDescribeOfATenElementSliceIsByteIdenticalButElevenIsCut(t *testing.T) {
	ten := make([]any, 10) // literal — see the comment on the depth-6 case above
	for i := range ten {
		ten[i] = i
	}
	if got, want := describe(ten), fmt.Sprintf("%v", ten); got != want {
		t.Fatalf("describe(10-element slice) = %q, want %q (10 fits)", got, want)
	}

	eleven := append(append([]any{}, ten...), "MARKER-INDEX-10-UNIQUE")
	got := describe(eleven)
	if !strings.Contains(got, "…") {
		t.Fatalf("describe(11-element slice) = %q, want a cut marker", got)
	}
	if strings.Contains(got, "MARKER-INDEX-10-UNIQUE") {
		t.Fatalf("describe(11-element slice) = %q, the 11th element should have been cut, not printed", got)
	}
}

// --- Case 5: element-count boundary, map, a different branch ----------------
func TestDescribeOfATenKeyMapIsByteIdenticalButElevenIsCut(t *testing.T) {
	ten := map[string]any{}
	for i := 0; i < 10; i++ { // literal — see the comment on the depth-6 case above
		ten[fmt.Sprintf("k%02d", i)] = i
	}
	if got, want := describe(ten), fmt.Sprintf("%v", ten); got != want {
		t.Fatalf("describe(10-key map) = %q, want %q (10 fits)", got, want)
	}

	eleven := map[string]any{}
	for k, v := range ten {
		eleven[k] = v
	}
	eleven["zzz-marker"] = "MARKER-KEY-UNIQUE"
	got := describe(eleven)
	if !strings.Contains(got, "…") {
		t.Fatalf("describe(11-key map) = %q, want a cut marker", got)
	}
	if strings.Contains(got, "MARKER-KEY-UNIQUE") {
		t.Fatalf("describe(11-key map) = %q, the 11th key sorts last and should have been cut", got)
	}
}

// --- Cases 6/7/8: self-reference, three shapes -------------------------------
//
// Each of these has its own timeout — go test -timeout says something in
// the whole binary hung, not which call. A select on time.After(2s) says
// exactly that this call is what returns, which is the property task
// 0049 §6 asks these three cases to measure, not any particular string.

func TestDescribeOfASelfReferentialSliceReturnsWithinTwoSeconds(t *testing.T) {
	s := []any{"before", nil, "after"}
	s[1] = s

	got := runWithDeadline(t, t.Name(), 2*time.Second, func() string { return describe(s) })
	if len(got) > describeMaxOutputBytes {
		t.Fatalf("describe(self-referential slice) is %d bytes, want <= %d", len(got), describeMaxOutputBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("describe(self-referential slice) = %q is not valid UTF-8", got)
	}
}

func TestDescribeOfASelfReferentialMapReturnsWithinTwoSeconds(t *testing.T) {
	m := map[string]any{"before": 1, "after": 2}
	m["self"] = m

	got := runWithDeadline(t, t.Name(), 2*time.Second, func() string { return describe(m) })
	if len(got) > describeMaxOutputBytes {
		t.Fatalf("describe(self-referential map) is %d bytes, want <= %d", len(got), describeMaxOutputBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("describe(self-referential map) = %q is not valid UTF-8", got)
	}
}

func TestDescribeOfATwoStepIndirectCycleReturnsWithinTwoSeconds(t *testing.T) {
	a := []any{"a-side", nil}
	b := []any{"b-side", nil}
	a[1] = b
	b[1] = a

	got := runWithDeadline(t, t.Name(), 2*time.Second, func() string { return describe(a) })
	if len(got) > describeMaxOutputBytes {
		t.Fatalf("describe(indirect cycle) is %d bytes, want <= %d", len(got), describeMaxOutputBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("describe(indirect cycle) = %q is not valid UTF-8", got)
	}
}

// --- Case 9: length, not depth -----------------------------------------------
//
// The test string is built entirely out of a 3-byte rune ("世", U+4E16) so
// that a naive "cut at byte 512" (D6, task 0049 §8) almost certainly lands
// mid-rune rather than on a boundary by luck: with the ellipsis reserved,
// the cut point is byte 509, and 509 is not a multiple of 3, so it falls
// inside a rune every single run, deterministically, rather than "usually".
// An all-ASCII test string would not pin this down — byte cuts and rune
// cuts agree everywhere on ASCII, and D6 would survive by accident.
func TestDescribeOfATenKilobyteLeafStringIsCutByLengthNotDepth(t *testing.T) {
	huge := strings.Repeat("世", 4000) // 12,000 bytes, ~10KB and then some
	if len(huge) <= 512 {
		t.Fatalf("test setup: the leaf string is not even longer than the ceiling")
	}

	got := describe(huge)
	// A LITERAL 512, not describeMaxOutputBytes — QA 0049 measured that
	// every length assertion in this file read the ceiling back out of the
	// constant it exists to pin down, the exact self-referential mistake
	// already fixed once for the depth/count constants above (see the
	// comment on the depth-6 case). Dropping the 512 constant entirely
	// (D5, task 0049 §8) went undetected for the same reason: a mutant
	// that widens describeMaxOutputBytes to 1<<30 still makes every
	// `<= describeMaxOutputBytes` check here trivially true. And the
	// lower bound matters just as much as the upper one — this input is
	// the one case in this file that genuinely exceeds 512 bytes before
	// truncation (case 3's depth-7 tree, checked against a literal 512
	// two cases up, never gets close to it), so an upper-bound-only check
	// here would also pass if some future change made truncation cut
	// everything down to a handful of bytes instead of close to the
	// ceiling — QA's own reproduction (widen 512 to 600) showed exactly
	// that gap: SURVIVED with only "<= 512" as a stand-in check on a case
	// that never reaches the ceiling at all.
	if len(got) > 512 {
		t.Fatalf("describe(10KB leaf) is %d bytes, want <= 512", len(got))
	}
	if len(got) < 508 {
		// 512 minus the 3-byte "…" leaves 509 bytes of budget, and cutting
		// back to a rune boundary on this all-3-byte-rune string costs at
		// most 2 more (509 is not a multiple of 3) — so anywhere from 507
		// to 509 bytes of the original string plus the 3-byte ellipsis:
		// 510 to 512 total. 508 gives a little headroom without accepting
		// a ceiling an order of magnitude smaller than the real one.
		t.Fatalf("describe(10KB leaf) is only %d bytes — that is nowhere near the 512-byte ceiling, as if truncation were cutting to some much smaller length", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("describe(10KB leaf) = %q is not valid UTF-8 — a cut landed mid-rune", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("describe(10KB leaf) = %q, want it to end in the cut marker", got)
	}
}

// --- Self-check: where no counterexample was found, and what was tried ------
//
// TestDescribeSurvivesAWideVarietyOfHandBuiltShapes is not one of the nine
// required cases; it is the record of "what miền was actually tried" the
// task's own Kết quả section is required to report, run as an actual test
// rather than only prose. Every shape here returns within a fixed budget
// and produces valid, bounded output — a self-check, not a proof, but one
// that runs on every `go test` rather than only in this task's memory.
func TestDescribeSurvivesAWideVarietyOfHandBuiltShapes(t *testing.T) {
	shapes := map[string]any{
		"deeply nested map of slices, depth 20": nestedTree(20, map[string]any{"k": []any{1, 2, 3}}),
		"wide flat slice, 500 elements":         make([]any, 500),
		"wide flat map, 500 keys":               nil, // filled in below
		"mixed cycle: slice inside a self-map": func() any {
			m := map[string]any{}
			m["self"] = []any{m, m, m}
			return m
		}(),
		"three-hop cycle": func() any {
			a := []any{1}
			b := []any{2}
			c := []any{3}
			a[0], b[0], c[0] = b, c, a
			return a
		}(),
		"nil inside a cycle":     func() any { s := []any{nil}; s[0] = s; return s }(),
		"empty containers deep":  nestedTree(8, []any{}),
		"float, int, bool mixed": []any{1, 1.5, int64(2), float32(3), true, false, nil},
	}
	wide := map[string]any{}
	for i := 0; i < 500; i++ {
		wide[fmt.Sprintf("k%03d", i)] = i
	}
	shapes["wide flat map, 500 keys"] = wide

	for name, v := range shapes {
		t.Run(name, func(t *testing.T) {
			got := runWithDeadline(t, name, 2*time.Second, func() string { return describe(v) })
			if len(got) > describeMaxOutputBytes {
				t.Fatalf("%s: describe result is %d bytes, want <= %d", name, len(got), describeMaxOutputBytes)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("%s: describe result %q is not valid UTF-8", name, got)
			}
		})
	}
}

// TestTruncateAtRuneNeverSplitsARune is a narrower, direct test of the
// truncation helper on its own, independent of describe's depth/shape
// logic — the D6 mutation (cut by byte, not rune) is aimed at this
// function specifically, and case 9 above is the end-to-end version of the
// same claim.
func TestTruncateAtRuneNeverSplitsARune(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 100, 509, 510, 511, 512, 513, 1000} {
		s := strings.Repeat("世", 400) // 1200 bytes, multi-byte throughout
		got := truncateAtRune(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateAtRune(s, %d) = %q is not valid UTF-8", n, got)
		}
		if len(got) > n && n >= 3 {
			t.Fatalf("truncateAtRune(s, %d) is %d bytes, want <= %d", n, len(got), n)
		}
	}
}
