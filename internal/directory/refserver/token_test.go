package refserver

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAssertionRefusals checks each check of a client assertion: the
// unchanged assertion is accepted, and each change of one header member or
// claim is refused 401 invalid_client (or accepted, where the contract
// allows it).
func TestAssertionRefusals(t *testing.T) {
	type change func(p assertionParts) assertionParts
	header := func(f func([]member) []member) change {
		return func(p assertionParts) assertionParts { p.header = f(p.header); return p }
	}
	claims := func(f func([]member) []member) change {
		return func(p assertionParts) assertionParts { p.claims = f(p.claims); return p }
	}
	setH := func(name, raw string) change { return header(func(m []member) []member { return set(m, name, raw) }) }
	setC := func(name, raw string) change { return claims(func(m []member) []member { return set(m, name, raw) }) }
	dropH := func(name string) change { return header(func(m []member) []member { return drop(m, name) }) }
	dropC := func(name string) change { return claims(func(m []member) []member { return drop(m, name) }) }
	twiceH := func(name string) change {
		return header(func(m []member) []member {
			for _, x := range m {
				if x.name == name {
					return append(m, x)
				}
			}
			panic(name)
		})
	}

	cases := []struct {
		name   string
		change change
		ok     bool
	}{
		{"unchanged", func(p assertionParts) assertionParts { return p }, true},
		{"typ absent", dropH("typ"), true},
		{"typ JWT", setH("typ", `"JWT"`), true},
		{"typ application/jwt", setH("typ", `"application/jwt"`), true},
		{"typ APPLICATION/CLIENT-AUTHENTICATION+JWT", setH("typ", `"APPLICATION/CLIENT-AUTHENTICATION+JWT"`), true},
		{"aud a one-element array", setC("aud", `["`+worldIss+`"]`), true},
		{"a kid, the key having none", setH("kid", `"any"`), true},
		{"nbf in the past", setC("nbf", "0"), true},
		{"jti of 256 bytes", setC("jti", `"`+strings.Repeat("j", 256)+`"`), true},
		{"an unknown claim", setC("note", `{"a":[1,2]}`), true},
		{"an unknown header member", setH("cty", `"x"`), true},

		{"typ at+jwt", setH("typ", `"at+jwt"`), false},
		{"typ jwt+other", setH("typ", `"other+jwt"`), false},
		{"typ null", setH("typ", `null`), false},
		{"typ a number", setH("typ", `1`), false},
		{"alg ES256 for an Ed25519 key", setH("alg", `"ES256"`), false},
		{"alg none", setH("alg", `"none"`), false},
		{"alg missing", dropH("alg"), false},
		{"kid a number", setH("kid", `5`), false},
		{"jwk member", setH("jwk", `{}`), false},
		{"jwk null", setH("jwk", `null`), false},
		{"jku", setH("jku", `"https://accounts.example.test/k"`), false},
		{"x5u", setH("x5u", `null`), false},
		{"x5c", setH("x5c", `[]`), false},
		{"crit", setH("crit", `["b64"]`), false},
		{"b64", setH("b64", `false`), false},
		{"header member twice", twiceH("alg"), false},
		{"iss missing", dropC("iss"), false},
		{"sub missing", dropC("sub"), false},
		{"sub not iss", setC("sub", `"`+srvB+`"`), false},
		{"iss a number", setC("iss", `5`), false},
		{"aud missing", dropC("aud"), false},
		{"aud empty array", setC("aud", `[]`), false},
		{"aud the issuer twice", setC("aud", `["`+worldIss+`","`+worldIss+`"]`), false},
		{"aud the /v1 audience", setC("aud", `"`+worldIss+`/v1"`), false},
		{"aud another case", setC("aud", `"HTTPS://accounts.example.test"`), false},
		{"aud an object", setC("aud", `{"aud":"`+worldIss+`"}`), false},
		{"iat missing", dropC("iat"), false},
		{"exp missing", dropC("exp"), false},
		{"iat a fraction", setC("iat", `1.5`), false},
		{"exp a string", setC("exp", `"9999999999"`), false},
		{"exp an exponent", setC("exp", `1e10`), false},
		{"nbf a string", setC("nbf", `"0"`), false},
		{"jti missing", dropC("jti"), false},
		{"jti empty", setC("jti", `""`), false},
		{"jti of 257 bytes", setC("jti", `"`+strings.Repeat("j", 257)+`"`), false},
		{"jti a number", setC("jti", `5`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWorld(t)
			p := tc.change(s.defaultAssertion(srvA, "EdDSA"))
			rec := s.tokenRequest(withAssertion(p.sign(s.key("server-ed25519"))))
			if tc.ok {
				if rec.Code != http.StatusOK {
					t.Fatalf("%d %s, want 200", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusUnauthorized || oauthError(t, rec) != "invalid_client" {
				t.Fatalf("%d %s, want 401 invalid_client", rec.Code, rec.Body)
			}
			if rec.Header().Get("WWW-Authenticate") != "" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("headers %v", rec.Header())
			}
		})
	}
}

// TestAssertionFormRefusals checks the assertion's form: its size, its
// parts and their encoding, and its signature.
func TestAssertionFormRefusals(t *testing.T) {
	s := newWorld(t)
	key := s.key("server-ed25519")
	good := s.defaultAssertion(srvA, "EdDSA").sign(key)
	parts := strings.Split(good, ".")
	big := s.defaultAssertion(srvA, "EdDSA")
	big.claims = set(big.claims, "pad", `"`+strings.Repeat("p", 4096)+`"`)
	sig := []byte(parts[2])
	sig[3] ^= 0x01 // another base64url letter
	for name, a := range map[string]string{
		"over 4096 bytes":       big.sign(key),
		"two parts":             parts[0] + "." + parts[1],
		"four parts":            good + ".x",
		"a padded header":       parts[0] + "=." + parts[1] + "." + parts[2],
		"no signature":          parts[0] + "." + parts[1] + ".",
		"a changed signature":   parts[0] + "." + parts[1] + "." + string(sig),
		"a truncated signature": parts[0] + "." + parts[1] + "." + parts[2][:40],
		"another key's":         s.defaultAssertion(srvA, "EdDSA").sign(s.key("enrol-ed25519")),
		"the disabled server's": s.defaultAssertion(srvB, "ES256").sign(s.key("server-p256")),
		"a header not JSON":     b64([]byte("{")) + "." + parts[1] + "." + parts[2],
		"a header an array":     b64([]byte(`["EdDSA"]`)) + "." + parts[1] + "." + parts[2],
	} {
		rec := s.tokenRequest(withAssertion(a))
		if rec.Code != http.StatusUnauthorized || oauthError(t, rec) != "invalid_client" {
			t.Errorf("%s: %d %s, want 401 invalid_client", name, rec.Code, rec.Body)
		}
	}
	if rec := s.tokenRequest(withAssertion(good)); rec.Code != http.StatusOK {
		t.Fatalf("the good assertion after the refusals: %d %s", rec.Code, rec.Body)
	}
}

// TestAssertionKidOfAKeyWithKid checks a kid the key was enrolled with must
// be the assertion's kid when the assertion sends one.
func TestAssertionKidOfAKeyWithKid(t *testing.T) {
	s := newWorld(t)
	var m map[string]string
	if err := json.Unmarshal(s.h.jwks["enrol-ed25519"], &m); err != nil {
		t.Fatal(err)
	}
	rec := s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, `{"kty":"OKP","crv":"Ed25519","x":"`+m["x"]+`","kid":"k1"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var created struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	key := s.key("enrol-ed25519")
	for kid, want := range map[string]int{`"k2"`: 401, `5`: 401, `"k1"`: 200} {
		p := s.defaultAssertion(created.ClientID, "EdDSA")
		p.header = set(p.header, "kid", kid)
		if rec := s.tokenRequest(withAssertion(p.sign(key))); rec.Code != want {
			t.Errorf("kid %s: %d %s, want %d", kid, rec.Code, rec.Body, want)
		}
	}
	p := s.defaultAssertion(created.ClientID, "EdDSA")
	if rec := s.tokenRequest(withAssertion(p.sign(key))); rec.Code != http.StatusOK {
		t.Errorf("no kid: %d %s, want 200", rec.Code, rec.Body)
	}
}

// TestTokenRequestRefusals checks the token request's own rules: the grant
// first, the five parameters each once (an empty value counts), no other
// client authentication, no resource or audience, the scope, and the
// assertion's two parameters together. A refusal before the assertion is
// spent leaves it usable.
func TestTokenRequestRefusals(t *testing.T) {
	q := url.QueryEscape
	cases := []struct {
		name, contentType, form, auth string
		status                        int
		code                          string
		spent                         bool // whether the assertion is spent by the request
	}{
		{"no grant_type", "", "client_assertion_type=" + q(assertionTypeJWT) + "&client_assertion=A", "", 400, "unsupported_grant_type", false},
		{"grant_type password twice", "", "grant_type=password&grant_type=password&client_assertion_type=" + q(assertionTypeJWT) + "&client_assertion=A", "", 400, "unsupported_grant_type", false},
		{"grant_type twice", "", "grant_type=client_credentials&" + formPrefix[len("grant_type=client_credentials&"):] + "A&grant_type=client_credentials", "", 400, "invalid_request", false},
		{"client_assertion twice, one empty", "", formPrefix + "A&client_assertion=", "", 400, "invalid_request", false},
		{"scope twice, one empty", "", formPrefix + "A&scope=&scope=directory%3Aread", "", 400, "invalid_request", false},
		{"client_id twice", "", formPrefix + "A&client_id=" + srvA + "&client_id=" + srvA, "", 400, "invalid_request", false},
		{"an Authorization header", "", formPrefix + "A", "Basic eDp5", 401, "invalid_client", false},
		{"client_secret", "", formPrefix + "A&client_secret=s", "", 401, "invalid_client", false},
		{"resource", "", formPrefix + "A&resource=" + q(worldIss+"/v1"), "", 400, "invalid_target", false},
		{"audience", "", formPrefix + "A&audience=x", "", 400, "invalid_target", false},
		{"a scope it lacks", "", formPrefix + "A&scope=other%3Aread", "", 400, "invalid_scope", false},
		{"a scope with two spaces", "", formPrefix + "A&scope=directory%3Aread++directory%3Aread", "", 400, "invalid_scope", false},
		{"client_assertion alone", "", "grant_type=client_credentials&client_assertion=A", "", 400, "invalid_request", false},
		{"client_assertion_type alone", "", "grant_type=client_credentials&client_assertion_type=" + q(assertionTypeJWT), "", 400, "invalid_request", false},
		{"another assertion type", "", "grant_type=client_credentials&client_assertion_type=urn%3Aother&client_assertion=A", "", 401, "invalid_client", false},
		{"no assertion", "", "grant_type=client_credentials", "", 401, "invalid_client", false},
		{"not a form", "application/json", formPrefix + "A", "", 400, "invalid_request", false},
		{"a malformed form", "", formPrefix + "A&x=%zz", "", 400, "invalid_request", false},
		{"another client_id", "", formPrefix + "A&client_id=" + srvB, "", 401, "invalid_client", false},

		{"an empty client_secret", "", formPrefix + "A&client_secret=", "", 200, "", true},
		{"its own client_id", "", formPrefix + "A&client_id=" + srvA, "", 200, "", true},
		{"scope directory:read", "", formPrefix + "A&scope=directory%3Aread", "", 200, "", true},
		{"an unknown parameter twice", "", formPrefix + "A&x=1&x=2", "", 200, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWorld(t)
			a := s.defaultAssertion(srvA, "EdDSA").sign(s.key("server-ed25519"))
			req := strings.Replace(tc.form, "client_assertion=A", "client_assertion="+q(a), 1)
			ct := tc.contentType
			if ct == "" {
				ct = "application/x-www-form-urlencoded"
			}
			r, _ := http.NewRequest(http.MethodPost, pathToken, strings.NewReader(req))
			r.Header.Set("Content-Type", ct)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			rec := s.do(r)
			if rec.Code != tc.status {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, tc.status)
			}
			if tc.code != "" && oauthError(t, rec) != tc.code {
				t.Fatalf("%s, want %s", rec.Body, tc.code)
			}
			again := s.tokenRequest(withAssertion(a))
			switch {
			case tc.spent && again.Code != http.StatusUnauthorized:
				t.Errorf("the assertion was not spent: %d", again.Code)
			case !tc.spent && again.Code != http.StatusOK:
				t.Errorf("the assertion was spent by a refused request: %d %s", again.Code, again.Body)
			}
		})
	}
}

// TestTokenBodyTooLarge checks a body over 64 KiB is a bare 413.
func TestTokenBodyTooLarge(t *testing.T) {
	s := newWorld(t)
	rec := s.tokenRequest(formPrefix + strings.Repeat("A", 64<<10))
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %q %v, want a bare 413", rec.Code, rec.Body, rec.Header())
	}
}

// TestTokenOK checks the 200 answer and that its token opens the directory.
func TestTokenOK(t *testing.T) {
	s := newWorld(t)
	rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(s.key("server-ed25519"))))
	if rec.Code != http.StatusOK || rec.Header().Get("Pragma") != "no-cache" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var ok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.TokenType != "bearer" || ok.ExpiresIn != 600 || ok.Scope != "directory:read" {
		t.Fatalf("%+v", ok)
	}
	if rec := s.directory("Bearer " + ok.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("directory: %d %s", rec.Code, rec.Body)
	}
	// A P-256 server's assertion: srv-b is disabled, so enable it first.
	delete(s.in.disabled, srvB)
	if rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvB, "ES256").sign(s.key("server-p256")))); rec.Code != http.StatusOK {
		t.Fatalf("ES256: %d %s", rec.Code, rec.Body)
	}
}

// TestReplayConcurrent checks one assertion presented by many callers at
// once is accepted exactly once: the ledger checks and spends in one step.
func TestReplayConcurrent(t *testing.T) {
	s := newWorld(t)
	a := withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(s.key("server-ed25519")))
	const n = 32
	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = s.tokenRequest(a).Code
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
		default:
			t.Errorf("status %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d presentations of one assertion accepted, want 1", ok, n)
	}
}

// TestReplayLedgerKeepsThroughLeeway checks the ledger: an entry is kept
// until exp plus the leeway, a replay inside the leeway is refused, and
// once the ledger has forgotten an entry, a clock moved back cannot bring
// the assertion back.
func TestReplayLedgerKeepsThroughLeeway(t *testing.T) {
	s := newWorld(t)
	t0 := s.clock
	key := s.key("server-ed25519")
	a := withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(key)) // exp = t0 + 60

	if rec := s.tokenRequest(a); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	// Inside the leeway, after another request has pruned the ledger.
	s.at(t0.Add(89 * time.Second))
	if rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(key))); rec.Code != http.StatusOK {
		t.Fatalf("a fresh assertion at t0+89: %d", rec.Code)
	}
	if rec := s.tokenRequest(a); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the replay at t0+89, inside the leeway: %d, want 401", rec.Code)
	}
	// Past the window the ledger may forget it; a fresh request prunes.
	s.at(t0.Add(95 * time.Second))
	if rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(key))); rec.Code != http.StatusOK {
		t.Fatalf("a fresh assertion at t0+95: %d", rec.Code)
	}
	// The clock moved back into the first assertion's window: still a
	// replay.
	s.at(t0.Add(10 * time.Second))
	if rec := s.tokenRequest(a); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the replay after the clock moved back: %d, want 401", rec.Code)
	}
}

// TestLedgerUnit checks the ledger's bounds directly.
func TestLedgerUnit(t *testing.T) {
	var l ledger
	l.spent = map[ledgerKey]int64{}
	t0 := time.Unix(1_800_000_000, 0)
	exp := t0.Unix() + 60
	if !l.spend("c", "j", exp, t0) {
		t.Fatal("first spend refused")
	}
	if l.spend("c", "j", exp, t0.Add(89*time.Second)) {
		t.Fatal("a replay at exp+29 accepted")
	}
	if _, kept := l.spent[ledgerKey{"c", "j"}]; !kept {
		t.Fatal("the entry was forgotten before exp+30")
	}
	if !l.spend("c", "other", exp+60, t0.Add(90*time.Second)) {
		t.Fatal("another jti refused")
	}
	if _, kept := l.spent[ledgerKey{"c", "j"}]; kept {
		t.Fatal("the entry was kept past exp+30")
	}
	if l.spend("c", "j", exp, t0.Add(10*time.Second)) {
		t.Fatal("a forgotten entry was accepted after the clock moved back")
	}
	if !l.spend("d", "j", exp+100, t0.Add(10*time.Second)) {
		t.Fatal("another client's jti refused")
	}
}

// TestLedgerFloorAfterClockMovesBack pins the ledger's floor as intended:
// once the ledger has been pruned at the latest time the server has seen, a
// clock moved back makes a fresh, never-seen assertion whose acceptance
// window (exp plus the leeway) ends at or before that time refused, until
// the clock catches up; one whose window ends after it is accepted.
func TestLedgerFloorAfterClockMovesBack(t *testing.T) {
	s := newWorld(t)
	t0 := s.clock
	key := s.key("server-ed25519")
	// iatOffset makes a fresh assertion with iat = the clock + off and
	// exp = iat + 60, so its window ends at clock + off + 90.
	fresh := func(off int64) string {
		p := s.defaultAssertion(srvA, "EdDSA")
		iat := s.clock.Unix() + off
		p.claims = set(set(p.claims, "iat", fmt.Sprint(iat)), "exp", fmt.Sprint(iat+60))
		return withAssertion(p.sign(key))
	}
	s.at(t0.Add(200 * time.Second))
	if rec := s.tokenRequest(fresh(0)); rec.Code != http.StatusOK {
		t.Fatalf("at t0+200: %d", rec.Code) // the floor is now t0+200
	}
	s.at(t0.Add(100 * time.Second)) // the clock moved back 100 s
	for _, c := range []struct {
		off  int64
		want int
	}{
		{0, 401},  // window ends t0+190, before the floor
		{10, 401}, // window ends t0+200, at the floor
		{11, 200}, // window ends t0+201, after the floor
		{30, 200}, // the furthest iat the leeway allows
	} {
		if rec := s.tokenRequest(fresh(c.off)); rec.Code != c.want {
			t.Errorf("at t0+100, iat offset %d: %d %s, want %d", c.off, rec.Code, rec.Body, c.want)
		}
	}
	s.at(t0) // moved back further: no assertion the window allows can pass
	if rec := s.tokenRequest(fresh(30)); rec.Code != http.StatusUnauthorized {
		t.Errorf("at t0, iat offset 30: %d, want 401", rec.Code)
	}
	s.at(t0.Add(200 * time.Second)) // the clock caught up
	if rec := s.tokenRequest(fresh(0)); rec.Code != http.StatusOK {
		t.Errorf("caught up: %d, want 200", rec.Code)
	}

	// The same bounds on the ledger alone.
	var l ledger
	l.spent = map[ledgerKey]int64{}
	t1 := time.Unix(1_800_000_000, 0)
	if !l.spend("c", "a", t1.Unix()+60, t1) {
		t.Fatal("first spend refused")
	}
	back := t1.Add(-120 * time.Second)
	if l.spend("c", "b", t1.Unix()-30, back) {
		t.Error("an assertion whose window ends at the floor accepted")
	}
	if !l.spend("c", "d", t1.Unix()-29, back) {
		t.Error("an assertion whose window ends a second after the floor refused")
	}
}

// TestAssertionNumericDateLimits checks iat, exp and nbf at and near the
// int64 limits: each is refused (none wraps into the window), and the
// assertion that follows still opens, nothing having been spent.
func TestAssertionNumericDateLimits(t *testing.T) {
	const maxI, minI = math.MaxInt64, math.MinInt64
	wrapped := func(a, b int64) int64 { return a + b } // Go's signed wrap
	cases := []struct {
		name          string
		iat, exp, nbf string // "" leaves the default; "-" drops nbf
		ok            bool
	}{
		{"nbf the epoch", "", "", "0", true},
		{"nbf the int64 maximum", "", "", fmt.Sprint(int64(maxI)), false},
		{"nbf one below the int64 maximum", "", "", fmt.Sprint(int64(maxI - 1)), false},
		{"nbf the int64 minimum", "", "", fmt.Sprint(int64(minI)), false},
		{"nbf one past the year 9999", "", "", fmt.Sprint(int64(maxNumericDate + 1)), false},
		{"nbf negative", "", "", "-1", false},
		{"nbf past the int64 range", "", "", "9223372036854775808", false},
		{"iat near the maximum, exp wrapped to iat + 60", fmt.Sprint(int64(maxI - 10)), fmt.Sprint(wrapped(maxI-10, 60)), "-", false},
		{"iat and exp the int64 maximum", fmt.Sprint(int64(maxI)), fmt.Sprint(int64(maxI)), "-", false},
		{"exp the int64 maximum", "", fmt.Sprint(int64(maxI)), "-", false},
		{"iat the int64 minimum, exp 60 later", fmt.Sprint(int64(minI)), fmt.Sprint(int64(minI + 60)), "-", false},
		{"exp past the int64 range", "", "9223372036854775867", "-", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWorld(t)
			key := s.key("server-ed25519")
			p := s.defaultAssertion(srvA, "EdDSA")
			if tc.iat != "" {
				p.claims = set(p.claims, "iat", tc.iat)
			}
			if tc.exp != "" {
				p.claims = set(p.claims, "exp", tc.exp)
			}
			if tc.nbf != "-" && tc.nbf != "" {
				p.claims = set(p.claims, "nbf", tc.nbf)
			}
			rec := s.tokenRequest(withAssertion(p.sign(key)))
			if tc.ok != (rec.Code == http.StatusOK) {
				t.Fatalf("%d %s, want ok=%v", rec.Code, rec.Body, tc.ok)
			}
			if !tc.ok && (rec.Code != http.StatusUnauthorized || oauthError(t, rec) != "invalid_client") {
				t.Fatalf("%d %s, want 401 invalid_client", rec.Code, rec.Body)
			}
			if rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(key))); rec.Code != http.StatusOK {
				t.Fatalf("the next assertion: %d", rec.Code)
			}
		})
	}
}

// TestNumericDate checks the range every numeric date is held to before
// anything converts it.
func TestNumericDate(t *testing.T) {
	for raw, ok := range map[string]bool{
		"0":                    true,
		"1800000000":           true,
		"253402300799":         true, // 9999-12-31T23:59:59Z
		"253402300800":         false,
		"-1":                   false,
		"9223372036854775807":  false,
		"-9223372036854775808": false,
		"9223372036854775808":  false,
		"1e3":                  false,
		"1.0":                  false,
		`"5"`:                  false,
		"null":                 false,
	} {
		if _, got := numericDate(json.RawMessage(raw)); got != ok {
			t.Errorf("%s: %v, want %v", raw, got, ok)
		}
	}
}

// TestDecodeB64URL checks a base64url segment holds the alphabet's
// characters alone, in the one canonical form of its bytes.
func TestDecodeB64URL(t *testing.T) {
	for s, ok := range map[string]bool{
		"":         true,
		"AQAB":     true,
		"_-8":      true,
		"AQ\nAB":   false,
		"AQAB\n":   false,
		"\rAQAB":   false,
		"AQ\r\nAB": false,
		"AQ AB":    false,
		"AQAB=":    false,
		"+/8":      false,
		"AB":       false, // stray bits: "AA" is the canonical form of its byte
		"A":        false,
	} {
		if _, err := decodeB64URL(s); (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", s, err, ok)
		}
	}
}

// TestAssertionSegmentsWithCRLF checks a carriage return or line feed in
// any segment of an assertion refuses it, and spends nothing.
func TestAssertionSegmentsWithCRLF(t *testing.T) {
	s := newWorld(t)
	key := s.key("server-ed25519")
	good := s.defaultAssertion(srvA, "EdDSA").sign(key)
	parts := strings.Split(good, ".")
	mid := func(p, ins string) string { return p[:len(p)/2] + ins + p[len(p)/2:] }
	for name, a := range map[string]string{
		"LF in the signature":   parts[0] + "." + parts[1] + "." + mid(parts[2], "\n"),
		"CR in the signature":   parts[0] + "." + parts[1] + "." + mid(parts[2], "\r"),
		"CRLF in the header":    mid(parts[0], "\r\n") + "." + parts[1] + "." + parts[2],
		"LF ending the claims":  parts[0] + "." + parts[1] + "\n." + parts[2],
		"LF ending the whole":   good + "\n",
		"CR starting the whole": "\r" + good,
	} {
		rec := s.tokenRequest(withAssertion(a))
		if rec.Code != http.StatusUnauthorized || oauthError(t, rec) != "invalid_client" {
			t.Errorf("%s: %d %s, want 401 invalid_client", name, rec.Code, rec.Body)
		}
	}
	if rec := s.tokenRequest(withAssertion(good)); rec.Code != http.StatusOK {
		t.Fatalf("the good assertion after the refusals: %d %s", rec.Code, rec.Body)
	}
}

// TestTokenRateLimit checks the token limit: the 429 carries Retry-After in
// seconds and the shape of an OAuth error.
func TestTokenRateLimit(t *testing.T) {
	s := newWorld(t)
	s.srv.limits[limitToken] = newLimiter(2)
	key := s.key("server-ed25519")
	for i := 0; i < 2; i++ {
		if rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(key))); rec.Code != http.StatusOK {
			t.Fatalf("%d", rec.Code)
		}
	}
	s.at(s.clock.Add(20 * time.Second))
	rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(key)))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "40" || oauthError(t, rec) != "temporarily_unavailable" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	s.at(s.clock.Add(40 * time.Second))
	if rec := s.tokenRequest(withAssertion(s.defaultAssertion(srvA, "EdDSA").sign(key))); rec.Code != http.StatusOK {
		t.Fatalf("after the window: %d", rec.Code)
	}
}
