package pager

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/sapedb/sapedb/internal/vfs"
)

// A led page store keeps pages and no commit point.
//
// One database will soon be several files — a partition that can be dropped by
// unlinking it, and a day of data that can expire without rewriting a tree.
// The moment there is more than one file, the question is what makes a
// transaction across them atomic, and there is only one answer that does not
// end in a recovery path: exactly one place where a commit becomes true.
//
// So these files have no meta pages. Where their tree root is, where their
// free list starts and how long they are lives in the leader's meta, which is
// written and synced last. A crash between the two leaves pages in a led file
// that nothing points at — the same harmless state a crash mid-transaction
// leaves in a single file, and the same code frees them, because the leader's
// free list is the one that was committed.
//
// What a led file does keep is page 0, saying what it is: the magic, the
// format, the page size, and the salt its pages are encrypted under. Written
// once when the file is made and never again, so it is not a second commit
// point — it is a label. Without it, "this is not an sapedb file" and "this is
// an sapedb file and the key is wrong" would be one answer, and a file dropped
// into the directory by accident would be read as pages.
//
// The cost is that a led file cannot be opened on its own. That is not a
// limitation to work around later: it is one database spread over files, and a
// partition without the leader that says which transaction it belongs to is
// not a smaller database, it is half of one.

// ErrLed is a led store being asked to do what only a leader can.
var ErrLed = errors.New("sapedb/pager: this file has no meta page; its leader commits for it")

// ErrNotLed is a leader being asked to do what only a led store can.
var ErrNotLed = errors.New("sapedb/pager: this file has meta pages of its own")

// Led is everything the leader remembers about one led store.
//
// It is what a Meta holds minus the facts that describe the key, because those
// are in the label and do not change.
type Led struct {
	Root      uint64
	Freelist  uint64
	PageCount uint64
}

// label is page 0 of a led file: what this file is, written once.
const (
	offLabelKey = HeaderBytes + 48 // u8, then the salt and check as in a meta
	labelBytes  = MetaBytes
)

// CreateLed makes a led page store: a label, and nothing else.
func CreateLed(file vfs.File, options Options) (*Pager, error) {
	pager := newPager(file, options.MaxPages)
	pager.reserved = 1
	pager.led = true
	pager.meta = Meta{TxID: 1, PageCount: 1}

	if len(options.Key) > 0 {
		if _, err := rand.Read(pager.meta.Salt[:]); err != nil {
			return nil, fmt.Errorf("sapedb/pager: no randomness for the salt: %w", err)
		}
		if err := makeCheck(options.Key, pager.meta.Salt[:], pager.meta.Check[:]); err != nil {
			return nil, err
		}
		cipher, err := newCrypt(options.Key, pager.meta.Salt[:])
		if err != nil {
			return nil, err
		}
		pager.cipher = cipher
		pager.meta.Encrypted = true
	}

	if err := pager.writeLabel(); err != nil {
		return nil, err
	}
	// Synced here because nothing will write this page again. A label that
	// reached the disk only because a later commit happened to flush it would
	// be a file that is sometimes not an sapedb file.
	if err := file.Sync(); err != nil {
		return nil, err
	}

	pager.committed = pager.meta.PageCount
	return pager, nil
}

// OpenLed opens a led store at the point its leader says it is.
func OpenLed(file vfs.File, at Led, options Options) (*Pager, error) {
	pager := newPager(file, options.MaxPages)
	pager.reserved = 1
	pager.led = true

	label, err := pager.readLabel()
	if err != nil {
		return nil, err
	}

	switch {
	case label.Encrypted && len(options.Key) == 0:
		return nil, fmt.Errorf("%w: no key was given", ErrKey)

	case label.Encrypted:
		if err := openCheck(options.Key, label.Salt[:], label.Check[:]); err != nil {
			return nil, err
		}
		cipher, err := newCrypt(options.Key, label.Salt[:])
		if err != nil {
			return nil, err
		}
		pager.cipher = cipher

	case len(options.Key) > 0:
		return nil, ErrNotEncrypted
	}

	// The leader is the authority on where this file is. Pages past PageCount
	// are what an interrupted transaction left, and are not read, not freed
	// and not counted — the next write simply allocates over them.
	pager.meta = Meta{
		TxID: 1, Root: at.Root, Freelist: at.Freelist, PageCount: at.PageCount,
		Encrypted: label.Encrypted, Salt: label.Salt, Check: label.Check,
	}
	if pager.meta.PageCount < 1 {
		pager.meta.PageCount = 1
	}
	pager.committed = pager.meta.PageCount

	if err := pager.readFreelist(pager.meta.Freelist); err != nil {
		return nil, err
	}
	return pager, nil
}

