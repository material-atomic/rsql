// Package pager is the file format: pages, their checksums, and the one write
// that makes a transaction real.
//
// The shape is copy-on-write. A transaction writes new pages, never the pages
// it read; when it is done it makes them durable and then writes one meta
// page. That meta page is smaller than a sector, so a disk either has the new
// one or it does not — there is no half-written commit, and therefore no
// recovery pass at startup. Recovery code only runs after a crash, which is
// exactly when nobody is watching; the cheapest way to have it be right is not
// to have it.
//
// Two meta pages alternate. Opening a file reads both and takes the newer one
// that checksums; a crash mid-commit leaves the older one, which is the state
// the last completed transaction left behind.
package pager

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/material-atomic/rsql/internal/vfs"
)

// PageBytes is the size of every page in the file.
//
// 4096: what the page cache and most filesystems work in, so a page write is
// one filesystem write, and what the quota is counted in — a plan limit is a
// page count, which is a number the engine can enforce rather than an estimate
// it has to keep.
const PageBytes = 4096

// MetaBytes is how much of a meta page is used. Under a sector, so writing one
// is atomic: the property the whole commit protocol rests on.
const MetaBytes = 256

// Magic marks the file as ours, and Format is the version of the layout.
// Both are read before anything else; a file that does not carry them is not
// opened, rather than read as if it were.
var Magic = [8]byte{'R', 'S', 'Q', 'L', 'D', 'B', 0, 1}

const Format uint16 = 1

// Kinds of page. The kind is written into the page, so a page reached through
// a stale pointer is recognised as the wrong thing rather than parsed as the
// right one.
const (
	KindMeta uint8 = 1
	KindLeaf uint8 = 2
	KindNode uint8 = 3
	KindFree uint8 = 4
	KindBlob uint8 = 5
	// KindLabel is page 0 of a file whose commit point is somewhere else. See
	// led.go.
	KindLabel uint8 = 6
)

// Page header: checksum, kind, page id, and room for a nonce and an
// authentication tag. The checksum covers everything after itself with the
// nonce and tag zeroed, so it is the same value before encryption and after
// decryption.
//
// The nonce and tag are reserved whether or not the database is encrypted, so
// that one layout describes both and turning encryption on is a property of a
// database rather than a second format.
const (
	offChecksum = 0  // u32
	offKind     = 4  // u8
	offReserved = 5  // 3 bytes, zero
	offPageID   = 8  // u64
	offNonce    = 16 // NonceBytes
	offTag      = 28 // TagBytes
	HeaderBytes = 48 // payload starts here
)

// Meta page layout, after the common page header.
//
// A meta page is never encrypted: something has to be readable without the key
// or "not our file" and "wrong key" become one answer. See crypt.go for what
// that leaks and why it is the trade taken.
const (
	offMagic     = HeaderBytes      // 8
	offFormat    = HeaderBytes + 8  // u16
	offPageSize  = HeaderBytes + 10 // u32
	offTxID      = HeaderBytes + 16 // u64
	offRoot      = HeaderBytes + 24 // u64
	offFreelist  = HeaderBytes + 32 // u64
	offPageCount = HeaderBytes + 40 // u64
	offEncrypted = HeaderBytes + 48 // u8
	offSalt      = HeaderBytes + 56 // SaltBytes
	offCheck     = HeaderBytes + 72 // NonceBytes + TagBytes
	metaEnd      = HeaderBytes + 72 + NonceBytes + TagBytes
)

var (
	ErrNotRsql      = errors.New("rsql/pager: not an rsql file")
	ErrFormat       = errors.New("rsql/pager: file format is from another version")
	ErrChecksum     = errors.New("rsql/pager: page failed its checksum")
	ErrNoMeta       = errors.New("rsql/pager: neither meta page is readable")
	ErrPageKind     = errors.New("rsql/pager: page is not of the expected kind")
	ErrQuota        = errors.New("rsql/pager: database is at its page limit")
	ErrOutOfRange   = errors.New("rsql/pager: page id past the end of the file")
	ErrReadOnlyPage = errors.New("rsql/pager: the pages at the front of a file are not written by hand")
	ErrTruncated    = errors.New("rsql/pager: the file is shorter than its meta page says")
)

