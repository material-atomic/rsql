package store

import (
	"fmt"
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
func sameValue(left, right any) bool {
	if matches(TypeNumber, left) && matches(TypeNumber, right) && left != nil && right != nil {
		return asNumber(left) == asNumber(right)
	}
	return left == right
}

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
