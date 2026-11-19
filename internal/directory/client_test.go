package directory

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// parseAssertion splits a compact JWS and returns its header, its claims
// (each as JSON members in order) and whether its signature verifies under
// pub.
func parseAssertion(t *testing.T, a string, pub ed25519.PublicKey) (header, claims jsonObject, headerText string, verified bool) {
	t.Helper()
	parts := strings.Split(a, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion %q is not three parts", a)
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.Strict().DecodeString(s)
		if err != nil {
			t.Fatalf("assertion part %q: %v", s, err)
		}
		return b
	}
	h, c, sig := dec(parts[0]), dec(parts[1]), dec(parts[2])
	header, err1 := parseDocument(h)
	claims, err2 := parseDocument(c)
	if err1 != nil || err2 != nil {
		t.Fatalf("assertion parts %s %s: %v, %v", h, c, err1, err2)
	}
	return header, claims, string(h), ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig)
}

// fixtureJWKKey is the public key of test-keys/<stem>.jwk.json.
func fixtureJWKKey(t *testing.T, stem string) ed25519.PublicKey {
	t.Helper()
	var j jwk
	decodeFixture(t, "fixtures/test-keys/"+stem+".jwk.json", &j)
	x, err := base64.RawURLEncoding.Strict().DecodeString(j.X)
	if err != nil || j.Kty != "OKP" || j.Crv != "Ed25519" || len(x) != ed25519.PublicKeySize {
		t.Fatalf("%s.jwk.json is not an Ed25519 JWK", stem)
	}
	return ed25519.PublicKey(x)
}

// TestAssertionRule10 signs assertions with srv-a's fixture key and checks
// each against section 2 and rule 10 as a service would: the header is
// exactly {"alg":"EdDSA","typ":"client-authentication+jwt"}; the claims are
// iss = sub = the client id, aud the issuer as a string (not an array, not
// the token endpoint, no trailing slash), iat the clock, exp 60 seconds
// later, and a jti of 16 random bytes in hex, never repeated; no claim is
// named twice; and the signature verifies under the fixture's public JWK.
func TestAssertionRule10(t *testing.T) {
	clock := worldClock
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, worldIssuer), Organization: worldOrg, Now: fixedClock(&clock)})
	if err != nil {
		t.Fatal(err)
	}
	pub := fixtureJWKKey(t, "server-ed25519")
	jtis := map[string]bool{}
	for range 3 {
		a, err := c.assertion()
		if err != nil {
			t.Fatal(err)
		}
		header, claims, headerText, ok := parseAssertion(t, a, pub)
		if !ok {
			t.Errorf("the signature does not verify under server-ed25519.jwk.json")
		}
		if headerText != `{"alg":"EdDSA","typ":"client-authentication+jwt"}` || len(header) != 2 {
			t.Errorf("header %s", headerText)
		}
		if claims.duplicate() || len(claims) != 6 {
			t.Errorf("claims %v: want iss, sub, aud, iat, exp, jti, each once", claims)
		}
		str := func(name string) string {
			v, ok := claims.one(name)
			s, isString := rawString(v)
			if !ok || !isString {
				t.Errorf("claim %s is %s, not a string", name, v)
			}
			return s
		}
		if str("iss") != worldClientID || str("sub") != worldClientID {
			t.Errorf("iss and sub are not the client id")
		}
		if aud := str("aud"); aud != worldIssuer {
			t.Errorf("aud %q, want the issuer %q byte for byte", aud, worldIssuer)
		}
		iat, _ := claims.one("iat")
		exp, _ := claims.one("exp")
		if want := fmt.Sprint(worldClock.Unix()); string(iat) != want {
			t.Errorf("iat %s, want %s", iat, want)
		}
		if want := fmt.Sprint(worldClock.Unix() + 60); string(exp) != want {
			t.Errorf("exp %s, want %s (exp - iat = 60)", exp, want)
		}
		jti := str("jti")
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(jti) || jtis[jti] {
			t.Errorf("jti %q is not 16 fresh bytes in hex", jti)
		}
		jtis[jti] = true
	}

	// The control: an assertion signed by another key does not verify.
	other := fixtureJWKKey(t, "enrol-ed25519")
	a, _ := c.assertion()
	if _, _, _, ok := parseAssertion(t, a, other); ok {
		t.Fatal("an assertion verifies under a key that did not sign it")
	}
}

// TestAssertionRandomnessFailure checks a failing randomness source fails
// the login rather than sending a jti that is not random.
func TestAssertionRandomnessFailure(t *testing.T) {
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, worldIssuer), Organization: worldOrg, Rand: bytes.NewReader(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.assertion(); err == nil {
		t.Fatal("an assertion with no randomness")
	}
}

