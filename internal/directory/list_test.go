package directory

import (
	"bytes"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	gossh "golang.org/x/crypto/ssh"
)

// Keys of the fixtures' corpus, for hand-made lists.
const (
	keyAda    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBy7bQtK7HKLg24Jda/ypO5MK6DYapUE7szlye2w4Phe"
	keyBob    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILQ3mKx8a1NGEfl7t1wWqtWcM/uQcOweBJK5zE5Jqd5e"
	keyCarol  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO8fd5tNsoV/rj9hOkas4nY6UkrUTJ5sYB5U4uqCxtlR"
	testHex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// listBody is a directory answer holding these user entries (raw JSON).
func listBody(entries ...string) []byte {
	return []byte(`{"contract":1,"organization":"example","version":"` + testHex64 + `","users":[` + strings.Join(entries, ",") + `]}`)
}

func reasons(l *List) string {
	var parts []string
	for _, s := range l.Skipped {
		parts = append(parts, fmt.Sprintf("%d=%s", s.Entry, s.Reason))
	}
	return strings.Join(parts, " ")
}

// TestEachSkipReason checks each of rule 7's reasons alone, and that its
// check runs in the fixed order: an entry failing two checks is skipped
// with the earlier one.
func TestEachSkipReason(t *testing.T) {
	ada := `{"name":"ada","admin":true,"keys":[{"key":"` + keyAda + `","label":"laptop"}]}`
	for _, tc := range []struct {
		name  string
		entry string
		want  SkipReason
	}{
		{"user member twice", `{"name":"bob","name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipDuplicateMember},
		{"unknown member twice", `{"name":"bob","admin":false,"x":1,"x":1,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipDuplicateMember},
		{"key member twice", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01T00:00:00Z","expires":"2027-01-01T00:00:00Z"}]}`, SkipDuplicateMember},
		{"duplicate member before malformed", `{"name":"bob","admin":"x","admin":"y","keys":null}`, SkipDuplicateMember},
		{"not an object", `["bob"]`, SkipMalformed},
		{"null", `null`, SkipMalformed},
		{"name null", `{"name":null,"admin":false,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipMalformed},
		{"name a number", `{"name":7,"admin":false,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipMalformed},
		{"admin missing", `{"name":"bob","keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipMalformed},
		{"admin null", `{"name":"bob","admin":null,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipMalformed},
		{"admin 1", `{"name":"bob","admin":1,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipMalformed},
		{"keys an object", `{"name":"bob","admin":false,"keys":{"key":"` + keyBob + `","label":"l"}}`, SkipMalformed},
		{"keys missing", `{"name":"bob","admin":false}`, SkipMalformed},
		{"a key a string", `{"name":"bob","admin":false,"keys":["` + keyBob + `"]}`, SkipMalformed},
		{"a key null", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"},null]}`, SkipMalformed},
		{"key not a string", `{"name":"bob","admin":false,"keys":[{"key":7,"label":"l"}]}`, SkipMalformed},
		{"label missing", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `"}]}`, SkipMalformed},
		{"expires null", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":null}]}`, SkipMalformed},
		{"expires a number", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":1}]}`, SkipMalformed},
		{"malformed before invalid name", `{"name":"Bob","admin":"no","keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipMalformed},
		{"upper case name", `{"name":"Bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipInvalidName},
		{"empty name", `{"name":"","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipInvalidName},
		{"name too long", `{"name":"` + "b" + strings.Repeat("o", 63) + `","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipInvalidName},
		{"name with a slash", `{"name":"../bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipInvalidName},
		{"invalid name before duplicate key", `{"name":"Bob","admin":false,"keys":[{"key":"` + keyAda + `","label":"l"}]}`, SkipInvalidName},
		{"its own key twice", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"a"},{"key":"` + keyBob + ` comment","label":"b"}]}`, SkipDuplicateKey},
		{"ada's key", `{"name":"bob","admin":false,"keys":[{"key":"` + keyAda + `","label":"l"}]}`, SkipDuplicateKey},
		{"no keys", `{"name":"bob","admin":false,"keys":[]}`, SkipNoUsableKey},
		{"only a bad line", `{"name":"bob","admin":false,"keys":[{"key":"ssh-ed25519 AAAA","label":"l"}]}`, SkipNoUsableKey},
		{"only a certificate", `{"name":"bob","admin":false,"keys":[{"key":"` + testCertLine(t) + `","label":"l"}]}`, SkipNoUsableKey},
		{"only an empty label", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":""}]}`, SkipNoUsableKey},
		{"labels shared", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"},{"key":"` + keyCarol + `","label":"l"}]}`, SkipNoUsableKey},
		{"a label shared with a refused line", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l"},{"key":"ssh-ed25519 AAAA","label":"l"}]}`, SkipNoUsableKey},
		{"a lower-case t", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01t00:00:00Z"}]}`, SkipNoUsableKey},
		{"a one-digit hour", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01T0:00:00Z"}]}`, SkipNoUsableKey},
		{"an offset of +24:00", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01T00:00:00+24:00"}]}`, SkipNoUsableKey},
		{"admin too large for a float", `{"name":"bob","admin":1e400,"keys":[{"key":"` + keyBob + `","label":"l"}]}`, SkipMalformed},
		{"a bare date", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01"}]}`, SkipNoUsableKey},
		{"no offset", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01T00:00:00"}]}`, SkipNoUsableKey},
		{"a comma before the fraction", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01T00:00:00,5Z"}]}`, SkipNoUsableKey},
		{"a space for the T", `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"l","expires":"2027-01-01 00:00:00Z"}]}`, SkipNoUsableKey},
		{"options on the line", `{"name":"bob","admin":false,"keys":[{"key":"no-pty ` + keyBob + `","label":"l"}]}`, SkipNoUsableKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := DecodeList(listBody(ada, tc.entry), worldOrg)
			if err != nil {
				t.Fatalf("the whole answer refused: %v", err)
			}
			// An entry listing ada's key leaves ada a duplicate key too.
			sharesAda := strings.Contains(tc.entry, keyAda)
			want := "1=" + string(tc.want)
			if sharesAda {
				want = "0=duplicate-key " + want
			}
			if got := reasons(l); got != want {
				t.Errorf("skipped %q, want %q", got, want)
			}
			switch {
			case sharesAda && len(l.Users) != 0:
				t.Errorf("users %v: ada, whose key another entry lists, was admitted", l.Users)
			case !sharesAda && (len(l.Users) != 1 || l.Users[0].Name != "ada"):
				t.Errorf("users %v: want ada alone", l.Users)
			}
		})
	}
}

// testCertLine is an SSH certificate line: config.ParseUserKey refuses it.
func testCertLine(t *testing.T) string {
	t.Helper()
	pk, err := config.ParseUserKey(keyBob)
	if err != nil {
		t.Fatal(err)
	}
	cert := &gossh.Certificate{Key: pk, CertType: gossh.UserCert, ValidPrincipals: []string{"bob"}, ValidBefore: gossh.CertTimeInfinity}
	signer, err := gossh.NewSignerFromKey(fixtureKey(t, "enrol-ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.SignCert(crand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(cert)))
}

// TestKeyIdentityIsItsBlob checks a key's fingerprint and canonical line
// come from its blob: the same key with a comment and other whitespace is
// one key, and the used key carries the line as sent.
func TestKeyIdentityIsItsBlob(t *testing.T) {
	variant := strings.Replace(keyBob, " ", " \t ", 1) + " bob@laptop.example.test"
	l, err := DecodeList(listBody(`{"name":"bob","admin":false,"keys":[{"key":"`+strings.ReplaceAll(variant, "\t", `\t`)+`","label":"l","expires":"2027-01-01T00:00:00.5+01:00"}]}`), worldOrg)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Users) != 1 || len(l.Users[0].Keys) != 1 {
		t.Fatalf("users %v", l.Users)
	}
	k := l.Users[0].Keys[0]
	pk, _ := config.ParseUserKey(keyBob)
	if k.Fingerprint != gossh.FingerprintSHA256(pk) || k.Canonical != keyBob || k.Line != variant {
		t.Errorf("key %+v", k)
	}
	if k.Expires == nil || k.Expires.UTC().Format("2006-01-02T15:04:05Z07:00") != "2026-12-31T23:00:00Z" {
		t.Errorf("expires %v", k.Expires)
	}
	if !bytes.Equal(k.PublicKey.Marshal(), pk.Marshal()) {
		t.Error("the parsed key is not bob's")
	}
}

// TestTopLevelRefusals checks rule 6 refuses the whole answer, and that its
// control, a good answer, is accepted.
func TestTopLevelRefusals(t *testing.T) {
	ada := `{"name":"ada","admin":true,"keys":[{"key":"` + keyAda + `","label":"laptop"}]}`
	good := string(listBody(ada))
	if _, err := DecodeList([]byte(good), worldOrg); err != nil {
		t.Fatalf("the control: %v", err)
	}
	for name, body := range map[string]string{
		"contract 1.0":            strings.Replace(good, `"contract":1`, `"contract":1.0`, 1),
		"contract \"1\"":          strings.Replace(good, `"contract":1`, `"contract":"1"`, 1),
		"contract null":           strings.Replace(good, `"contract":1`, `"contract":null`, 1),
		"organization missing":    strings.Replace(good, `"organization":"example",`, ``, 1),
		"organization upper":      strings.Replace(good, `"organization":"example"`, `"organization":"Example"`, 1),
		"organization null":       strings.Replace(good, `"organization":"example"`, `"organization":null`, 1),
		"version missing":         strings.Replace(good, `"version":"`+testHex64+`",`, ``, 1),
		"version short":           strings.Replace(good, testHex64, testHex64[1:], 1),
		"version a number":        strings.Replace(good, `"`+testHex64+`"`, `7`, 1),
		"users an object":         strings.Replace(good, `"users":[`+ada+`]`, `"users":{"ada":`+ada+`}`, 1),
		"organization twice":      strings.Replace(good, `"organization":"example"`, `"organization":"example","organization":"example"`, 1),
		"an array":                `[` + good + `]`,
		"a string":                `"x"`,
		"empty":                   ``,
		"whitespace":              " \n",
		"trailing garbage":        good + `x`,
		"two values":              good + ` {}`,
		"cut short":               good[:len(good)-1],
		"invalid UTF-8 in a name": strings.Replace(good, `"ada"`, "\"ad\xff\"", 1),
		"17 levels":               strings.Replace(good, `"contract":1`, `"x":`+strings.Repeat("[", 16)+strings.Repeat("]", 16)+`,"contract":1`, 1),
	} {
		if l, err := DecodeList([]byte(body), worldOrg); err == nil {
			t.Errorf("%s: accepted: %s", name, describeList(l))
		}
	}
	// Whitespace after the value, 16 levels and an ignored member are fine.
	for name, body := range map[string]string{
		"trailing whitespace": good + " \r\n\t",
		"16 levels":           strings.Replace(good, `"contract":1`, `"x":`+strings.Repeat("[", 15)+strings.Repeat("]", 15)+`,"contract":1`, 1),
		"rules and more":      strings.Replace(good, `"contract":1`, `"rules":{"version":0},"generated_at":"2026-11-16T06:00:00Z","contract":1`, 1),
	} {
		if _, err := DecodeList([]byte(body), worldOrg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := DecodeList([]byte(good), ""); err == nil {
		t.Error("decoded with no organization to accept")
	}
}

// permuteUsers is body with its users entries in the order perm gives.
func permuteUsers(t *testing.T, body []byte, perm []int) []byte {
	t.Helper()
	obj, ok := parseObject(body)
	if !ok {
		t.Fatal("not an object")
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range obj {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(m.name)
		b.Write(name)
		b.WriteByte(':')
		if m.name != "users" {
			b.Write(m.value)
			continue
		}
		elems, _ := parseArray(m.value)
		b.WriteByte('[')
		for j, p := range perm {
			if j > 0 {
				b.WriteByte(',')
			}
			b.Write(elems[p])
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return b.Bytes()
}

// TestOrderIndependent decodes every good client fixture with its entries
// shuffled: each entry is skipped for the same reason, or admitted the
// same, wherever it stands (rule 7: the checks are order-independent).
func TestOrderIndependent(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, rel := range fixtureFiles(t, "fixtures/client", ".json") {
		c := loadClientFixture(t, rel)
		if c.Expect.Result != "good" {
			continue
		}
		body := c.served(t).body
		base, err := DecodeList(body, worldOrg)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		obj, _ := parseObject(body)
		u, _ := obj.one("users")
		elems, _ := parseArray(u)
		for range 5 {
			perm := r.Perm(len(elems))
			l, err := DecodeList(permuteUsers(t, body, perm), worldOrg)
			if err != nil {
				t.Fatalf("%s %v: %v", c.Name, perm, err)
			}
			// Map each skip back to its original index.
			back := &List{}
			for _, s := range l.Skipped {
				back.Skipped = append(back.Skipped, Skipped{Entry: perm[s.Entry], Reason: s.Reason})
			}
			sort.Slice(back.Skipped, func(i, j int) bool { return back.Skipped[i].Entry < back.Skipped[j].Entry })
			if got, w := reasons(back), reasons(base); got != w {
				t.Errorf("%s %v: skipped %s, want %s", c.Name, perm, got, w)
			}
			if got, w := describeList(&List{Users: l.Users}), describeList(&List{Users: base.Users}); got != w {
				t.Errorf("%s %v: users %s, want %s", c.Name, perm, got, w)
			}
		}
	}
}

// TestParseDateTime holds a key's expires to RFC 3339's date-time (section
// 5.6) as ruled for this client: upper-case T and Z only, two-digit fields
// within their ranges, a fraction after a dot only, an offset [+-]hh:mm with
// hh 00-23 and mm 00-59, and no leap second.
func TestParseDateTime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // the instant in UTC, RFC3339Nano; "" when refused
	}{
		{"2027-01-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"2027-01-01T00:00:00.5+01:00", "2026-12-31T23:00:00.5Z"},
		{"2027-01-01T00:00:00-00:00", "2027-01-01T00:00:00Z"},
		{"2027-01-01T00:00:00+00:00", "2027-01-01T00:00:00Z"},
		{"2027-06-30T23:59:59-23:59", "2027-07-01T23:58:59Z"},
		{"2027-01-01T00:00:00.123456789123Z", "2027-01-01T00:00:00.123456789Z"},
		{"2028-02-29T12:00:00Z", "2028-02-29T12:00:00Z"},
		{"2000-02-29T12:00:00Z", "2000-02-29T12:00:00Z"},
		{"0000-01-01T00:00:00Z", "0000-01-01T00:00:00Z"},
		// The reviewer's table.
		{"2027-01-01t00:00:00Z", ""},
		{"2027-01-01T00:00:00z", ""},
		{"2027-01-01t00:00:00z", ""},
		{"2016-12-31T23:59:60Z", ""},
		{"2027-01-01T00:00:00+0100", ""},
		{"2027-01-01T24:00:00Z", ""},
		// Go's parser accepts these; RFC 3339 does not.
		{"2027-01-01T0:00:00Z", ""},
		{"2027-01-01T00:00:00+24:00", ""},
		{"2027-01-01T00:00:00+00:60", ""},
		{"2027-01-01T00:00:00,5Z", ""},
		// More of the grammar.
		{"2027-01-01", ""},
		{"2027-01-01T00:00:00", ""},
		{"2027-01-01 00:00:00Z", ""},
		{"2027-01-01T00:00:00.Z", ""},
		{"2027-01-01T00:00Z", ""},
		{"2027-1-01T00:00:00Z", ""},
		{"27-01-01T00:00:00Z", ""},
		{"2027-13-01T00:00:00Z", ""},
		{"2027-00-01T00:00:00Z", ""},
		{"2027-01-00T00:00:00Z", ""},
		{"2027-01-32T00:00:00Z", ""},
		{"2027-02-29T00:00:00Z", ""},
		{"1900-02-29T00:00:00Z", ""},
		{"2027-04-31T00:00:00Z", ""},
		{"2027-01-01T00:60:00Z", ""},
		{"2027-01-01T00:00:00+01", ""},
		{"2027-01-01T00:00:00+1:00", ""},
		{"2027-01-01T00:00:00+01:00:00", ""},
		{"2027-01-01T00:00:00UTC", ""},
		{"2027-01-01T00:00:00ZZ", ""},
		{" 2027-01-01T00:00:00Z", ""},
		{"2027-01-01T00:00:00Z ", ""},
		{"+2027-01-01T00:00:00Z", ""},
		{"2027-01-01T00:00:00.5.5Z", ""},
		{"２０２７-01-01T00:00:00Z", ""},
	} {
		got, ok := parseDateTime(tc.in)
		switch {
		case tc.want == "" && ok:
			t.Errorf("%q: accepted as %s", tc.in, got.UTC().Format(time.RFC3339Nano))
		case tc.want != "" && !ok:
			t.Errorf("%q: refused", tc.in)
		case tc.want != "" && got.UTC().Format(time.RFC3339Nano) != tc.want:
			t.Errorf("%q: %s, want %s", tc.in, got.UTC().Format(time.RFC3339Nano), tc.want)
		}
	}
}

// TestFingerprintsCarryEveryCountedKey checks a user carries the identity of
// every key its entry lists that rule 7 counts, used or not, beside the keys
// used: a key with an empty label, an expiry that is not RFC 3339 or a
// shared label is in Fingerprints but not in Keys; a line the parser
// refuses is in neither.
func TestFingerprintsCarryEveryCountedKey(t *testing.T) {
	entry := `{"name":"bob","admin":false,"keys":[` +
		`{"key":"` + keyAda + `","label":""},` +
		`{"key":"ssh-ed25519 AAAA","label":"bad"},` +
		`{"key":"` + keyBob + `","label":"used"},` +
		`{"key":"` + keyCarol + `","label":"x","expires":"2027-01-01"}]}`
	l, err := DecodeList(listBody(entry), worldOrg)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Users) != 1 {
		t.Fatalf("users %v, skipped %s", l.Users, reasons(l))
	}
	fp := func(line string) string {
		pk, err := config.ParseUserKey(line)
		if err != nil {
			t.Fatal(err)
		}
		return gossh.FingerprintSHA256(pk)
	}
	u := l.Users[0]
	if want := []string{fp(keyAda), fp(keyBob), fp(keyCarol)}; strings.Join(u.Fingerprints, " ") != strings.Join(want, " ") {
		t.Errorf("fingerprints %v, want %v", u.Fingerprints, want)
	}
	if len(u.Keys) != 1 || u.Keys[0].Fingerprint != fp(keyBob) {
		t.Errorf("keys used %v, want bob's alone", u.Keys)
	}
}

// TestNumbersAreNotConverted checks a number no float can hold is a wrong
// type where a member is read (the entry is skipped as malformed) and is
// ignored where it is not, at the top level and in an entry: it never
// refuses the whole answer.
func TestNumbersAreNotConverted(t *testing.T) {
	ada := `{"name":"ada","admin":true,"keys":[{"key":"` + keyAda + `","label":"laptop"}]}`
	bob := `{"name":"bob","admin":1e400,"keys":[{"key":"` + keyBob + `","label":"l"}]}`
	carol := `{"name":"carol","admin":false,"huge":-1e999999,"keys":[{"key":"` + keyCarol + `","label":"l","n":123456789012345678901234567890e400}]}`
	body := strings.Replace(string(listBody(ada, bob, carol)), `"contract":1`, `"ignored":1e400,"contract":1`, 1)
	l, err := DecodeList([]byte(body), worldOrg)
	if err != nil {
		t.Fatalf("the whole answer refused: %v", err)
	}
	if got := reasons(l); got != "1=malformed" {
		t.Errorf("skipped %q, want 1=malformed", got)
	}
	if len(l.Users) != 2 || l.Users[0].Name != "ada" || l.Users[1].Name != "carol" {
		t.Errorf("users %v, want ada and carol", l.Users)
	}
	// The token answer: expires_in too large is a refused answer, not a
	// crash; an ignored member too large is ignored.
	if _, _, err := decodeTokenAnswer([]byte(`{"access_token":"t","token_type":"bearer","expires_in":1e400}`)); err == nil {
		t.Error("expires_in 1e400 accepted")
	}
	if _, _, err := decodeTokenAnswer([]byte(`{"access_token":"t","token_type":"bearer","expires_in":600,"x":1e400}`)); err != nil {
		t.Errorf("an ignored member 1e400: %v", err)
	}
}

// TestUnpairedSurrogateRefusesTheAnswer checks a string escaping half of a
// UTF-16 surrogate pair, anywhere in a directory answer, refuses the whole
// answer (it is not valid text, rule 6), while a pair, and a backslash that
// only looks like the start of one, are accepted.
func TestUnpairedSurrogateRefusesTheAnswer(t *testing.T) {
	ada := `{"name":"ada","admin":true,"keys":[{"key":"` + keyAda + `","label":"laptop"}]}`
	bob := func(label string) string {
		return `{"name":"bob","admin":false,"keys":[{"key":"` + keyBob + `","label":"` + label + `"}]}`
	}
	// esc is a JSON \u escape of hex, written without a backslash in this
	// file so no tool turns it into the character it names.
	esc := func(hex string) string { return string(rune(92)) + "u" + hex }
	for name, body := range map[string]string{
		"a lone high surrogate in a label":   string(listBody(ada, bob(esc("d800")))),
		"a lone low surrogate in a label":    string(listBody(ada, bob(esc("dc00")))),
		"two high surrogates":                string(listBody(ada, bob(esc("d800")+esc("d800")))),
		"a high surrogate then a character":  string(listBody(ada, bob(esc("d800")+"x"))),
		"a high surrogate at a string's end": string(listBody(ada, bob("a"+esc("D800")))),
		"reversed halves":                    string(listBody(ada, bob(esc("de00")+esc("d83d")))),
		"in a name":                          strings.Replace(string(listBody(ada)), `"ada"`, `"ad`+esc("dfff")+`a"`, 1),
		"in an ignored member":               strings.Replace(string(listBody(ada)), `"contract":1`, `"x":{"y":["`+esc("dbff")+`"]},"contract":1`, 1),
		"in a member name":                   strings.Replace(string(listBody(ada)), `"contract":1`, `"`+esc("d800")+`":1,"contract":1`, 1),
	} {
		if l, err := DecodeList([]byte(body), worldOrg); err == nil {
			t.Errorf("%s: accepted: %s", name, describeList(l))
		}
	}
	for name, body := range map[string]string{
		"a pair":                       string(listBody(ada, bob(esc("d83d")+esc("de00")))),
		"a pair in upper case":         string(listBody(ada, bob(esc("D83D")+esc("DE00")))),
		"an escaped backslash then u":  string(listBody(ada, bob(string(rune(92))+esc("d800")))),
		"a character below surrogates": string(listBody(ada, bob(esc("d7ff")))),
		"a character above surrogates": string(listBody(ada, bob(esc("e000")))),
		"an escaped quote then a pair": string(listBody(ada, bob(string(rune(92))+`"`+esc("d83d")+esc("de00")))),
	} {
		l, err := DecodeList([]byte(body), worldOrg)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(l.Users) != 2 {
			t.Errorf("%s: users %v, skipped %s", name, l.Users, reasons(l))
		}
	}
}

// TestSkippedName checks a skipped entry carries its name when it had one
// as a string, for the log.
func TestSkippedName(t *testing.T) {
	l, err := DecodeList(listBody(`{"name":"Bob","admin":false,"keys":[]}`, `{"admin":false,"keys":[]}`), worldOrg)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Skipped) != 2 || l.Skipped[0].Name != "Bob" || l.Skipped[1].Name != "" {
		t.Fatalf("skipped %+v", l.Skipped)
	}
	if l.Version != testHex64 || l.Organization != worldOrg || l.Users == nil {
		t.Errorf("list %+v", l)
	}
}
