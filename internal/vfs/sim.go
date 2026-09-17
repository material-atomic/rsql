package vfs

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
)

// SectorBytes is the unit a disk is assumed to write atomically. A write
// larger than this can land in part: the engine must never need more than one
// sector to be atomic.
const SectorBytes = 512

// ErrPowerCut is returned by every operation after the power has gone.
var ErrPowerCut = errors.New("rsql/vfs: power cut")

// Faults is what a simulated disk is allowed to do to you. Every one of them
// is something a real disk, filesystem or kernel has been observed to do.
type Faults struct {
	// TornWrite is the chance, per write crossing a sector boundary, that only
	// some of its sectors land. A write is atomic within one sector and
	// nowhere else.
	TornWrite float64

	// LyingSync is the chance, per Sync, that it reports success while leaving
	// the data where it was. Consumer drives with write caches do this, and so
	// do some virtualised filesystems.
	LyingSync float64

	// ReorderWrites lets writes since the last Sync become durable in an order
	// of the simulator's choosing rather than the order they were made.
	// Without a Sync between them, a disk promises nothing about order.
	ReorderWrites bool

	// PowerCutAfter cuts the power once this many writes have been made. Zero
	// leaves the power on.
	PowerCutAfter int
}

// SimDisk is a file that lives in memory and can be told to misbehave.
//
// It keeps two images: what is durable — what a restart would find — and what
// has been written since the last successful Sync. A power cut throws the
// second away.
type SimDisk struct {
	durable []byte
	pending map[int64]byte
	// order is the sequence pending writes were made in, so a crash can honour
	// or scramble it.
	order []int64

	random *rand.Rand
	faults Faults

	writes int
	cut    bool
	closed bool

	// Trace records what the simulator did, so a failing seed can be read back.
	Trace []string
}

// NewSim opens an empty simulated disk. The same seed and the same sequence of
// calls always produce the same faults, so a failure replays exactly.
func NewSim(seed int64, faults Faults) *SimDisk {
	return &SimDisk{
		pending: map[int64]byte{},
		random:  rand.New(rand.NewSource(seed)),
		faults:  faults,
	}
}

func (d *SimDisk) logf(format string, args ...any) {
	d.Trace = append(d.Trace, fmt.Sprintf(format, args...))
}

func (d *SimDisk) alive() error {
	if d.closed {
		return ErrClosed
	}
	if d.cut {
		return ErrPowerCut
	}
	return nil
}

// ReadAt reads what the file looks like right now: durable bytes, with
// anything written since laid over them.
func (d *SimDisk) ReadAt(p []byte, off int64) (int, error) {
	if err := d.alive(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, errors.New("rsql/vfs: negative offset")
	}

	size := d.size()
	if off >= size {
		return 0, fmt.Errorf("rsql/vfs: read at %d past end %d", off, size)
	}

	n := 0
	for i := range p {
		at := off + int64(i)
		if at >= size {
			break
		}
		if value, ok := d.pending[at]; ok {
			p[i] = value
		} else if at < int64(len(d.durable)) {
			p[i] = d.durable[at]
		} else {
			p[i] = 0
		}
		n++
	}

	if n < len(p) {
		return n, fmt.Errorf("rsql/vfs: short read of %d of %d bytes", n, len(p))
	}
	return n, nil
}

// WriteAt writes bytes that are not durable until Sync says so — and may be
// torn on the way, if the faults allow it.
func (d *SimDisk) WriteAt(p []byte, off int64) (int, error) {
	if err := d.alive(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, errors.New("rsql/vfs: negative offset")
	}

	d.writes++

	landed := len(p)
	if d.faults.TornWrite > 0 && crossesSector(off, len(p)) && d.random.Float64() < d.faults.TornWrite {
		// A torn write lands whole sectors, not whole calls.
		sectors := sectorsOf(off, len(p))
		keep := d.random.Intn(sectors) // 0..sectors-1 of them
		landed = sectorPrefix(off, len(p), keep)
		d.logf("write %d bytes at %d torn after %d bytes", len(p), off, landed)
	}

	for i := 0; i < landed; i++ {
		at := off + int64(i)
		if _, seen := d.pending[at]; !seen {
			d.order = append(d.order, at)
		}
		d.pending[at] = p[i]
	}

	if d.faults.PowerCutAfter > 0 && d.writes >= d.faults.PowerCutAfter {
		d.logf("power cut after %d writes", d.writes)
		d.cut = true
		return landed, ErrPowerCut
	}

	if landed < len(p) {
		return landed, nil
	}
	return len(p), nil
}

