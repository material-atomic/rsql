package dbkey

import (
	"encoding/hex"
	"testing"
)

// goldenSecret is the secret every vector below is computed against. Its
// value does not matter — it exists only so every row shares the same
// secret except the one row whose whole point is a different secret.
const goldenSecret = "correct horse battery staple"

// Every hex string in this file was NOT produced by calling dbkey.Key and
// copying its answer — that would only prove dbkey.Key agrees with itself.
// It was produced by a throwaway program (kept outside this repo, in the
// task's scratchpad) that inlined the exact call internal/cli/cli.go made at
// this task's base commit (bd3df4d): hkdf.Key(sha256.New, []byte(secret),
// []byte(account+"/"+db), dbKeyLabel, 32), with dbKeyLabel a literal copy
// of what is now this package's Label constant (see dbkey.go),
//
// run under the same docker image house-rules.md names for Go:
//
//	docker run --rm -v "$PWD":/src -v "$HOME/go/pkg/mod":/go/pkg/mod -w /src \
//	  golang:1.24-alpine sh -c "go run main.go"
//
// So this pins dbkey.Key to what the two pre-refactor duplicated copies
// already agreed on producing, not to whatever this package's own code
// happens to compute today. A refactor that changed the derivation — the
// hash, the byte order of the two strings hkdf.Key takes, which one is the
// info and which is the label, the requested length — would still compile
// and would still make internal/cli and internal/server agree with each
// other, because both call this one function. It would not agree with a
// database file that already exists, and this is the only thing in the
// suite that would notice.
func TestKeyMatchesTheGoldenVectorFromBeforeTheRefactor(t *testing.T) {
	key, err := Key(goldenSecret, "acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Compare the whole key, never a prefix of it. HKDF's output is a
	// deterministic prefix: the first 16 bytes of a 32-byte derivation are
	// also the first 16 bytes of a 31-byte one, because the expansion blocks
	// do not depend on the requested length. So a comparison narrowed to 16
	// or 32 hex characters still matches after someone changes pager.KeyBytes
	// out from under Key — and a 31-byte key writes, reads and serves end to
	// end without complaint, because internal/cli and internal/server both
	// ask this one function for it and so both get the same wrong key. That
	// mutation is observable nowhere else in the tree; measured, a prefix-only
	// comparison here together with a KeyBytes-1 derivation leaves all 14
	// packages green. This assertion is what stands between that and nothing.
	const want = "b2fbc36b64c7a4bb11e410d6e1e01d9d5aaed7d004e4df1237a4a7144d8f4004"
	if got := hex.EncodeToString(key); got != want {
		t.Errorf("Key(%q, %q, %q) = %s, want %s (the value the pre-refactor code produced)",
			goldenSecret, "acme", "main", got, want)
	}
}

// TestKeyTable is the universal claim "Key depends on all three arguments,
// and on how they are combined" turned into a table — one row per axis, each
// row changing exactly one thing from the base row, and each row pinned to
// its own golden hex rather than merely asserted "different from the base".
// A table that only checked "not equal to base" would pass for a mutant that
// collapsed every non-base row onto one shared wrong answer, so long as that
// wrong answer differed from base; pinning each row's own hex closes that.
//
// All five hexes came from the same scratch program as the test above, in
// the same run.
func TestKeyTable(t *testing.T) {
	cases := []struct {
		name                string
		secret, account, db string
		want                string
	}{
		{
			name: "base", secret: goldenSecret, account: "acme", db: "main",
			want: "b2fbc36b64c7a4bb11e410d6e1e01d9d5aaed7d004e4df1237a4a7144d8f4004",
		},
		{
			// Axis: secret. Account and db held at the base row's values.
			name: "secret differs", secret: "a different secret entirely", account: "acme", db: "main",
			want: "8ef9f11e7862f8840a1d224dacfc827fae20e36b6b03d2e1b1d976eaa4628af0",
		},
		{
			// Axis: account. Secret and db held at the base row's values.
			name: "account differs", secret: goldenSecret, account: "bravo", db: "main",
			want: "9ecd7e0819029b8bdcca8b190a56f6c61b89a94067646686c8c4294dcf1cd5c4",
		},
		{
			// Axis: db. Secret and account held at the base row's values.
			name: "db differs", secret: goldenSecret, account: "acme", db: "other",
			want: "95a3c255b57d66d4dec81140837129e910b444a27d05c2c4ffe7eff72a78ffa4",
		},
		{
			// Axis: which of account/db lands in which position. Same two
			// strings as base, swapped — this is the row that would catch a
			// refactor that flipped the two arguments Key hands to the info
			// string, which M2 in the task's mutation catalogue plants.
			name: "account and db swapped", secret: goldenSecret, account: "main", db: "acme",
			want: "bd9f452ea5853bee135ca99138881703559d791904487e4dad7d664ed4ec4417",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, err := Key(c.secret, c.account, c.db)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(key); got != c.want {
				t.Errorf("Key(%q, %q, %q) = %s, want %s", c.secret, c.account, c.db, got, c.want)
			}
		})
	}
}

// TestKeyDoesNotDistinguishWhereTheSlashFallsInAccountOrDB is the finding the
// task asked this file to make and report rather than fix: Key builds its
// info string as account+"/"+name, and that concatenation cannot tell
// ("a/b", "c") apart from ("a", "b/c") — both produce the literal string
// "a/b/c". This row exists so that fact is asserted, not just described.
//
// Whether that is reachable is a separate question, measured in the task's
// Result section rather than assumed here: it turns on whether
// SAPEDB_ACCOUNT/SAPEDB_DB (read by internal/cli with no shape check at all)
// or a connection string's account/db (checked by internal/server's
// usableComponent, which refuses any component containing "/") can ever
// actually carry a "/". This test only pins the arithmetic; it does not
// claim the ambiguity is exploitable.
func TestKeyDoesNotDistinguishWhereTheSlashFallsInAccountOrDB(t *testing.T) {
	left, err := Key(goldenSecret, "a/b", "c")
	if err != nil {
		t.Fatal(err)
	}
	right, err := Key(goldenSecret, "a", "b/c")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(left) != hex.EncodeToString(right) {
		t.Fatalf("expected these to collide (both build the info string %q), but got different keys: %x vs %x",
			"a/b/c", left, right)
	}
	const want = "5c3d27f090a379ed14913c228b177608f14b03b4c5c943ac5ed0354a9eac21c2"
	if got := hex.EncodeToString(left); got != want {
		t.Errorf("the colliding key = %s, want %s", got, want)
	}
}
