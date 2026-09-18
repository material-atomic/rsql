package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Declared operations are the only way into a database.
//
// There is no query on the wire. A caller names an operation that is already
// stored here and passes arguments to it, and the operation says exactly which
// collection it touches, which index it walks, how far, what it returns and
// who may run it. Everything a query would decide at runtime is decided when
// the operation is declared, which has two consequences worth the whole
// design:
//
//   - Injection has nowhere to happen. An argument is a value in a bound or a
//     field of a document; it can never become part of the access path,
//     because the access path was fixed before the argument existed.
//   - The cost is known before anything runs. A scan without a declared limit
//     is a scan whose cost nobody has worked out, so declaring one is refused
//     rather than defaulted — a default there is a promise made on behalf of
//     whoever has to serve it.
//
// Operations are versioned and old versions are kept. Changing one writes a
// new version rather than replacing the old, so an audit record that says
// which operation ran can still be read back years later.
const spaceOps byte = 0x04 // 0x04 | name | 0x00 | version -> the declaration

// What an operation does. There is no general expression language on purpose:
// each of these has a cost that can be described in a sentence.
const (
	ActionGet    = "get"    // one document by its primary key
	ActionScan   = "scan"   // a stretch of one index
	ActionCount  = "count"  // the same stretch, counted rather than returned
	ActionTotals = "totals" // a declared rollup, read
	ActionInsert = "insert" // a new document; refused if the key is taken
	ActionPut    = "put"    // a document, replacing one under the same key
	ActionUpdate = "update" // named fields of an existing document
	ActionDelete = "delete" // one document by its primary key
	ActionBatch  = "batch"  // several of the above, as one transaction
)

// ClusteredIndex is the name for walking documents in primary-key order, which
// is the order they are stored in.
const ClusteredIndex = "_key"

// Which way a scan runs along its index, as an operation writes it down.
const (
	DirectionForward = "forward" // the order the index declared
	DirectionReverse = "reverse" // that order, read back to front
)

var (
	ErrNoOperation = errors.New("rsql/store: no such operation")
	ErrArgument    = errors.New("rsql/store: the arguments do not match what the operation declares")
	ErrNotAllowed  = errors.New("rsql/store: the caller may not run this operation")
	ErrExists      = errors.New("rsql/store: a document already has that primary key")
	ErrMissing     = errors.New("rsql/store: the document this step needs is not there")
	ErrCondition   = errors.New("rsql/store: the document is not in the state this operation requires")
	ErrUncommitted = errors.New("rsql/store: a batch needs a database with nothing half-written in it")
)

