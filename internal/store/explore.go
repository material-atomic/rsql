package store

import (
	"fmt"
)

// Exploring is looking inside a database without declaring an operation first.
//
// Every database needs it. Something is wrong at two in the morning, and the
// operation that would answer the question was never written because nobody
// knew the question would be asked. A store that cannot be looked into is one
// people copy out to somewhere that can, which is worse in every way.
//
// What it is not is a query language. The way this is usually solved — mongo
// runs JavaScript inside the server, and every production deployment turns it
// off — makes the shell a second, more powerful interface than the one the
// application uses, available to whoever reaches the port. Here an access
// somebody types has exactly the shapes an operation may declare: a document
// by its key, a stretch of a declared index, a count of one. The only
// difference is when the parameters arrive.
//
// That is not a restriction bolted on afterwards. Explore builds the Operation
// the typed access would have to be declared as, checks it with the same
// validation a declaration gets, and runs it down the same path. An access
// that could not be declared cannot be typed, because there is nowhere for it
// to go. And the declaration it built is handed back, so exploring ends in
// something to commit rather than in a habit of poking at production.
//
// Two things follow from it being the same engine:
//
//   - A scan has a limit, always. Exploring is where an unbounded scan would
//     be reached for, and this store has none to reach for.
//   - Every access is written to the change log with who ran it. It costs a
//     commit for a read, which is the price of nobody being able to look
//     through a production database quietly. Replicas replay the entry as
//     nothing, because nothing changed.

// MostRows is the most an exploratory scan may return.
//
// A number, not "unlimited", for the same reason a declared scan has a limit:
// the operator at two in the morning is exactly the person who will scan a
// collection of forty million documents to see what is in it, and find out
// how big it was by waiting.
const MostRows = 1000

// Access is one thing an operator asks to look at.
//
// It is the parameters of an operation, arriving now instead of having been
// declared. Which is why there is no field here for anything an operation
// cannot declare.
type Access struct {
	// Kind is "get", "scan" or "count". Nothing writes: a change to a
	// production database should be something somebody wrote down, reviewed
	// and can run again, which is a declared operation.
	Kind string `json:"kind"`

	Collection string `json:"collection"`

	// Key is which document, for a get.
	Key any `json:"key,omitempty"`

	// Index, From, To and Limit are a scan, exactly as a declaration spells
	// one. An empty Index is the clustered one, the same as everywhere else.
	Index string `json:"index,omitempty"`
	From  *Bound `json:"from,omitempty"`
	To    *Bound `json:"to,omitempty"`
	Limit int    `json:"limit,omitempty"`

	// Projection is which fields to show.
	Projection []string `json:"projection,omitempty"`
}

// Explore runs an access and hands back the operation it would be.
//
// The operation comes back whether or not the caller wants it, because the
// point of the shell is that looking around produces something declarable. A
// shell that only printed rows would leave the next person doing the same
// exploration again.
func (s *Store) Explore(caller Caller, access Access) (Operation, Result, error) {
	operation, err := s.asOperation(access)
	if err != nil {
		return Operation{}, Result{}, err
	}

	// The same check a declaration gets. An access that would be refused as a
	// declaration is refused here, in the same words, so the shell cannot be
	// the place where a rule quietly does not apply.
	if err := s.validateOperation(&operation); err != nil {
		return Operation{}, Result{}, err
	}

	result, err := s.perform(caller, operation, nil)
	if err != nil {
		return Operation{}, Result{}, err
	}
	result.Operation = ""
	result.Version = 0

	// Recorded after the read, because how many rows came back is part of what
	// was done: "read one document" and "read a thousand" are different
	// answers to whoever is reading the log later.
	//
	// The rows themselves are not recorded. A log that held everything anybody
	// read would be a second copy of the database, growing fastest exactly
	// when somebody is investigating, and it would carry those documents into
	// every replica and every change feed.
	if _, err := s.record(Change{
		Kind:       ChangeRead,
		Collection: access.Collection,
		Key:        access.Key,
		Operation:  &operation,
		By:         Attribution{Operation: "explore", Actor: caller.Actor},
	}); err != nil {
		return Operation{}, Result{}, err
	}
	if err := s.Commit(); err != nil {
		return Operation{}, Result{}, err
	}

	return operation, result, nil
}

// asOperation is the declaration the typed access would have to be.
func (s *Store) asOperation(access Access) (Operation, error) {
	operation := Operation{
		Collection: access.Collection,
		Projection: access.Projection,
	}

	switch access.Kind {
	case "get":
		if access.Key == nil {
			return Operation{}, fmt.Errorf("%w: a get says which document by its key", ErrDeclaration)
		}
		operation.Action = ActionGet
		operation.Key = &Term{Value: access.Key, Constant: true}

	case "scan", "count":
		operation.Action = ActionScan
		if access.Kind == "count" {
			operation.Action = ActionCount
		}
		operation.Index = access.Index
		operation.From = endpoint(access.From)
		operation.To = endpoint(access.To)

		// Capped rather than refused when it is missing. An operator who does
		// not say is not asking for everything — they have not thought about
		// it, and the honest answer is a page of rows and a flag saying there
		// was more.
		operation.Limit = access.Limit
		if operation.Limit <= 0 || operation.Limit > MostRows {
			operation.Limit = MostRows
		}

	default:
		return Operation{}, fmt.Errorf("%w: a shell can get, scan or count, not %q", ErrDeclaration, access.Kind)
	}

	// Named after what it does, because this is a draft of a declaration and a
	// draft with no name is one nobody will finish.
	operation.Name = access.Collection + "." + access.Kind
	return operation, nil
}

// endpoint turns typed bound values into the terms a declaration holds.
func endpoint(bound *Bound) *Endpoint {
	if bound == nil {
		return nil
	}
	end := &Endpoint{Exclusive: bound.Exclusive, Terms: make([]Term, 0, len(bound.Values))}
	for _, value := range bound.Values {
		end.Terms = append(end.Terms, Term{Value: value, Constant: true})
	}
	return end
}

// Catalogue is what a database holds: the collections and their indexes, and
// the operations declared against them.
type Catalogue struct {
	Collections []Spec      `json:"collections"`
	Operations  []Operation `json:"operations"`
}

// WhatIsHere answers "what is in this database", and records that it was
// asked.
//
// Audited like any other access, and for the same reason twice over: it is the
// first thing somebody who should not be here would ask, and the first thing
// somebody who should be here would ask. A log that shows the scan but not the
// question that found the collection to scan tells half the story.
func (s *Store) WhatIsHere(caller Caller) (Catalogue, error) {
	here := Catalogue{}

	for _, name := range s.Collections() {
		collection, err := s.Collection(name)
		if err != nil {
			return Catalogue{}, err
		}
		here.Collections = append(here.Collections, collection.Spec())
	}

	operations, err := s.Operations()
	if err != nil {
		return Catalogue{}, err
	}
	here.Operations = operations

	if _, err := s.record(Change{
		Kind: ChangeRead,
		By:   Attribution{Operation: "catalogue", Actor: caller.Actor},
	}); err != nil {
		return Catalogue{}, err
	}
	if err := s.Commit(); err != nil {
		return Catalogue{}, err
	}

	return here, nil
}
