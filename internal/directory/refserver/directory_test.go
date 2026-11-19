package refserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// TestBearerRefusals checks each check of an access token on
// /v1/directory: the unchanged token opens the list, and each change of one
// part is refused 401 invalid_token with the same body (a sound token of a
// disabled server: 403).
func TestBearerRefusals(t *testing.T) {
	type change func(s *scenario, p assertionParts) string
	edit := func(f func(p *assertionParts)) change {
		return func(s *scenario, p assertionParts) string { f(&p); return "Bearer " + s.signBearer(p) }
	}
	setH := func(name, raw string) change {
		return edit(func(p *assertionParts) { p.header = set(p.header, name, raw) })
	}
	setC := func(name, raw string) change {
		return edit(func(p *assertionParts) { p.claims = set(p.claims, name, raw) })
	}
	dropC := func(name string) change {
		return edit(func(p *assertionParts) { p.claims = drop(p.claims, name) })
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change change
		want   int
	}{
		{"unchanged", edit(func(*assertionParts) {}), 200},
		{"scheme in another case", func(s *scenario, p assertionParts) string { return "bearer " + s.signBearer(p) }, 200},
		{"aud holding /v1 among others", setC("aud", `["x","`+worldIss+`/v1"]`), 200},

		{"another scheme", func(s *scenario, p assertionParts) string { return "Basic " + s.signBearer(p) }, 401},
		{"no token", func(*scenario, assertionParts) string { return "Bearer " }, 401},
		{"two spaces", func(s *scenario, p assertionParts) string { return "Bearer  " + s.signBearer(p) }, 401},
		{"typ JWT", setH("typ", `"JWT"`), 401},
		{"typ application/at+jwt", setH("typ", `"application/at+jwt"`), 401},
		{"typ missing", edit(func(p *assertionParts) { p.header = drop(p.header, "typ") }), 401},
		{"alg RS384", setH("alg", `"RS384"`), 401},
		{"alg none", setH("alg", `"none"`), 401},
		{"kid missing", edit(func(p *assertionParts) { p.header = drop(p.header, "kid") }), 401},
		{"kid another", setH("kid", `"test-signing-2"`), 401},
		{"header member twice", edit(func(p *assertionParts) { p.header = append(p.header, p.header[0]) }), 401},
		{"signed with another key", func(s *scenario, p assertionParts) string {
			saved := s.h.signing
			s.h.signing = other
			defer func() { s.h.signing = saved }()
			return "Bearer " + s.signBearer(p)
		}, 401},
		{"signature corrupted", func(s *scenario, p assertionParts) string {
			tok := s.signBearer(p)
			i := strings.LastIndexByte(tok, '.')
			sig := []byte(tok[i+1:])
			if sig[0] == 'A' {
				sig[0] = 'B'
			} else {
				sig[0] = 'A'
			}
			return "Bearer " + tok[:i+1] + string(sig)
		}, 401},
		{"iss another", setC("iss", `"https://other.example.test"`), 401},
		{"iss missing", dropC("iss"), 401},
		{"aud the string", setC("aud", `"`+worldIss+`/v1"`), 401},
		{"aud the issuer", setC("aud", `["`+worldIss+`"]`), 401},
		{"exp missing", dropC("exp"), 401},
		{"nbf missing", dropC("nbf"), 401},
		{"exp a string", setC("exp", `"9999999999"`), 401},
		{"gty another", setC("gty", `"authorization_code"`), 401},
		{"gty missing", dropC("gty"), 401},
		{"sub another", setC("sub", `"`+srvB+`"`), 401},
		{"client_id missing", dropC("client_id"), 401},
		{"client_id and sub another server's", edit(func(p *assertionParts) {
			p.claims = set(set(p.claims, "sub", `"`+srvB+`"`), "client_id", `"`+srvB+`"`)
		}), 403}, // a sound token of the disabled srv-b
		{"scp missing", dropC("scp"), 401},
		{"claim twice", edit(func(p *assertionParts) { p.claims = append(p.claims, p.claims[0]) }), 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWorld(t)
			auth := tc.change(s, s.bearerParts(srvA))
			rec := s.directory(auth)
			if rec.Code != tc.want {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if v1Error(t, rec) != "invalid_token" || rec.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
					t.Errorf("%v %s", rec.Header(), rec.Body)
				}
				if !strings.Contains(rec.Body.String(), invalidTokenMessage) {
					t.Errorf("the 401 body is not the one every 401 carries: %s", rec.Body)
				}
			}
		})
	}
}

