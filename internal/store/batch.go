package store

import (
	"fmt"
	"reflect"
)

// A batch is several writes as one transaction.
//
// It is how this store does the things a database is usually asked for an
// interactive transaction to do — an order and its payment and the ledger
// lines that record it, all landing together or not at all. Declared like
// everything else, so its cost is known before it runs and no client holds a
// transaction open while everybody else waits on it.
//
// Two things make that enough without `begin`:
//
//   - Inside a batch nothing else is writing. The engine takes one writer, so
//     a step may read what an earlier step wrote and nobody has changed it in
//     between. The classic hazard — two transactions each checking a balance
//     and both spending it — cannot happen here.
//   - A caller that read something over the network a moment ago says so: a
//     step carries conditions the document must already satisfy. That is the
//     whole of optimistic locking, and it turns "the order was still pending
//     when I looked" into something the store checks rather than hopes.
//
// If any step fails, the transaction is abandoned. Not the step — the
// transaction, because there is one transaction at a time and this is it.
//
// Which is why a batch refuses to start on top of uncommitted work. There are
// no savepoints here and there will not be, so abandoning a failed batch would
// take whatever came before it as well — silently, and only when something
// went wrong, which is the worst moment to discover it. The first test written
// against this tripped over exactly that: it declared three collections, ran a
// batch that was meant to fail, and lost the declarations. A rule that has to
// be remembered is a rule that will be forgotten, so it is checked instead.

// runBatch does every step, or none of them.
func (s *Store) runBatch(caller Caller, operation Operation, values map[string]any, result *Result) error {
	if s.pages.Pending() {
		return fmt.Errorf("%w: commit or roll back what is already written before running %q",
			ErrUncommitted, operation.Name)
	}

	by := Attribution{
		Operation: operation.Name,
		Version:   operation.Version,
		Actor:     caller.Actor,
		WriteID:   caller.WriteID,
	}

	// What each named step produced, for the steps that come after it.
	produced := map[string]any{}

	for i := range operation.Steps {
		step := &operation.Steps[i]

		where := fmt.Sprintf("step %d", i+1)
		if step.Name != "" {
			where = fmt.Sprintf("step %q", step.Name)
		}

		key, err := s.runStep(by, step, values, produced, result)
		if err != nil {
			// The whole transaction goes, not the step. A batch that left its
			// first two writes behind would be the thing it exists to prevent.
			if abandoned := s.Rollback(); abandoned != nil {
				return fmt.Errorf("%s: %w (and abandoning it failed: %v)", where, err, abandoned)
			}
			result.Rows, result.Changed, result.Count = nil, 0, 0
			return fmt.Errorf("%s: %w", where, err)
		}

		if step.Name != "" {
			produced[step.Name] = key
		}
		result.Key = key
	}

	return nil
}

// runStep does one step, after checking what it requires.
func (s *Store) runStep(by Attribution, step *Step, values map[string]any,
	produced map[string]any, result *Result) (any, error) {

	collection, err := s.Collection(step.Collection)
	if err != nil {
		return nil, err
	}

	// The key first, because everything a step checks is about the document it
	// names.
	var key any
	if step.Key != nil {
		if key, err = resolveIn(*step.Key, values, produced); err != nil {
			return nil, err
		}
	}

	var document map[string]any
	found := false
	if key != nil {
		if document, found, err = collection.Get(key); err != nil {
			return nil, err
		}
	}

	if step.Exists != nil {
		switch {
		case *step.Exists && !found:
			return nil, fmt.Errorf("%w: %v in %q", ErrMissing, key, step.Collection)
		case !*step.Exists && found:
			return nil, fmt.Errorf("%w: %v in %q", ErrExists, key, step.Collection)
		}
	}

	if len(step.Require) > 0 {
		if !found {
			return nil, fmt.Errorf("%w: %v in %q is not there to check", ErrMissing, key, step.Collection)
		}
		if err := satisfies(document, step.Require, values, produced, step.Collection, key); err != nil {
			return nil, err
		}
	}

	switch step.Action {
	case ActionGet:
		if found {
			result.Rows = append(result.Rows, project(document, nil))
			result.Count = len(result.Rows)
		}
		return key, nil

	case ActionInsert, ActionPut:
		written, err := buildIn(step.Document, values, produced)
		if err != nil {
			return nil, err
		}
		if step.Action == ActionInsert {
			if at, carries := written[collection.spec.Key.Path]; carries && at != nil {
				if _, taken, err := collection.Get(at); err != nil {
					return nil, err
				} else if taken {
					return nil, fmt.Errorf("%w: %v in %q", ErrExists, at, step.Collection)
				}
			}
		}
		made, err := collection.PutBy(by, written)
		if err != nil {
			return nil, err
		}
		result.Changed++
		return made, nil

	case ActionUpdate:
		if !found {
			return nil, fmt.Errorf("%w: %v in %q", ErrMissing, key, step.Collection)
		}
		changes, err := buildIn(step.Set, values, produced)
		if err != nil {
			return nil, err
		}
		for field, value := range changes {
			document[field] = value
		}
		if _, err := collection.PutBy(by, document); err != nil {
			return nil, err
		}
		result.Changed++
		return key, nil

	case ActionDelete:
		removed, err := collection.DeleteBy(by, key)
		if err != nil {
			return nil, err
		}
		if removed {
			result.Changed++
		}
		return key, nil
	}

	return nil, fmt.Errorf("%w: a step cannot %q", ErrDeclaration, step.Action)
}