// Operation is a declaration: everything about a call except its arguments.
type Operation struct {
	Name       string      `json:"name"`
	Collection string      `json:"collection"`
	Action     string      `json:"action"`
	Input      []Parameter `json:"input,omitempty"`

	// Key is which document, for the actions that work on exactly one.
	Key *Term `json:"key,omitempty"`

	// Rollup is which declared total to read, for an operation that reads one.
	Rollup string `json:"rollup,omitempty"`

	// Index, From and To are the access path of a scan: which index, and where
	// along it to start and stop. Nothing else may narrow a scan, so nothing
	// else can be smuggled into one.
	Index string    `json:"index,omitempty"`
	From  *Endpoint `json:"from,omitempty"`
	To    *Endpoint `json:"to,omitempty"`

	// Direction is which way along that index the scan runs: "forward", the
	// index's own order, or "reverse", that order read back to front. Absent
	// is forward.
	//
	// It is a Term rather than a plain string so that one mechanism covers
	// both ways of deciding it — a constant term fixes the direction in the
	// declaration, an argument term lets the caller choose per call — and both
	// are already the vocabulary everything else here is written in.
	//
	// From is where the walk STARTS and To where it ends, so reversing swaps
	// which of them is the upper end of the stretch. One declaration read both
	// ways is therefore two different stretches, not one stretch read from two
	// sides: a caller that flips the direction and leaves from and to alone
	// gets a different set of rows, and has to swap the two values itself to
	// get the same ones back. A declaration with both ends written as
	// constants, read against its fixed direction, comes back empty.
	//
	// It is still the one thing about a scan an argument may decide, and it is
	// safe for a reason that does not generalise to anything else: it widens
	// nothing. The index is the declared one, the limit is the declared one,
	// the work is the same walk over the same pages, and the set of rows the
	// declaration can reach is the same from either end — which is why a
	// constant bound written at one end only is refused beside a caller-chosen
	// direction, since that one would widen. See checkDirection.
	//
	// "Reverse" reverses the order the index declared, as a whole. An index on
	// (a ascending, b descending) read in reverse gives (a descending, b
	// ascending), not (a descending, b descending) — that last one is a third
	// order and still needs an index of its own. The word is deliberately not
	// "descending", which is what a single field is.
	Direction *Term `json:"direction,omitempty"`

	// Document is what an insert or a put writes; Set is what an update
	// changes. Both are built from arguments and constants and nothing else.
	Document map[string]Term `json:"document,omitempty"`
	Set      map[string]Term `json:"set,omitempty"`

	// Steps are what a batch does, in order and in one transaction.
	Steps []Step `json:"steps,omitempty"`

	// Projection is the fields a read returns. Empty returns the document.
	Projection []string `json:"projection,omitempty"`

	// Limit is the most rows a scan may return. Required for a scan: it is the
	// declaration of what this operation costs.
	Limit int `json:"limit,omitempty"`

	// Scopes are what a caller must hold. Checked fail-closed: an operation
	// that declares a scope is refused to a caller that does not present it.
	Scopes []string `json:"scopes,omitempty"`

	// Version is assigned when the operation is declared, so a declaration
	// being written — a schema file, or the draft the shell prints — does not
	// carry one. Omitted rather than zero, because a file saying version 0
	// claims something nobody gave it.
	Version int `json:"version,omitempty"`
}

// Parameter is one declared argument.
type Parameter struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Default  any    `json:"default,omitempty"`
}

// Term is where a value comes from: an argument of the call, a constant
// written into the declaration, or what an earlier step of a batch produced.
// Exactly one of the three.
type Term struct {
	Arg   string `json:"arg,omitempty"`
	Value any    `json:"value,omitempty"`
	// Constant distinguishes a declared value of null from no value at all,
	// which JSON alone cannot.
	Constant bool `json:"constant,omitempty"`

	// Step names an earlier step of the same batch, and Field says what of it
	// — only "key" for now, which is what an order needs to give its payment.
	//
	// This is the whole of the dataflow between steps, and it is deliberately
	// this small: anything richer is an expression language, which is the
	// thing this store exists not to have.
	Step  string `json:"step,omitempty"`
	Field string `json:"field,omitempty"`
}

// Step is one part of a batch.
type Step struct {
	// Name lets a later step refer to what this one produced.
	Name       string `json:"name,omitempty"`
	Action     string `json:"action"`
	Collection string `json:"collection"`

	Key      *Term           `json:"key,omitempty"`
	Document map[string]Term `json:"document,omitempty"`
	Set      map[string]Term `json:"set,omitempty"`

	// Exists says the document this step names must, or must not, already be
	// there. Absent means it is not checked.
	Exists *bool `json:"exists,omitempty"`

	// Require are conditions the document must already satisfy. They are how a
	// caller that read something a moment ago over the network says "only if
	// it is still that way" — the whole of optimistic locking, and the reason
	// this store needs no interactive transaction to do the classic
	// order-payment-ledger flow safely.
	Require []Condition `json:"require,omitempty"`
}

// Condition is one thing that must already be true of a document.
type Condition struct {
	Path string `json:"path"`
	// Equals is the value the field must have. Absent says it must not be
	// there at all. Exactly one of the two.
	Equals *Term `json:"equals,omitempty"`
	Absent bool  `json:"absent,omitempty"`
}

