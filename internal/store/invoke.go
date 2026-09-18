package store

import (
	"fmt"
)

// Invoke runs a declared operation. This is the only way to read or write.
//
// Nothing here decides anything the declaration did not already decide: the
// arguments are checked against what was declared, the values they carry go
// into the places the declaration left for them, and the access path is the
// one that was written down. An argument cannot widen a scan, reach another
// collection, or become part of a key.
func (s *Store) Invoke(caller Caller, name string, version int, arguments map[string]any) (Result, error) {
	operation, found, err := s.Operation(name, version)
	if err != nil || !found {
		return Result{}, err
	}

	if err := allowed(caller, operation); err != nil {
		return Result{}, err
	}

	values, err := bind(operation, arguments)
	if err != nil {
		return Result{}, err
	}

	collection, err := s.Collection(operation.Collection)
	if err != nil {
		return Result{}, err
	}

	result := Result{Operation: operation.Name, Version: operation.Version}
	by := Attribution{
		Operation: operation.Name,
		Version:   operation.Version,
		Actor:     caller.Actor,
		WriteID:   caller.WriteID,
	}

	// A write that arrives twice under one id is answered from what the first
	// one did. The connection dropping after the write landed but before the
	// answer got back is the ordinary case, and a caller that retries then must
	// not end up with two documents.
	if writes(operation.Action) && caller.WriteID != "" {
		lsn, key, done, err := s.Wrote(caller.WriteID)
		if err != nil {
			return Result{}, err
		}
		if done {
			result.Key = key
			result.Changed = 1
			result.Repeated = lsn
			return result, nil
		}
	}

	switch operation.Action {
	case ActionGet:
		key, err := resolve(*operation.Key, values)
		if err != nil {
			return Result{}, err
		}
		document, found, err := collection.Get(key)
		if err != nil {
			return Result{}, err
		}
		if found {
			result.Rows = []map[string]any{project(document, operation.Projection)}
			result.Count = 1
		}

	case ActionScan, ActionCount:
		within, err := bounds(operation, values)
		if err != nil {
			return Result{}, err
		}
		if err := s.run(collection, operation, within, &result); err != nil {
			return Result{}, err
		}

	case ActionInsert, ActionPut:
		document, err := build(operation.Document, values)
		if err != nil {
			return Result{}, err
		}
		if operation.Action == ActionInsert {
			if key, carries := document[collection.spec.Key.Path]; carries && key != nil {
				if _, taken, err := collection.Get(key); err != nil {
					return Result{}, err
				} else if taken {
					return Result{}, fmt.Errorf("%w: %v in %q", ErrExists, key, collection.spec.Name)
				}
			}
		}
		key, err := collection.PutBy(by, document)
		if err != nil {
			return Result{}, err
		}
		result.Key = key
		result.Changed = 1

	case ActionUpdate:
		key, err := resolve(*operation.Key, values)
		if err != nil {
			return Result{}, err
		}
		document, found, err := collection.Get(key)
		if err != nil {
			return Result{}, err
		}
		if !found {
			break
		}
		changes, err := build(operation.Set, values)
		if err != nil {
			return Result{}, err
		}
		for field, value := range changes {
			document[field] = value
		}
		if _, err := collection.PutBy(by, document); err != nil {
			return Result{}, err
		}
		result.Key = key
		result.Changed = 1

	case ActionDelete:
		key, err := resolve(*operation.Key, values)
		if err != nil {
			return Result{}, err
		}
		removed, err := collection.DeleteBy(by, key)
		if err != nil {
			return Result{}, err
		}
		result.Key = key
		if removed {
			result.Changed = 1
		}

	default:
		return Result{}, fmt.Errorf("%w: %q", ErrDeclaration, operation.Action)
	}

	return result, nil
}

// run walks the declared stretch of the declared index, stopping at the
// declared limit and saying so.
func (s *Store) run(collection *Collection, operation Operation, within Range, result *Result) error {
	limit := operation.Limit
	counting := operation.Action == ActionCount

	visit := func(key any, document map[string]any) bool {
		if counting {
			// A declared limit stops a count too, and says so. Without one it
			// counts the whole range: a count returns a number rather than
			// rows, so the cost is a walk and the answer is not partial.
			if limit > 0 && result.Count >= limit {
				result.Truncated = true
				return false
			}
			result.Count++
			return true
		}

		if len(result.Rows) >= limit {
			// One row past the limit is what tells the caller there was more.
			result.Truncated = true
			return false
		}
		result.Rows = append(result.Rows, project(document, operation.Projection))
		result.Count = len(result.Rows)
		return true
	}

	if operation.Index == ClusteredIndex || operation.Index == "" {
		return collection.walkRange(within, visit)
	}

	return collection.Scan(operation.Index, within, func(entry Found) bool {
		document, found, err := collection.Get(entry.Key)
		if err != nil || !found {
			// An index entry with no document behind it is damage, not an
			// empty row: the two are written together and must stay together.
			return false
		}
		return visit(entry.Key, document)
	})
}