// TestBearerHeaders checks the Authorization header itself: none, two, or
// a cookie only.
func TestBearerHeaders(t *testing.T) {
	s := newWorld(t)
	tok := "Bearer " + s.signBearer(s.bearerParts(srvA))
	for name, auth := range map[string][]string{"none": nil, "two": {tok, tok}} {
		if rec := s.directory(auth...); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	if rec := s.directory(tok); rec.Code != http.StatusOK {
		t.Fatalf("one: %d", rec.Code)
	}
}

// TestBearerTimeBoundaries checks exp and nbf with the 30 seconds of
// leeway, at and just past each boundary.
func TestBearerTimeBoundaries(t *testing.T) {
	s := newWorld(t)
	t0 := s.clock
	tok := "Bearer " + s.signBearer(s.bearerParts(srvA)) // exp = t0 + 600
	for _, c := range []struct {
		at   time.Duration
		want int
	}{{629 * time.Second, 200}, {630 * time.Second, 401}, {629*time.Second + 999*time.Millisecond, 200}} {
		s.at(t0.Add(c.at))
		if rec := s.directory(tok); rec.Code != c.want {
			t.Errorf("at t0+%s: %d, want %d", c.at, rec.Code, c.want)
		}
	}
	s.at(t0)
	for off, want := range map[int64]int{30: 200, 31: 401} {
		p := s.bearerParts(srvA)
		p.claims = set(p.claims, "nbf", fmt.Sprint(t0.Unix()+off))
		if rec := s.directory("Bearer " + s.signBearer(p)); rec.Code != want {
			t.Errorf("nbf now+%d: %d, want %d", off, rec.Code, want)
		}
	}
}

// TestBearerNumericDateLimits checks exp and nbf at and near the int64
// limits and at the end of the range numeric dates are held to: nothing
// wraps into the window.
func TestBearerNumericDateLimits(t *testing.T) {
	for _, c := range []struct {
		claim, value string
		want         int
	}{
		{"exp", fmt.Sprint(int64(maxNumericDate)), 200},
		{"exp", fmt.Sprint(int64(maxNumericDate + 1)), 401},
		{"exp", fmt.Sprint(int64(math.MaxInt64)), 401},
		{"exp", fmt.Sprint(int64(math.MaxInt64 - 1)), 401},
		{"exp", "9223372036854775808", 401},
		{"nbf", "0", 200},
		{"nbf", "-1", 401},
		{"nbf", fmt.Sprint(int64(math.MaxInt64)), 401},
		{"nbf", fmt.Sprint(int64(math.MaxInt64 - 1)), 401},
		{"nbf", fmt.Sprint(int64(math.MinInt64)), 401},
		{"nbf", fmt.Sprint(int64(maxNumericDate + 1)), 401},
	} {
		s := newWorld(t)
		p := s.bearerParts(srvA)
		p.claims = set(p.claims, c.claim, c.value)
		if rec := s.directory("Bearer " + s.signBearer(p)); rec.Code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.claim, c.value, rec.Code, rec.Body, c.want)
		}
	}
}

// TestDirectoryAnswerOverLimit checks the whole encoded answer is measured
// before a 200 is committed: a list whose answer takes more than rule 1's
// 8 MiB is answered as the storage outage, with the reason logged, and one
// just under it is served.
func TestDirectoryAnswerOverLimit(t *testing.T) {
	// Each label is 64 bytes of '&', which the encoder escapes to six bytes
	// each: about 490 bytes a key, 20 keys a user.
	generate := func(users int) []User {
		out := make([]User, users)
		seed := make([]byte, ed25519.SeedSize)
		label := strings.Repeat("&", 63)
		for i := range out {
			out[i] = User{Name: fmt.Sprintf("u%05d", i)}
			for j := 0; j < maxKeysPerUser; j++ {
				seed[0], seed[1], seed[2] = byte(i), byte(i>>8), byte(j)
				pk, err := gossh.NewPublicKey(ed25519.NewKeyFromSeed(seed).Public())
				if err != nil {
					t.Fatal(err)
				}
				line := strings.TrimSuffix(string(gossh.MarshalAuthorizedKey(pk)), "\n")
				out[i].Keys = append(out[i].Keys, Key{Key: line, Label: fmt.Sprintf("%c%s", 'a'+j, label)})
			}
		}
		return out
	}
	for _, c := range []struct {
		users int
		want  int
	}{{850, 200}, {870, 503}} {
		s := newWorld(t)
		var logged []string
		s.srv.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
		s.in.users = generate(c.users)
		rec := s.directory("Bearer " + s.signBearer(s.bearerParts(srvA)))
		if rec.Code != c.want {
			t.Fatalf("%d users: %d (%d bytes), want %d", c.users, rec.Code, rec.Body.Len(), c.want)
		}
		switch c.want {
		case http.StatusOK:
			if n := rec.Body.Len(); n > maxDirectoryBody || n < maxDirectoryBody-maxDirectoryBody/32 {
				t.Fatalf("the control's answer takes %d bytes; it should be just under %d", n, maxDirectoryBody)
			}
		default:
			if v1Error(t, rec) != "UNAVAILABLE" {
				t.Errorf("%s", rec.Body)
			}
			if !strings.Contains(strings.Join(logged, "\n"), "more than rule 1's 8388608") {
				t.Errorf("the reason was not logged: %q", logged)
			}
		}
	}
}