// Endpoint is one end of a scan: values for the first fields of the index, and
// whether that point is included.
type Endpoint struct {
	Terms     []Term `json:"terms"`
	Exclusive bool   `json:"exclusive,omitempty"`
}

// Caller is the context of one call: who is running the operation, what they
// hold, and their own id for the write if it is one.
type Caller struct {
	Scopes []string
	// Actor is recorded with any change this call makes, so the log says who
	// as well as what.
	Actor string
	// WriteID is the caller's own id for this write. The same id arriving twice
	// is answered from what the first one did rather than applied again, which
	// is what makes a retry after a lost connection safe.
	WriteID string
}

// Result is what running an operation produced.
type Result struct {
	Operation string           `json:"operation"`
	Version   int              `json:"version"`
	Rows      []map[string]any `json:"rows,omitempty"`
	Count     int              `json:"count,omitempty"`
	Key       any              `json:"key,omitempty"`
	Changed   int              `json:"changed,omitempty"`
	// Truncated says the scan stopped at its declared limit and there was
	// more. A caller that does not look at this is reading a partial answer as
	// a whole one, so it is a field rather than a silence.
	Truncated bool `json:"truncated,omitempty"`
	// Repeated is the log entry a write id had already produced, when this call
	// was a retry of a write that had already landed. Nothing was written
	// again.
	Repeated uint64 `json:"repeated,omitempty"`
}

// DeclareOperation stores an operation, as a new version if the name is
// already declared.
func (s *Store) DeclareOperation(operation Operation) (Operation, error) {
	if err := s.validateOperation(&operation); err != nil {
		return Operation{}, err
	}

	latest, found, err := s.Operation(operation.Name, 0)
	if err != nil && !errors.Is(err, ErrNoOperation) {
		return Operation{}, err
	}
	operation.Version = 1
	if found {
		operation.Version = latest.Version + 1
	}

	encoded, err := json.Marshal(operation)
	if err != nil {
		return Operation{}, err
	}
	if err := s.tree.Put(operationKey(operation.Name, operation.Version), encoded); err != nil {
		return Operation{}, err
	}
	if _, err := s.record(Change{Kind: ChangeOperation, Operation: &operation}); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

// Operation reads a declaration back. Version zero is the latest.
func (s *Store) Operation(name string, version int) (Operation, bool, error) {
	if version > 0 {
		value, found, err := s.tree.Get(operationKey(name, version))
		if err != nil || !found {
			if err == nil {
				err = fmt.Errorf("%w: %q version %d", ErrNoOperation, name, version)
			}
			return Operation{}, false, err
		}
		operation := Operation{}
		if err := json.Unmarshal(value, &operation); err != nil {
			return Operation{}, false, fmt.Errorf("%w: operation %q: %v", ErrDamaged, name, err)
		}
		return operation, true, nil
	}

	// The versions of one name sit together and their numbers are written big
	// end first, so the last one along is the newest.
	prefix := operationPrefix(name)
	var newest []byte
	err := s.tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		newest = append(newest[:0], value...)
		return true
	})
	if err != nil {
		return Operation{}, false, err
	}
	if newest == nil {
		return Operation{}, false, fmt.Errorf("%w: %q", ErrNoOperation, name)
	}

	operation := Operation{}
	if err := json.Unmarshal(newest, &operation); err != nil {
		return Operation{}, false, fmt.Errorf("%w: operation %q: %v", ErrDamaged, name, err)
	}
	return operation, true, nil
}

// Operations is every declared name, with the newest version of each.
func (s *Store) Operations() ([]Operation, error) {
	prefix := []byte{spaceOps}
	byName := map[string]Operation{}
	var order []string

	err := s.tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		operation := Operation{}
		if err := json.Unmarshal(value, &operation); err != nil {
			return false
		}
		if _, seen := byName[operation.Name]; !seen {
			order = append(order, operation.Name)
		}
		byName[operation.Name] = operation
		return true
	})
	if err != nil {
		return nil, err
	}

	all := make([]Operation, 0, len(order))
	for _, name := range order {
		all = append(all, byName[name])
	}
	return all, nil
}

