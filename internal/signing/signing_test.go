package signing

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

type fixture struct {
	Secret string `json:"secret"`
	Label  string `json:"label"`
	Cases  []struct {
		Name      string `json:"name"`
		AccountID string `json:"accountId"`
		Password  string `json:"password"`
		DBName    string `json:"dbname"`
		Derived   string `json:"derived"`
		Direct    string `json:"direct"`
	} `json:"cases"`
}

func load(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile("../../fixtures/signing.json")
	if err != nil {
		t.Fatalf("the shared fixture must be readable: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(f.Cases) < 6 {
		t.Fatalf("fixture: expected at least 6 cases, got %d", len(f.Cases))
	}
	return f
}

// The single most valuable test in this service: the same triples the
// TypeScript side signs, and the same digests.
func TestFixtureAgreesWithTheAppSide(t *testing.T) {
	f := load(t)
	hexOnly := regexp.MustCompile(`^[0-9a-f]{64}$`)

	for _, c := range f.Cases {
		parts := Parts{AccountID: c.AccountID, Password: c.Password, DBName: c.DBName}

		derived, err := Sign(parts, f.Secret, f.Label)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if derived != c.Derived {
			t.Errorf("%s (derived):\n got  %s\n want %s", c.Name, derived, c.Derived)
		}

		direct, err := Sign(parts, f.Secret, Direct)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if direct != c.Direct {
			t.Errorf("%s (direct):\n got  %s\n want %s", c.Name, direct, c.Direct)
		}

		for _, sig := range []string{derived, direct} {
			if !hexOnly.MatchString(sig) {
				t.Errorf("%s: %q is not 64 lower-case hex characters", c.Name, sig)
			}
		}
		if derived == direct {
			t.Errorf("%s: the two modes must not agree, or a mode mismatch would go unnoticed", c.Name)
		}
		if !Verify(c.Derived, parts, f.Secret, f.Label) || !Verify(c.Direct, parts, f.Secret, Direct) {
			t.Errorf("%s: a signature from the fixture must verify", c.Name)
		}
		if Verify(c.Direct, parts, f.Secret, f.Label) {
			t.Errorf("%s: a mode mismatch must be a failed signature", c.Name)
		}
	}
}

func TestTheDelimiterKeepsTwoSplitsApart(t *testing.T) {
	f := load(t)
	var one, other string
	var joinOne, joinOther string

	for _, c := range f.Cases {
		if strings.Contains(c.Name, "split of") {
			one, joinOne = c.Derived, c.AccountID+c.Password+c.DBName
		}
		if strings.Contains(c.Name, "other split") {
			other, joinOther = c.Derived, c.AccountID+c.Password+c.DBName
		}
	}
	if one == "" || other == "" {
		t.Fatal("fixture: the two split cases must be present")
	}
	if joinOne != joinOther {
		t.Fatalf("fixture: the split cases must hold the same characters: %q vs %q", joinOne, joinOther)
	}
	if one == other {
		t.Error("two different triples signed the same — the delimiter is not doing its job")
	}
}

func TestPasswordCharset(t *testing.T) {
	good := []string{strings.Repeat("y", 16), strings.Repeat("y", 128), "aA0._~-aA0._~-aA0"}
	bad := []string{strings.Repeat("y", 15), strings.Repeat("y", 129), "mật-khẩu-đủ-dài-rồi", "has spaces here!", "colon:in-password"}

	for _, p := range good {
		if !ValidPassword(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range bad {
		if ValidPassword(p) {
			t.Errorf("%q should be refused", p)
		}
		if _, err := Sign(Parts{AccountID: "u", Password: p, DBName: "p"}, "s", DefaultLabel); err == nil {
			t.Errorf("%q should not sign", p)
		}
	}
}

func TestRefusesWhatWouldMakeTheMessageAmbiguous(t *testing.T) {
	ok := strings.Repeat("y", 16)
	for _, parts := range []Parts{
		{AccountID: "a:b", Password: ok, DBName: "p"},
		{AccountID: "u", Password: ok, DBName: "p:q"},
		{AccountID: "", Password: ok, DBName: "p"},
	} {
		if _, err := Sign(parts, "s", DefaultLabel); err == nil {
			t.Errorf("%+v should not sign", parts)
		}
	}
	if _, err := Sign(Parts{AccountID: "u", Password: ok, DBName: "p"}, "", DefaultLabel); err == nil {
		t.Error("an empty secret should not sign")
	}
}

func TestVerifyIsFalseNotFatal(t *testing.T) {
	parts := Parts{AccountID: "u", Password: strings.Repeat("y", 16), DBName: "p"}
	sig, _ := Sign(parts, "secret", DefaultLabel)

	for _, bad := range []string{"", "zz", sig[:62], sig + "00"} {
		if Verify(bad, parts, "secret", DefaultLabel) {
			t.Errorf("%q should not verify", bad)
		}
	}
	if !Verify(strings.ToUpper(sig), parts, "secret", DefaultLabel) {
		t.Error("hex case is not part of the contract")
	}
}

// A label nobody set must be the ordinary derived one, not the plainer form.
//
// The two produce different signatures, so a server that quietly chose the
// plain form would refuse every connection string a client signed normally —
// and every test in this repository would still pass, because both sides of
// this repository would agree with each other. That is what happened, and it
// was found by running a TypeScript client against the Go server rather than
// by any test here.
func TestAnUnsetLabelIsTheDefaultOneAndNotTheDirectForm(t *testing.T) {
	parts := Parts{AccountID: "acme", Password: "a-password-of-the-right-shape", DBName: "main"}
	const secret = "the secret only the control plane has"

	blank, err := Sign(parts, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	derived, err := Sign(parts, secret, DefaultLabel)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := Sign(parts, secret, Direct)
	if err != nil {
		t.Fatal(err)
	}

	if blank != derived {
		t.Errorf("an unset label signs differently from the default one:\n %s\n %s", blank, derived)
	}
	if blank == direct {
		t.Error("an unset label selected the direct form")
	}
	if !Verify(blank, parts, secret, "") || !Verify(blank, parts, secret, DefaultLabel) {
		t.Error("a signature made with no label does not verify with the default one")
	}
	if Verify(direct, parts, secret, "") {
		t.Error("a direct signature verified against an unset label")
	}
}