// writes says whether an action changes anything, which decides whether a
// write id means something for it.
func writes(action string) bool {
	switch action {
	case ActionInsert, ActionPut, ActionUpdate, ActionDelete:
		return true
	}
	return false
}

// allowed is the fail-closed scope check: every scope the operation names must
// be one the caller presents.
func allowed(caller Caller, operation Operation) error {
	held := map[string]bool{}
	for _, scope := range caller.Scopes {
		held[scope] = true
	}
	for _, scope := range operation.Scopes {
		if !held[scope] {
			return fmt.Errorf("%w: %q needs %q", ErrNotAllowed, operation.Name, scope)
		}
	}
	return nil
}

// bind checks the arguments of a call against what the operation declares, and
// fills in the defaults.
//
// An argument the operation does not declare is refused rather than ignored.
// Ignoring it would let a caller believe something was applied that was not,
// which for a store meant to be driven by generated code is the worst of the
// three possible answers.
func bind(operation Operation, arguments map[string]any) (map[string]any, error) {
	values := make(map[string]any, len(operation.Input))
	declared := make(map[string]bool, len(operation.Input))

	for _, parameter := range operation.Input {
		declared[parameter.Name] = true

		value, given := arguments[parameter.Name]
		if !given {
			if parameter.Required {
				return nil, fmt.Errorf("%w: %q needs %q", ErrArgument, operation.Name, parameter.Name)
			}
			if parameter.Default != nil {
				values[parameter.Name] = parameter.Default
			}
			continue
		}
		if !matches(parameter.Type, value) {
			return nil, fmt.Errorf("%w: %q is a %s, and this is %T", ErrArgument, parameter.Name, parameter.Type, value)
		}
		values[parameter.Name] = value
	}

	for name := range arguments {
		if !declared[name] {
			return nil, fmt.Errorf("%w: %q does not take %q", ErrArgument, operation.Name, name)
		}
	}
	return values, nil
}

// resolve is the value a term stands for.
func resolve(term Term, values map[string]any) (any, error) {
	if term.Arg == "" {
		return term.Value, nil
	}
	value, given := values[term.Arg]
	if !given {
		return nil, fmt.Errorf("%w: %q was not given and has no default", ErrArgument, term.Arg)
	}
	return value, nil
}

// bounds turns the declared endpoints into the range to walk.
//
// An endpoint whose arguments were not given falls away: an operation that
// takes an optional "since" is unbounded below when nobody passes one, which
// is the ordinary way such an operation is written.
func bounds(operation Operation, values map[string]any) (Range, error) {
	within := Range{}

	for _, end := range []struct {
		declared **Endpoint
		into     **Bound
	}{
		{&operation.From, &within.From},
		{&operation.To, &within.To},
	} {
		endpoint := *end.declared
		if endpoint == nil {
			continue
		}

		bound := &Bound{Exclusive: endpoint.Exclusive}
		for _, term := range endpoint.Terms {
			if term.Arg != "" {
				if _, given := values[term.Arg]; !given {
					bound = nil
					break
				}
			}
			value, err := resolve(term, values)
			if err != nil {
				return Range{}, err
			}
			bound.Values = append(bound.Values, value)
		}
		*end.into = bound
	}
	return within, nil
}

// build makes a document, or the changes to one, out of terms.
func build(terms map[string]Term, values map[string]any) (map[string]any, error) {
	document := make(map[string]any, len(terms))

	for field, term := range terms {
		if term.Arg != "" {
			if _, given := values[term.Arg]; !given {
				// An optional argument that was not passed leaves the field
				// alone rather than writing a null over it.
				continue
			}
		}
		value, err := resolve(term, values)
		if err != nil {
			return nil, err
		}
		document[field] = value
	}
	return document, nil
}

// project keeps only the declared fields. No projection returns the document.
func project(document map[string]any, fields []string) map[string]any {
	if len(fields) == 0 {
		return document
	}

	kept := make(map[string]any, len(fields))
	for _, path := range fields {
		if value, found := at(document, path); found {
			kept[path] = value
		}
	}
	return kept
}