func (s *Store) validateOperation(operation *Operation) error {
	if err := usableName(operation.Name); err != nil {
		return fmt.Errorf("operation name: %w", err)
	}

	collection, err := s.Collection(operation.Collection)
	if err != nil {
		return err
	}

	parameters := map[string]Parameter{}
	for _, parameter := range operation.Input {
		if err := usableName(parameter.Name); err != nil {
			return fmt.Errorf("argument name: %w", err)
		}
		if _, seen := parameters[parameter.Name]; seen {
			return fmt.Errorf("%w: two arguments are called %q", ErrDeclaration, parameter.Name)
		}
		switch parameter.Type {
		case TypeString, TypeNumber, TypeBool, TypeAny:
		default:
			return fmt.Errorf("%w: argument %q is declared %q", ErrDeclaration, parameter.Name, parameter.Type)
		}
		if parameter.Default != nil && !matches(parameter.Type, parameter.Default) {
			return fmt.Errorf("%w: the default for %q is not a %s", ErrDeclaration, parameter.Name, parameter.Type)
		}
		if parameter.Required && parameter.Default != nil {
			return fmt.Errorf("%w: %q is required and also has a default", ErrDeclaration, parameter.Name)
		}
		parameters[parameter.Name] = parameter
	}

	// earlier is the steps already declared, so a reference forward or to
	// itself is refused where it is written rather than found at call time.
	earlier := map[string]bool{}

	check := func(term Term, wanted string, where string) error {
		sources := 0
		if term.Arg != "" {
			sources++
		}
		if term.Value != nil || term.Constant {
			sources++
		}
		if term.Step != "" {
			sources++
		}
		if sources != 1 {
			return fmt.Errorf("%w: %s must be exactly one of an argument, a value, or an earlier step", ErrDeclaration, where)
		}

		if term.Step != "" {
			if !earlier[term.Step] {
				return fmt.Errorf("%w: %s uses step %q, which does not come before it", ErrDeclaration, where, term.Step)
			}
			if term.Field != "key" {
				return fmt.Errorf("%w: %s asks a step for %q, and a step gives only its key", ErrDeclaration, where, term.Field)
			}
			return nil
		}

		if term.Arg == "" {
			if !matches(wanted, term.Value) {
				return fmt.Errorf("%w: %s is a %s, and the value is %T", ErrDeclaration, where, wanted, term.Value)
			}
			return nil
		}
		parameter, found := parameters[term.Arg]
		if !found {
			return fmt.Errorf("%w: %s uses %q, which is not an argument of this operation", ErrDeclaration, where, term.Arg)
		}
		// Checked here so that it cannot fail at call time: an argument whose
		// type does not fit where it is used would sort somewhere else in the
		// index, and the caller would get an empty answer with no error.
		if wanted != TypeAny && parameter.Type != TypeAny && parameter.Type != wanted {
			return fmt.Errorf("%w: %s is a %s, and %q is declared %s", ErrDeclaration, where, wanted, term.Arg, parameter.Type)
		}
		return nil
	}

	switch operation.Action {
	case ActionGet, ActionDelete, ActionUpdate:
		if operation.Key == nil {
			return fmt.Errorf("%w: a %s says which document by its key", ErrDeclaration, operation.Action)
		}
		if err := check(*operation.Key, collection.spec.Key.Type, "the key"); err != nil {
			return err
		}
		if operation.Action == ActionUpdate && len(operation.Set) == 0 {
			return fmt.Errorf("%w: an update changes nothing", ErrDeclaration)
		}
		for field, term := range operation.Set {
			if err := check(term, TypeAny, "the field "+field); err != nil {
				return err
			}
		}

	case ActionTotals:
		rollup, found := collection.rollup(operation.Rollup)
		if !found {
			return fmt.Errorf("%w: %q of %q", ErrNoRollup, operation.Rollup, operation.Collection)
		}
		fields := rollup.Group
		for _, endpoint := range []*Endpoint{operation.From, operation.To} {
			if endpoint == nil {
				continue
			}
			if len(endpoint.Terms) > len(fields) {
				return fmt.Errorf("%w: %d bound values for a rollup grouped by %d fields",
					ErrDeclaration, len(endpoint.Terms), len(fields))
			}
			for i, term := range endpoint.Terms {
				if err := check(term, fields[i].Type, fmt.Sprintf("the bound on %q", fields[i].Path)); err != nil {
					return err
				}
			}
		}
		if operation.Limit <= 0 {
			return fmt.Errorf("%w: reading a rollup must declare how many rows it may return", ErrDeclaration)
		}
		if err := totalsAcross(collection, rollup, operation); err != nil {
			return err
		}

	case ActionScan, ActionCount:
		fields, err := scanFields(collection, operation.Index)
		if err != nil {
			return err
		}
		for _, endpoint := range []*Endpoint{operation.From, operation.To} {
			if endpoint == nil {
				continue
			}
			if len(endpoint.Terms) > len(fields) {
				return fmt.Errorf("%w: %d bound values for an index of %d fields", ErrDeclaration, len(endpoint.Terms), len(fields))
			}
			for i, term := range endpoint.Terms {
				if err := check(term, fields[i].Type, fmt.Sprintf("the bound on %q", fields[i].Path)); err != nil {
					return err
				}
			}
		}
		// Only a scan may carry one at all — see the refusal below — so a count
		// that wrote a direction down should hear about that rather than about
		// whether the word it chose was spelled right.
		if operation.Action == ActionScan {
			if err := checkDirection(operation, parameters, check); err != nil {
				return err
			}
		}
		if operation.Action == ActionScan && operation.Limit <= 0 {
			return fmt.Errorf("%w: a scan must declare how many rows it may return", ErrDeclaration)
		}
		if err := scanAcross(collection, operation); err != nil {
			return err
		}
		if operation.Limit < 0 {
			return fmt.Errorf("%w: a limit of %d", ErrDeclaration, operation.Limit)
		}

	case ActionBatch:
		if len(operation.Steps) == 0 {
			return fmt.Errorf("%w: a batch does nothing", ErrDeclaration)
		}
		for i := range operation.Steps {
			step := &operation.Steps[i]
			if err := s.validateStep(step, i, earlier, check); err != nil {
				return err
			}
			if step.Name != "" {
				if earlier[step.Name] {
					return fmt.Errorf("%w: two steps are called %q", ErrDeclaration, step.Name)
				}
				earlier[step.Name] = true
			}
		}

	case ActionInsert, ActionPut:
		if len(operation.Document) == 0 {
			return fmt.Errorf("%w: an %s writes nothing", ErrDeclaration, operation.Action)
		}
		for field, term := range operation.Document {
			wanted := TypeAny
			if field == collection.spec.Key.Path {
				wanted = collection.spec.Key.Type
			}
			if err := check(term, wanted, "the field "+field); err != nil {
				return err
			}
		}
		if _, writesKey := operation.Document[collection.spec.Key.Path]; !writesKey && collection.spec.Key.Auto == "" {
			return fmt.Errorf("%w: %q does not generate keys, so the operation must write %q",
				ErrDeclaration, collection.spec.Name, collection.spec.Key.Path)
		}

	default:
		return fmt.Errorf("%w: %q is not something an operation can do", ErrDeclaration, operation.Action)
	}

	// A direction is a word in a declaration, and a word nothing reads is how a
	// caller ends up believing a promise nobody made. Only a scan reads one.
	//
	// A count walks an index, so it looked at first like it should be allowed
	// one — but a count hands back a number, and min(rows, limit) is the same
	// number from either end, Truncated with it. That is the same reason
	// totals is refused, so it gets the same answer.
	if operation.Direction != nil && operation.Action != ActionScan {
		why := "does not walk an index, so it has no direction"
		if operation.Action == ActionCount {
			why = "hands back a number, and that number is the same from either end"
		}
		return fmt.Errorf("%w: a %s %s", ErrDeclaration, operation.Action, why)
	}

	for _, path := range operation.Projection {
		if path == "" {
			return fmt.Errorf("%w: the projection names a field with no path", ErrDeclaration)
		}
	}
	return nil
}

