package refserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// enrolBody is an enrolment request's body with the given public_key text.
func enrolBody(code, name, key string) string {
	c, _ := json.Marshal(code)
	n, _ := json.Marshal(name)
	return fmt.Sprintf(`{"code":%s,"name":%s,"public_key":%s}`, c, n, key)
}

// enrolKey is the public key no world server holds; okp is its members.
const (
	okp      = `"kty":"OKP","crv":"Ed25519","x":"NHeag29J2Xiwi2c44mZPGMwy4iqkMSHXAs3I7EdsKEc"`
	enrolKey = `{` + okp + `}`
)

// edKey is an OKP JWK whose x is the given 32 bytes.
func edKey(x []byte) string {
	return `{"kty":"OKP","crv":"Ed25519","x":"` + base64.RawURLEncoding.EncodeToString(x) + `"}`
}

// leBytes is n as 32 little-endian bytes, the top bit set when sign is.
func leBytes(n *big.Int, sign bool) []byte {
	be := n.FillBytes(make([]byte, 32))
	le := make([]byte, 32)
	for i := range be {
		le[31-i] = be[i]
	}
	if sign {
		le[31] |= 0x80
	}
	return le
}

// TestEnrolmentRefusals checks each refusal of an enrolment request is the
// contract's exact answer, and that a request refused before the code is
// looked up leaves the code unspent: the same code then enrols.
func TestEnrolmentRefusals(t *testing.T) {
	p256 := `"kty":"EC","crv":"P-256","x":"lgXRKGbOcqgqy6-Z1LKCCXLo0KV-X_Cm1lTAjnP1UX0"`
	p256y := `"y":"ENsG2DlqEy3iHEW1uaPvXafkkGOruSH8dXAchekkckE"`
	deep := strings.Repeat("[", 8) + strings.Repeat("]", 8)
	bad := func(key string) string { return enrolBody(srvCCode, enrolName, key) }
	type refusal struct {
		name, contentType, body string
		status                  int
		code                    string
	}
	cases := []refusal{
		{"text/plain", "text/plain", bad(enrolKey), 415, "UNSUPPORTED_MEDIA_TYPE"},
		{"no media type", "", bad(enrolKey), 415, "UNSUPPORTED_MEDIA_TYPE"},
		{"over 8 KiB", "application/json", bad(`{` + okp + `,"kid":"` + strings.Repeat("k", 8<<10) + `"}`), 413, "PAYLOAD_TOO_LARGE"},
		{"not JSON", "application/json", `{"code":`, 400, "INVALID_REQUEST"},
		{"an array", "application/json", `[` + bad(enrolKey) + `]`, 400, "INVALID_REQUEST"},
		{"trailing data", "application/json", bad(enrolKey) + `{}`, 400, "INVALID_REQUEST"},
		{"code twice", "application/json", `{"code":"x","code":"` + srvCCode + `","name":"n","public_key":` + enrolKey + `}`, 400, "INVALID_REQUEST"},
		{"unknown member twice", "application/json", `{"x":1,"x":2,"code":"` + srvCCode + `","name":"n","public_key":` + enrolKey + `}`, 400, "INVALID_REQUEST"},
		{"a member named twice inside an unknown one", "application/json", `{"x":{"a":1,"a":2},"code":"` + srvCCode + `","name":"n","public_key":` + enrolKey + `}`, 400, "INVALID_REQUEST"},
		{"invalid UTF-8", "application/json", `{"code":"` + srvCCode + `","name":"n","x":"` + "\xff" + `","public_key":` + enrolKey + `}`, 400, "INVALID_REQUEST"},
		{"no code", "application/json", `{"name":"n","public_key":` + enrolKey + `}`, 400, "INVALID_REQUEST"},
		{"empty code", "application/json", enrolBody("", enrolName, enrolKey), 400, "INVALID_REQUEST"},
		{"code of 65 bytes", "application/json", enrolBody(strings.Repeat("c", 65), enrolName, enrolKey), 400, "INVALID_REQUEST"},
		{"code a number", "application/json", `{"code":5,"name":"n","public_key":` + enrolKey + `}`, 400, "INVALID_REQUEST"},
		{"no name", "application/json", `{"code":"` + srvCCode + `","public_key":` + enrolKey + `}`, 400, "INVALID_REQUEST"},
		{"empty name", "application/json", enrolBody(srvCCode, "", enrolKey), 400, "INVALID_REQUEST"},
		{"name of 256", "application/json", enrolBody(srvCCode, strings.Repeat("n", 256), enrolKey), 400, "INVALID_REQUEST"},
		{"name with a control character", "application/json", enrolBody(srvCCode, "a\tb", enrolKey), 400, "INVALID_REQUEST"},
		{"name not ASCII", "application/json", enrolBody(srvCCode, "café", enrolKey), 400, "INVALID_REQUEST"},
		{"no public_key", "application/json", `{"code":"` + srvCCode + `","name":"n"}`, 400, "INVALID_REQUEST"},

		// Every fault of the key is INVALID_PUBLIC_KEY, its size and shape
		// included.
		{"key a string", "application/json", bad(`"ssh-ed25519 AAAA"`), 400, "INVALID_PUBLIC_KEY"},
		{"key an array", "application/json", bad(`[` + enrolKey + `]`), 400, "INVALID_PUBLIC_KEY"},
		{"key null", "application/json", bad(`null`), 400, "INVALID_PUBLIC_KEY"},
		{"key over 2048 bytes", "application/json", bad(`{` + okp + `,"kid":"` + strings.Repeat("k", 2048) + `"}`), 400, "INVALID_PUBLIC_KEY"},
		{"private member d", "application/json", bad(`{` + okp + `,"d":"NHeag29J2Xiwi2c44mZPGMwy4iqkMSHXAs3I7EdsKEc"}`), 400, "INVALID_PUBLIC_KEY"},
		{"private member p", "application/json", bad(`{` + okp + `,"p":"x"}`), 400, "INVALID_PUBLIC_KEY"},
		{"private member k", "application/json", bad(`{` + okp + `,"k":"x"}`), 400, "INVALID_PUBLIC_KEY"},
		{"another member", "application/json", bad(`{` + okp + `,"x5c":[]}`), 400, "INVALID_PUBLIC_KEY"},
		{"y on an OKP key", "application/json", bad(`{` + okp + `,` + p256y + `}`), 400, "INVALID_PUBLIC_KEY"},
		{"RSA", "application/json", bad(`{"kty":"RSA","n":"AQAB","e":"AQAB"}`), 400, "INVALID_PUBLIC_KEY"},
		{"X25519", "application/json", bad(`{"kty":"OKP","crv":"X25519","x":"NHeag29J2Xiwi2c44mZPGMwy4iqkMSHXAs3I7EdsKEc"}`), 400, "INVALID_PUBLIC_KEY"},
		{"P-384", "application/json", bad(`{"kty":"EC","crv":"P-384","x":"lgXRKGbOcqgqy6-Z1LKCCXLo0KV-X_Cm1lTAjnP1UX0",` + p256y + `}`), 400, "INVALID_PUBLIC_KEY"},
		{"x padded", "application/json", bad(`{"kty":"OKP","crv":"Ed25519","x":"NHeag29J2Xiwi2c44mZPGMwy4iqkMSHXAs3I7EdsKEc="}`), 400, "INVALID_PUBLIC_KEY"},
		{"x standard base64", "application/json", bad(`{"kty":"OKP","crv":"Ed25519","x":"7+iGf8mkKzS2jTSNmrFW/kpXGFrMRrVckdxsVnHI6lM"}`), 400, "INVALID_PUBLIC_KEY"},
		{"x of 31 bytes", "application/json", bad(edKey(make([]byte, 31))), 400, "INVALID_PUBLIC_KEY"},
		// The standard decoder skips CR and LF even in strict mode; the
		// contract's base64url has neither.
		{"x with a line feed", "application/json", bad(`{"kty":"OKP","crv":"Ed25519","x":"NHeag29J2Xiwi2c44mZPGMwy4iqk\nMSHXAs3I7EdsKEc"}`), 400, "INVALID_PUBLIC_KEY"},
		{"x with a carriage return", "application/json", bad(`{"kty":"OKP","crv":"Ed25519","x":"NHeag29J2Xiwi2c44mZPGMwy4iqk\rMSHXAs3I7EdsKEc"}`), 400, "INVALID_PUBLIC_KEY"},
		{"x ending in CRLF", "application/json", bad(`{"kty":"OKP","crv":"Ed25519","x":"NHeag29J2Xiwi2c44mZPGMwy4iqkMSHXAs3I7EdsKEc\r\n"}`), 400, "INVALID_PUBLIC_KEY"},
		{"P-256 y with a line feed", "application/json", bad(`{` + p256 + `,"y":"ENsG2DlqEy3iHEW1uaPvXafkkGOruSH8dXAchekkck\nE"}`), 400, "INVALID_PUBLIC_KEY"},
		{"x missing", "application/json", bad(`{"kty":"OKP","crv":"Ed25519"}`), 400, "INVALID_PUBLIC_KEY"},
		{"EC without y", "application/json", bad(`{` + p256 + `}`), 400, "INVALID_PUBLIC_KEY"},
		{"EC off the curve", "application/json", bad(`{` + p256 + `,"y":"ENsG2DlqEy3iHEW1uaPvXafkkGOruSH8dXAchekkckA"}`), 400, "INVALID_PUBLIC_KEY"},
		{"alg not the key's", "application/json", bad(`{` + okp + `,"alg":"ES256"}`), 400, "INVALID_PUBLIC_KEY"},
		{"use enc", "application/json", bad(`{` + okp + `,"use":"enc"}`), 400, "INVALID_PUBLIC_KEY"},
		{"key_ops sign only", "application/json", bad(`{` + okp + `,"key_ops":["sign"]}`), 400, "INVALID_PUBLIC_KEY"},
		{"key_ops repeated", "application/json", bad(`{` + okp + `,"key_ops":["verify","verify"]}`), 400, "INVALID_PUBLIC_KEY"},
		{"key_ops a string", "application/json", bad(`{` + okp + `,"key_ops":"verify"}`), 400, "INVALID_PUBLIC_KEY"},
		{"key_ops encrypt", "application/json", bad(`{` + okp + `,"key_ops":["verify","encrypt"]}`), 400, "INVALID_PUBLIC_KEY"},
		{"kid a number", "application/json", bad(`{` + okp + `,"kid":5}`), 400, "INVALID_PUBLIC_KEY"},
		{"kty twice", "application/json", bad(`{"kty":"OKP",` + okp + `}`), 400, "INVALID_PUBLIC_KEY"},
		{"nested deeper than eight", "application/json", bad(`{` + okp + `,"kid":` + deep + `}`), 400, "INVALID_PUBLIC_KEY"},
		{"y = p, not canonical", "application/json", bad(edKey(leBytes(ed25519P, false))), 400, "INVALID_PUBLIC_KEY"},
		{"y = p + 1, not canonical", "application/json", bad(edKey(leBytes(new(big.Int).Add(ed25519P, big.NewInt(1)), false))), 400, "INVALID_PUBLIC_KEY"},
	}
	for i, y := range ed25519SmallOrderY {
		for _, sign := range []bool{false, true} {
			cases = append(cases, refusal{fmt.Sprintf("small order %d sign %v", i, sign), "application/json", bad(edKey(leBytes(y, sign))), 400, "INVALID_PUBLIC_KEY"})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWorld(t)
			rec := s.post(pathEnroll, tc.contentType, tc.body)
			if rec.Code != tc.status || v1Error(t, rec) != tc.code {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control %q", got)
			}
			if rec := s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, enrolKey)); rec.Code != http.StatusCreated {
				t.Fatalf("the code did not stay unspent: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// TestEnrolmentAcceptsKeyVariants checks the optional JWK members the
// contract allows, and a P-256 key.
func TestEnrolmentAcceptsKeyVariants(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := ec.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	for _, key := range []string{
		`{` + okp + `,"alg":"EdDSA","use":"sig","kid":"k1"}`,
		`{` + okp + `,"key_ops":["verify"]}`,
		`{` + okp + `,"key_ops":["sign","verify"]}`,
		`{"kty":"EC","crv":"P-256","x":"` + enc(pt[1:33]) + `","y":"` + enc(pt[33:]) + `","alg":"ES256"}`,
	} {
		s := newWorld(t)
		if rec := s.post(pathEnroll, "application/json; charset=utf-8", enrolBody(srvCCode, enrolName, key)); rec.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", key, rec.Code, rec.Body)
		}
	}
}

// TestEnrolmentKeyInUseByDisabledServer checks a key a disabled server
// holds is still in use: a disabled server comes back only with a new key.
func TestEnrolmentKeyInUseByDisabledServer(t *testing.T) {
	s := newWorld(t)
	key := string(s.h.jwks["server-p256"]) // the disabled srv-b's
	rec := s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, key))
	if rec.Code != http.StatusConflict || v1Error(t, rec) != "PUBLIC_KEY_IN_USE" {
		t.Fatalf("%d %s, want 409 PUBLIC_KEY_IN_USE", rec.Code, rec.Body)
	}
	// The same key with a kid and members in another order is the same key.
	var m map[string]string
	if err := json.Unmarshal(s.h.jwks["server-ed25519"], &m); err != nil {
		t.Fatal(err)
	}
	rec = s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, `{"x":"`+m["x"]+`","crv":"Ed25519","kty":"OKP","kid":"other"}`))
	if rec.Code != http.StatusConflict || v1Error(t, rec) != "PUBLIC_KEY_IN_USE" {
		t.Fatalf("%d %s, want 409 PUBLIC_KEY_IN_USE", rec.Code, rec.Body)
	}
}

