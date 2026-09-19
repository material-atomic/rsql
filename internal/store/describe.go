package store

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// describe formats a value for an error message the way satisfies (batch.go)
// needs to: readable for the ordinary case, and guaranteed to return — in
// bounded time, with a bounded-length result — for the case an ordinary
// %v cannot survive.
//
// Task 0042's own measurement is why this exists: satisfies formats a
// condition's "wanted" and a document's "value" with fmt.Errorf("%w: ... %v
// ...", ...), and fmt's printer has no cycle protection for a []any or a
// map[string]any that holds itself through a bare interface — it recurses
// the identical shape sameValue's own recursion would and dies the identical
// way, fatal error: stack overflow, uncatchable by any recover(). That is
// reachable today by any direct Go caller of the exported Invoke passing a
// self-referential argument through {"equals": {"arg": ...}} (door two —
// bind() in invoke.go hands a caller's argument to resolveIn with no
// encoding step in between; see cycle_test.go for the measured proof this
// crash happens through satisfies' formatting and not through sameValue's
// own recursion, which survives a single self-referential operand fine).
//
// The fix asked for, and the one this is: do not lose the value (%v on
// "wanted" is what someone debugging a refused optimistic-locking check
// actually needs to read), and make the message for every value that was
// never a problem come out byte-for-byte the same as it does today. Losing
// either of those would be a worse trade than the bug.
//
// How: fits (below) walks v the same two branches sameValue's own type
// switch walks — []any and map[string]any, the only two container shapes a
// document field or a declared TypeAny argument can ever actually be built
// from (json.Unmarshal only ever produces those two container types plus
// nil/bool/float64/string; a direct Go caller of Invoke could in principle
// hand over some other container-like type, but that is already outside
// what sameValue itself walks into — it falls back to reflect.DeepEqual for
// anything that is not one of these two, and this mirrors that boundary
// rather than widening it) — counting depth and the element count at each
// level as it goes, with a ceiling on both that makes the walk return an
// answer even when v holds itself. If the walk says the shape is small and
// shallow enough, fmt.Sprintf is safe to call on it — a value that visibly
// bottoms out within the depth ceiling cannot also be cyclic, since an
// actual cycle only ever manifests as a container path that never bottoms
// out — and the untouched result of that real fmt.Sprintf is what is
// returned, which is what makes the ordinary case byte-identical to
// today's. Only when the shape does not fit (or the one further ceiling on
// output length also doesn't, checked after the fact rather than during the
// walk — see describe below) does this fall back to building the string by
// hand, one bounded recursive call at a time, which is the path that
// terminates instead of hanging when v truly does hold itself.
const (
	// describeMaxDepth is how many levels of []any / map[string]any nesting
	// fits() will walk (and renderCapped will expand) before giving up and
	// printing "…" instead of continuing. A depth-6 value is still walked in
	// full; a depth-7 one is not. See TestDescribeOfADepth6TreeIsStillByte
	// IdenticalToFmtSprintfV and TestDescribeOfADepth7TreeIsCutJustOutside
	// TheDepthBoundary for the exact boundary this pins down — the task
	// that set this number named 5 and 7 as its two cases, which leaves the
	// boundary itself (6, and the one-mutation-away value of 5) unpinned;
	// the depth-6 case here closes that gap.
	describeMaxDepth = 6

	// describeMaxElementsPerLevel is the most elements — slice or map,
	// counted the same way — any single level may hold before fits() gives
	// up on it. Ten fits; eleven does not.
	describeMaxElementsPerLevel = 10

	// describeMaxOutputBytes is the longest string describe() will ever
	// hand back, byte length, applied after formatting (there is no way to
	// know a value's rendered length before rendering it, so this is
	// checked once the candidate string exists rather than budgeted during
	// the walk).
	describeMaxOutputBytes = 512
)

// describe is what satisfies calls instead of handing a value straight to
// %v.
func describe(v any) string {
	if fits(v, describeMaxDepth) {
		// v is shallow and small enough that calling the real formatter on
		// it cannot recurse forever — the walk above already proved every
		// container path it could take bottoms out inside the depth
		// ceiling, and a value that bottoms out cannot also be the kind of
		// self-reference that would make fmt's own recursion run forever.
		// This is the whole reason the ordinary case is byte-identical:
		// it truly is the same call, "%v", made on the same value.
		rendered := fmt.Sprintf("%v", v)
		if len(rendered) <= describeMaxOutputBytes {
			return rendered
		}
		// The shape fit but the text did not — a handful of ordinary-
		// looking short strings can still add up past 512 bytes with
		// nothing structurally wrong. Cut the real rendering rather than
		// rebuild it: for every shape this package actually walks (a
		// scalar, or a slice/map of scalars and of other such slices/maps)
		// the hand-built renderer below produces the identical bracket
		// text fmt's own %v does — same "[a b c]", same sorted "map[k:v]"
		// (fmt has sorted map keys for %v since Go 1.12) — so cutting the
		// real string is the same result as rebuilding it by hand up to
		// the cut point, for less work and one fewer place the two could
		// ever drift apart.
		return truncateAtRune(rendered, describeMaxOutputBytes)
	}

	// v does not fit — too deep, too wide at some level, or its shape could
	// not be trusted to bottom out at all. Build the string by hand,
	// bounded by the exact same depth ceiling fits() just used, which is
	// what makes this terminate instead of recursing forever on a value
	// that (directly, or through something it contains) holds itself.
	return truncateAtRune(renderCapped(v, 0), describeMaxOutputBytes)
}

