package store

import (
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// These tests measure the self-referential-value trade the godoc on
// sameValue (batch.go) now documents, instead of leaving it as prose for
// the next reader to take on faith. Task 0042's QA rounds caught two things
// wrong with the version of that story the code carried before this file
// existed: the "who can reach it" sentence was both too wide (it named the
// operator shell and JSON clients, which cannot build a cycle at all) and
// too narrow in a way that mattered more (it missed that a direct Go caller
// of the exported Invoke can crash the process without sameValue's own
// recursion ever running long). Both directions of that mistake are
// measured below rather than argued about.

// buildCycle is a []any holding itself at index 0 — the simplest value that
// (directly or through something it contains) contains itself.
func buildCycle() []any {
	v := []any{nil}
	v[0] = v
	return v
}

// TestSameValueSurvivesASingleSelfReferentialOperand is the correction to
// an earlier draft of sameValue's godoc, which said any self-referential
// value reaching sameValue recurses forever. Measured instead: it does not,
// as long as the OTHER operand is an ordinary finite value. sameValue's
// type switch always descends into whichever operand it is looking at, so
// the walk bottoms out the moment that operand's real, finite structure
// runs out — a length mismatch or a failed type assertion ends the call the
// same way it would end an ordinary comparison, regardless of what the
// other side is still willing to do. This is what makes the paragraph in
// batch.go's godoc about the document side being safe actually hold: a
// document field (always finite, see TestPutRefusesToStoreASelfReferential
// ValueBelow) can be paired against a cyclic "wanted" from door two without
// hanging, no matter which argument position it lands in.
func TestSameValueSurvivesASingleSelfReferentialOperand(t *testing.T) {
	cycle := buildCycle()

	cases := []struct {
		name        string
		left, right any
	}{
		{"finite array is shorter than the cycle's own length", []any{"a", "b"}, cycle},
		{"finite array matches the cycle's length, differs one level down", []any{"a"}, cycle},
		{"the cycle is on the left this time, finite on the right", cycle, []any{"a"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan bool, 1)
			go func() { done <- sameValue(c.left, c.right) }()
			select {
			case got := <-done:
				// The exact value does not matter here (a cyclic value is
				// never something a real condition would compare against
				// something finite and expect true); returning at all,
				// promptly, is the property this test exists to pin down.
				t.Logf("sameValue returned %v without hanging", got)
			case <-time.After(5 * time.Second):
				t.Fatal("sameValue did not return within 5s against a single self-referential operand")
			}
		})
	}
}