// checkDirection refuses a direction that could not be worked out, at the point
// where it is written rather than at the point where somebody reads rows in an
// order they did not expect.
func checkDirection(operation *Operation, parameters map[string]Parameter,
	check func(Term, string, string) error) error {

	term := operation.Direction
	if term == nil {
		return nil
	}
	if err := check(*term, TypeString, "the direction"); err != nil {
		return err
	}

	if term.Arg == "" {
		if _, err := directionOf(term.Value); err != nil {
			return fmt.Errorf("%w: the direction: %v", ErrDeclaration, err)
		}
		return nil
	}

	// check has already said the argument is declared and is a string.
	parameter := parameters[term.Arg]

	// A direction that depends on whether an argument turned up is two orders
	// wearing one name, and the caller who leaves it out cannot tell which one
	// it got. Refused here rather than defaulted at call time, for the same
	// reason a scan must declare a limit.
	if !parameter.Required && parameter.Default == nil {
		return fmt.Errorf("%w: %q chooses the direction, so it must be required or carry a default — a scan whose order is optional has two orders",
			ErrDeclaration, term.Arg)
	}
	if parameter.Default != nil {
		if _, err := directionOf(parameter.Default); err != nil {
			return fmt.Errorf("%w: the default for %q: %v", ErrDeclaration, term.Arg, err)
		}
	}
	return boundsSurviveReversal(operation, term.Arg)
}

