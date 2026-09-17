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
)

// Page header: checksum, kind, page id. The checksum covers everything after
// itself, so it is computed over the page as written.
const (
	offChecksum = 0  // u32
	offKind     = 4  // u8
	offReserved = 5  // 3 bytes, zero
	offPageID   = 8  // u64
	HeaderBytes = 16 // payload starts here
)

// Meta page layout, after the common page header.
const (
	offMagic     = HeaderBytes      // 8
	offFormat    = HeaderBytes + 8  // u16
	offPageSize  = HeaderBytes + 10 // u32
	offTxID      = HeaderBytes + 16 // u64
	offRoot      = HeaderBytes + 24 // u64
	offFreelist  = HeaderBytes + 32 // u64
	offPageCount = HeaderBytes + 40 // u64
)

var (
	ErrNotRsql      = errors.New("rsql/pager: not an rsql file")
	ErrFormat       = errors.New("rsql/pager: file format is from another version")
	ErrChecksum     = errors.New("rsql/pager: page failed its checksum")
	ErrNoMeta       = errors.New("rsql/pager: neither meta page is readable")
	ErrPageKind     = errors.New("rsql/pager: page is not of the expected kind")
	ErrQuota        = errors.New("rsql/pager: database is at its page limit")
	ErrOutOfRange   = errors.New("rsql/pager: page id past the end of the file")
	ErrReadOnlyPage = errors.New("rsql/pager: meta pages are written by Commit")
	ErrTruncated    = errors.New("rsql/pager: the file is shorter than its meta page says")
)

// castagnoli is the polynomial with hardware support on the machines this runs
// on, so checking every page on every read costs close to nothing.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Meta is what one completed transaction left behind.
type Meta struct {
	TxID      uint64
	Root      uint64
	Freelist  uint64
	PageCount uint64
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
}

// Create writes a fresh database: two meta pages, no data.
func Create(file vfs.File, maxPages uint64) (*Pager, error) {
	pager := &Pager{file: file, maxPages: maxPages, meta: Meta{TxID: 1, Root: 0, Freelist: 0, PageCount: 2}}

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

// Open reads an existing database and takes the newer of its two meta pages.
func Open(file vfs.File, maxPages uint64) (*Pager, error) {
	pager := &Pager{file: file, maxPages: maxPages}

	first, firstErr := pager.readMeta(0)
	second, secondErr := pager.readMeta(1)

	switch {
	case firstErr != nil && secondErr != nil:
		// Both unreadable: say which way it failed, since "not our file" and
		// "our file, damaged" call for different answers.
		if errors.Is(firstErr, ErrNotRsql) || errors.Is(firstErr, ErrFormat) {
			return nil, firstErr
		}
		return nil, fmt.Errorf("%w: %v / %v", ErrNoMeta, firstErr, secondErr)

	case secondErr != nil:
		pager.meta, pager.nextMeta = first, 1

	case firstErr != nil:
		pager.meta, pager.nextMeta = second, 0

	case second.TxID > first.TxID:
		pager.meta, pager.nextMeta = second, 0

	default:
		pager.meta, pager.nextMeta = first, 1
	}

	pager.committed = pager.meta.PageCount
	return pager, nil
}

// Meta is the state of the last completed transaction.
func (p *Pager) Meta() Meta { return p.meta }

// Allocate hands out the next page id, refusing to go past the quota.
func (p *Pager) Allocate() (uint64, error) {
	if p.maxPages > 0 && p.meta.PageCount >= p.maxPages {
		return 0, fmt.Errorf("%w: %d pages", ErrQuota, p.maxPages)
	}
	id := p.meta.PageCount
	p.meta.PageCount++
	return id, nil
}

// Dirty reports whether a page was allocated by the transaction in progress.
//
// Nothing a reader can reach points at such a page — the committed meta page
// does not count it — so it may be written again in place. That is what keeps
// copy-on-write from allocating a fresh page every time the same node is
// touched twice before a commit: the copy is owed to readers, and a page no
// reader can see is owed nothing.
func (p *Pager) Dirty(id uint64) bool { return id > 1 && id >= p.committed }

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

	if err := verify(data, id); err != nil {
		return nil, err
	}

	return &Page{ID: id, Kind: data[offKind], Data: data}, nil
}

// Write puts a page in the file. It is not part of the database until Commit
// says so.
func (p *Pager) Write(page *Page) error {
	if page.ID <= 1 {
		return ErrReadOnlyPage
	}
	if len(page.Data) != PageBytes {
		return fmt.Errorf("rsql/pager: page %d is %d bytes, want %d", page.ID, len(page.Data), PageBytes)
	}

	seal(page.Data, page.ID, page.Kind)
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
	if err := p.file.Sync(); err != nil {
		return err
	}

	next := Meta{TxID: p.meta.TxID + 1, Root: root, Freelist: p.meta.Freelist, PageCount: p.meta.PageCount}
	if err := p.writeMeta(p.nextMeta, next); err != nil {
		return err
	}
	if err := p.file.Sync(); err != nil {
		return err
	}

	p.meta = next
	p.nextMeta = 1 - p.nextMeta
	p.committed = next.PageCount
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

	return Meta{
		TxID:      binary.BigEndian.Uint64(data[offTxID:]),
		Root:      binary.BigEndian.Uint64(data[offRoot:]),
		Freelist:  binary.BigEndian.Uint64(data[offFreelist:]),
		PageCount: binary.BigEndian.Uint64(data[offPageCount:]),
	}, nil
}

// Close releases the file. It does not commit: anything not committed was
// never part of the database.
func (p *Pager) Close() error { return p.file.Close() }

// seal writes the page's own header and its checksum.
func seal(data []byte, id uint64, kind uint8) {
	data[offKind] = kind
	data[offReserved], data[offReserved+1], data[offReserved+2] = 0, 0, 0
	binary.BigEndian.PutUint64(data[offPageID:], id)
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
