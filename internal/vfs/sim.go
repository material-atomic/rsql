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
	// TornWrite is the chance that a write still in flight when the power goes
	// lands as whole sectors of its front and no more. A write is atomic within
	// one sector and nowhere else — but only a write that was never synced can
	// tear: an honest Sync delivers every byte.
	//
	// It also lets a prefix of the pending writes survive a crash at all, which
	// is what makes a power cut mid-transaction something to test rather than a
	// clean loss of everything.
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
// It keeps two things: the durable image — what a restart would find — and the
// writes made since the last successful Sync. A power cut decides what happens
// to those: they may be lost, reordered, or land in part.
//
// A write is recorded whole. Tearing happens at the crash, not at the call: a
// disk that acknowledged a write and then honoured an fsync does deliver every
// byte of it. Only writes still in flight when the power goes can land half
// done, so that is the only place this simulator tears one.
type SimDisk struct {
	durable []byte
	// current is the file as the writer sees it: durable with every pending
	// write laid over it. Kept as an image rather than rebuilt per read, so a
	// long run between syncs does not make reads quadratic.
	current []byte
	// pending is the writes since the last honest Sync, in the order they were
	// made. Each is kept whole: what a crash does to it is decided then.
	pending []pendingWrite

	random *rand.Rand
	faults Faults

	writes int
	cut    bool
	closed bool

	// Trace records what the simulator did, so a failing seed can be read back.
	Trace []string
}

// pendingWrite is one WriteAt that has not become durable yet.
type pendingWrite struct {
	off  int64
	data []byte
}

// NewSim opens an empty simulated disk. The same seed and the same sequence of
// calls always produce the same faults, so a failure replays exactly.
func NewSim(seed int64, faults Faults) *SimDisk {
	return &SimDisk{
		random: rand.New(rand.NewSource(seed)),
		faults: faults,
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

	size := int64(len(d.current))
	if off >= size {
		return 0, fmt.Errorf("rsql/vfs: read at %d past end %d", off, size)
	}

	n := copy(p, d.current[off:])
	if n < len(p) {
		return n, fmt.Errorf("rsql/vfs: short read of %d of %d bytes", n, len(p))
	}
	return n, nil
}

// WriteAt records a write. It is not durable until Sync says so, and what a
// crash makes of it is decided at the crash.
func (d *SimDisk) WriteAt(p []byte, off int64) (int, error) {
	if err := d.alive(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, errors.New("rsql/vfs: negative offset")
	}

	d.writes++
	write := pendingWrite{off: off, data: append([]byte(nil), p...)}
	d.pending = append(d.pending, write)
	d.current = grow(d.current, off+int64(len(p)))
	copy(d.current[off:], write.data)

	if d.faults.PowerCutAfter > 0 && d.writes >= d.faults.PowerCutAfter {
		d.logf("power cut after %d writes", d.writes)
		d.cut = true
		return len(p), ErrPowerCut
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
		d.logf("sync lied about %d pending writes", len(d.pending))
		return nil
	}

	// Everything pending is already in the current image, which is therefore
	// exactly what a restart would now find.
	d.durable = append([]byte(nil), d.current...)
	d.pending = nil
	d.logf("sync made everything durable")
	return nil
}

// apply puts the first n bytes of a write into the durable image.
func (d *SimDisk) apply(write pendingWrite, n int) {
	d.durable = grow(d.durable, write.off+int64(n))
	copy(d.durable[write.off:], write.data[:n])
}

// grow lengthens an image with zeros, the way writing past the end of a file
// leaves a hole.
func grow(image []byte, size int64) []byte {
	for int64(len(image)) < size {
		image = append(image, 0)
	}
	return image
}

// land decides what the pending writes leave behind when the power goes.
//
// With no faults asked for, nothing does: a write that was never synced was
// never there, which is the rule the engine is built against. With faults, a
// prefix of them lands — in an order of the simulator's choosing if writes may
// be reordered — and one of those may land as whole sectors and no more.
func (d *SimDisk) land() {
	if len(d.pending) == 0 {
		return
	}
	if !d.faults.ReorderWrites && d.faults.TornWrite <= 0 {
		d.logf("crash lost all %d pending writes", len(d.pending))
		d.pending = nil
		d.current = append([]byte(nil), d.durable...)
		return
	}

	writes := append([]pendingWrite(nil), d.pending...)
	if d.faults.ReorderWrites {
		d.random.Shuffle(len(writes), func(i, j int) { writes[i], writes[j] = writes[j], writes[i] })
	}
	kept := d.random.Intn(len(writes) + 1)

	for _, write := range writes[:kept] {
		n := len(write.data)
		if d.faults.TornWrite > 0 && crossesSector(write.off, n) && d.random.Float64() < d.faults.TornWrite {
			// A torn write lands whole sectors, not whole calls.
			sectors := sectorsOf(write.off, n)
			n = sectorPrefix(write.off, n, d.random.Intn(sectors))
			d.logf("write of %d bytes at %d torn after %d bytes", len(write.data), write.off, n)
		}
		d.apply(write, n)
	}

	d.logf("crash kept %d of %d pending writes, reordered=%v", kept, len(writes), d.faults.ReorderWrites)
	d.pending = nil
	d.current = append([]byte(nil), d.durable...)
}

func (d *SimDisk) size() int64 { return int64(len(d.current)) }

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
		d.durable = grow(d.durable, size)
	}
	if size < int64(len(d.current)) {
		d.current = d.current[:size]
	} else {
		d.current = grow(d.current, size)
	}

	kept := d.pending[:0]
	for _, write := range d.pending {
		if write.off >= size {
			continue
		}
		if end := write.off + int64(len(write.data)); end > size {
			write.data = write.data[:size-write.off]
		}
		kept = append(kept, write)
	}
	d.pending = kept

	d.logf("truncate to %d", size)
	return nil
}

// Close ends this handle. A closed disk still remembers what was durable.
func (d *SimDisk) Close() error {
	d.closed = true
	return nil
}

// Crash cuts the power and hands back the file a restart would find. The disk
// this was called on is spent.
func (d *SimDisk) Crash() *SimDisk {
	d.land()

	image := append([]byte(nil), d.durable...)
	d.cut = true

	restarted := NewSim(d.random.Int63(), d.faults)
	restarted.durable = image
	restarted.current = append([]byte(nil), image...)
	restarted.Trace = append(restarted.Trace, fmt.Sprintf("restarted with %d durable bytes", len(image)))
	return restarted
}

// Restore puts a durable image into the disk, for a test that starts from the
// file another disk left behind.
func (d *SimDisk) Restore(image []byte) {
	d.durable = append([]byte(nil), image...)
	d.current = append([]byte(nil), image...)
	d.pending = nil
	d.cut = false
	d.writes = 0
}

// Durable is the image a restart would find right now, for a test to inspect.
func (d *SimDisk) Durable() []byte {
	return append([]byte(nil), d.durable...)
}

// Pending is how many bytes have been written but not made durable.
func (d *SimDisk) Pending() int {
	total := 0
	for _, write := range d.pending {
		total += len(write.data)
	}
	return total
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

// SortedOffsets is where the pending writes start, in order, for a test that
// wants to look.
func (d *SimDisk) SortedOffsets() []int64 {
	out := make([]int64, 0, len(d.pending))
	for _, write := range d.pending {
		out = append(out, write.off)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