// TestEveryLoginIsANewAssertion pulls repeatedly through 401s and token
// failures and checks no jti reaches the service twice.
func TestEveryLoginIsANewAssertion(t *testing.T) {
	fs := newFakeService(t)
	invalid := served{status: 401, header: http.Header{"Content-Type": {"application/json"}}, body: []byte(`{"error":"invalid_client","error_description":"x"}`)}
	serverErr := served{status: 500, header: http.Header{}, body: []byte(`{"error":"server_error"}`)}
	fs.tokens = []served{invalid, serverErr, goodToken, goodToken}
	notValid := served{status: 401, header: http.Header{}, body: []byte(`{"error":{"code":"invalid_token","message":"m"}}`)}
	fs.directory = []served{notValid, {status: 200, body: worldListBody(t)}}
	c := fs.client(t, worldOrg)
	for i := range 4 {
		_, err := c.Pull(context.Background())
		t.Logf("pull %d: %v", i, err)
	}
	if len(fs.assertions) != 4 {
		t.Fatalf("%d logins, want 4", len(fs.assertions))
	}
	seen := map[string]bool{}
	for _, a := range fs.assertions {
		_, claims, _, _ := parseAssertion(t, a, fixtureJWKKey(t, "server-ed25519"))
		v, _ := claims.one("jti")
		if seen[string(v)] {
			t.Errorf("the jti %s was sent twice", v)
		}
		seen[string(v)] = true
	}
}

// TestDroppedLoginSignsANewAssertion drops the connection of the first
// login after reading it: the pull is a transport failure, and the next
// pull's login carries a new assertion (two jtis arrive, different). It
// does not by itself prove the transport cannot resend a token request
// (HTTP/1 would not resend this one either way): that is
// TestTokenRequestBodyCannotBeRewound's and TestRefusedStreamIsNotRetried's.
func TestDroppedLoginSignsANewAssertion(t *testing.T) {
	var mu sync.Mutex
	var jtis []string
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			_, claims, _, _ := parseAssertion(t, r.PostForm.Get("client_assertion"), fixtureJWKKey(t, "server-ed25519"))
			v, _ := claims.one("jti")
			mu.Lock()
			jtis = append(jtis, string(v))
			logins++
			first := logins == 1
			mu.Unlock()
			if first {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			goodToken.write(w)
		case "/v1/directory":
			served{status: 200, body: worldListBody(t)}.write(w)
		}
	}))
	defer srv.Close()
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, srv.URL), Organization: worldOrg, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(context.Background()); err == nil {
		t.Fatal("the first pull succeeded over a dropped connection")
	} else if f := asFailure(t, err); f.Kind != KindTransport || f.Step != StepToken {
		t.Fatalf("first pull: %v", f)
	}
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("second pull: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(jtis) != 2 || jtis[0] == jtis[1] {
		t.Fatalf("jtis %v: want two, different", jtis)
	}
}

// bodyRecorder is a transport (the package's test hook) that records, for
// each request with a body, whether a transport could rewind that body
// (GetBody set), then hands the request on.
type bodyRecorder struct {
	next http.RoundTripper

	mu         sync.Mutex
	rewindable map[string]bool // "METHOD /path": GetBody != nil
}

func (b *bodyRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.Body != http.NoBody {
		b.mu.Lock()
		b.rewindable[req.Method+" "+req.URL.Path] = req.GetBody != nil
		b.mu.Unlock()
	}
	return b.next.RoundTrip(req)
}

// TestTokenRequestBodyCannotBeRewound checks, on the request a transport
// receives, that the token request's body (which carries the assertion)
// and the enrolment request's body have no GetBody: no transport can
// rewind them to send them a second time.
func TestTokenRequestBodyCannotBeRewound(t *testing.T) {
	fs := newFakeService(t)
	fs.directory = []served{{status: 200, body: worldListBody(t)}}
	fs.enroll = []served{{status: 201, body: []byte(good201)}}
	rec := &bodyRecorder{next: worldTransport(t, fs.srv), rewindable: map[string]bool{}}
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, worldIssuer), Organization: worldOrg, baseTransport: rec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), EnrolRequest{Issuer: worldIssuer, Code: "c", Name: "n", baseTransport: rec}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, req := range []string{"POST /oauth2/token", "POST /v1/servers/enroll"} {
		rewindable, seen := rec.rewindable[req]
		if !seen || rewindable {
			t.Errorf("%s: seen %v, GetBody set %v; want seen, and no GetBody", req, seen, rewindable)
		}
	}
}

