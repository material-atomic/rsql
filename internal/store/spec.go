// Package store is collections of JSON documents, their primary keys, and the
// indexes declared on them.
//
// Everything lives in one tree, told apart by what the key starts with: the
// catalogue of collections, the documents in primary-key order, and one
// stretch per index. One tree means one transaction covers a document and
// every index entry that describes it, so an index cannot be left saying
// something the document does not.
//
// Indexes are declared inside the collection and nowhere else. The store never
// decides on its own that something is worth indexing: an index is a cost paid
// on every write, and a cost nobody asked for is the sort that is discovered
// much later.
package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/material-atomic/rsql/internal/keys"
)

// Types a field may be declared as. Declaring it is not bureaucracy: the key
// encoding puts values of different types in different places, so an index
// that receives a number where it was promised a string would sort documents
// away from where anything looks for them.
const (
	TypeString = "string"
	TypeNumber = "number"
	TypeBool   = "bool"
	TypeAny    = "any"
)

// What an index does with documents that have no value for a field.
const (
	MissingSkip  = "skip"
	MissingFirst = "first"
	MissingLast  = "last"
)

var (
	ErrName         = errors.New("rsql/store: that is not a usable name")
	ErrDeclaration  = errors.New("rsql/store: the declaration does not make sense")
	ErrIncompatible = errors.New("rsql/store: this does not match what was declared before")
	ErrNoCollection = errors.New("rsql/store: no such collection")
	ErrNoIndex      = errors.New("rsql/store: no such index")
	ErrNoRollup     = errors.New("rsql/store: no such rollup")
	ErrNoKey        = errors.New("rsql/store: the document has no primary key")
	ErrType         = errors.New("rsql/store: the value is not the type the field was declared as")
	ErrDuplicate    = errors.New("rsql/store: a unique index already holds this value")
	ErrDamaged      = errors.New("rsql/store: what is stored does not read back")
)

// Spec is a collection as it was declared.
type Spec struct {
	Name    string  `json:"name"`
	Key     Key     `json:"key"`
	Indexes []Index `json:"indexes"`

	// Partition divides this collection into files, decided by the key. Nil
	// is one file, which is what most collections want. See partition.go.
	Partition *Partition `json:"partition,omitempty"`

	// Rollups are totals kept up to date by every write, in the same
	// transaction as the write. See rollup.go.
	Rollups []Rollup `json:"rollups,omitempty"`

	// ID and the counters are assigned by the store. They are in the stored
	// descriptor so that adding an index never renumbers the ones already
	// there — an index id is written into every one of its keys.
	ID           uint32 `json:"id"`
	NextIndexID  uint16 `json:"next_index_id"`
	NextRollupID uint16 `json:"next_rollup_id"`
}

// Key is the primary key: where it lives in the document and what it is.
//
// The path is a single field rather than a dotted one. A primary key that
// moves around inside the document is a primary key nobody can point at.
type Key struct {
	Path string `json:"path"`
	Type string `json:"type"`
	// Auto is "ulid" to have one made when the document does not carry it, or
	// empty to require the writer to supply it.
	Auto string `json:"auto,omitempty"`
}

// Index is one declared index.
type Index struct {
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
	Unique bool    `json:"unique,omitempty"`
	// Array is the one field whose array elements are indexed separately, if
	// any. One per index: two would multiply out to every pair of elements,
	// which is how an index ends up larger than the data.
	Array string `json:"array,omitempty"`
	// Include is carried in the index entry so that a read that wants only
	// these fields never touches the document.
	Include []string `json:"include,omitempty"`

	ID uint16 `json:"id"`
}

// Field is one component of an index key.
type Field struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Descending bool   `json:"descending,omitempty"`
	// Missing must be said out loud. A default here would silently decide
	// which documents a range query returns.
	Missing string `json:"missing"`
}

func (f Field) encoding() keys.Field {
	field := keys.Field{Descending: f.Descending}
	switch f.Missing {
	case MissingFirst:
		field.Missing = keys.MissingFirst
	case MissingLast:
		field.Missing = keys.MissingLast
	default:
		field.Missing = keys.MissingSkip
	}
	return field
}