// fits reports whether v — walked the way sameValue walks it — never
// exceeds depthBudget levels of []any / map[string]any nesting and never
// holds more than describeMaxElementsPerLevel elements at any one level.
//
// This alone is what makes the walk terminate on a self-referential value:
// depthBudget only ever counts down, so a container that holds itself is
// indistinguishable, from here, from a container nested one level deeper
// than the ceiling allows — both stop the walk at the same place, in the
// same bounded number of steps, without either one needing to be told
// apart from the other first. Telling them apart is not this function's
// job; not hanging on either one is.
func fits(v any, depthBudget int) bool {
	switch typed := v.(type) {
	case []any:
		if depthBudget <= 0 || len(typed) > describeMaxElementsPerLevel {
			return false
		}
		for _, element := range typed {
			if !fits(element, depthBudget-1) {
				return false
			}
		}
		return true

	case map[string]any:
		if depthBudget <= 0 || len(typed) > describeMaxElementsPerLevel {
			return false
		}
		for _, value := range typed {
			if !fits(value, depthBudget-1) {
				return false
			}
		}
		return true

	default:
		// Not one of the two container shapes this walks — a scalar, nil,
		// or (reachable only through the direct Go API, not through this
		// package's own two doors) some other Go type entirely. Either
		// way it is a leaf as far as this recursion is concerned, exactly
		// as it is a leaf as far as sameValue's own type switch is
		// concerned, and a leaf always fits: there is nowhere further
		// down for it to hold a cycle through the two shapes this is
		// watching for.
		return true
	}
}

// renderCapped builds a value's text by hand, the same bracket shape %v
// itself would use, but bounded to describeMaxDepth levels and
// describeMaxElementsPerLevel elements per level so that it returns even
// when v holds itself. depth is how many levels of nesting have already
// been walked to reach v; describe always starts this at 0.
func renderCapped(v any, depth int) string {
	switch typed := v.(type) {
	case []any:
		if depth >= describeMaxDepth {
			return "…"
		}
		shown, cut := typed, false
		if len(shown) > describeMaxElementsPerLevel {
			shown, cut = shown[:describeMaxElementsPerLevel], true
		}
		parts := make([]string, 0, len(shown)+1)
		for _, element := range shown {
			parts = append(parts, renderCapped(element, depth+1))
		}
		if cut {
			parts = append(parts, "…")
		}
		return "[" + strings.Join(parts, " ") + "]"

	case map[string]any:
		if depth >= describeMaxDepth {
			return "…"
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		// fmt has printed a map[string]V's keys in sorted order for %v
		// since Go 1.12, precisely so two runs of the same map do not
		// print two different strings. Matching that order here is what
		// keeps this branch's output identical to the real thing whenever
		// the two happen to agree on everything else.
		sort.Strings(keys)
		shown, cut := keys, false
		if len(shown) > describeMaxElementsPerLevel {
			shown, cut = shown[:describeMaxElementsPerLevel], true
		}
		parts := make([]string, 0, len(shown)+1)
		for _, key := range shown {
			parts = append(parts, key+":"+renderCapped(typed[key], depth+1))
		}
		if cut {
			parts = append(parts, "…")
		}
		return "map[" + strings.Join(parts, " ") + "]"

	default:
		// A leaf never recurses, so it is always safe to hand straight to
		// the real formatter regardless of how deep the walk already is —
		// the recursion this whole file exists to bound only runs through
		// the two container cases above.
		return fmt.Sprintf("%v", typed)
	}
}

// truncateAtRune cuts s to at most max bytes, ending on a whole rune and an
// ellipsis rather than a byte that happened to split one in half — a cut
// string is going into a log line and an error message a person reads, and
// a UTF-8 string broken mid-rune is a string that stops being valid UTF-8
// at all, not just a string that lost some characters.
//
// A no-op when s already fits: this is also what describe calls on a value
// that fit the shape check but rendered long anyway, and the ordinary case
// there (rendered well under 512 bytes) must come back untouched.
func truncateAtRune(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…" // U+2026, 3 bytes in UTF-8
	limit := max - len(ellipsis)
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + ellipsis
}