// TestCheckKeyBlob checks the served-list validator's own rules on a key's
// wire blob, apart from the SSH parser: a non-positive RSA modulus or
// exponent and a security key's application that is not UTF-8 are refused.
func TestCheckKeyBlob(t *testing.T) {
	str := func(b []byte) []byte {
		return append([]byte{byte(len(b) >> 24), byte(len(b) >> 16), byte(len(b) >> 8), byte(len(b))}, b...)
	}
	blob := func(fields ...[]byte) []byte {
		var out []byte
		for _, f := range fields {
			out = append(out, str(f)...)
		}
		return out
	}
	n := append([]byte{0x00, 0xc0}, make([]byte, 255)...)
	n[len(n)-1] = 1
	neg := append([]byte{0xc0}, make([]byte, 255)...)
	e := []byte{0x01, 0x00, 0x01}
	ed := make([]byte, 32)
	q := append([]byte{4}, make([]byte, 64)...)
	cases := []struct {
		name, typ string
		blob      []byte
		ok        bool
	}{
		{"rsa", gossh.KeyAlgoRSA, blob([]byte(gossh.KeyAlgoRSA), e, n), true},
		{"rsa negative modulus", gossh.KeyAlgoRSA, blob([]byte(gossh.KeyAlgoRSA), e, neg), false},
		{"rsa zero modulus", gossh.KeyAlgoRSA, blob([]byte(gossh.KeyAlgoRSA), e, []byte{0}), false},
		{"rsa empty modulus", gossh.KeyAlgoRSA, blob([]byte(gossh.KeyAlgoRSA), e, nil), false},
		{"rsa negative exponent", gossh.KeyAlgoRSA, blob([]byte(gossh.KeyAlgoRSA), []byte{0x81}, n), false},
		{"rsa trailing bytes", gossh.KeyAlgoRSA, append(blob([]byte(gossh.KeyAlgoRSA), e, n), 0), false},
		{"sk-ed25519", gossh.KeyAlgoSKED25519, blob([]byte(gossh.KeyAlgoSKED25519), ed, []byte("ssh:")), true},
		{"sk-ed25519 empty application", gossh.KeyAlgoSKED25519, blob([]byte(gossh.KeyAlgoSKED25519), ed, nil), true},
		{"sk-ed25519 application not UTF-8", gossh.KeyAlgoSKED25519, blob([]byte(gossh.KeyAlgoSKED25519), ed, []byte{0xff, 0xfe}), false},
		{"sk-ecdsa", gossh.KeyAlgoSKECDSA256, blob([]byte(gossh.KeyAlgoSKECDSA256), []byte("nistp256"), q, []byte("ssh:")), true},
		{"sk-ecdsa application not UTF-8", gossh.KeyAlgoSKECDSA256, blob([]byte(gossh.KeyAlgoSKECDSA256), []byte("nistp256"), q, []byte("ssh:\xc3")), false},
		{"sk-ecdsa truncated", gossh.KeyAlgoSKECDSA256, blob([]byte(gossh.KeyAlgoSKECDSA256), []byte("nistp256"), q)[:40], false},
		{"ed25519 not inspected", gossh.KeyAlgoED25519, nil, true},
	}
	for _, c := range cases {
		if err := checkKeyBlob(c.typ, c.blob); (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// TestDirectoryServerRecord checks the server's own record after the token:
// gone is 401, disabled is 403, and an outage is 503.
func TestDirectoryServerRecord(t *testing.T) {
	s := newWorld(t)
	unknown := "srv_0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c"
	if rec := s.directory("Bearer " + s.signBearer(s.bearerParts(unknown))); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown: %d", rec.Code)
	}
	tok := "Bearer " + s.signBearer(s.bearerParts(srvA))
	s.in.fail = true
	if rec := s.directory(tok); rec.Code != http.StatusServiceUnavailable || v1Error(t, rec) != "UNAVAILABLE" {
		t.Errorf("outage: %d %s", rec.Code, rec.Body)
	}
	s.in.fail = false
	s.in.disabled[srvA] = true
	if rec := s.directory(tok); rec.Code != http.StatusForbidden || v1Error(t, rec) != "SERVER_DISABLED" {
		t.Errorf("disabled: %d %s", rec.Code, rec.Body)
	}
}

// disablingInputs disables a server at its first read of the users: the
// server's record is read again after the list.
type disablingInputs struct {
	*worldInputs
	clientID string
}

func (d disablingInputs) Users() ([]User, error) {
	u, err := d.worldInputs.Users()
	d.mu.Lock()
	d.disabled[d.clientID] = true
	d.mu.Unlock()
	return u, err
}

// TestDirectoryDisabledWhileReading checks a server disabled while its list
// is read gets 403, never a list read after it was disabled.
func TestDirectoryDisabledWhileReading(t *testing.T) {
	s := newWorld(t)
	s.srv.inputs = disablingInputs{s.in, srvA}
	if rec := s.directory("Bearer " + s.signBearer(s.bearerParts(srvA))); rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s, want 403", rec.Code, rec.Body)
	}
}

// newSSHKey is a fresh Ed25519 SSH key's canonical line.
func newSSHKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(gossh.MarshalAuthorizedKey(pk)), "\n")
}

