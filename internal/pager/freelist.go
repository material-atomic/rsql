package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// The freelist is what makes copy-on-write affordable: every write leaves the
// pages it replaced behind, and without somewhere to put them a database that
// is only ever updated still grows forever.
//
// The whole difficulty is *when* a page may be handed out again. Three things
// can still be looking at a page a transaction just replaced:
//
//   - The transaction that is running. Its own commit is not durable yet, so
//     everything it replaced still belongs to the last committed state. Those
//     pages wait for the commit.
//   - A crash. After transaction N commits, the two meta pages describe N and
//     N-1, and a crash during N+1 falls back to N. So N+1 may reuse what N
//     replaced — those pages belong to N-1, which nothing will fall back to —
//     but never what it is replacing itself.
//   - A reader. A snapshot taken at transaction S reads the tree of S, so a
//     page freed by transaction N is still part of what any snapshot older
//     than N can see. Held snapshots therefore hold pages back.
//
// Freed pages carry the transaction that freed them for exactly that reason,
// and become available only once no older snapshot is held. A page freed by
// the transaction in progress is a special case: if that transaction also
// allocated it, nothing durable and no reader has ever referred to it, and it
// can be handed straight back out.
const (
	freeNext   = 0 // u64: the rest of the list
	freeCount  = 8 // u32: entries on this page
	freeHeader = 12
	freeEntry  = 16 // u64 transaction, u64 page
)

// freePerPage is how many freed pages one list page records.
const freePerPage = (PageBytes - HeaderBytes - freeHeader) / freeEntry

// ErrFreelist is a free list that does not read back as one.
var ErrFreelist = errors.New("rsql/pager: the free list is not readable")

type freelist struct {
	// ready is free for the taking right now.
	ready []uint64
	// pending is what each committed transaction freed, waiting for the
	// snapshots that can still see those pages to be let go.
	pending map[uint64][]uint64
	// freeing is what the transaction in progress has replaced. It belongs to
	// the last committed state until this one commits.
	freeing []uint64
	// seen stops a page being freed twice in one transaction, which would hand
	// the same page out to two callers.
	seen map[uint64]bool
	// chain is the pages the persisted list occupies. They become garbage as
	// soon as the list is written again.
	chain []uint64
}

func newFreelist() *freelist {
	return &freelist{pending: map[uint64][]uint64{}, seen: map[uint64]bool{}}
}

func (f *freelist) count() int {
	total := len(f.ready)
	for _, ids := range f.pending {
		total += len(ids)
	}
	return total
}

// Free says a page is no longer part of the database.
//
// It is not necessarily free to use: when it may be handed out again is what
// the list is for.
func (p *Pager) Free(id uint64) {
	if id <= 1 || p.free.seen[id] {
		return
	}
	p.free.seen[id] = true

	if p.Dirty(id) {
		// Allocated by this transaction and now unwanted: nothing durable and
		// no reader ever saw it, so it can go straight back out.
		p.free.ready = append(p.free.ready, id)
		return
	}
	p.free.freeing = append(p.free.freeing, id)
}

// Snapshot pins the last committed transaction. Pages that were part of it are
// not handed out again until the snapshot is released.
func (p *Pager) Snapshot() *Snapshot {
	p.readers[p.meta.TxID]++
	return &Snapshot{pager: p, TxID: p.meta.TxID, Root: p.meta.Root}
}

// Snapshot is a hold on the database as one transaction left it.
type Snapshot struct {
	pager *Pager
	TxID  uint64
	Root  uint64
}

// Release lets go. Releasing twice is harmless.
func (s *Snapshot) Release() {
	if s.pager == nil {
		return
	}
	if s.pager.readers[s.TxID] <= 1 {
		delete(s.pager.readers, s.TxID)
	} else {
		s.pager.readers[s.TxID]--
	}
	s.pager = nil
}

// oldestHeld is the earliest transaction any reader is still looking at, or
// zero when nobody is.
func (p *Pager) oldestHeld() uint64 {
	oldest := uint64(0)
	for txid := range p.readers {
		if oldest == 0 || txid < oldest {
			oldest = txid
		}
	}
	return oldest
}

// promote moves whatever is no longer visible to anyone into the ready list.
func (p *Pager) promote() {
	if len(p.free.pending) == 0 {
		return
	}

	oldest := p.oldestHeld()
	transactions := make([]uint64, 0, len(p.free.pending))
	for txid := range p.free.pending {
		transactions = append(transactions, txid)
	}
	// Oldest first, so pages come back in a stable order rather than whatever
	// order the map happens to be in.
	sort.Slice(transactions, func(i, j int) bool { return transactions[i] < transactions[j] })

	for _, txid := range transactions {
		// A page freed by a transaction that has not committed still belongs to
		// the last committed state, which is where a crash would land. That
		// includes the transaction being committed right now: its own garbage
		// becomes available to the next one, never to itself.
		if txid > p.meta.TxID {
			continue
		}
		// A page freed by txid was part of the tree of txid-1 and everything
		// before it. A snapshot older than txid can still reach it.
		if oldest != 0 && oldest < txid {
			continue
		}
		p.free.ready = append(p.free.ready, p.free.pending[txid]...)
		delete(p.free.pending, txid)
	}
}