// TestNoRedirectFollowed answers each request with a redirect to a server
// that would answer well: it is never asked, and each is a failure.
func TestNoRedirectFollowed(t *testing.T) {
	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		goodToken.write(w)
	}))
	defer elsewhere.Close()
	redirect := served{status: http.StatusFound, header: http.Header{"Location": {elsewhere.URL + "/v1/directory"}}}
	for _, code := range []int{301, 302, 303, 307, 308} {
		redirect.status = code

		fs := newFakeService(t)
		fs.directory = []served{redirect}
		_, err := fs.client(t, worldOrg).Pull(context.Background())
		if err == nil || asFailure(t, err).Kind != KindRedirect {
			t.Errorf("directory %d: %v, want a redirect failure", code, err)
		}

		fs = newFakeService(t)
		fs.tokens = []served{redirect}
		_, err = fs.client(t, worldOrg).Pull(context.Background())
		if err == nil || asFailure(t, err).Kind != KindRedirect {
			t.Errorf("token %d: %v, want a redirect failure", code, err)
		}

		fs = newFakeService(t)
		fs.enroll = []served{redirect}
		_, err = Enroll(context.Background(), EnrolRequest{Issuer: fs.srv.URL, Code: "c", Name: "n", AllowPlainHTTP: true})
		if err == nil || asFailure(t, err).Kind != KindRedirect {
			t.Errorf("enrolment %d: %v, want a redirect failure", code, err)
		} else if !asFailure(t, err).Lost() {
			t.Errorf("enrolment %d: a redirect is not a lost answer", code)
		}
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("a redirect was followed %d times", n)
	}
}

// TestPlainHTTPOnlyToLoopback checks the plain-HTTP dialer refuses every
// address that is not loopback, on the address dialled, and that plain
// HTTP needs the flag.
func TestPlainHTTPOnlyToLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:80": true, "127.8.9.10:443": true, "[::1]:80": true, "[::ffff:127.0.0.1]:80": true,
		"10.0.0.1:80": false, "192.0.2.1:80": false, "[2001:db8::1]:80": false, "0.0.0.0:80": false, "[::]:80": false,
	} {
		if err := loopbackOnly("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("%s: %v, want allowed=%v", addr, err, ok)
		}
	}

	if _, err := newHTTPClient(transportConfig{issuer: "http://127.0.0.1:1"}); err == nil {
		t.Error("plain HTTP without allow_plain_http")
	}
	if _, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, "http://127.0.0.1:1"), Organization: worldOrg}); err == nil {
		t.Error("a client for a plain HTTP issuer without AllowPlainHTTP")
	}
	if _, err := newHTTPClient(transportConfig{issuer: "ftp://127.0.0.1", allowPlainHTTP: true}); err == nil {
		t.Error("an issuer that is neither https nor http")
	}

	// Real requests: a documentation address (never routed) is refused at
	// the dial, before any connection is tried; the local service, reached
	// by its address and by the name localhost, answers.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { goodToken.write(w) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	for _, host := range []string{"192.0.2.1", "[2001:db8::1]", "127.0.0.1", "localhost"} {
		issuer := fmt.Sprintf("http://%s:%s", host, port)
		c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, issuer), Organization: worldOrg, AllowPlainHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = c.accessToken(ctx, &skew{})
		cancel()
		local := host == "127.0.0.1" || host == "localhost"
		if local && err != nil {
			t.Errorf("%s refused: %v", host, err)
		}
		if !local && (err == nil || !strings.Contains(err.Error(), "only to a loopback address")) {
			t.Errorf("%s: %v, want the dial refused", host, err)
		}
	}
}

