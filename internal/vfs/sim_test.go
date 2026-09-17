package vfs

import (
	"bytes"
	"errors"
	"testing"
)

func write(t *testing.T, disk *SimDisk, off int64, data []byte) {
	t.Helper()
	if _, err := disk.WriteAt(data, off); err != nil && !errors.Is(err, ErrPowerCut) {
		t.Fatalf("write at %d: %v", off, err)
	}
}

func read(t *testing.T, disk *SimDisk, off int64, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := disk.ReadAt(buf, off); err != nil {
		t.Fatalf("read at %d: %v", off, err)
	}
	return buf
}

func TestWritesAreVisibleButNotDurableUntilSync(t *testing.T) {
	disk := NewSim(1, Faults{})

	write(t, disk, 0, []byte("hello"))
	if got := read(t, disk, 0, 5); string(got) != "hello" {
		t.Errorf("a write must be visible to the writer: %q", got)
	}
	if len(disk.Durable()) != 0 {
		t.Errorf("nothing is durable before Sync, got %d bytes", len(disk.Durable()))
	}

	// The power goes before the sync: the write was never there.
	after := disk.Crash()
	if len(after.Durable()) != 0 {
		t.Errorf("a crash before Sync must leave nothing, got %q", after.Durable())
	}

	fresh := NewSim(1, Faults{})
	write(t, fresh, 0, []byte("hello"))
	if err := fresh.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Crash().Durable(); string(got) != "hello" {
		t.Errorf("after Sync a crash must keep it, got %q", got)
	}
}

func TestASyncThatLiesLosesTheWrite(t *testing.T) {
	// Every sync lies: the engine is told yes and the bytes stay where they were.
	disk := NewSim(7, Faults{LyingSync: 1})

	write(t, disk, 0, []byte("important"))
	if err := disk.Sync(); err != nil {
		t.Fatalf("a lying sync still reports success: %v", err)
	}

	if got := disk.Crash().Durable(); len(got) != 0 {
		t.Errorf("the sync lied, so nothing should have survived: %q", got)
	}
}

func TestATornWriteLandsWholeSectorsAndNothingElse(t *testing.T) {
	// Always torn, and big enough to cross sectors.
	disk := NewSim(3, Faults{TornWrite: 1})
	page := bytes.Repeat([]byte("x"), SectorBytes*4)

	n, err := disk.WriteAt(page, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n == len(page) {
		t.Fatalf("this write was meant to tear, got all %d bytes", n)
	}
	if n%SectorBytes != 0 {
		t.Errorf("a tear lands whole sectors; %d bytes is not a multiple of %d", n, SectorBytes)
	}

	if err := disk.Sync(); err != nil {
		t.Fatal(err)
	}
	durable := disk.Durable()
	if len(durable) != n {
		t.Fatalf("durable is %d bytes, the write landed %d", len(durable), n)
	}
	if !bytes.Equal(durable, page[:n]) {
		t.Error("the part that landed must be the front of what was written")
	}
}

func TestAWriteInsideOneSectorIsAtomic(t *testing.T) {
	// The one promise a disk does make, and the one the engine may rely on.
	for seed := int64(0); seed < 50; seed++ {
		disk := NewSim(seed, Faults{TornWrite: 1})
		data := bytes.Repeat([]byte("a"), 64)

		n, err := disk.WriteAt(data, 100) // 100..163, inside one sector
		if err != nil {
			t.Fatal(err)
		}
		if n != len(data) {
			t.Fatalf("seed %d: a write inside one sector must not tear, landed %d of %d", seed, n, len(data))
		}
	}
}

func TestPowerCutStopsEverything(t *testing.T) {
	disk := NewSim(5, Faults{PowerCutAfter: 2})

	write(t, disk, 0, []byte("one"))
	if _, err := disk.WriteAt([]byte("two"), 10); !errors.Is(err, ErrPowerCut) {
		t.Fatalf("the second write is where the power goes, got %v", err)
	}
	if !disk.PowerIsOut() {
		t.Error("the disk should know the power is out")
	}

	if _, err := disk.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrPowerCut) {
		t.Errorf("reads after a power cut: want ErrPowerCut, got %v", err)
	}
	if err := disk.Sync(); !errors.Is(err, ErrPowerCut) {
		t.Errorf("sync after a power cut: want ErrPowerCut, got %v", err)
	}
}