// castagnoli is the polynomial with hardware support on the machines this runs
// on, so checking every page on every read costs close to nothing.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Meta is what one completed transaction left behind, plus the facts about
// this database that every transaction carries forward.
type Meta struct {
	TxID      uint64
	Root      uint64
	Freelist  uint64
	PageCount uint64

	// Encrypted, Salt and Check describe the key. They do not change after the
	// database is created, and are copied into every meta page so that either
	// one is enough to open the file.
	Encrypted bool
	Salt      [SaltBytes]byte
	Check     [NonceBytes + TagBytes]byte
}

// Page is one page, header and all.
type Page struct {
	ID   uint64
	Kind uint8
	Data []byte // PageBytes long; payload starts at HeaderBytes
}

// Payload is the part of a page a caller may write.
func (p *Page) Payload() []byte {
	return p.Data[HeaderBytes:]
}

// Pager reads and writes pages, and turns a set of written pages into a
// transaction.
type Pager struct {
	file vfs.File
	meta Meta
	// which meta page the next commit writes: commits alternate, so the one
	// being overwritten is never the one a crash would fall back to.
	nextMeta uint64
	// maxPages is the quota, enforced by the engine rather than accounted for
	// elsewhere: a page that would go past it is not allocated.
	maxPages uint64
	// committed is how many pages the last completed transaction had. Anything
	// from here up was allocated by the transaction in progress.
	committed uint64
	// taken is the pages this transaction has allocated from the free list.
	// They are below the committed mark but are no less its own.
	taken map[uint64]bool
	// free is what earlier transactions left behind, and when it may be used.
	free *freelist
	// readers counts the snapshots held at each transaction.
	readers map[uint64]int
	// cipher is how pages are encrypted, or nil for a database with no key.
	cipher *crypt
	// reserved is how many pages at the front are not data: two meta pages in
	// an ordinary file, one label in a led one.
	reserved uint64
	// led says the commit point is in another file. See led.go.
	led bool
}

// Create writes a fresh database: two meta pages, no data.
func Create(file vfs.File, maxPages uint64) (*Pager, error) {
	return CreateWith(file, Options{MaxPages: maxPages})
}