// satisfies checks what a step requires of the document it names.
func satisfies(document map[string]any, conditions []Condition, values map[string]any,
	produced map[string]any, collection string, key any) error {

	for _, condition := range conditions {
		value, present := at(document, condition.Path)

		if condition.Absent {
			if present {
				return fmt.Errorf("%w: %v in %q still has %q", ErrCondition, key, collection, condition.Path)
			}
			continue
		}

		wanted, err := resolveIn(*condition.Equals, values, produced)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("%w: %v in %q has no %q, and it must be %v",
				ErrCondition, key, collection, condition.Path, wanted)
		}
		if !sameValue(value, wanted) {
			return fmt.Errorf("%w: %v in %q has %q of %v, and it must be %v",
				ErrCondition, key, collection, condition.Path, value, wanted)
		}
	}
	return nil
}

// sameValue compares two values the way a condition means it.
//
// Numbers from JSON are float64 and numbers from a declaration may be written
// as an integer, so comparing them as `any` would make 1 and 1.0 different
// things — which is the sort of difference nobody debugging a refused write
// would think to look for.
//
// That reasoning does not stop at the outer value. condition.Equals is
// declared TypeAny — a condition checks a document field, not an index bound,
// so there is nothing here for a constant to encode into and so nothing to
// refuse at declaration time (see task 0042) — and TypeAny accepts a slice or
// an object exactly as it accepts a string. Either side of the comparison can
// therefore be a []any or a map[string]any, and the 1-vs-1.0 difference can
// sit one level down inside it just as easily as at the top: {"totals":[1]}
// and {"totals":[1.0]} are the same document written two ways. A plain ==
// on such a value does not just get that wrong, it panics outright — two
// interface values whose dynamic type is a slice or a map are exactly what
// Go means by "comparing uncomparable type", and unlike a Term's Value (see
// sameTerm in ops.go) this is an ordinary field pulled out of a stored
// document, not something a schema already limited. So this walks down: an
// array compares element by element, an object compares by key, and only a
// leaf that cannot be either falls back to reflect.DeepEqual — which never
// panics, but which is not asked to do the numeric work, because DeepEqual
// compares dynamic types and would call 1 and 1.0 different again.
//
// The trade this makes, so the next person does not have to rediscover it by
// tripping over it: this recursion keeps no set of pairs it has already
// visited, so a value that (directly or through something it contains) holds
// itself makes it recurse forever instead of returning. JSON cannot build
// one — json.Unmarshal only ever produces a tree — and that is why the left
// side of every call satisfies makes, a document field read back out of this
// store, can never be one: Put marshals a document to JSON before writing
// it, json.Marshal refuses a cycle with its own error instead of hanging, so
// a cyclic document is never stored in the first place. A declared
// condition's constant (door one) is refused the same way, at declare time,
// by DeclareOperation's own Marshal.
//
// Task 0042's QA round asked for the reach of this to be measured rather
// than asserted, and the measured answer is narrower than an earlier draft
// of this comment claimed. A single self-referential operand does not make
// this function loop: the recursion always switches on and descends into
// whichever value it is looking at, and the moment that walk reaches an
// operand that is one of the two JSON-guaranteed trees above, the walk
// bottoms out at that tree's real, finite depth — a length mismatch or a
// failed type assertion ends it, exactly the way it would end an ordinary
// comparison, regardless of what the other operand is doing. So sameValue
// itself can only recurse forever if BOTH operands are self-referential at
// once, which — because a document field never can be — means the only way
// to trigger it is to call this unexported function directly with two
// hand-built cyclic values. Only code inside this package can do that: a
// test.
//
// That is not the same claim as "nothing outside this package can crash the
// process with a cycle," and conflating the two would be the same kind of
// mistake this paragraph is replacing. Door two — {"equals": {"arg": ...}} —
// is declared TypeAny, and bind() (invoke.go) hands a caller's argument to
// resolveIn exactly as given, with no encoding step in between. A caller of
// the exported Invoke, in Go, in the same process, can pass a self-
// referential slice or map as that argument, and nothing here refuses it.
// sameValue survives that call — the paragraph above is why — but satisfies,
// one call above it, does not: every condition that fails to match, which is
// the ordinary outcome an optimistic-locking check exists to produce, formats
// the value into its error with fmt.Errorf("%v", ...), and fmt's printer has
// no cycle protection for a slice or map holding itself through a bare
// interface. It recurses the identical shape sameValue would have and dies
// the identical way. So the door this change leaves open is not sameValue's
// own recursion; it is the error message one frame above it, and it is
// reachable by any direct Go caller of Invoke, not only by a test — this
// repo's own front doors do not reach it (cli/shell.go decodes every typed
// word with json.Unmarshal before it ever becomes a Term or an argument, and
// server.go decodes the wire frame the same way), but nothing stops code
// that embeds this store as a library from calling Invoke with a hand-built
// cyclic argument directly.
//
// Either way it happens, it happens the same way: the goroutine's stack
// grows until the runtime gives up, and it gives up with `fatal error: stack
// overflow` — a fatal error, not a panic, which no recover() anywhere can
// catch, including one sitting at a connection's boundary. reflect.DeepEqual
// would close the sameValue half of this (it tracks visited pairs and
// terminates); it would not touch the satisfies/fmt.Errorf half at all,
// because that crash never reaches sameValue. So this fix trades one
// difference that only shows up through the direct Go API (1 and 1.0
// disagreeing one level down, the reason this function walks at all) for a
// cost of its own making (the recursion above) sitting next to a second cost
// that was already there before this fix touched anything and is not this
// fix's to close (the error path's unguarded %v). Going back to DeepEqual
// would not buy that second one back — it has its own numeric mistake, and
// it never runs the code that has the %v problem either. Both costs are
// written down here, next to each other, because whoever reads this godoc
// asking "can a cycle reach here" deserves the sharper answer, not the one
// that stops at the function whose name is in the question.
func sameValue(left, right any) bool {
	if matches(TypeNumber, left) && matches(TypeNumber, right) && left != nil && right != nil {
		return asNumber(left) == asNumber(right)
	}

	switch typed := left.(type) {
	case []any:
		other, ok := right.([]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for i := range typed {
			if !sameValue(typed[i], other[i]) {
				return false
			}
		}
		return true

	case map[string]any:
		other, ok := right.(map[string]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for key, value := range typed {
			match, present := other[key]
			if !present || !sameValue(value, match) {
				return false
			}
		}
		return true

	default:
		// left is not a []any or a map[string]any here (nil included: a type
		// switch on a nil interface falls to default, same as everything
		// comparable). That is not the same thing as "left is comparable",
		// and the difference is the reason this line says DeepEqual and has
		// to keep saying it: a []string, or a map[string]int, reaches here
		// too. No JSON decoder builds one, but a Go caller of Invoke can hand
		// one over, and a plain == on two of them panics with exactly the
		// "comparing uncomparable type" this function was rewritten to stop
		// doing. DeepEqual's own nil case is x == y on the interfaces
		// themselves, which is always safe — a nil interface never shares a
		// dynamic type with anything, comparable or not, so there is nothing
		// for it to panic on.
		return reflect.DeepEqual(left, right)
	}
}

// asNumber flattens every number shape into one float64, which is what lets
// the numeric branch of sameValue be a single ==.
//
// This case list and the TypeNumber case of matches (store.go) are two
// switches over the same set of types, and nothing makes them agree. Adding a
// fifth number type to matches alone is the dangerous direction: sameValue
// would take its numeric branch for that type, this function would answer 0
// for both sides, and every two values of it would compare equal — a
// condition that always passes, which for an optimistic lock means a write
// that should have been refused goes through. No panic, no vet error, and no
// test here would say so. Adding it here alone costs much less: the pair
// falls through to DeepEqual and only the 1-is-1.0 rule stops reaching it.
func asNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	}
	return 0
}

// resolveIn is resolve, plus what an earlier step produced.
func resolveIn(term Term, values map[string]any, produced map[string]any) (any, error) {
	if term.Step != "" {
		key, ran := produced[term.Step]
		if !ran {
			return nil, fmt.Errorf("%w: step %q has not run", ErrDeclaration, term.Step)
		}
		return key, nil
	}
	return resolve(term, values)
}

// buildIn is build, plus what an earlier step produced.
func buildIn(terms map[string]Term, values map[string]any, produced map[string]any) (map[string]any, error) {
	document := make(map[string]any, len(terms))

	for field, term := range terms {
		if term.Arg != "" {
			if _, given := values[term.Arg]; !given {
				continue
			}
		}
		value, err := resolveIn(term, values, produced)
		if err != nil {
			return nil, err
		}
		document[field] = value
	}
	return document, nil
}