// TestServeList checks what the directory serves: keys that have expired
// left out at their instant, users with no key left left out, users ordered
// by name and keys by label, and a version that changes with the content
// only.
func TestServeList(t *testing.T) {
	k1, k2, k3 := newSSHKey(t), newSSHKey(t), newSSHKey(t)
	t0 := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	users := []User{
		{Name: "zed", Admin: false, Keys: []Key{{Key: k3, Label: "only", Expires: "2026-11-16T07:00:00Z"}}},
		{Name: "amy", Admin: true, Keys: []Key{{Key: k1, Label: "z-laptop"}, {Key: k2, Label: "a-desk", Expires: "2026-11-16T06:30:00Z"}}},
		{Name: "bob", Admin: false, Keys: []Key{}},
	}
	served, err := serveList(users, t0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(served)
	want := `[{"name":"amy","admin":true,"keys":[{"key":"` + k2 + `","label":"a-desk","expires":"2026-11-16T06:30:00Z"},{"key":"` + k1 + `","label":"z-laptop"}]},{"name":"zed","admin":false,"keys":[{"key":"` + k3 + `","label":"only","expires":"2026-11-16T07:00:00Z"}]}]`
	if string(b) != want {
		t.Fatalf("served\n %s\nwant\n %s", b, want)
	}
	v0 := listVersion(served)
	if again, _ := serveList(users, t0.Add(29*time.Minute)); listVersion(again) != v0 {
		t.Error("the version changed with no change of content")
	}
	at, _ := serveList(users, t0.Add(30*time.Minute)) // amy's desk key expires at its instant
	if len(at[0].Keys) != 1 || listVersion(at) == v0 {
		t.Errorf("at the key's expiry: %+v", at)
	}
	late, _ := serveList(users, t0.Add(time.Hour)) // zed's only key has expired
	if len(late) != 1 || late[0].Name != "amy" {
		t.Errorf("past the last key's expiry: %+v", late)
	}
	empty, err := serveList(nil, t0)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("an empty list: %v %v", empty, err)
	}
}

