package pager

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/vfs"
)

var secret = []byte("a passphrase nobody else has")

func encrypted(t *testing.T, seed int64, key []byte) (*vfs.SimDisk, *Pager) {
	t.Helper()
	disk := vfs.NewSim(seed, vfs.Faults{})
	pager, err := CreateWith(disk, Options{Key: key})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return disk, pager
}

// marked writes a page whose payload is easy to look for in the file.
func marked(t *testing.T, pager *Pager, text string) uint64 {
	t.Helper()
	id, err := pager.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	page := pager.NewPage(id, KindLeaf)
	copy(page.Payload(), text)
	if err := pager.Write(page); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAnEncryptedDatabaseReadsBackWithItsKey(t *testing.T) {
	disk, pager := encrypted(t, 70, secret)

	ids := make([]uint64, 0, 40)
	for i := 0; i < 40; i++ {
		ids = append(ids, marked(t, pager, fmt.Sprintf("page number %d", i)))
	}
	if err := pager.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(70, vfs.Faults{})
	restart.Restore(disk.Durable())
	reopened, err := OpenWith(restart, Options{Key: secret})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	for i, id := range ids {
		page, err := reopened.Read(id)
		if err != nil {
			t.Fatalf("read page %d: %v", id, err)
		}
		want := fmt.Sprintf("page number %d", i)
		if got := string(bytes.TrimRight(page.Payload()[:len(want)], "\x00")); got != want {
			t.Errorf("page %d reads %q, want %q", id, got, want)
		}
	}
}

// The point of the whole thing: what is on the disk is not the documents.
func TestTheFileDoesNotHoldWhatWasWritten(t *testing.T) {
	const text = "MEDICAL-RECORD-OF-A-REAL-PERSON"

	// First without a key, to prove the search would find it if it were there.
	plain, pager := encrypted(t, 71, nil)
	id := marked(t, pager, text)
	if err := pager.Commit(id); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain.Durable(), []byte(text)) {
		t.Fatal("an unencrypted database does not hold its own contents; this test proves nothing as written")
	}

	// And then with one.
	locked, keyed := encrypted(t, 72, secret)
	id = marked(t, keyed, text)
	if err := keyed.Commit(id); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(locked.Durable(), []byte(text)) {
		t.Error("the file holds the plaintext of what was written to it")
	}
	// Nor in any of the pieces of it that a careless cipher would leave.
	for _, piece := range []string{text[:12], text[8:20], "REAL-PERSON"} {
		if bytes.Contains(locked.Durable(), []byte(piece)) {
			t.Errorf("the file holds %q", piece)
		}
	}
}

func TestTheWrongKeyIsRefusedBeforeAnythingIsRead(t *testing.T) {
	disk, pager := encrypted(t, 73, secret)
	id := marked(t, pager, "something")
	if err := pager.Commit(id); err != nil {
		t.Fatal(err)
	}
	image := disk.Durable()

	for name, key := range map[string][]byte{
		"a different passphrase": []byte("not the passphrase at all"),
		"one character out":      []byte("a passphrase nobody else ha"),
		"empty":                  nil,
	} {
		t.Run(name, func(t *testing.T) {
			restart := vfs.NewSim(73, vfs.Faults{})
			restart.Restore(image)

			_, err := OpenWith(restart, Options{Key: key})
			if !errors.Is(err, ErrKey) {
				t.Errorf("want ErrKey, got %v", err)
			}
			// Offering no key at all is a different mistake from offering the
			// wrong one, and the message is the only place that can say so.
			if says := strings.Contains(fmt.Sprint(err), "no key was given"); says != (len(key) == 0) {
				t.Errorf("key %q: the message reads %q", key, err)
			}
			// And not the answer given to a file that is not ours at all: the
			// two mistakes are different and have to read differently.
			if errors.Is(err, ErrNotSapedb) || errors.Is(err, ErrNoMeta) {
				t.Errorf("a wrong key reads as a damaged file: %v", err)
			}
		})
	}
}

func TestAKeyOfferedToADatabaseThatHasNoneIsRefused(t *testing.T) {
	disk, pager := encrypted(t, 74, nil)
	id := marked(t, pager, "in the open")
	if err := pager.Commit(id); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(74, vfs.Faults{})
	restart.Restore(disk.Durable())
	if _, err := OpenWith(restart, Options{Key: secret}); !errors.Is(err, ErrNotEncrypted) {
		t.Errorf("want ErrNotEncrypted, got %v", err)
	}
}