// TestRequestsGoOnlyToTheIssuer checks the HTTP client refuses a request to
// any other scheme or host than its issuer's, even one that reaches the
// same service.
func TestRequestsGoOnlyToTheIssuer(t *testing.T) {
	fs := newFakeService(t)
	hc, err := newHTTPClient(transportConfig{issuer: fs.srv.URL, allowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		strings.Replace(fs.srv.URL, "127.0.0.1", "localhost", 1) + "/oauth2/token",
		strings.Replace(fs.srv.URL, "http://", "https://", 1) + "/oauth2/token",
	} {
		req, err := newRequest(context.Background(), http.MethodPost, target, strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := hc.Do(req); err == nil || !strings.Contains(err.Error(), "not the issuer") {
			if resp != nil {
				_ = resp.Body.Close()
			}
			t.Errorf("%s: %v, want the request refused", target, err)
		}
	}
	if n := len(fs.paths); n != 0 {
		t.Fatalf("%d requests reached the service", n)
	}
	// The control: the issuer itself is asked.
	req, _ := newRequest(context.Background(), http.MethodPost, fs.srv.URL+"/oauth2/token", strings.NewReader("x"))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

// TestSelfSignedCertificateIsAFailedRefresh checks an https service whose
// certificate the system does not trust is a transport failure, and nothing
// is sent past the handshake.
func TestSelfSignedCertificateIsAFailedRefresh(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		goodToken.write(w)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake
	srv.StartTLS()
	defer srv.Close()
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, srv.URL), Organization: worldOrg})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Pull(context.Background())
	if err == nil {
		t.Fatal("a pull over an untrusted certificate succeeded")
	}
	if f := asFailure(t, err); f.Kind != KindTransport || f.Status != 0 {
		t.Fatalf("%v, want a transport failure", f)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the service was asked %d times", n)
	}
	// The control: the same service, its certificate trusted through the
	// one transport option a caller has (RootCAs), answers.
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c, err = NewClient(ClientConfig{Enrolment: worldEnrolment(t, srv.URL), Organization: worldOrg, RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.accessToken(context.Background(), &skew{}); err != nil {
		t.Fatalf("with the certificate trusted: %v", err)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the service was asked %d times with its certificate trusted, want once", n)
	}
}

// TestOversizeAnswers checks each answer is refused one byte over its limit
// (counted on the bytes read) and accepted at it.
func TestOversizeAnswers(t *testing.T) {
	// A directory answer of exactly n bytes: the world's list with an
	// ignored member padded to size.
	list := func(n int) []byte {
		base := worldListBody(t)
		head := []byte(`{"pad":"`)
		tail := append([]byte(`",`), base[1:]...)
		pad := n - len(head) - len(tail)
		return append(append(head, bytes.Repeat([]byte("x"), pad)...), tail...)
	}
	for _, n := range []int{MaxDirectoryBody, MaxDirectoryBody + 1} {
		for _, chunked := range []bool{false, true} {
			fs := newFakeService(t)
			body := list(n)
			s := served{status: 200, header: http.Header{"Content-Type": {"application/json"}}, body: body}
			if chunked {
				// No Content-Length: the limit is counted on the bytes read.
				s.header.Set("Transfer-Encoding", "chunked")
			} else {
				s.header.Set("Content-Length", strconv.Itoa(len(body)))
			}
			fs.directory = []served{s}
			_, err := fs.client(t, worldOrg).Pull(context.Background())
			switch {
			case n == MaxDirectoryBody && err != nil:
				t.Errorf("%d bytes (chunked %v): %v", n, chunked, err)
			case n > MaxDirectoryBody && (err == nil || asFailure(t, err).Kind != KindTooLarge):
				t.Errorf("%d bytes (chunked %v): %v, want too large", n, chunked, err)
			}
		}
	}

	// A token answer one byte over 64 KiB.
	fs := newFakeService(t)
	pad := MaxAnswerBody + 1 - len(goodToken.body) - len(`,"pad":""`)
	big := append(bytes.TrimSuffix(goodToken.body, []byte("}")), []byte(`,"pad":"`+strings.Repeat("x", pad)+`"}`)...)
	fs.tokens = []served{{status: 200, header: goodToken.header, body: big}}
	if len(big) != MaxAnswerBody+1 {
		t.Fatalf("the token answer is %d bytes", len(big))
	}
	if _, err := fs.client(t, worldOrg).Pull(context.Background()); err == nil || asFailure(t, err).Kind != KindTooLarge {
		t.Errorf("a token answer over 64 KiB: %v", err)
	}
	// The control at 64 KiB exactly.
	fs = newFakeService(t)
	exact := append(append([]byte{}, big[:len(big)-3]...), `"}`...)
	fs.tokens = []served{{status: 200, header: goodToken.header, body: exact}}
	fs.directory = []served{{status: 200, body: worldListBody(t)}}
	if _, err := fs.client(t, worldOrg).Pull(context.Background()); err != nil {
		t.Errorf("a token answer of 64 KiB: %v", err)
	}
}

// TestTokenAnswerChecks holds a token endpoint's 200 to section 2: anything
// but a non-empty access_token, a bearer token_type and a positive integer
// expires_in (with a scope, when present, holding directory:read) is a
// failed refresh, and the directory is not asked.
func TestTokenAnswerChecks(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"scope":"directory:read"}`:     true,
		`{"access_token":"opaque-test-token","token_type":"Bearer","expires_in":600}`:                              true,
		`{"access_token":"opaque-test-token","token_type":"BEARER","expires_in":1,"scope":"other directory:read"}`: true,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"scope":"directory:write"}`:    false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"scope":null}`:                 false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"scope":""}`:                   false,
		`{"access_token":"","token_type":"bearer","expires_in":600}`:                                               false,
		`{"token_type":"bearer","expires_in":600}`:                                                                 false,
		`{"access_token":7,"token_type":"bearer","expires_in":600}`:                                                false,
		`{"access_token":"opaque-test-token","token_type":"mac","expires_in":600}`:                                 false,
		`{"access_token":"opaque-test-token","expires_in":600}`:                                                    false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":0}`:                                false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":-5}`:                               false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600.5}`:                            false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":"600"}`:                            false,
		`{"access_token":"opaque-test-token","token_type":"bearer"}`:                                               false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"access_token":"u"}`:           false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600} {}`:                           false,
		`[]`: false,
		// The checks every JSON answer meets (CONTRACT.md, Conventions),
		// in a member the client ignores.
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"x":"\ud800"}`:                 false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"x":"\ud83d\ude00"}`:           true,
		"{\"access_token\":\"t\",\"token_type\":\"bearer\",\"expires_in\":600,\"x\":\"\xff\"}":                     false,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"x":` + nestedArrays(15) + `}`: true,
		`{"access_token":"opaque-test-token","token_type":"bearer","expires_in":600,"x":` + nestedArrays(16) + `}`: false,
	} {
		fs := newFakeService(t)
		fs.tokens = []served{{status: 200, header: goodToken.header, body: []byte(body)}}
		fs.directory = []served{{status: 200, body: worldListBody(t)}}
		_, err := fs.client(t, worldOrg).Pull(context.Background())
		if (err == nil) != ok {
			t.Errorf("%s: %v, want accepted=%v", body, err, ok)
		}
		if !ok && err != nil && asFailure(t, err).Kind != KindBadAnswer {
			t.Errorf("%s: %v, want a refused answer", body, err)
		}
		if !ok && fs.count("GET /v1/directory") != 0 {
			t.Errorf("%s: the directory was asked", body)
		}
	}
}

// TestTokenReuse checks an access token is reused until 60 seconds before
// it ends, and a new login is made from then on.
func TestTokenReuse(t *testing.T) {
	fs := newFakeService(t)
	fs.directory = []served{{status: 200, body: worldListBody(t)}}
	clock := worldClock
	c := fs.clientAt(t, worldOrg, &clock)
	pullAt := func(at time.Duration) {
		clock = worldClock.Add(at)
		if _, err := c.Pull(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	pullAt(0)
	pullAt(539 * time.Second)
	if n := fs.count("POST /oauth2/token"); n != 1 {
		t.Fatalf("%d logins within the token's reuse, want 1", n)
	}
	pullAt(540 * time.Second)
	if n := fs.count("POST /oauth2/token"); n != 2 {
		t.Fatalf("%d logins at 60 s before the token's end, want 2", n)
	}
	for _, b := range fs.bearers {
		if b != "Bearer opaque-token" {
			t.Errorf("Authorization %q", b)
		}
	}
}

// TestDirectory401LogsInOnce checks a 401 from the directory brings one new
// login and one more try: a good answer then is a good pull, a second 401 a
// failure.
func TestDirectory401LogsInOnce(t *testing.T) {
	notValid := served{status: 401, header: http.Header{"Www-Authenticate": {`Bearer error="invalid_token"`}}, body: []byte(`{"error":{"code":"invalid_token","message":"m"}}`)}
	fs := newFakeService(t)
	fs.directory = []served{notValid, {status: 200, body: worldListBody(t)}}
	if _, err := fs.client(t, worldOrg).Pull(context.Background()); err != nil {
		t.Fatalf("a 401 then a good answer: %v", err)
	}
	if n := fs.count("POST /oauth2/token"); n != 2 {
		t.Errorf("%d logins, want 2", n)
	}
	fs = newFakeService(t)
	fs.directory = []served{notValid}
	c := fs.client(t, worldOrg)
	_, err := c.Pull(context.Background())
	if err == nil || asFailure(t, err).Kind != KindInvalidToken {
		t.Fatalf("two 401s: %v", err)
	}
	if d, tk := fs.count("GET /v1/directory"), fs.count("POST /oauth2/token"); d != 2 || tk != 2 {
		t.Errorf("%d directory requests and %d logins, want 2 and 2", d, tk)
	}
	// The token the second 401 refused is forgotten too: the next pull
	// logs in before it asks the directory, and never sends it again.
	fs.mu.Lock()
	fs.directory = []served{{status: 200, body: worldListBody(t)}}
	fs.mu.Unlock()
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if want := []string{"POST /oauth2/token", "GET /v1/directory", "POST /oauth2/token", "GET /v1/directory", "POST /oauth2/token", "GET /v1/directory"}; strings.Join(fs.paths, ",") != strings.Join(want, ",") {
		t.Errorf("requests %v, want %v", fs.paths, want)
	}
}

// TestFailureStatuses checks how each status of each step is classified.
func TestFailureStatuses(t *testing.T) {
	ra := http.Header{"Retry-After": {"30"}}
	for _, tc := range []struct {
		step   Step
		status int
		header http.Header
		body   string
		want   Kind
		wait   bool
	}{
		{StepToken, 400, nil, `{"error":"invalid_client","error_description":"d"}`, KindInvalidClient, false},
		{StepToken, 401, nil, `{"error":"invalid_client","error_description":"d"}`, KindInvalidClient, false},
		{StepToken, 400, nil, `{"error":"invalid_request","error_description":"d"}`, KindRefused, false},
		// Only the code invalid_client is a refused login.
		{StepToken, 401, nil, `{"error":"invalid_request","error_description":"d"}`, KindRefused, false},
		{StepToken, 401, nil, `{"error":"unknown_code"}`, KindRefused, false},
		{StepToken, 401, nil, `<html>401</html>`, KindStatus, false},
		{StepToken, 401, nil, ``, KindStatus, false},
		{StepToken, 401, nil, `{"ERROR":"invalid_client"}`, KindStatus, false},
		{StepToken, 401, nil, `{"error":"invalid_client","error":"invalid_client"}`, KindStatus, false},
		{StepToken, 403, nil, `{"error":"invalid_client"}`, KindRefused, false},
		{StepToken, 400, nil, `{"error":{"code":"invalid_client"}}`, KindRefused, false},
		{StepToken, 429, ra, `{"error":"temporarily_unavailable"}`, KindRateLimited, true},
		{StepToken, 500, ra, `{"error":"server_error"}`, KindServerError, false},
		{StepToken, 503, ra, `{"error":"server_error"}`, KindUnavailable, true},
		{StepToken, 404, nil, `not json`, KindStatus, false},
		{StepDirectory, 401, nil, ``, KindInvalidToken, false},
		{StepDirectory, 403, ra, `{"error":{"code":"SERVER_DISABLED","message":"m"}}`, KindServerDisabled, false},
		{StepDirectory, 403, nil, `{"error":{"code":"INSUFFICIENT_SCOPE","message":"m"}}`, KindRefused, false},
		// Exact member names, and no member named twice.
		{StepDirectory, 403, nil, `{"ERROR":{"code":"SERVER_DISABLED","message":"m"}}`, KindStatus, false},
		{StepDirectory, 403, nil, `{"error":{"CODE":"SERVER_DISABLED","message":"m"}}`, KindStatus, false},
		{StepDirectory, 403, nil, `{"error":{"code":"INSUFFICIENT_SCOPE","code":"SERVER_DISABLED"}}`, KindStatus, false},
		{StepDirectory, 403, nil, `{"error":{"code":"SERVER_DISABLED"},"error":{"code":"INSUFFICIENT_SCOPE"}}`, KindStatus, false},
		// A member of another case beside code is ignored like any
		// unknown member.
		{StepDirectory, 403, nil, `{"error":{"code":"SERVER_DISABLED","CODE":"ignored"}}`, KindServerDisabled, false},
		{StepDirectory, 502, nil, ``, KindServerError, false},
		{StepEnroll, 409, nil, `{"error":{"code":"ENROLMENT_CODE_SPENT","message":"m"}}`, KindRefused, false},
	} {
		h := tc.header
		if h == nil {
			h = http.Header{}
		}
		f := statusFailure(tc.step, &answer{status: tc.status, header: h, body: []byte(tc.body)})
		if f.Kind != tc.want || f.HasRetryAfter != tc.wait {
			t.Errorf("%s %d %s: %s (retry after %v), want %s (%v)", tc.step, tc.status, tc.body, f.Kind, f.HasRetryAfter, tc.want, tc.wait)
		}
	}
}

// TestErrorBody reads an error answer's code and message by the exact
// member names, in either shape, and recognises nothing in an answer that
// names a member twice or is not JSON text.
func TestErrorBody(t *testing.T) {
	for _, tc := range []struct{ body, code, message string }{
		{`{"error":{"code":"SERVER_DISABLED","message":"m"}}`, "SERVER_DISABLED", "m"},
		{`{"error":"invalid_client","error_description":"d"}`, "invalid_client", "d"},
		{`{"error":"invalid_client"}`, "invalid_client", ""},
		{`{"error":{"code":"X","message":"m","more":1e400},"other":1e400}`, "X", "m"},
		{`{"Error":{"code":"SERVER_DISABLED"}}`, "", ""},
		{`{"error":{"Code":"SERVER_DISABLED"}}`, "", ""},
		{`{"error_description":"d","error":"invalid_client","error_description":"e"}`, "", ""},
		{`{"error":{"code":"A","message":"m","message":"n"}}`, "", ""},
		{`{"error":{"code":""}}`, "", ""},
		{`{"error":7}`, "", ""},
		{`{"error":"\ud800"}`, "", ""},
		{`{"error":"x"} trailing`, "", ""},
		// Members of another spelling or case beside the located ones are
		// ignored; a located member's name is compared after unescaping.
		{`{"error":{"code":"SERVER_DISABLED","CODE":"ignored"}}`, "SERVER_DISABLED", ""},
		{`{"Error":"x","error":{"code":"X","message":"m"}}`, "X", "m"},
		{`{"error":{"c\u006fde":"X","message":"m"}}`, "X", "m"},
		// A member named twice in either object read, whichever it is, and
		// names compared after unescaping.
		{`{"error":{"code":"X"},"\u0065rror":{"code":"Y"}}`, "", ""},
		{`{"error":{"code":"X"},"x":1,"x":2}`, "", ""},
		// The checks every JSON answer meets.
		{"{\"error\":{\"code\":\"X\",\"message\":\"\xff\"}}", "", ""},
		{`{"error":{"code":"X","message":"m","n":` + nestedArrays(14) + `}}`, "X", "m"},
		{`{"error":{"code":"X","message":"m","n":` + nestedArrays(15) + `}}`, "", ""},
		{`{"error":{"code":"X","message":"\ud83d\ude00"}}`, "X", "\U0001F600"},
		{`not json`, "", ""},
		{``, "", ""},
	} {
		code, message := errorBody([]byte(tc.body))
		if code != tc.code || message != tc.message {
			t.Errorf("%s: %q %q, want %q %q", tc.body, code, message, tc.code, tc.message)
		}
	}
}

// TestRetryAfterRule2 is rule 2's grammar, beyond the client fixtures.
func TestRetryAfterRule2(t *testing.T) {
	for _, tc := range []struct {
		status int
		values []string
		want   time.Duration
		ok     bool
	}{
		{429, []string{"0"}, 0, true},
		{503, []string{"120"}, 120 * time.Second, true},
		{429, []string{" \t45 \t"}, 45 * time.Second, true},
		{429, []string{"0000000030"}, 30 * time.Second, true},
		{429, []string{"3600"}, time.Hour, true},
		{429, []string{"3601"}, time.Hour, true},
		{429, []string{"00000000000000000000000000003601"}, time.Hour, true},
		{429, []string{"99999999999999999999999"}, time.Hour, true},
		{429, nil, 0, false},
		{429, []string{""}, 0, false},
		{429, []string{"  "}, 0, false},
		{429, []string{"+30"}, 0, false},
		{429, []string{"-5"}, 0, false},
		{429, []string{"30.0"}, 0, false},
		{429, []string{"3 0"}, 0, false},
		{429, []string{"30s"}, 0, false},
		{429, []string{"٣٠"}, 0, false},
		{429, []string{"30\n"}, 0, false},
		{429, []string{"Wed, 18 Nov 2026 07:28:00 GMT"}, 0, false},
		{429, []string{"30", "30"}, 0, false},
		{429, []string{"30, 30"}, 0, false},
		{403, []string{"30"}, 0, false},
		{500, []string{"30"}, 0, false},
		{200, []string{"30"}, 0, false},
	} {
		h := http.Header{}
		for _, v := range tc.values {
			h.Add("Retry-After", v)
		}
		got, ok := RetryAfter(tc.status, h)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%d %q: %s %v, want %s %v", tc.status, tc.values, got, ok, tc.want, tc.ok)
		}
	}
}

// TestDateAndSkew checks a pull reports the largest difference between an
// answer's Date and the client's clock, and a 401 invalid_client carries
// both clocks.
func TestDateAndSkew(t *testing.T) {
	fs := newFakeService(t)
	tok := goodToken
	tok.header = tok.header.Clone()
	tok.header.Set("Date", worldClock.Add(-45*time.Second).Format(http.TimeFormat))
	fs.tokens = []served{tok}
	fs.directory = []served{{status: 200, header: http.Header{"Date": {worldClock.Add(10 * time.Second).Format(http.TimeFormat)}}, body: worldListBody(t)}}
	p, err := fs.client(t, worldOrg).Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasSkew || p.Skew != -45*time.Second {
		t.Errorf("skew %s (%v), want -45s", p.Skew, p.HasSkew)
	}
	if !p.FetchedAt.Equal(worldClock) || p.FetchedAt != p.FetchedAt.Round(0) {
		t.Errorf("fetched at %v", p.FetchedAt)
	}

	fs = newFakeService(t)
	fs.tokens = []served{{status: 401, header: http.Header{"Date": {worldClock.Add(time.Hour).Format(http.TimeFormat)}}, body: []byte(`{"error":"invalid_client","error_description":"bad \"clock\""}`)}}
	_, err = fs.client(t, worldOrg).Pull(context.Background())
	f := asFailure(t, err)
	if !f.Date.Equal(worldClock.Add(time.Hour)) || !f.Local.Equal(worldClock) {
		t.Errorf("date %v local %v", f.Date, f.Local)
	}
	msg := f.Error()
	for _, want := range []string{`"invalid_client"`, `"bad \"clock\""`, "2026-11-16T07:00:00Z", "2026-11-16T06:00:00Z"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not hold %q", msg, want)
		}
	}
}

// TestNewClientRefuses checks a client is not made from an enrolment it
// cannot use.
func TestNewClientRefuses(t *testing.T) {
	good := worldEnrolment(t, worldIssuer)
	for name, mutate := range map[string]func(e *Enrolment, cfg *ClientConfig){
		"no enrolment":           func(e *Enrolment, cfg *ClientConfig) { cfg.Enrolment = nil },
		"no key":                 func(e *Enrolment, cfg *ClientConfig) { e.PrivateKey = nil },
		"organization not one":   func(e *Enrolment, cfg *ClientConfig) { cfg.Organization = "Example" },
		"no organization at all": func(e *Enrolment, cfg *ClientConfig) { cfg.Organization, e.Organization = "", "" },
		"another token endpoint": func(e *Enrolment, cfg *ClientConfig) { e.TokenEndpoint = "https://elsewhere.example.test/oauth2/token" },
		"an issuer with a path": func(e *Enrolment, cfg *ClientConfig) {
			e.Issuer, e.TokenEndpoint = "https://accounts.example.test/prefix", "https://accounts.example.test/prefix/oauth2/token"
		},
		"plain HTTP without the option": func(e *Enrolment, cfg *ClientConfig) {
			e.Issuer, e.TokenEndpoint = "http://127.0.0.1:1", "http://127.0.0.1:1/oauth2/token"
		},
	} {
		e := *good
		cfg := ClientConfig{Enrolment: &e, Organization: worldOrg}
		mutate(&e, &cfg)
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("%s: a client was made", name)
		}
	}
	if _, err := NewClient(ClientConfig{Enrolment: good, Organization: worldOrg}); err != nil {
		t.Fatalf("the control: %v", err)
	}
}

// TestNoConfiguredOrganizationIsTheEnrolments checks a client with no
// organization configured accepts the organization its enrolment named, and
// refuses an answer naming another (rule 5).
func TestNoConfiguredOrganizationIsTheEnrolments(t *testing.T) {
	fs := newFakeService(t)
	fs.directory = []served{{status: 200, body: worldListBody(t)}}
	e := worldEnrolment(t, fs.srv.URL)
	c, err := NewClient(ClientConfig{Enrolment: e, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.organization != worldOrg {
		t.Fatalf("organization %q, want the enrolment's %q", c.organization, worldOrg)
	}
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("a list naming the enrolment's organization: %v", err)
	}
	e.Organization = "other"
	c, err = NewClient(ClientConfig{Enrolment: e, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(context.Background()); !errors.Is(err, ErrOrganization) {
		t.Fatalf("a list naming another organization than the enrolment's: %v", err)
	}
	// A configured organization wins over the enrolment's.
	c, err = NewClient(ClientConfig{Enrolment: e, Organization: worldOrg, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("the configured organization: %v", err)
	}
}

// TestFailureUnwraps checks a refused answer's cause is reachable.
func TestFailureUnwraps(t *testing.T) {
	fs := newFakeService(t)
	good := compactJSON(t, worldListBody(t))
	body := bytes.Replace(good, []byte(`"organization":"example"`), []byte(`"organization":"other"`), 1)
	if bytes.Equal(body, good) {
		t.Fatal("the body names no organization to change")
	}
	fs.directory = []served{{status: 200, body: body}}
	_, err := fs.client(t, worldOrg).Pull(context.Background())
	if !errors.Is(err, ErrOrganization) {
		t.Fatalf("%v does not wrap ErrOrganization", err)
	}
	body = bytes.Replace(good, []byte(`"contract":1`), []byte(`"contract":2`), 1)
	fs.directory = []served{{status: 200, body: body}}
	if _, err := fs.client(t, worldOrg).Pull(context.Background()); !errors.Is(err, ErrContract) {
		t.Fatalf("%v does not wrap ErrContract", err)
	}
}