// validate says whether a declaration can be stored at all, and why not.
func (s *Spec) validate() error {
	if err := usableName(s.Name); err != nil {
		return fmt.Errorf("collection name: %w", err)
	}

	if strings.Contains(s.Key.Path, ".") || s.Key.Path == "" {
		return fmt.Errorf("%w: the primary key must be one field of the document, not %q", ErrDeclaration, s.Key.Path)
	}
	switch s.Key.Type {
	case TypeString, TypeNumber:
	default:
		return fmt.Errorf("%w: a primary key is a string or a number, not %q", ErrDeclaration, s.Key.Type)
	}

	if s.Partition != nil {
		if err := s.Partition.check(s.Key); err != nil {
			return err
		}
	}

	named := map[string]bool{}
	for i := range s.Rollups {
		if err := s.Rollups[i].validate(); err != nil {
			return err
		}
		if named[s.Rollups[i].Name] {
			return fmt.Errorf("%w: two rollups of %q are called %q", ErrDeclaration, s.Name, s.Rollups[i].Name)
		}
		named[s.Rollups[i].Name] = true
	}
	switch s.Key.Auto {
	case "":
	case "ulid":
		if s.Key.Type != TypeString {
			return fmt.Errorf("%w: a generated ULID is a string, but the key is declared %q", ErrDeclaration, s.Key.Type)
		}
	default:
		return fmt.Errorf("%w: %q is not something the store knows how to generate", ErrDeclaration, s.Key.Auto)
	}

	seen := map[string]bool{}
	for i := range s.Indexes {
		index := &s.Indexes[i]
		if err := usableName(index.Name); err != nil {
			return fmt.Errorf("index name: %w", err)
		}
		if seen[index.Name] {
			return fmt.Errorf("%w: two indexes are called %q", ErrDeclaration, index.Name)
		}
		seen[index.Name] = true

		if len(index.Fields) == 0 {
			return fmt.Errorf("%w: index %q has no fields", ErrDeclaration, index.Name)
		}

		paths := map[string]bool{}
		for _, field := range index.Fields {
			if field.Path == "" {
				return fmt.Errorf("%w: index %q has a field with no path", ErrDeclaration, index.Name)
			}
			if paths[field.Path] {
				return fmt.Errorf("%w: index %q names %q twice", ErrDeclaration, index.Name, field.Path)
			}
			paths[field.Path] = true

			switch field.Type {
			case TypeString, TypeNumber, TypeBool, TypeAny:
			default:
				return fmt.Errorf("%w: index %q declares %q as %q", ErrDeclaration, index.Name, field.Path, field.Type)
			}
			switch field.Missing {
			case MissingSkip, MissingFirst, MissingLast:
			default:
				return fmt.Errorf("%w: index %q must say what happens to documents without %q (skip, first or last)",
					ErrDeclaration, index.Name, field.Path)
			}
		}

		if index.Array != "" && !paths[index.Array] {
			return fmt.Errorf("%w: index %q spreads over %q, which is not one of its fields", ErrDeclaration, index.Name, index.Array)
		}
		for _, path := range index.Include {
			if path == "" {
				return fmt.Errorf("%w: index %q includes a field with no path", ErrDeclaration, index.Name)
			}
		}
	}

	return nil
}

// sameShape reports whether an index is the one that was already built, or a
// different index wearing its name.
func sameShape(first, second Index) bool {
	if first.Unique != second.Unique || first.Array != second.Array {
		return false
	}
	if len(first.Fields) != len(second.Fields) || len(first.Include) != len(second.Include) {
		return false
	}
	for i := range first.Fields {
		if first.Fields[i] != second.Fields[i] {
			return false
		}
	}
	for i := range first.Include {
		if first.Include[i] != second.Include[i] {
			return false
		}
	}
	return true
}

func usableName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: it is empty", ErrName)
	case len(name) > 128:
		return fmt.Errorf("%w: %d characters is more than 128", ErrName, len(name))
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: it holds a zero byte", ErrName)
	}
	return nil
}