// TestSameValueOfTwoMutuallyCyclicValuesDiesFatallyAndUncatchably is the
// one case that DOES make sameValue's own recursion run forever: both
// operands self-referential at once. That can only be arranged by calling
// this unexported function directly with two hand-built cyclic values,
// which means only code inside this package — a test — can trigger it; see
// the godoc paragraph this measures.
//
// A real cycle cannot be run inside this test's own goroutine: it would
// take the whole `go test` binary down with it, which is a worse outcome
// than the bug this records. So the crash happens in a child process — this
// same test binary, re-executed with one environment variable set — and
// the parent only inspects the child's exit status and output. That is the
// standard library's own pattern for anything that has to die (os/exec's
// TestHelperProcess convention; log's fatal tests use the same shape).
// debug.SetMaxStack in the child keeps the cost of proving this down to a
// few hundred milliseconds instead of the ~1GB the runtime would otherwise
// grow the stack to before giving up.
func TestSameValueOfTwoMutuallyCyclicValuesDiesFatallyAndUncatchably(t *testing.T) {
	if os.Getenv("SAPEDB_CYCLE_CHILD") == "1" {
		debug.SetMaxStack(16 << 20)
		// If a fatal error were catchable, this recover would catch it.
		// Measured, not assumed: it does not run, because the runtime never
		// gets back here to run it.
		defer func() {
			if r := recover(); r != nil {
				os.Stdout.WriteString("RECOVERED\n")
				os.Exit(3)
			}
		}()
		sameValue(buildCycle(), buildCycle())
		os.Stdout.WriteString("RETURNED\n")
		os.Exit(4)
	}

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestSameValueOfTwoMutuallyCyclicValuesDiesFatallyAndUncatchably$",
		"-test.timeout=60s")
	cmd.Env = append(os.Environ(), "SAPEDB_CYCLE_CHILD=1")
	out, err := cmd.CombinedOutput()
	text := string(out)

	if err == nil {
		t.Fatalf("the child exited cleanly; sameValue on two mutually cyclic values is supposed to die\n%s", text)
	}
	if strings.Contains(text, "RECOVERED") {
		t.Fatalf("recover() caught the crash — the godoc's \"no recover() can catch this\" claim is wrong\n%s", text)
	}
	if !strings.Contains(text, "fatal error:") || !strings.Contains(text, "stack overflow") {
		t.Fatalf("the child died, but not of a fatal stack overflow:\n%s", text)
	}
	// Without this, "the child died of a fatal stack overflow" is also true
	// of an overflow that has nothing to do with sameValue — a coordinator
	// review measured this directly: swapping the child's body above for an
	// unrelated infinite recursion still leaves every assertion up to this
	// point green, so nothing here was actually pinning the overflow to
	// sameValue's own recursion. Its sibling test below pins the crash to
	// satisfies by requiring "store.satisfies" in the trace; this does the
	// same for the mechanism this test claims to measure.
	if !strings.Contains(text, "store.sameValue") {
		t.Fatalf("the child died of a stack overflow, but not inside sameValue — this test would then be measuring the wrong mechanism:\n%s", text)
	}
}

// TestArgDoorCyclicValueCrashesThroughSatisfiesFormattingNotSameValue is the
// finding that corrected the "who can reach it" claim in the other
// direction from the test above. It answers a question the earlier
// versions of this task's tests never asked: given that sameValue itself
// survives a single cyclic operand (measured above), is a self-referential
// value passed through the exported Invoke — door two, {"equals":
// {"arg": ...}}, which bind() (invoke.go) hands to resolveIn with no
// encoding step at all — actually harmless end to end?
//
// Measured answer: no. A condition that a cyclic "wanted" fails to match —
// the ordinary outcome for a batch condition, and this one is built to fail
// on purpose — makes satisfies (batch.go) format that value into its error
// with fmt.Errorf("%w: ... %v ...", ..., wanted). fmt's own printer has no
// cycle protection for a slice holding itself through a bare interface, so
// it recurses the identical shape sameValue would have and dies the
// identical way: fatal error: stack overflow, from inside fmt, several
// frames away from sameValue, which by then has already returned. This is
// reachable by any direct Go caller of the exported Invoke — not a test
// calling an unexported function, and not through this repo's own front
// doors (cli/shell.go and server.go both decode every value they hand to
// Invoke with json.Unmarshal first, which cannot build a cycle) — but
// through code embedding this store as a library and calling Invoke with a
// hand-built argument. Run as a child process for the same reason as the
// test above: this is fatal, not a panic, and would take go test down with
// it if run inline.
func TestArgDoorCyclicValueCrashesThroughSatisfiesFormattingNotSameValue(t *testing.T) {
	if os.Getenv("SAPEDB_ARGDOOR_CYCLE_CHILD") == "1" {
		debug.SetMaxStack(16 << 20)
		defer func() {
			if r := recover(); r != nil {
				os.Stdout.WriteString("RECOVERED\n")
				os.Exit(3)
			}
		}()

		// The in-memory simulated disk every other test in this package
		// uses (store_test.go's fresh helper) — no real files needed, and
		// it takes the *testing.T this child still has, since it is still
		// the same test function, just past the fork.
		_, s := fresh(t, 531)
		widgets, err := s.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}})
		if err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		yes := true
		if _, err := s.DeclareOperation(Operation{
			Name: "widgets.check", Collection: "widgets", Action: ActionBatch,
			Input: []Parameter{
				{Name: "id", Type: TypeString, Required: true},
				{Name: "t", Type: TypeAny, Required: true},
			},
			Steps: []Step{{
				Action: ActionUpdate, Collection: "widgets", Key: &Term{Arg: "id"}, Exists: &yes,
				Require: []Condition{{Path: "tags", Equals: &Term{Arg: "t"}}},
				Set:     map[string]Term{"ok": {Value: true}},
			}},
		}); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		if err := s.Commit(); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		if _, err := widgets.Put(map[string]any{"id": "w1", "tags": []any{"a", "b"}}); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		if err := s.Commit(); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}

		// The condition cannot match: a cyclic value is never the array
		// stored on the document, so satisfies is guaranteed to reach its
		// error-formatting line, not the success path.
		_, _ = s.Invoke(Caller{}, "widgets.check", 0, map[string]any{
			"id": "w1", "t": buildCycle(),
		})
		os.Stdout.WriteString("RETURNED\n")
		os.Exit(4)
	}

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestArgDoorCyclicValueCrashesThroughSatisfiesFormattingNotSameValue$",
		"-test.timeout=60s")
	cmd.Env = append(os.Environ(), "SAPEDB_ARGDOOR_CYCLE_CHILD=1")
	out, err := cmd.CombinedOutput()
	text := string(out)

	if strings.Contains(text, "SETUP FAILED") {
		t.Fatalf("child setup failed before the measurement even started:\n%s", text)
	}
	if err == nil {
		t.Fatalf("the child returned from Invoke normally; a cyclic argument through door two is supposed to crash it via satisfies' error formatting\n%s", text)
	}
	if strings.Contains(text, "RECOVERED") {
		t.Fatalf("recover() caught it — this crash is supposed to be as uncatchable as sameValue's own\n%s", text)
	}
	if !strings.Contains(text, "fatal error:") || !strings.Contains(text, "stack overflow") {
		t.Fatalf("the child died, but not of a fatal stack overflow:\n%s", text)
	}
	if !strings.Contains(text, "store.satisfies") {
		t.Fatalf("the child died of a stack overflow, but not inside satisfies — this test would then be measuring the wrong mechanism:\n%s", text)
	}
}