// Every page write draws a fresh nonce. Deriving one from the page number and
// the transaction would look tidier and be wrong: this engine writes the same
// page more than once inside a transaction, and a repeated nonce under one key
// takes AES-GCM apart completely.
func TestTheSameContentIsNeverTheSameCiphertext(t *testing.T) {
	_, pager := encrypted(t, 75, secret)

	const text = "exactly the same bytes every time"
	seen := map[string]bool{}

	for i := 0; i < 8; i++ {
		id, err := pager.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		page := pager.NewPage(id, KindLeaf)
		copy(page.Payload(), text)
		if err := pager.Write(page); err != nil {
			t.Fatal(err)
		}

		// What the cipher produced, as it now stands in the page buffer.
		written := string(page.Data[HeaderBytes : HeaderBytes+64])
		if seen[written] {
			t.Fatal("two pages of the same content encrypted to the same bytes")
		}
		seen[written] = true
	}

	// And the same page rewritten inside one transaction, which is the case a
	// nonce derived from the page number would get wrong.
	id := marked(t, pager, text)
	first, err := pager.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	page := pager.NewPage(id, KindLeaf)
	copy(page.Payload(), text)
	if err := pager.Write(page); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Data[offNonce:offNonce+NonceBytes], page.Data[offNonce:offNonce+NonceBytes]) {
		t.Error("rewriting one page in one transaction reused its nonce")
	}
}

func TestAPageThatWasChangedOnDiskIsRefused(t *testing.T) {
	disk, pager := encrypted(t, 76, secret)
	first := marked(t, pager, "one")
	second := marked(t, pager, "two")
	if err := pager.Commit(first); err != nil {
		t.Fatal(err)
	}
	image := disk.Durable()

	for name, damage := range map[string]func(image []byte){
		"a bit flipped in the ciphertext": func(image []byte) {
			image[int(first)*PageBytes+HeaderBytes+9] ^= 0x08
		},
		"a bit flipped in the tag": func(image []byte) {
			image[int(first)*PageBytes+offTag] ^= 0x01
		},
		"a bit flipped in the nonce": func(image []byte) {
			image[int(first)*PageBytes+offNonce] ^= 0x01
		},
		"the payload of another page spliced in": func(image []byte) {
			// The header of the page it claims to be, the contents of another:
			// what an attacker with the file but not the key would try.
			from := int(second) * PageBytes
			to := int(first) * PageBytes
			copy(image[to+offNonce:to+PageBytes], image[from+offNonce:from+PageBytes])
		},
	} {
		t.Run(name, func(t *testing.T) {
			damaged := append([]byte(nil), image...)
			damage(damaged)

			restart := vfs.NewSim(76, vfs.Faults{})
			restart.Restore(damaged)
			reopened, err := OpenWith(restart, Options{Key: secret})
			if err != nil {
				t.Fatalf("the meta pages are untouched, so it must open: %v", err)
			}

			_, err = reopened.Read(first)
			if !errors.Is(err, ErrDecrypt) && !errors.Is(err, ErrChecksum) {
				t.Errorf("want a refusal, got %v", err)
			}
		})
	}
}