// Flush makes this store's pages durable and says where it now is.
//
// Everything Commit does except deciding that it happened. The leader takes
// what comes back, records it, and its own commit is what makes this true.
func (p *Pager) Flush(root uint64) (Led, error) {
	if !p.led {
		return Led{}, ErrNotLed
	}

	// The same order as a commit, because it is the same reasoning: the list
	// pages from last time become this transaction's garbage, freed before the
	// roll so they wait a transaction like anything else.
	for _, id := range p.free.chain {
		p.Free(id)
	}
	if len(p.free.freeing) > 0 {
		next := p.meta.TxID + 1
		p.free.pending[next] = append(p.free.pending[next], p.free.freeing...)
		p.free.freeing = nil
	}

	head, chain, err := p.writeFreelist()
	if err != nil {
		return Led{}, err
	}

	// Synced before the leader writes anything of its own. The leader's meta
	// is what says these pages are part of the database, and it must not be
	// able to say so before they are on the disk.
	if err := p.file.Sync(); err != nil {
		return Led{}, err
	}

	p.meta.TxID++
	p.meta.Root, p.meta.Freelist = root, head
	p.committed = p.meta.PageCount
	p.free.chain = chain
	p.free.seen = map[uint64]bool{}
	p.taken = map[uint64]bool{}

	return Led{Root: p.meta.Root, Freelist: p.meta.Freelist, PageCount: p.meta.PageCount}, nil
}

// Abandon puts a led store back where its leader says it is.
//
// The leader's own rollback reads its committed meta again; this is the same
// question asked of the same authority, which is why it takes the answer
// rather than finding one.
func (p *Pager) Abandon(at Led) error {
	if !p.led {
		return ErrNotLed
	}
	p.meta.Root, p.meta.Freelist, p.meta.PageCount = at.Root, at.Freelist, at.PageCount
	if p.meta.PageCount < 1 {
		p.meta.PageCount = 1
	}
	p.committed = p.meta.PageCount
	p.taken = map[uint64]bool{}
	p.free = newFreelist()
	return p.readFreelist(p.meta.Freelist)
}

// At is where this led store is now, for a leader recording it.
func (p *Pager) At() Led {
	return Led{Root: p.meta.Root, Freelist: p.meta.Freelist, PageCount: p.meta.PageCount}
}

func (p *Pager) writeLabel() error {
	data := make([]byte, labelBytes)

	copy(data[offMagic:], Magic[:])
	binary.BigEndian.PutUint16(data[offFormat:], Format)
	binary.BigEndian.PutUint32(data[offPageSize:], PageBytes)
	if p.meta.Encrypted {
		data[offEncrypted] = 1
	}
	copy(data[offSalt:], p.meta.Salt[:])
	copy(data[offCheck:], p.meta.Check[:])

	seal(data, 0, KindLabel)

	_, err := p.file.WriteAt(data, 0)
	return err
}

func (p *Pager) readLabel() (Meta, error) {
	data := make([]byte, labelBytes)
	if _, err := p.file.ReadAt(data, 0); err != nil {
		return Meta{}, err
	}

	if string(data[offMagic:offMagic+8]) != string(Magic[:]) {
		return Meta{}, ErrNotSapedb
	}
	if format := binary.BigEndian.Uint16(data[offFormat:]); format != Format {
		return Meta{}, fmt.Errorf("%w: file says %d, this build reads %d", ErrFormat, format, Format)
	}
	if size := binary.BigEndian.Uint32(data[offPageSize:]); size != PageBytes {
		return Meta{}, fmt.Errorf("%w: pages are %d bytes, this build uses %d", ErrFormat, size, PageBytes)
	}
	if err := verify(data, 0); err != nil {
		return Meta{}, err
	}
	// A meta page here would be a leader being opened as a led store, which is
	// a mistake worth a different sentence from "damaged".
	if data[offKind] != KindLabel {
		return Meta{}, fmt.Errorf("%w: page 0 is a %d, not a label", ErrPageKind, data[offKind])
	}

	label := Meta{Encrypted: data[offEncrypted] == 1}
	copy(label.Salt[:], data[offSalt:offSalt+SaltBytes])
	copy(label.Check[:], data[offCheck:offCheck+NonceBytes+TagBytes])
	return label, nil
}