// TestServeListRefusals checks a list that breaks a rule for a service is
// refused whole, as an outage, never served.
func TestServeListRefusals(t *testing.T) {
	k1, k2 := newSSHKey(t), newSSHKey(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := gossh.NewPublicKey(pub)
	_, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	ca, _ := gossh.NewSignerFromKey(caPriv)
	cert := &gossh.Certificate{Key: sshPub, CertType: gossh.UserCert, ValidBefore: gossh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	certLine := strings.TrimSuffix(string(gossh.MarshalAuthorizedKey(cert)), "\n")
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	smallPub, _ := gossh.NewPublicKey(&small.PublicKey)
	smallLine := strings.TrimSuffix(string(gossh.MarshalAuthorizedKey(smallPub)), "\n")
	many := make([]Key, 21)
	for i := range many {
		many[i] = Key{Key: newSSHKey(t), Label: fmt.Sprintf("k%02d", i)}
	}
	one := func(name string, keys ...Key) []User { return []User{{Name: name, Keys: keys}} }
	for name, users := range map[string][]User{
		"a reserved name":         one("admin", Key{Key: k1, Label: "l"}),
		"an invalid name":         one("Ada", Key{Key: k1, Label: "l"}),
		"a name of 64":            one("a"+strings.Repeat("b", 63), Key{Key: k1, Label: "l"}),
		"a name twice":            append(one("ada", Key{Key: k1, Label: "l"}), one("ada", Key{Key: k2, Label: "l"})...),
		"a key under two users":   append(one("ada", Key{Key: k1, Label: "l"}), one("bob", Key{Key: k1, Label: "l"})...),
		"a key twice in one user": one("ada", Key{Key: k1, Label: "a"}, Key{Key: k1, Label: "b"}),
		"a key with a comment":    one("ada", Key{Key: k1 + " me@example.test", Label: "l"}),
		"a key with options":      one("ada", Key{Key: "no-pty " + k1, Label: "l"}),
		"a key not a key":         one("ada", Key{Key: "ssh-ed25519 AAAA", Label: "l"}),
		"a certificate":           one("ada", Key{Key: certLine, Label: "l"}),
		"an RSA key of 1024 bits": one("ada", Key{Key: smallLine, Label: "l"}),
		"an empty label":          one("ada", Key{Key: k1, Label: ""}),
		"a label of 65 bytes":     one("ada", Key{Key: k1, Label: strings.Repeat("l", 65)}),
		"a control character":     one("ada", Key{Key: k1, Label: "a\x7fb"}),
		"a label twice":           one("ada", Key{Key: k1, Label: "l"}, Key{Key: k2, Label: "l"}),
		"expires a date":          one("ada", Key{Key: k1, Label: "l", Expires: "2027-01-01"}),
		"expires with an offset":  one("ada", Key{Key: k1, Label: "l", Expires: "2027-01-01T01:00:00+01:00"}),
		"expires with a fraction": one("ada", Key{Key: k1, Label: "l", Expires: "2027-01-01T00:00:00.5Z"}),
		"21 keys":                 one("ada", many...),
	} {
		if _, err := serveList(users, time.Now()); err == nil {
			t.Errorf("%s: served", name)
		}
	}
	// Through the endpoint, such a list is an outage.
	s := newWorld(t)
	s.in.users = one("root", Key{Key: k1, Label: "l"})
	if rec := s.directory("Bearer " + s.signBearer(s.bearerParts(srvA))); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a list with a reserved name: %d %s, want 503", rec.Code, rec.Body)
	}
}

// TestRoutes checks unknown paths and methods, and that no path is cleaned
// into a redirect.
func TestRoutes(t *testing.T) {
	s := newWorld(t)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/v1/nothing", 404},
		{http.MethodGet, "/v1//directory", 404},
		{http.MethodGet, "/v1/directory/", 404},
		{http.MethodGet, "/v1/x/../directory", 404},
		{http.MethodPost, pathDirectory, 405},
		{http.MethodGet, pathToken, 405},
		{http.MethodGet, pathEnroll, 405},
		{http.MethodGet, pathJWKS, 200},
		{http.MethodGet, pathMetadata, 200},
	} {
		req, _ := http.NewRequest(c.method, "http://accounts.example.test"+c.path, nil)
		req.URL.Path = c.path
		rec := s.do(req)
		if rec.Code != c.want || (rec.Code >= 300 && rec.Code < 400) {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}

// TestNewRefuses checks New's own checks.
func TestNewRefuses(t *testing.T) {
	in := &worldInputs{}
	for _, iss := range []string{"", "accounts.example.test", "https://accounts.example.test/", "https://accounts.example.test/v1", "https://u@accounts.example.test", "https://accounts.example.test?x", "ftp://accounts.example.test"} {
		if _, err := New(Config{Issuer: iss, Organization: "example", Inputs: in}); err == nil {
			t.Errorf("issuer %q accepted", iss)
		}
	}
	if _, err := New(Config{Issuer: worldIss, Organization: "Example", Inputs: in}); err == nil {
		t.Error("an organization out of rule 5 accepted")
	}
	if _, err := New(Config{Issuer: worldIss, Organization: "example"}); err == nil {
		t.Error("no inputs accepted")
	}
	if _, err := New(Config{Issuer: worldIss, Organization: "example", Inputs: in, KeyID: "k"}); err == nil {
		t.Error("a key id without a key accepted")
	}
	srv, err := New(Config{Issuer: "http://127.0.0.1:8443", Organization: "example", Inputs: in})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(srv.KeyID(), "ref-") || srv.KeyID() == fixtureKeyID {
		t.Errorf("a made key's id %q", srv.KeyID())
	}
}