// CreateWith is Create with everything the database is made with, which for
// now means the key it is encrypted under.
func CreateWith(file vfs.File, options Options) (*Pager, error) {
	pager := newPager(file, options.MaxPages)
	pager.meta = Meta{TxID: 1, Root: 0, Freelist: 0, PageCount: 2}

	if len(options.Key) > 0 {
		if _, err := rand.Read(pager.meta.Salt[:]); err != nil {
			return nil, fmt.Errorf("rsql/pager: no randomness for the salt: %w", err)
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

	// Both meta pages, so a first crash still finds one.
	for _, id := range []uint64{0, 1} {
		if err := pager.writeMeta(id, pager.meta); err != nil {
			return nil, err
		}
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}

	pager.nextMeta = 0
	pager.committed = pager.meta.PageCount
	return pager, nil
}

func newPager(file vfs.File, maxPages uint64) *Pager {
	return &Pager{
		file:     file,
		maxPages: maxPages,
		reserved: 2,
		taken:    map[uint64]bool{},
		free:     newFreelist(),
		readers:  map[uint64]int{},
	}
}

// Open reads an existing database and takes the newer of its two meta pages.
func Open(file vfs.File, maxPages uint64) (*Pager, error) {
	return OpenWith(file, Options{MaxPages: maxPages})
}

// OpenWith is Open with the key, when the database has one.
func OpenWith(file vfs.File, options Options) (*Pager, error) {
	pager := newPager(file, options.MaxPages)

	if err := pager.loadMeta(); err != nil {
		return nil, err
	}

	// Whether the key is right is settled here, before a single page of data is
	// read: a wrong key found later looks exactly like a damaged file.
	switch {
	case pager.meta.Encrypted && len(options.Key) == 0:
		return nil, fmt.Errorf("%w: no key was given", ErrKey)

	case pager.meta.Encrypted:
		if err := openCheck(options.Key, pager.meta.Salt[:], pager.meta.Check[:]); err != nil {
			return nil, err
		}
		cipher, err := newCrypt(options.Key, pager.meta.Salt[:])
		if err != nil {
			return nil, err
		}
		pager.cipher = cipher

	case len(options.Key) > 0:
		return nil, ErrNotEncrypted
	}

	if err := pager.readFreelist(pager.meta.Freelist); err != nil {
		return nil, err
	}
	return pager, nil
}

// loadMeta takes the newer of the two meta pages, which is the last completed
// transaction.
//
// One function rather than two, because opening a file and abandoning a
// transaction are the same question — "what did the last commit leave?" — and
// two answers to one question is one of them being wrong eventually.
func (p *Pager) loadMeta() error {
	first, firstErr := p.readMeta(0)
	second, secondErr := p.readMeta(1)

	switch {
	case firstErr != nil && secondErr != nil:
		// Both unreadable: say which way it failed, since "not our file" and
		// "our file, damaged" call for different answers.
		if errors.Is(firstErr, ErrNotRsql) || errors.Is(firstErr, ErrFormat) {
			return firstErr
		}
		return fmt.Errorf("%w: %v / %v", ErrNoMeta, firstErr, secondErr)

	case secondErr != nil:
		p.meta, p.nextMeta = first, 1

	case firstErr != nil:
		p.meta, p.nextMeta = second, 0

	case second.TxID > first.TxID:
		p.meta, p.nextMeta = second, 0

	default:
		p.meta, p.nextMeta = first, 1
	}

	p.committed = p.meta.PageCount
	return nil
}

// Rollback throws away everything written since the last commit.
//
// Copy-on-write makes this nearly free, and that is the design paying for
// itself: nothing written since the commit is durable, nothing durable points
// at any of it, and the pages it used were never counted by the meta on disk.
// So abandoning a transaction is reading the file's own answer to "what did
// the last commit leave?" — the same answer opening it would get.
//
// Nothing is written here. A crash mid-rollback is a crash mid-transaction,
// which was already the case a moment before.
func (p *Pager) Rollback() error {
	if err := p.loadMeta(); err != nil {
		return err
	}

	// Reading the list back is the part that matters: pages the abandoned
	// transaction marked as rubbish are pages the live tree points at again,
	// and anything still holding them as rubbish would hand them out to be
	// written over. Nothing complains when that happens — the damage is found
	// later by whoever reads through a pointer that still leads there.
	//
	// Clearing `taken` is housekeeping rather than a guard: no page of the
	// committed tree can be in it, so nothing observable depends on it. It is
	// here so that "a rollback leaves none of the transaction's bookkeeping"
	// is true without an exception nobody would remember.
	p.taken = map[uint64]bool{}
	return p.readFreelist(p.meta.Freelist)
}

// Pending reports whether anything has been written since the last commit.
//
// A caller that is about to do something it may have to abandon needs to know
// whether abandoning would take somebody else's work with it. There is one
// transaction at a time, so the answer is not "which part is mine" — it is
// "is any of this not mine".
func (p *Pager) Pending() bool {
	return len(p.taken) > 0 || len(p.free.freeing) > 0
}

// Meta is the state of the last completed transaction.
func (p *Pager) Meta() Meta { return p.meta }

// Allocate hands out a page: one an earlier transaction left behind if any is
// free to use, and otherwise a new one at the end of the file.
func (p *Pager) Allocate() (uint64, error) {
	p.promote()

	if last := len(p.free.ready) - 1; last >= 0 {
		id := p.free.ready[last]
		p.free.ready = p.free.ready[:last]
		p.taken[id] = true
		delete(p.free.seen, id)
		return id, nil
	}

	// The quota counts the file, not the tree: reusing a page costs nothing, so
	// only growing is refused.
	if p.maxPages > 0 && p.meta.PageCount >= p.maxPages {
		return 0, fmt.Errorf("%w: %d pages", ErrQuota, p.maxPages)
	}
	id := p.meta.PageCount
	p.meta.PageCount++
	p.taken[id] = true
	return id, nil
}

// Dirty reports whether a page was allocated by the transaction in progress.
//
// Nothing a reader can reach points at such a page — the committed meta page
// does not count it — so it may be written again in place. That is what keeps
// copy-on-write from allocating a fresh page every time the same node is
// touched twice before a commit: the copy is owed to readers, and a page no
// reader can see is owed nothing.
//
// The pages at the front of a file are not excluded here, and a mutation that
// let them through survived every test until it was looked at: they cannot be
// reached. Allocate never hands one out and committed is never below them, so
// the clause was true of nothing. What actually keeps the label and the meta
// pages safe is Write refusing them, which is tested. A guard that cannot be
// observed is not a second line of defence, it is a claim nobody checks.
func (p *Pager) Dirty(id uint64) bool {
	return id >= p.committed || p.taken[id]
}

// NewPage is an empty page of a kind, ready to be filled and written.
func (p *Pager) NewPage(id uint64, kind uint8) *Page {
	page := &Page{ID: id, Kind: kind, Data: make([]byte, PageBytes)}
	return page
}

// Read returns a page, refusing one whose checksum does not hold — a page the
// disk changed under us is not data, and the caller must never see it as if it
// were.
func (p *Pager) Read(id uint64) (*Page, error) {
	if id >= p.meta.PageCount {
		return nil, fmt.Errorf("%w: page %d of %d", ErrOutOfRange, id, p.meta.PageCount)
	}

	data := make([]byte, PageBytes)
	if _, err := p.file.ReadAt(data, int64(id)*PageBytes); err != nil {
		// The meta page counts this page, so the file should reach it. That it
		// does not is damage, and is reported as ours rather than as the disk's.
		return nil, fmt.Errorf("%w: page %d: %v", ErrTruncated, id, err)
	}

	if p.cipher != nil {
		if err := p.cipher.decrypt(data, id); err != nil {
			return nil, err
		}
	}

	if err := verify(data, id); err != nil {
		return nil, err
	}

	return &Page{ID: id, Kind: data[offKind], Data: data}, nil
}

// Write puts a page in the file. It is not part of the database until Commit
// says so.
func (p *Pager) Write(page *Page) error {
	if page.ID < p.reserved {
		return ErrReadOnlyPage
	}
	if len(page.Data) != PageBytes {
		return fmt.Errorf("rsql/pager: page %d is %d bytes, want %d", page.ID, len(page.Data), PageBytes)
	}

	seal(page.Data, page.ID, page.Kind)
	if p.cipher != nil {
		if err := p.cipher.encrypt(page.Data); err != nil {
			return err
		}
	}

	_, err := p.file.WriteAt(page.Data, int64(page.ID)*PageBytes)
	return err
}

// Commit makes everything written so far durable, then writes one meta page.
//
// The order is the whole protocol: data first, then a sync, then the meta page
// that points at it, then a sync. A crash before the meta page lands leaves
// the previous transaction, whole. A crash after it leaves the new one, whole.
// There is no third outcome, which is why there is no recovery pass.
func (p *Pager) Commit(root uint64) error {
	if p.led {
		return ErrLed
	}

	next := p.meta
	next.TxID, next.Root = p.meta.TxID+1, root

	// The list pages written last time are replaced by the ones written below,
	// so they are this transaction's garbage like any other page. Freed before
	// the roll, so that they wait a transaction like everything else: a crash
	// here still falls back to a state that points at them.
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
		return err
	}
	next.Freelist = head
	next.PageCount = p.meta.PageCount

	if err := p.file.Sync(); err != nil {
		return err
	}
	if err := p.writeMeta(p.nextMeta, next); err != nil {
		return err
	}
	if err := p.file.Sync(); err != nil {
		return err
	}

	p.meta = next
	p.nextMeta = 1 - p.nextMeta
	p.committed = next.PageCount
	p.free.chain = chain
	p.free.seen = map[uint64]bool{}
	p.taken = map[uint64]bool{}
	return nil
}

func (p *Pager) writeMeta(id uint64, meta Meta) error {
	/* Exactly the bytes that will be written, and no more: the checksum covers
	   what lands on the disk, so it must be computed over the same length that
	   is read back. */
	data := make([]byte, MetaBytes)

	copy(data[offMagic:], Magic[:])
	binary.BigEndian.PutUint16(data[offFormat:], Format)
	binary.BigEndian.PutUint32(data[offPageSize:], PageBytes)
	binary.BigEndian.PutUint64(data[offTxID:], meta.TxID)
	binary.BigEndian.PutUint64(data[offRoot:], meta.Root)
	binary.BigEndian.PutUint64(data[offFreelist:], meta.Freelist)
	binary.BigEndian.PutUint64(data[offPageCount:], meta.PageCount)
	if meta.Encrypted {
		data[offEncrypted] = 1
	}
	copy(data[offSalt:], meta.Salt[:])
	copy(data[offCheck:], meta.Check[:])

	seal(data, id, KindMeta)

	/* One write, under a sector: a meta page has to land in one piece, and a
	   disk only promises that within a sector. */
	_, err := p.file.WriteAt(data, int64(id)*PageBytes)
	return err
}

func (p *Pager) readMeta(id uint64) (Meta, error) {
	data := make([]byte, MetaBytes)
	if _, err := p.file.ReadAt(data, int64(id)*PageBytes); err != nil {
		return Meta{}, err
	}

	if string(data[offMagic:offMagic+8]) != string(Magic[:]) {
		return Meta{}, ErrNotRsql
	}
	if format := binary.BigEndian.Uint16(data[offFormat:]); format != Format {
		return Meta{}, fmt.Errorf("%w: file says %d, this build reads %d", ErrFormat, format, Format)
	}
	if size := binary.BigEndian.Uint32(data[offPageSize:]); size != PageBytes {
		return Meta{}, fmt.Errorf("%w: pages are %d bytes, this build uses %d", ErrFormat, size, PageBytes)
	}
	if err := verify(data, id); err != nil {
		return Meta{}, err
	}
	if data[offKind] != KindMeta {
		return Meta{}, ErrPageKind
	}

	meta := Meta{
		TxID:      binary.BigEndian.Uint64(data[offTxID:]),
		Root:      binary.BigEndian.Uint64(data[offRoot:]),
		Freelist:  binary.BigEndian.Uint64(data[offFreelist:]),
		PageCount: binary.BigEndian.Uint64(data[offPageCount:]),
		Encrypted: data[offEncrypted] == 1,
	}
	copy(meta.Salt[:], data[offSalt:offSalt+SaltBytes])
	copy(meta.Check[:], data[offCheck:offCheck+NonceBytes+TagBytes])
	return meta, nil
}

// Close releases the file. It does not commit: anything not committed was
// never part of the database.
func (p *Pager) Close() error { return p.file.Close() }

// seal writes the page's own header and its checksum.
func seal(data []byte, id uint64, kind uint8) {
	data[offKind] = kind
	data[offReserved], data[offReserved+1], data[offReserved+2] = 0, 0, 0
	binary.BigEndian.PutUint64(data[offPageID:], id)

	// Zero while hashing, and zeroed again after decryption, so the checksum is
	// one value whichever side of the cipher it is computed on.
	for i := offNonce; i < offTag+TagBytes; i++ {
		data[i] = 0
	}
	binary.BigEndian.PutUint32(data[offChecksum:], crc32.Checksum(data[offKind:], castagnoli))
}

// verify checks a page against its own header.
//
// The id is part of what is checked: a page that is whole but is the wrong
// page — read through a stale pointer, or moved by a filesystem that shuffled
// blocks — is caught here rather than parsed as if it belonged.
func verify(data []byte, id uint64) error {
	want := binary.BigEndian.Uint32(data[offChecksum:])
	if got := crc32.Checksum(data[offKind:], castagnoli); got != want {
		return fmt.Errorf("%w: page %d", ErrChecksum, id)
	}
	if got := binary.BigEndian.Uint64(data[offPageID:]); got != id {
		return fmt.Errorf("%w: page %d says it is page %d", ErrChecksum, id, got)
	}
	return nil
}
