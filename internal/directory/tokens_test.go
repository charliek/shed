package directory

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

// signingKey is the committed token-signing key's public half.
func signingKey(t *testing.T) *rsa.PublicKey {
	t.Helper()
	k, ok := parsePKCS8(t, "fixtures/test-keys/token-signing-key.pem").(*rsa.PrivateKey)
	if !ok {
		t.Fatal("token-signing-key.pem is not an RSA key")
	}
	if k.N.BitLen() != 2048 {
		t.Errorf("token-signing-key.pem is %d bits, want 2048", k.N.BitLen())
	}
	return &k.PublicKey
}

func parsePKCS8(t *testing.T, rel string) any {
	t.Helper()
	block, rest := pem.Decode(readFixture(t, rel))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("%s is not one PKCS#8 PEM block", rel)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return k
}

// literalToken is the bearer token an exchange's first request carries.
func literalToken(t *testing.T, rel string) string {
	t.Helper()
	var ex struct {
		Request struct {
			Headers map[string]string `json:"headers"`
		} `json:"request"`
	}
	decodeFixture(t, rel, &ex)
	tok, ok := strings.CutPrefix(ex.Request.Headers["Authorization"], "Bearer ")
	if !ok {
		t.Fatalf("%s: no bearer token in the request", rel)
	}
	return tok
}

// jws is a compact JWS split into its parts.
type jws struct {
	header, claims []byte
	signingInput   string
	signature      []byte
}

func splitJWS(t *testing.T, tok string) jws {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("token part %q is not unpadded base64url: %v", s, err)
		}
		return b
	}
	return jws{header: dec(parts[0]), claims: dec(parts[1]), signingInput: parts[0] + "." + parts[1], signature: dec(parts[2])}
}

func (j jws) verifies(pub *rsa.PublicKey) bool {
	sum := sha256.Sum256([]byte(j.signingInput))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], j.signature) == nil
}

func claimsOf(t *testing.T, j jws) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(j.claims))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("claims are not a JSON object: %v", err)
	}
	return m
}

// TestLiteralTokenInvalid checks directory-invalid-token's token: it verifies
// under the committed signing key, with exactly the header and claims
// CONTRACT.md states, and it expired before the world's clock.
func TestLiteralTokenInvalid(t *testing.T) {
	j := splitJWS(t, literalToken(t, "fixtures/exchanges/directory-invalid-token.json"))
	if !j.verifies(signingKey(t)) {
		t.Fatal("directory-invalid-token's token does not verify under token-signing-key.pem")
	}
	if got, want := string(j.header), `{"typ":"at+jwt","alg":"RS256","kid":"test-signing-1"}`; got != want {
		t.Errorf("header\n got  %s\n want %s", got, want)
	}
	const client = "srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
	want := map[string]any{
		"aud":       []any{"https://accounts.example.test/v1"},
		"client_id": client,
		"exp":       json.Number("1767226200"),
		"gty":       "client_credentials",
		"iat":       json.Number("1767225600"),
		"iss":       "https://accounts.example.test",
		"jti":       "test-" + client + "-1767225600",
		"nbf":       json.Number("1767225600"),
		"scp":       []any{"directory:read"},
		"sub":       client,
	}
	got := claimsOf(t, j)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claims\n got  %v\n want %v", got, want)
	}
	exp, err := got["exp"].(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}
	var world struct {
		Clock time.Time `json:"clock"`
	}
	decodeFixture(t, "fixtures/world.json", &world)
	if !time.Unix(exp, 0).Before(world.Clock) {
		t.Errorf("the token's exp %s is not before the world's clock %s", time.Unix(exp, 0).UTC(), world.Clock)
	}
}

// TestLiteralTokenForeign checks directory-foreign-token's token: another
// issuer's, signed with a key that is not the committed one, so it does not
// verify under it.
func TestLiteralTokenForeign(t *testing.T) {
	j := splitJWS(t, literalToken(t, "fixtures/exchanges/directory-foreign-token.json"))
	if j.verifies(signingKey(t)) {
		t.Fatal("directory-foreign-token's token verifies under token-signing-key.pem; it must not")
	}
	var h map[string]any
	if err := json.Unmarshal(j.header, &h); err != nil {
		t.Fatal(err)
	}
	if h["alg"] != "RS256" || h["kid"] != "foreign-test-1" {
		t.Errorf("header %s: want alg RS256 and kid foreign-test-1", j.header)
	}
	c := claimsOf(t, j)
	for k, v := range map[string]any{
		"iss": "https://signin.example.test", "client_id": "accounts", "jti": "foreign-access-token",
	} {
		if c[k] != v {
			t.Errorf("claim %s = %v, want %v", k, c[k], v)
		}
	}
	if !reflect.DeepEqual(c["aud"], []any{"accounts"}) {
		t.Errorf("claim aud = %v, want [accounts]", c["aud"])
	}
}

// TestTestKeysMatch checks that each committed key pair agrees with itself:
// every *.jwk.json is exactly the public half of its PEM, and the jwks
// exchange serves the token-signing key's public half under its key id.
func TestTestKeysMatch(t *testing.T) {
	b64 := base64.RawURLEncoding.EncodeToString
	for _, name := range []string{"server-ed25519", "server-p256", "enrol-ed25519"} {
		var want string
		switch k := parsePKCS8(t, "fixtures/test-keys/"+name+".pem").(type) {
		case ed25519.PrivateKey:
			want = `{"kty":"OKP","crv":"Ed25519","x":"` + b64(k.Public().(ed25519.PublicKey)) + `"}`
		case *ecdsa.PrivateKey:
			pt, err := k.PublicKey.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			want = `{"kty":"EC","crv":"P-256","x":"` + b64(pt[1:33]) + `","y":"` + b64(pt[33:65]) + `"}`
		default:
			t.Fatalf("%s.pem holds a %T", name, k)
		}
		if got := strings.TrimSuffix(string(readFixture(t, "fixtures/test-keys/"+name+".jwk.json")), "\n"); got != want {
			t.Errorf("%s.jwk.json\n got  %s\n want %s", name, got, want)
		}
	}

	pub := signingKey(t)
	var ex struct {
		Response struct {
			Body struct {
				Keys []map[string]string `json:"keys"`
			} `json:"body"`
		} `json:"response"`
	}
	decodeFixture(t, "fixtures/exchanges/jwks.json", &ex)
	want := []map[string]string{{
		"alg": "RS256", "e": b64(big.NewInt(int64(pub.E)).Bytes()), "kid": "test-signing-1",
		"kty": "RSA", "n": b64(pub.N.Bytes()), "use": "sig",
	}}
	if !reflect.DeepEqual(ex.Response.Body.Keys, want) {
		t.Errorf("jwks serves %v, want %v", ex.Response.Body.Keys, want)
	}
}