// One passphrase used for two databases must not give them one key. The salt
// is what makes that true, and it is drawn per database.
func TestOnePassphraseGivesTwoDatabasesTwoKeys(t *testing.T) {
	const text = "the very same sentence in both"

	first, one := encrypted(t, 77, secret)
	second, two := encrypted(t, 78, secret)

	if one.Meta().Salt == two.Meta().Salt {
		t.Fatal("two databases were made with the same salt")
	}

	a := marked(t, one, text)
	b := marked(t, two, text)
	if err := one.Commit(a); err != nil {
		t.Fatal(err)
	}
	if err := two.Commit(b); err != nil {
		t.Fatal(err)
	}

	// A page of one database, put into the other at the same position, is not
	// readable there — which is what a shared key would have allowed.
	image := first.Durable()
	borrowed := second.Durable()
	copy(image[int(a)*PageBytes:(int(a)+1)*PageBytes], borrowed[int(b)*PageBytes:(int(b)+1)*PageBytes])

	restart := vfs.NewSim(79, vfs.Faults{})
	restart.Restore(image)
	reopened, err := OpenWith(restart, Options{Key: secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Read(a); !errors.Is(err, ErrDecrypt) {
		t.Errorf("a page from another database read here: %v", err)
	}
}

// Encryption must not cost the durability the engine is built on: with honest
// syncs, a committed root still always reads.
func TestAnEncryptedDatabaseSurvivesACrashTheSameWay(t *testing.T) {
	for seed := int64(0); seed < 30; seed++ {
		disk := vfs.NewSim(seed, vfs.Faults{})
		pager, err := CreateWith(disk, Options{Key: secret})
		if err != nil {
			t.Fatal(err)
		}
		root := marked(t, pager, "committed")
		if err := pager.Commit(root); err != nil {
			t.Fatal(err)
		}

		faults := vfs.Faults{TornWrite: 0.5, ReorderWrites: true, PowerCutAfter: int(seed%7) + 1}
		crashing := vfs.NewSim(seed, faults)
		crashing.Restore(disk.Durable())

		if working, err := OpenWith(crashing, Options{Key: secret}); err == nil {
			for i := 0; i < 3; i++ {
				id, err := working.Allocate()
				if err != nil {
					break
				}
				page := working.NewPage(id, KindLeaf)
				copy(page.Payload(), "uncommitted")
				_ = working.Write(page)
				_ = working.Commit(id)
			}
		}

		after, err := OpenWith(crashing.Crash(), Options{Key: secret})
		if err != nil {
			t.Fatalf("seed %d: the file must open after any crash: %v\n%v", seed, err, crashing.Trace)
		}
		page, err := after.Read(after.Meta().Root)
		if err != nil {
			t.Fatalf("seed %d: the surviving root must read: %v\n%v", seed, err, crashing.Trace)
		}
		if got := string(bytes.TrimRight(page.Payload()[:11], "\x00")); got != "committed" && got != "uncommitted" {
			t.Fatalf("seed %d: the root reads as %q", seed, got)
		}
	}
}

// The meta page stays readable without the key on purpose, and that is where
// the answer "your key is wrong" comes from. What it costs is written down
// here as well as in the code: the size of the file and how many transactions
// it has seen are in the open.
func TestTheMetaPageIsReadableWithoutTheKey(t *testing.T) {
	disk, pager := encrypted(t, 80, secret)
	id := marked(t, pager, "hidden")
	if err := pager.Commit(id); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(80, vfs.Faults{})
	restart.Restore(disk.Durable())
	unopened := newPager(restart, 0)

	meta, err := unopened.readMeta(0)
	if err != nil {
		t.Fatalf("the meta page must be readable without the key: %v", err)
	}
	if !meta.Encrypted {
		t.Error("the meta page does not say the database is encrypted")
	}
	if meta.PageCount == 0 {
		t.Error("the meta page does not carry the size")
	}

	// And the whole meta layout still fits under a sector, which is what makes
	// a commit atomic.
	if metaEnd > MetaBytes {
		t.Fatalf("the meta layout runs to %d bytes of %d", metaEnd, MetaBytes)
	}
}

// A key derived for one purpose must not be the key used for another. The page
// key and the value that proves a key is right are both derived from the same
// secret and the same salt, and only the label keeps them apart.
func TestOneSecretGivesDifferentKeysForDifferentPurposes(t *testing.T) {
	salt := bytes.Repeat([]byte{0x5a}, SaltBytes)

	page, err := hkdf.Key(sha256.New, secret, salt, pageLabel, KeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	check, err := hkdf.Key(sha256.New, secret, salt, checkLabel, KeyBytes)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(page, check) {
		t.Error("the key that encrypts pages and the key that checks the passphrase are the same key")
	}

	// And a different salt gives a different key from the same secret, which is
	// what stops two databases sharing one.
	other, err := hkdf.Key(sha256.New, secret, bytes.Repeat([]byte{0xa5}, SaltBytes), pageLabel, KeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(page, other) {
		t.Error("the salt makes no difference to the derived key")
	}
}

// A page buffer that has been written once has a nonce and a tag in it. Writing
// it again must produce a page that reads back, which it only does because the
// checksum is taken with those fields zeroed on both sides of the cipher.
func TestAPageBufferMayBeWrittenTwice(t *testing.T) {
	for _, key := range [][]byte{nil, secret} {
		disk := vfs.NewSim(81, vfs.Faults{})
		pager, err := CreateWith(disk, Options{Key: key})
		if err != nil {
			t.Fatal(err)
		}

		id, err := pager.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		page := pager.NewPage(id, KindLeaf)
		copy(page.Payload(), "written once")
		if err := pager.Write(page); err != nil {
			t.Fatal(err)
		}

		// The same buffer again, without going back through NewPage.
		copy(page.Payload(), "written twice")
		if err := pager.Write(page); err != nil {
			t.Fatal(err)
		}

		back, err := pager.Read(id)
		if err != nil {
			t.Fatalf("key=%v: reading a page written twice: %v", key != nil, err)
		}
		if got := string(bytes.TrimRight(back.Payload()[:13], "\x00")); got != "written twice" {
			t.Errorf("key=%v: the page reads %q", key != nil, got)
		}
	}
}