// TestEnrolmentNameOfDisabledServer checks a code for a disabled server's
// name enrols a new server: only a live server's name is in use.
func TestEnrolmentNameOfDisabledServer(t *testing.T) {
	s := newWorld(t)
	s.in.codes["code-for-srv-b"] = Code{ServerName: "srv-b"}
	rec := s.post(pathEnroll, "application/json", enrolBody("code-for-srv-b", enrolName, enrolKey))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s, want 201", rec.Code, rec.Body)
	}
}

// TestEnrolmentOneCodeConcurrently checks a code enrols once when many
// requests carry it at the same time, each with its own key.
func TestEnrolmentOneCodeConcurrently(t *testing.T) {
	s := newWorld(t)
	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		x := make([]byte, 32)
		x[0] = byte(i + 2)
		x[1] = 0x55 // well clear of the small-order points
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, edKey(x)))
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}()
	}
	wg.Wait()
	created := 0
	for i, c := range codes {
		switch {
		case c == http.StatusCreated:
			created++
		case c == http.StatusConflict && strings.Contains(bodies[i], `"ENROLMENT_CODE_SPENT"`):
		default:
			t.Errorf("%d %s, want 201 or 409 ENROLMENT_CODE_SPENT", c, bodies[i])
		}
	}
	if created != 1 {
		t.Fatalf("%d enrolments with one code, want 1 (%v)", created, codes)
	}
}