// TestPutAndDeclareOperationBothRefuseACyclicValueBeforeItCanEverBeStored is
// why a document field is always safe to switch on: both of the doors that
// write something durable (a document, or a declared constant) marshal to
// JSON before anything is written, and json.Marshal refuses a cycle with an
// ordinary error instead of hanging. Neither of these needs a child
// process — refusing is not fatal, it is exactly the ordinary error path
// this store uses everywhere else.
func TestPutAndDeclareOperationBothRefuseACyclicValueBeforeItCanEverBeStored(t *testing.T) {
	cycle := buildCycle()

	_, store := fresh(t, 530)
	widgets, err := store.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Put refuses a cyclic document field", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Put on a cyclic document panicked instead of returning an error: %v", r)
			}
		}()
		_, err := widgets.Put(map[string]any{"id": "w1", "loop": cycle})
		if err == nil {
			t.Fatal("Put accepted a cyclic document field; it could then be read back and reach sameValue")
		}
		t.Logf("Put on a cyclic document field: %v", err)
	})

	t.Run("DeclareOperation refuses a cyclic constant", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("DeclareOperation with a cyclic constant panicked instead of returning an error: %v", r)
			}
		}()
		_, err := store.DeclareOperation(Operation{
			Name: "widgets.loopy", Collection: "widgets", Action: ActionBatch,
			Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
			Steps: []Step{{
				Action: ActionUpdate, Collection: "widgets", Key: &Term{Arg: "id"},
				Require: []Condition{{Path: "loop", Equals: &Term{Value: cycle}}},
				Set:     map[string]Term{"ok": {Value: true}},
			}},
		})
		if err == nil {
			if commitErr := store.Commit(); commitErr == nil {
				t.Fatal("DeclareOperation stored a cyclic constant; it could then be read back and reach sameValue")
			}
			return
		}
		t.Logf("DeclareOperation with a cyclic constant: %v", err)
	})
}
