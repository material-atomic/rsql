package store

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/sapedb/sapedb/internal/ulid"
)

// What decides which partition a document goes in.
//
// The primary key, and nothing else. That is a restriction and it is the one
// that makes everything else work: reading a document by its key, changing it,
// deleting it and writing it all know which file to open without looking
// anywhere first. Partition on a field of the document instead and a delete by
// key has to search every partition to find what it is deleting — which is the
// opposite of what partitions are for.
//
// It costs less than it sounds, because the key of a partitioned collection is
// a ULID and a ULID already carries the millisecond it was made. "Keep ninety
// days" needs no field at all: it is already in the key of every document.
//
// The consequence worth stating out loud: an index on a partitioned collection
// is local to its partition. It has to be, or dropping a partition would mean
// finding and deleting its entries in a global index, and dropping would stop
// being an unlink. Everything about how a scan is allowed to be declared
// follows from that, and is checked where the operation is declared.

// How a collection is divided.
const (
	ByTime = "time"
	ByHash = "hash"
)

// How often a time-partitioned collection starts a new one.
const (
	EveryDay   = "day"
	EveryMonth = "month"
	EveryYear  = "year"
)

// Partition is how a collection is divided into files.
type Partition struct {
	// By is "time" or "hash".
	By string `json:"by"`

	// Every is how long one partition covers: "day", "month" or "year". Time
	// only.
	Every string `json:"every,omitempty"`

	// Keep is how many of those to keep, oldest dropped as new ones start.
	// Zero keeps everything. Time only.
	//
	// This is the reason partitions exist: it turns "delete ninety days of
	// data" into one unlink instead of a walk through a tree that ends with a
	// free list bigger than the data was.
	Keep int `json:"keep,omitempty"`

	// Into is how many partitions a hash spreads over. Hash only.
	Into int `json:"into,omitempty"`
}

// check refuses a division that cannot work, where it is written.
func (p *Partition) check(key Key) error {
	switch p.By {
	case ByTime:
		// The time has to come from somewhere, and the only thing every
		// operation already has is the key. A ULID carries the millisecond it
		// was made; a key the writer chooses carries nothing.
		if key.Type != TypeString || key.Auto != "ulid" {
			return fmt.Errorf("%w: a collection partitioned by time needs a ulid key, because that is what carries the time", ErrDeclaration)
		}
		switch p.Every {
		case EveryDay, EveryMonth, EveryYear:
		default:
			return fmt.Errorf("%w: a partition covers a day, a month or a year, not %q", ErrDeclaration, p.Every)
		}
		if p.Keep < 0 {
			return fmt.Errorf("%w: keeping %d partitions is not a number of partitions", ErrDeclaration, p.Keep)
		}
		if p.Into != 0 {
			return fmt.Errorf("%w: a time partition is not divided into a number of pieces", ErrDeclaration)
		}

	case ByHash:
		if p.Into < 2 {
			return fmt.Errorf("%w: a hash spreads over at least two partitions, not %d", ErrDeclaration, p.Into)
		}
		if p.Into > 4096 {
			return fmt.Errorf("%w: %d partitions is %d open files", ErrDeclaration, p.Into, p.Into)
		}
		if p.Every != "" || p.Keep != 0 {
			return fmt.Errorf("%w: a hash partition has no age, so nothing about it expires", ErrDeclaration)
		}

	default:
		return fmt.Errorf("%w: a collection is divided by time or by hash, not by %q", ErrDeclaration, p.By)
	}
	return nil
}

// name is the partition a key belongs in.
func (p *Partition) name(collection string, key any) (string, error) {
	text, isText := key.(string)
	if !isText {
		return "", fmt.Errorf("%w: %q is partitioned and its key is %T", ErrType, collection, key)
	}

	switch p.By {
	case ByTime:
		at, err := ulid.Time(text)
		if err != nil {
			return "", fmt.Errorf("%w: %q is partitioned by time and %q is not a ulid: %v",
				ErrType, collection, text, err)
		}
		return collection + "-" + period(at, p.Every), nil

	case ByHash:
		sum := fnv.New32a()
		_, _ = sum.Write([]byte(text))
		return fmt.Sprintf("%s-h%03d", collection, sum.Sum32()%uint32(p.Into)), nil
	}

	return "", fmt.Errorf("%w: %q is divided by %q", ErrDeclaration, collection, p.By)
}

// period is the stretch of time a moment falls in, written so that partition
// names sort in the order the periods happened.
func period(at time.Time, every string) string {
	at = at.UTC()
	switch every {
	case EveryDay:
		return at.Format("2006-01-02")
	case EveryMonth:
		return at.Format("2006-01")
	case EveryYear:
		return at.Format("2006")
	}
	return ""
}

// expired is the partitions that have fallen out of what is kept.
//
// Worked out from the names rather than from a clock, so that a database that
// has not been written to for a year does not drop everything the moment
// somebody opens it. What ages a collection is new data arriving, which is the
// same thing that made the partitions.
func (p *Partition) expired(here []string) []string {
	if p.By != ByTime || p.Keep <= 0 || len(here) <= p.Keep {
		return nil
	}
	// Names sort in the order the periods happened, so the oldest are first.
	return here[:len(here)-p.Keep]
}

// samePartition says whether two declarations divide a collection the same way.
func samePartition(before, now *Partition) bool {
	if before == nil || now == nil {
		return before == now
	}
	return *before == *now
}

// describePartition is how a collection is divided, in a sentence.
func describePartition(p *Partition) string {
	if p == nil {
		return "into one file"
	}
	if p.By == ByTime {
		return "by time, a partition per " + p.Every
	}
	return fmt.Sprintf("by hash, over %d partitions", p.Into)
}