// boundsSurviveReversal refuses the one shape in which letting the caller pick
// the direction hands them rows the declaration was written to keep from them.
//
// A constant in a bound is the only part of a stretch a caller cannot move. It
// is how a declaration pins a floor, or a tenant, and leaves the rest of the
// range to the call. But From is where the walk starts, so reversing swaps
// which end of the stretch is the floor — and a constant written at one end
// only stops being a floor the moment the caller passes "reverse". Everything
// underneath it comes back, and no forward call of the same declaration could
// have reached any of it whatever it passed. Refused here, because "a direction
// widens nothing" has to be true of the declaration rather than of the call.
//
// What survives is a constant that says the same thing from both ends: same
// position, same value. Then it confines the stretch whichever way the walk
// enters, which is what "one tenant, either way along time" needs — and it is
// already the shape scanAcross forces on every partitioned scan.
//
// Arguments at both ends are safe without any of this: reversing only permutes
// values the caller was choosing anyway. So is an end left unwritten, which is
// the whole rest of the index in that direction and no narrower for being read
// backwards. A direction fixed by a constant is safe too, and never reaches
// here: there is only one order, and whoever wrote the bound chose it.
func boundsSurviveReversal(operation *Operation, chosenBy string) error {
	at := func(end *Endpoint, i int) *Term {
		if end == nil || i >= len(end.Terms) {
			return nil
		}
		return &end.Terms[i]
	}
	fixed := func(term *Term) bool { return term != nil && term.Arg == "" }

	width := 0
	for _, end := range []*Endpoint{operation.From, operation.To} {
		if end != nil && len(end.Terms) > width {
			width = len(end.Terms)
		}
	}

	for i := 0; i < width; i++ {
		from, to := at(operation.From, i), at(operation.To, i)
		if !fixed(from) && !fixed(to) {
			continue
		}
		// Comparing terms with == is what scanAcross does, and it is safe for
		// the same reason: check has already said a constant bound holds a
		// value of the index field's own type, so nothing uncomparable is in
		// there.
		if from == nil || to == nil || *from != *to {
			return fmt.Errorf("%w: %q chooses the direction, so bound value %d must be written the same at both ends — a constant on one end only is a floor the reverse walk turns into a ceiling, and the rows under it are rows no forward call of this operation can reach",
				ErrDeclaration, chosenBy, i+1)
		}
	}
	return nil
}