// Sync makes what has been written durable — unless this is one of the syncs
// that lies.
func (d *SimDisk) Sync() error {
	if err := d.alive(); err != nil {
		return err
	}

	if d.faults.LyingSync > 0 && d.random.Float64() < d.faults.LyingSync {
		d.logf("sync lied about %d pending bytes", len(d.pending))
		return nil
	}

	d.commit(len(d.order))
	d.logf("sync made everything durable")
	return nil
}

// commit moves the first n pending writes into the durable image.
func (d *SimDisk) commit(n int) {
	offsets := d.order
	if n < len(offsets) {
		offsets = offsets[:n]
	}

	for _, at := range offsets {
		value, ok := d.pending[at]
		if !ok {
			continue
		}
		for int64(len(d.durable)) <= at {
			d.durable = append(d.durable, 0)
		}
		d.durable[at] = value
		delete(d.pending, at)
	}

	if n >= len(d.order) {
		d.order = d.order[:0]
	} else {
		d.order = append([]int64(nil), d.order[n:]...)
	}
}

func (d *SimDisk) size() int64 {
	size := int64(len(d.durable))
	for at := range d.pending {
		if at+1 > size {
			size = at + 1
		}
	}
	return size
}

// Size is what a restart would report, plus anything written since.
func (d *SimDisk) Size() (int64, error) {
	if err := d.alive(); err != nil {
		return 0, err
	}
	return d.size(), nil
}

// Truncate cuts the file. It takes effect at once, durable and pending alike —
// a shortening a restart would see.
func (d *SimDisk) Truncate(size int64) error {
	if err := d.alive(); err != nil {
		return err
	}
	if size < 0 {
		return errors.New("rsql/vfs: negative size")
	}

	if size < int64(len(d.durable)) {
		d.durable = d.durable[:size]
	} else {
		for int64(len(d.durable)) < size {
			d.durable = append(d.durable, 0)
		}
	}

	for at := range d.pending {
		if at >= size {
			delete(d.pending, at)
		}
	}
	kept := d.order[:0]
	for _, at := range d.order {
		if at < size {
			kept = append(kept, at)
		}
	}
	d.order = kept

	d.logf("truncate to %d", size)
	return nil
}

// Close ends this handle. A closed disk still remembers what was durable.
func (d *SimDisk) Close() error {
	d.closed = true
	return nil
}

// Crash throws away everything that was not durable and hands back the file a
// restart would find. The disk this was called on is spent.
//
// With ReorderWrites, a prefix of the pending writes survives in an order of
// the simulator's choosing — which is what a disk with a write cache may leave
// behind when the power goes between two syncs.
func (d *SimDisk) Crash() *SimDisk {
	if d.faults.ReorderWrites && len(d.order) > 0 {
		scrambled := append([]int64(nil), d.order...)
		d.random.Shuffle(len(scrambled), func(i, j int) { scrambled[i], scrambled[j] = scrambled[j], scrambled[i] })
		survivors := d.random.Intn(len(scrambled) + 1)
		d.order = scrambled
		d.commit(survivors)
		d.logf("crash kept %d of %d pending writes, reordered", survivors, len(scrambled))
	}

	image := append([]byte(nil), d.durable...)
	d.cut = true

	restarted := NewSim(d.random.Int63(), d.faults)
	restarted.durable = image
	restarted.Trace = append(restarted.Trace, fmt.Sprintf("restarted with %d durable bytes", len(image)))
	return restarted
}

// Durable is the image a restart would find right now, for a test to inspect.
func (d *SimDisk) Durable() []byte {
	return append([]byte(nil), d.durable...)
}

// Pending is how many bytes have been written but not made durable.
func (d *SimDisk) Pending() int {
	return len(d.pending)
}

// PowerIsOut reports whether the power has been cut.
func (d *SimDisk) PowerIsOut() bool {
	return d.cut
}

func crossesSector(off int64, length int) bool {
	if length == 0 {
		return false
	}
	return off/SectorBytes != (off+int64(length)-1)/SectorBytes
}

func sectorsOf(off int64, length int) int {
	first := off / SectorBytes
	last := (off + int64(length) - 1) / SectorBytes
	return int(last-first) + 1
}

// sectorPrefix is how many bytes of a write land when only the first `keep`
// sectors of it do.
func sectorPrefix(off int64, length int, keep int) int {
	if keep <= 0 {
		return 0
	}
	boundary := ((off / SectorBytes) + int64(keep)) * SectorBytes
	landed := boundary - off
	if landed > int64(length) {
		return length
	}
	return int(landed)
}

// SortedOffsets is the pending offsets in order, for a test that wants to look.
func (d *SimDisk) SortedOffsets() []int64 {
	out := append([]int64(nil), d.order...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
