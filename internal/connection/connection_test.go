package connection

import (
	"errors"
	"strings"
	"testing"

	"github.com/material-atomic/rsql/internal/signing"
)

const secret = "the secret only the control plane has"

func signed(t *testing.T, account, password, host, name string) string {
	t.Helper()
	signature, err := signing.Sign(
		signing.Parts{AccountID: account, Password: password, DBName: name}, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	return Connection{
		Account: account, Password: password, Host: host, Port: DefaultPort,
		DBName: name, Signature: signature,
	}.String()
}

func TestAConnectionStringGoesApartAndBackTogether(t *testing.T) {
	text := signed(t, "acme", "a-password-of-the-right-shape", "db.example", "main")

	parsed, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Account != "acme" || parsed.DBName != "main" || parsed.Host != "db.example" {
		t.Errorf("parsed as %+v", parsed)
	}
	if parsed.String() != text {
		t.Errorf("it came back as %q", parsed.String())
	}
	if err := parsed.Verify(secret, ""); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestAPortIsTakenOrDefaulted(t *testing.T) {
	text := signed(t, "acme", "a-password-of-the-right-shape", "db.example", "main")

	// No port means the one a client assumes.
	without := strings.Replace(text, ":7433", "", 1)
	parsed, err := Parse(without)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Port != DefaultPort {
		t.Errorf("no port became %d", parsed.Port)
	}

	for _, port := range []string{":0", ":70000", ":-1", ":http"} {
		broken := strings.Replace(text, ":7433", port, 1)
		if _, err := Parse(broken); err == nil {
			t.Errorf("port %q was accepted", port)
		}
	}
}

// These strings are secrets, so somebody debugging one cannot print it. The
// error has to say which field is at fault or they are staring at a line they
// dare not show anyone.
func TestWhatIsMissingIsNamed(t *testing.T) {
	for want, text := range map[string]string{
		"account":  "rsql://:pw@host:7433/db?sig=ab",
		"password": "rsql://acme@host:7433/db?sig=ab",
		"host":     "rsql://acme:pw@/db?sig=ab",
		"dbname":   "rsql://acme:pw@host:7433/?sig=ab",
		"sig":      "rsql://acme:pw@host:7433/db",
	} {
		_, err := Parse(text)
		if !errors.Is(err, ErrField) {
			t.Errorf("%s: want ErrField, got %v", want, err)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}

	if _, err := Parse("postgres://acme:pw@host/db?sig=ab"); !errors.Is(err, ErrScheme) {
		t.Errorf("another scheme: want ErrScheme, got %v", err)
	}
}

// The signature covers the account, the password and the database together, so
// none of them can be changed without it failing. The host is deliberately not
// covered: the same credentials have to work against a primary, a replica and
// a laptop.
func TestTheSignatureCoversWhoAndWhatButNotWhere(t *testing.T) {
	text := signed(t, "acme", "a-password-of-the-right-shape", "db.example", "main")

	for name, changed := range map[string]string{
		"another account":  strings.Replace(text, "acme:", "evil:", 1),
		"another password": strings.Replace(text, "a-password-of-the-right-shape", "another-password-here", 1),
		"another database": strings.Replace(text, "/main?", "/other?", 1),
	} {
		if err := VerifyString(changed, secret, ""); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: want ErrSignature, got %v", name, err)
		}
	}

	moved := strings.Replace(text, "db.example", "replica.example", 1)
	if err := VerifyString(moved, secret, ""); err != nil {
		t.Errorf("moving the host broke the signature: %v", err)
	}

	if err := VerifyString(text, "another secret entirely", ""); !errors.Is(err, ErrSignature) {
		t.Errorf("another secret: want ErrSignature, got %v", err)
	}
}

func TestRedactingLeavesNothingWorthStealing(t *testing.T) {
	password := "a-password-of-the-right-shape"
	text := signed(t, "acme", password, "db.example", "main")

	parsed, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}

	redacted := parsed.Redact()
	if strings.Contains(redacted, password) || strings.Contains(redacted, parsed.Signature) {
		t.Errorf("redacted still holds a secret: %s", redacted)
	}
	// And is still useful: it says which database is being talked about.
	if !strings.Contains(redacted, "acme") || !strings.Contains(redacted, "main") {
		t.Errorf("redacted says nothing useful: %s", redacted)
	}

	// The failure a caller is most likely to log is the one that must be safe.
	err = parsed.Verify("another secret", "")
	if strings.Contains(err.Error(), password) {
		t.Errorf("the failure holds the password: %v", err)
	}
}