// validateStep checks one step of a batch against the collection it names.
func (s *Store) validateStep(step *Step, at int, earlier map[string]bool,
	check func(Term, string, string) error) error {

	where := fmt.Sprintf("step %d", at+1)
	if step.Name != "" {
		where = fmt.Sprintf("step %q", step.Name)
	}

	collection, err := s.Collection(step.Collection)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}

	switch step.Action {
	case ActionGet, ActionUpdate, ActionDelete:
		if step.Key == nil {
			return fmt.Errorf("%w: %s says which document by its key", ErrDeclaration, where)
		}
		if err := check(*step.Key, collection.spec.Key.Type, where+" key"); err != nil {
			return err
		}
		if step.Action == ActionUpdate && len(step.Set) == 0 {
			return fmt.Errorf("%w: %s changes nothing", ErrDeclaration, where)
		}
		for field, term := range step.Set {
			if err := check(term, TypeAny, where+" field "+field); err != nil {
				return err
			}
		}

	case ActionInsert, ActionPut:
		if len(step.Document) == 0 {
			return fmt.Errorf("%w: %s writes nothing", ErrDeclaration, where)
		}
		for field, term := range step.Document {
			wanted := TypeAny
			if field == collection.spec.Key.Path {
				wanted = collection.spec.Key.Type
			}
			if err := check(term, wanted, where+" field "+field); err != nil {
				return err
			}
		}
		if _, writes := step.Document[collection.spec.Key.Path]; !writes && collection.spec.Key.Auto == "" {
			return fmt.Errorf("%w: %q does not generate keys, so %s must write %q",
				ErrDeclaration, collection.spec.Name, where, collection.spec.Key.Path)
		}

	default:
		return fmt.Errorf("%w: %s does %q, which a step cannot do", ErrDeclaration, where, step.Action)
	}

	for i, condition := range step.Require {
		if condition.Path == "" {
			return fmt.Errorf("%w: %s condition %d names no field", ErrDeclaration, where, i+1)
		}
		if (condition.Equals == nil) == !condition.Absent {
			return fmt.Errorf("%w: %s condition %d must be either a value it equals or absent", ErrDeclaration, where, i+1)
		}
		if condition.Equals != nil {
			if err := check(*condition.Equals, TypeAny, fmt.Sprintf("%s condition on %q", where, condition.Path)); err != nil {
				return err
			}
		}
	}

	return nil
}

// scanFields is the fields of the index an operation walks, primary key
// included as the clustered one.
func scanFields(collection *Collection, name string) ([]Field, error) {
	if name == ClusteredIndex || name == "" {
		return []Field{{
			Path:    collection.spec.Key.Path,
			Type:    collection.spec.Key.Type,
			Missing: MissingSkip,
		}}, nil
	}

	index, found := collection.index(name)
	if !found {
		return nil, fmt.Errorf("%w: %q of %q", ErrNoIndex, name, collection.spec.Name)
	}
	return index.Fields, nil
}

func operationPrefix(name string) []byte {
	key := append([]byte{spaceOps}, name...)
	return append(key, 0)
}

func operationKey(name string, version int) []byte {
	key := operationPrefix(name)
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], uint32(version))
	return append(key, raw[:]...)
}