// ed25519D is the curve constant d = -121665/121666 mod p.
func ed25519D() *big.Int {
	p := ed25519P
	d := new(big.Int).Mul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), p))
	return d.Mod(d, p)
}

// TestEd25519SmallOrderPoints checks the small-order y coordinates: the
// order-8 ones are the roots of d*y^4 + 2*y^2 - 1 (a point that doubles to
// y = 0, an order-4 point).
func TestEd25519SmallOrderPoints(t *testing.T) {
	p, d := ed25519P, ed25519D()
	for _, y := range []*big.Int{ed25519Order8Y, new(big.Int).Sub(p, ed25519Order8Y)} {
		y2 := new(big.Int).Exp(y, big.NewInt(2), p)
		v := new(big.Int).Mul(d, new(big.Int).Exp(y2, big.NewInt(2), p))
		v.Add(v, new(big.Int).Lsh(y2, 1))
		v.Sub(v, big.NewInt(1))
		if v.Mod(v, p).Sign() != 0 {
			t.Errorf("y = %s is not of order 8", y)
		}
	}
	// A real key passes.
	if err := checkEd25519Point(leBytes(big.NewInt(12345), false)); err != nil {
		t.Errorf("an ordinary y is refused: %v", err)
	}
}

// TestEnrolmentEd25519NotAPoint checks rule 11's other branch: a canonical
// x that is not on the curve enrols here, and every assertion for it fails.
func TestEnrolmentEd25519NotAPoint(t *testing.T) {
	p, d := ed25519P, ed25519D()
	exp := new(big.Int).Rsh(new(big.Int).Sub(p, big.NewInt(1)), 1)
	var y *big.Int
	for c := int64(2); ; c++ {
		cand := big.NewInt(c)
		y2 := new(big.Int).Mul(cand, cand)
		u := new(big.Int).Sub(y2, big.NewInt(1))
		v := new(big.Int).Add(new(big.Int).Mul(d, y2), big.NewInt(1))
		x2 := new(big.Int).Mul(u, new(big.Int).ModInverse(v, p))
		x2.Mod(x2, p)
		// Euler's criterion: x2 has no square root when x2^((p-1)/2) = -1.
		if new(big.Int).Exp(x2, exp, p).Cmp(new(big.Int).Sub(p, big.NewInt(1))) == 0 {
			y = cand
			break
		}
	}
	s := newWorld(t)
	rec := s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, edKey(leBytes(y, false))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var created struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// Any signature, here one by another key, is refused.
	a := s.defaultAssertion(created.ClientID, "EdDSA").sign(s.key("enrol-ed25519"))
	if rec := s.tokenRequest(withAssertion(a)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d %s, want 401", rec.Code, rec.Body)
	}
}

// TestEnrolmentOutage checks a failed code lookup is 503 UNAVAILABLE and
// spends nothing.
func TestEnrolmentOutage(t *testing.T) {
	s := newWorld(t)
	s.in.fail = true
	rec := s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, enrolKey))
	if rec.Code != http.StatusServiceUnavailable || v1Error(t, rec) != "UNAVAILABLE" || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("%d %s %v, want 503 UNAVAILABLE", rec.Code, rec.Body, rec.Header())
	}
	s.in.fail = false
	if rec := s.post(pathEnroll, "application/json", enrolBody(srvCCode, enrolName, enrolKey)); rec.Code != http.StatusCreated {
		t.Fatalf("%d after the outage", rec.Code)
	}
}