func TestARestartSeesExactlyWhatWasDurable(t *testing.T) {
	disk := NewSim(11, Faults{})

	write(t, disk, 0, []byte("committed"))
	if err := disk.Sync(); err != nil {
		t.Fatal(err)
	}
	write(t, disk, 100, []byte("lost"))

	after := disk.Crash()

	if got := read(t, after, 0, 9); string(got) != "committed" {
		t.Errorf("the synced write must be there: %q", got)
	}
	size, err := after.Size()
	if err != nil {
		t.Fatal(err)
	}
	if size != 9 {
		t.Errorf("the file is what was durable: want 9 bytes, got %d", size)
	}
	if after.Pending() != 0 {
		t.Error("a restarted disk has nothing pending")
	}
}

func TestReorderedWritesKeepAPrefixOfAnArbitraryOrder(t *testing.T) {
	// Without a Sync between them, a disk promises nothing about which writes
	// survive — so the engine must never need two writes to land together.
	survived := map[int]bool{}

	for seed := int64(0); seed < 40; seed++ {
		disk := NewSim(seed, Faults{ReorderWrites: true})
		for i := 0; i < 4; i++ {
			write(t, disk, int64(i)*SectorBytes, []byte{byte('a' + i)})
		}

		durable := disk.Crash().Durable()
		count := 0
		for i := 0; i < 4; i++ {
			at := i * SectorBytes
			if at < len(durable) && durable[at] == byte('a'+i) {
				count++
			}
		}
		survived[count] = true
	}

	if len(survived) < 2 {
		t.Errorf("over 40 seeds the number of surviving writes never varied: %v", survived)
	}
}

func TestSameSeedSameTrace(t *testing.T) {
	run := func(seed int64) ([]string, []byte) {
		disk := NewSim(seed, Faults{TornWrite: 0.5, LyingSync: 0.5, ReorderWrites: true})
		for i := 0; i < 20; i++ {
			data := bytes.Repeat([]byte{byte(i)}, SectorBytes*3)
			_, _ = disk.WriteAt(data, int64(i)*SectorBytes*2)
			if i%3 == 0 {
				_ = disk.Sync()
			}
		}
		after := disk.Crash()
		return disk.Trace, after.Durable()
	}

	firstTrace, firstImage := run(42)
	secondTrace, secondImage := run(42)
	otherTrace, _ := run(43)

	if len(firstTrace) == 0 {
		t.Fatal("a run with faults must record what it did")
	}
	if !equalStrings(firstTrace, secondTrace) {
		t.Errorf("the same seed must replay exactly:\n %v\n %v", firstTrace, secondTrace)
	}
	if !bytes.Equal(firstImage, secondImage) {
		t.Error("the same seed must leave the same image")
	}
	if equalStrings(firstTrace, otherTrace) {
		t.Error("two seeds producing the same trace means the faults are not driven by the seed")
	}
}

func TestTruncateIsSeenByARestart(t *testing.T) {
	disk := NewSim(13, Faults{})
	write(t, disk, 0, bytes.Repeat([]byte("z"), 100))
	if err := disk.Sync(); err != nil {
		t.Fatal(err)
	}

	if err := disk.Truncate(10); err != nil {
		t.Fatal(err)
	}
	size, _ := disk.Size()
	if size != 10 {
		t.Fatalf("size after truncate is %d", size)
	}
	if got := disk.Crash().Durable(); len(got) != 10 {
		t.Errorf("a restart sees the shorter file, got %d bytes", len(got))
	}
}

func TestReadsPastTheEndAreAnError(t *testing.T) {
	disk := NewSim(17, Faults{})
	write(t, disk, 0, []byte("abc"))

	if _, err := disk.ReadAt(make([]byte, 1), 10); err == nil {
		t.Error("reading past the end must be an error, not zeros")
	}
	if _, err := disk.ReadAt(make([]byte, 10), 0); err == nil {
		t.Error("a short read must be an error")
	}
}

func TestAClosedDiskRefusesEverything(t *testing.T) {
	disk := NewSim(19, Faults{})
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := disk.WriteAt([]byte("x"), 0); !errors.Is(err, ErrClosed) {
		t.Errorf("write after close: want ErrClosed, got %v", err)
	}
	if _, err := disk.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Errorf("read after close: want ErrClosed, got %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