// scanAcross refuses a scan of a partitioned collection that would come back
// in an order nobody asked for.
//
// An index on a partitioned collection is local to its partition — it has to
// be, or dropping a partition would stop being an unlink — so a scan that
// crosses partitions is the results of several indexes one after another.
// Whether that is the right order depends on the collection, and it is
// knowable here, which is the only place it should ever be decided.
//
// Time: partition names sort in the order the periods happened, and the key is
// a ulid, so partition order is key order. Every index entry ends with the key.
// So the concatenation is correctly ordered as long as the declared fields of
// the index are all fixed and the only thing varying is the key. A scan that
// leaves a declared field free would return, say, every account of January
// before every account of February.
//
// Hash: partition order is hash order, which is no order at all. Nothing that
// crosses partitions can be ordered, so nothing may.
//
// A direction changes none of this. Reversing a sequence that is in key order
// leaves it in key order — that part holds unconditionally, and it is why a
// scan that may not run forward may not run backward either, and why there is
// nothing extra to refuse here and nothing extra to allow.
//
// What is conditional is whether the sequence is in key order in the first
// place, and that is this function's business rather than the walk's. It
// compares Terms as they are written, and a bound whose argument is optional
// falls away entirely at call time — so a scan this function passed as "one
// account, every partition" can still run as "every account, one partition
// after another". That hole is the same in both directions and predates the
// direction; see task 0016.
func scanAcross(collection *Collection, operation *Operation) error {
	divided := collection.spec.Partition
	if divided == nil {
		return nil
	}

	where := fmt.Sprintf("%q is divided %s", collection.spec.Name, describePartition(divided))

	if divided.By == ByHash {
		return fmt.Errorf("%w: %s, so a scan of it would run over partitions in hash order, which is no order; read it by key",
			ErrDeclaration, where)
	}

	// The clustered index is the key itself, and partition order is key order,
	// so walking every partition in turn is exactly key order.
	if operation.Index == "" || operation.Index == ClusteredIndex {
		return nil
	}

	index, found := collection.index(operation.Index)
	if !found {
		return fmt.Errorf("%w: %q of %q", ErrNoIndex, operation.Index, collection.spec.Name)
	}

	if operation.From == nil || operation.To == nil ||
		len(operation.From.Terms) != len(index.Fields) || len(operation.To.Terms) != len(index.Fields) {
		return fmt.Errorf("%w: %s, so a scan on %q must fix all %d of its fields and let only the key vary — otherwise the partitions come back one after another rather than in order",
			ErrDeclaration, where, index.Name, len(index.Fields))
	}
	for i := range index.Fields {
		if operation.From.Terms[i] != operation.To.Terms[i] {
			return fmt.Errorf("%w: %s, so a scan on %q must fix %q rather than range over it",
				ErrDeclaration, where, index.Name, index.Fields[i].Path)
		}
	}
	return nil
}

// totalsAcross refuses a rollup read of a partitioned collection that would
// have to add up groups it has not finished finding.
//
// A rollup row is a total, and a group's total is the sum of what each
// partition holds for it. Adding them up while walking is fine when the read
// names one group, because there is one number to arrive at. Over a range it
// is not: the read would have to hold every group in the range until the last
// partition had been walked, which is memory nobody declared, bounded by the
// data rather than by the operation.
func totalsAcross(collection *Collection, rollup *Rollup, operation *Operation) error {
	if collection.spec.Partition == nil {
		return nil
	}

	where := fmt.Sprintf("%q is divided %s", collection.spec.Name, describePartition(collection.spec.Partition))

	if operation.From == nil || operation.To == nil ||
		len(operation.From.Terms) != len(rollup.Group) || len(operation.To.Terms) != len(rollup.Group) {
		return fmt.Errorf("%w: %s, so reading %q must name one group — every partition holds part of each total, and finding them all over a range is work nobody declared",
			ErrDeclaration, where, rollup.Name)
	}
	for i := range rollup.Group {
		if operation.From.Terms[i] != operation.To.Terms[i] {
			return fmt.Errorf("%w: %s, so reading %q must fix %q rather than range over it",
				ErrDeclaration, where, rollup.Name, rollup.Group[i].Path)
		}
	}
	return nil
}
