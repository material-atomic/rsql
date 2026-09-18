// Package dbkey is the one place that derives the key a database file is
// encrypted with. It exists because internal/cli and internal/server used to
// each carry their own copy of the label and the hkdf.Key call — two source
// locations for one fact that must never disagree, with nothing in the tree
// that noticed if they did. A patrol (internal/naming) could see the label's
// text drift; it could not see someone edit the byte order of the info string
// or swap the label and the info argument, because neither of those changes
// touches the old product name the patrol watches for. Collapsing both
// callers onto this package removes the drift entirely, rather than
// detecting it after the fact.
package dbkey

import (
	"crypto/hkdf"
	"crypto/sha256"

	"github.com/sapedb/sapedb/internal/pager"
)

// Label must not change. It still carries the product's old name on purpose,
// not by oversight: it is baked into every encrypted database file that
// already exists, and changing it — without a migration that re-derives and
// re-encrypts every affected database under the new label — is a silent,
// unannounced key rotation. The key this package derives would stop matching
// the key the file was written under, which reads as "wrong secret" for a
// secret that is right. This constant moving is a migration's job, not a
// refactor's.
const Label = "rsql/server:database:v1"

// Key derives the key a database file is encrypted with, from the secret
// that is the single root of trust and the account/name that names the
// database. Both internal/cli (writing a file) and internal/server (opening
// it) must call exactly this, or the two processes derive different keys for
// the same file.
func Key(secret, account, name string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(secret), []byte(account+"/"+name), Label, pager.KeyBytes)
}