// writeFreelist puts the list in the file and returns the page it starts at.
//
// The list needs pages of its own, and taking them changes what the list says,
// so the size is settled first and the contents written afterwards.
func (p *Pager) writeFreelist() (uint64, []uint64, error) {
	var ids []uint64
	for {
		needed := (p.free.count() + freePerPage - 1) / freePerPage
		if needed <= len(ids) {
			break
		}
		// Taking a ready page removes an entry as well as adding a page, so
		// this settles rather than chasing itself.
		id, err := p.Allocate()
		if err != nil {
			return 0, nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil, nil
	}

	type record struct{ txid, page uint64 }
	entries := make([]record, 0, p.free.count())
	for _, page := range p.free.ready {
		entries = append(entries, record{0, page}) // free for anyone
	}
	transactions := make([]uint64, 0, len(p.free.pending))
	for txid := range p.free.pending {
		transactions = append(transactions, txid)
	}
	sort.Slice(transactions, func(i, j int) bool { return transactions[i] < transactions[j] })
	for _, txid := range transactions {
		for _, page := range p.free.pending[txid] {
			entries = append(entries, record{txid, page})
		}
	}

	// Written back to front, so every page is written already knowing where the
	// rest of the list continues.
	next := uint64(0)
	for i := len(ids) - 1; i >= 0; i-- {
		from := i * freePerPage
		to := min(from+freePerPage, len(entries))

		page := p.NewPage(ids[i], KindFree)
		payload := page.Payload()
		binary.BigEndian.PutUint64(payload[freeNext:], next)
		binary.BigEndian.PutUint32(payload[freeCount:], uint32(to-from))
		for j, entry := range entries[from:to] {
			at := freeHeader + j*freeEntry
			binary.BigEndian.PutUint64(payload[at:], entry.txid)
			binary.BigEndian.PutUint64(payload[at+8:], entry.page)
		}
		if err := p.Write(page); err != nil {
			return 0, nil, err
		}
		next = ids[i]
	}

	return ids[0], ids, nil
}

// readFreelist reads the list back at startup.
//
// Everything on it is ready: no reader can exist yet, and a page freed by the
// transaction this file was opened at belongs to the one before it, which
// nothing will fall back to.
func (p *Pager) readFreelist(head uint64) error {
	p.free = newFreelist()

	for id := head; id != 0; {
		if id <= 1 || id >= p.meta.PageCount {
			return fmt.Errorf("%w: it runs through page %d of %d", ErrFreelist, id, p.meta.PageCount)
		}
		if len(p.free.chain) > int(p.meta.PageCount) {
			return fmt.Errorf("%w: it does not end", ErrFreelist)
		}

		page, err := p.Read(id)
		if err != nil {
			return err
		}
		if page.Kind != KindFree {
			return fmt.Errorf("%w: page %d is kind %d", ErrFreelist, id, page.Kind)
		}
		p.free.chain = append(p.free.chain, id)

		payload := page.Payload()
		count := int(binary.BigEndian.Uint32(payload[freeCount:]))
		if count > freePerPage {
			return fmt.Errorf("%w: page %d claims %d entries", ErrFreelist, id, count)
		}
		for j := 0; j < count; j++ {
			at := freeHeader + j*freeEntry
			freed := binary.BigEndian.Uint64(payload[at+8:])
			if freed <= 1 || freed >= p.meta.PageCount {
				return fmt.Errorf("%w: it holds page %d of %d", ErrFreelist, freed, p.meta.PageCount)
			}
			p.free.ready = append(p.free.ready, freed)
		}

		id = binary.BigEndian.Uint64(payload[freeNext:])
	}

	return nil
}

// FreePages is how many pages are on the list, for tests and for whatever
// reports on a database.
func (p *Pager) FreePages() int { return p.free.count() }

// FreeSet is every page the list holds, in use by nothing. Nothing in the
// engine needs this; a test that checks the tree and the list never claim the
// same page does.
func (p *Pager) FreeSet() map[uint64]bool {
	set := make(map[uint64]bool, p.free.count()+len(p.free.freeing))
	for _, id := range p.free.ready {
		set[id] = true
	}
	for _, ids := range p.free.pending {
		for _, id := range ids {
			set[id] = true
		}
	}
	for _, id := range p.free.freeing {
		set[id] = true
	}
	return set
}
