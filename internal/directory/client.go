package directory

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
)

// The assertion's fixed parts (section 2 and rule 10).
const (
	assertionHeader   = `{"alg":"EdDSA","typ":"client-authentication+jwt"}`
	assertionLifetime = 60 // seconds: exp - iat
	assertionType     = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// directoryScope is the scope a token must carry.
	directoryScope = "directory:read"
	// tokenEarly is how long before its end an access token stops being
	// reused.
	tokenEarly = 60 * time.Second
	// maxTokenReuse bounds how long one access token is reused, whatever
	// its expires_in says (the contract's tokens last ten minutes).
	maxTokenReuse = time.Hour
)

// ClientConfig is what a Client needs.
type ClientConfig struct {
	// Enrolment is the server's enrolment: its client id, key, issuer and
	// token endpoint.
	Enrolment *Enrolment
	// Organization is the configured organization every answer must name;
	// "" (none configured) means the enrolment's (rule 5).
	Organization string
	// AllowPlainHTTP allows an http issuer (auth.directory.allow_plain_http):
	// requests then go only to a loopback address, checked on the address
	// dialled, through no proxy.
	AllowPlainHTTP bool
	// RootCAs, when set, are the root certificates an https issuer is
	// checked against in place of the system's: for tests against a TLS
	// test server. Nothing else of the transport can be configured.
	RootCAs *x509.CertPool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Rand is the source of each assertion's jti; nil means crypto/rand.
	Rand io.Reader

	// baseTransport replaces the HTTP transport under the issuer and
	// redirect rules: tests in this package only.
	baseTransport http.RoundTripper
}

// Client logs in to the service and pulls the directory. It keeps its access
// token in memory only. It is safe for concurrent use, but does not join
// concurrent pulls: each Pull is its own requests.
type Client struct {
	clientID      string
	issuer        string
	tokenEndpoint string
	key           ed25519.PrivateKey
	organization  string
	hc            *http.Client
	now           func() time.Time
	rand          io.Reader

	mu         sync.Mutex
	token      string
	tokenUntil time.Time
}

// NewClient returns a Client for an enrolled server.
func NewClient(cfg ClientConfig) (*Client, error) {
	e := cfg.Enrolment
	if e == nil {
		return nil, errors.New("no enrolment")
	}
	if len(e.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("the enrolment has no Ed25519 key")
	}
	org := e.AcceptedOrganization(cfg.Organization)
	if !ValidOrganization(org) {
		return nil, fmt.Errorf("organization %q is not one", org)
	}
	if err := config.CheckDirectoryIssuer(e.Issuer); err != nil {
		return nil, fmt.Errorf("the enrolment's issuer: %w", err)
	}
	if e.TokenEndpoint != e.Issuer+"/oauth2/token" {
		return nil, fmt.Errorf("token endpoint %q is not the issuer's", e.TokenEndpoint)
	}
	hc, err := newHTTPClient(transportConfig{
		issuer: e.Issuer, allowPlainHTTP: cfg.AllowPlainHTTP, rootCAs: cfg.RootCAs, base: cfg.baseTransport,
	})
	if err != nil {
		return nil, err
	}
	c := &Client{
		clientID:      e.ClientID,
		issuer:        e.Issuer,
		tokenEndpoint: e.TokenEndpoint,
		key:           e.PrivateKey,
		organization:  org,
		hc:            hc,
		now:           cfg.Now,
		rand:          cfg.Rand,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.rand == nil {
		c.rand = rand.Reader
	}
	return c, nil
}

// Pull is one good pull.
type Pull struct {
	// List is the answer, decoded.
	List *List
	// Body is the answer's bytes, as read.
	Body []byte
	// FetchedAt is when the answer arrived, on the wall clock (no
	// monotonic reading).
	FetchedAt time.Time
	// Skew is the largest difference between an answer's Date and the
	// client's clock among this pull's answers (Date minus local, signed);
	// HasSkew is false when no answer carried a Date.
	Skew    time.Duration
	HasSkew bool
}

// skew keeps the largest clock difference seen.
type skew struct {
	d  time.Duration
	ok bool
}

func (s *skew) see(a *answer) {
	if a == nil || a.date.IsZero() {
		return
	}
	d := a.date.Sub(a.local)
	if !s.ok || d.Abs() > s.d.Abs() {
		s.d, s.ok = d, true
	}
}

// Pull logs in if it holds no usable access token, then fetches the
// directory. On a 401 from the directory it logs in again, once, with a new
// assertion, and tries once more; a second 401 is a failure. Every token a
// 401 refused is forgotten, so the next pull logs in again. Every failure
// is a *Failure. ctx bounds the whole of it.
func (c *Client) Pull(ctx context.Context) (*Pull, error) {
	var sk skew
	tok, err := c.accessToken(ctx, &sk)
	if err != nil {
		return nil, err
	}
	p, f := c.fetch(ctx, tok, &sk, nil)
	if f != nil && f.Kind == KindInvalidToken {
		c.forget(tok)
		first := tok
		if tok, err = c.accessToken(ctx, &sk); err != nil {
			return nil, err
		}
		p, f = c.fetch(ctx, tok, &sk, []string{first})
		if f != nil && f.Kind == KindInvalidToken {
			c.forget(tok)
		}
	}
	if f != nil {
		return nil, f
	}
	p.Skew, p.HasSkew = sk.d, sk.ok
	return p, nil
}

// fetch is one GET of the directory with tok; earlier are the access tokens
// this pull sent before it (the one a 401 refused). A 200 that would be a
// good pull is refused when its body holds tok or one of earlier (echoes):
// the service has reflected a credential this server sent, and nothing of
// that answer may be kept or logged.
func (c *Client) fetch(ctx context.Context, tok string, sk *skew, earlier []string) (*Pull, *Failure) {
	sent := append([]string{tok}, earlier...)
	req, err := newRequest(ctx, http.MethodGet, c.issuer+"/v1/directory", nil)
	if err != nil {
		return nil, &Failure{Step: StepDirectory, Kind: KindTransport, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	a, f := exchange(c.hc, req, StepDirectory, http.StatusOK, MaxDirectoryBody, c.now)
	sk.see(a)
	if f != nil {
		return nil, scrub(f, sent...)
	}
	p, f := classifyDirectory(a, c.organization)
	if f == nil {
		echoed, err := echoes(a.body, sent)
		switch {
		case err != nil:
			return nil, a.fail(StepDirectory, KindBadAnswer, errors.New("the answer could not be read again for the credential check"))
		case echoed:
			return nil, a.fail(StepDirectory, KindCredentialEchoed, errEchoed)
		}
	}
	return p, scrub(f, sent...)
}

// errEchoed is the failure of a directory answer that holds the access token
// the request carried. It never holds the token.
var errEchoed = errors.New("the answer holds the access token this server sent with the request; it is refused, and nothing of it is kept")

// echoes reports whether body, a directory answer that rules 1, 5 and 6
// accepted, holds any of sent (the access tokens this pull carried): as its
// bytes stand, or inside any JSON string (a member's name or a value, an
// ignored member's included) once its escapes are read, so an escaped
// spelling of a token is found too. An error is a body the decoder could not
// read again, which the caller refuses.
func echoes(body []byte, sent []string) (bool, error) {
	var live []string
	for _, s := range sent {
		if s != "" {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return false, nil
	}
	for _, s := range live {
		if bytes.Contains(body, []byte(s)) {
			return true, nil
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if s, ok := tok.(string); ok {
			for _, t := range live {
				if strings.Contains(s, t) {
					return true, nil
				}
			}
		}
	}
}

// classifyDirectory turns one answer of GET /v1/directory into a good pull
// or a failure: a 200 whose body rules 1, 5 and 6 accept is good; anything
// else fails.
func classifyDirectory(a *answer, organization string) (*Pull, *Failure) {
	if a.status != http.StatusOK {
		return nil, statusFailure(StepDirectory, a)
	}
	list, err := DecodeList(a.body, organization)
	if err != nil {
		return nil, a.fail(StepDirectory, KindBadAnswer, err)
	}
	return &Pull{List: list, Body: a.body, FetchedAt: a.local}, nil
}

// accessToken returns the access token in memory while it is usable, and
// otherwise logs in for a new one.
func (c *Client) accessToken(ctx context.Context, sk *skew) (string, error) {
	c.mu.Lock()
	tok, until := c.token, c.tokenUntil
	c.mu.Unlock()
	if tok != "" && wallClock(c.now).Before(until) {
		return tok, nil
	}
	tok, lifetime, a, f := c.requestToken(ctx)
	sk.see(a)
	if f != nil {
		return "", f
	}
	if reuse := lifetime - tokenEarly; reuse > 0 {
		c.mu.Lock()
		c.token, c.tokenUntil = tok, a.local.Add(reuse)
		c.mu.Unlock()
	}
	return tok, nil
}

// forget drops tok from memory, if it is the token held.
func (c *Client) forget(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == tok {
		c.token, c.tokenUntil = "", time.Time{}
	}
}

// requestToken logs in: client credentials with a fresh assertion, sent
// once. A failure's text taken from the answer never holds the assertion
// (scrub).
func (c *Client) requestToken(ctx context.Context) (string, time.Duration, *answer, *Failure) {
	assertion, err := c.assertion()
	if err != nil {
		return "", 0, nil, &Failure{Step: StepToken, Kind: KindTransport, Err: err}
	}
	tok, lifetime, a, f := c.sendToken(ctx, assertion)
	return tok, lifetime, a, scrub(f, assertion)
}

// sendToken sends the token request carrying assertion.
func (c *Client) sendToken(ctx context.Context, assertion string) (string, time.Duration, *answer, *Failure) {
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {assertionType},
		"client_assertion":      {assertion},
	}
	req, err := newRequest(ctx, http.MethodPost, c.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, nil, &Failure{Step: StepToken, Kind: KindTransport, Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a, f := exchange(c.hc, req, StepToken, http.StatusOK, MaxAnswerBody, c.now)
	if f != nil {
		return "", 0, a, f
	}
	if a.status != http.StatusOK {
		return "", 0, a, statusFailure(StepToken, a)
	}
	tok, lifetime, err := decodeTokenAnswer(a.body)
	if err != nil {
		return "", 0, a, a.fail(StepToken, KindBadAnswer, err)
	}
	return tok, lifetime, a, nil
}

// decodeTokenAnswer reads a token endpoint's 200: access_token a non-empty
// string, token_type bearer (any case), expires_in a positive integer, and
// scope, when present, holding directory:read.
func decodeTokenAnswer(body []byte) (string, time.Duration, error) {
	obj, err := parseDocument(body)
	if err != nil {
		return "", 0, err
	}
	if obj.duplicate() {
		return "", 0, errors.New("a member is named twice")
	}
	v, _ := obj.one("access_token")
	tok, ok := rawString(v)
	if !ok || tok == "" {
		return "", 0, errors.New("no access_token")
	}
	v, _ = obj.one("token_type")
	if tt, ok := rawString(v); !ok || !strings.EqualFold(tt, "bearer") {
		return "", 0, errors.New("token_type is not bearer")
	}
	v, _ = obj.one("expires_in")
	secs, ok := rawPositiveInt(v)
	if !ok {
		return "", 0, errors.New("expires_in is not a positive integer")
	}
	// No member is named twice, so scope is present exactly when one finds it.
	if v, present := obj.one("scope"); present {
		scope, ok := rawString(v)
		if !ok || !slices.Contains(strings.Split(scope, " "), directoryScope) {
			return "", 0, fmt.Errorf("scope does not include %s", directoryScope)
		}
	}
	return tok, time.Duration(min(secs, int64(maxTokenReuse/time.Second))) * time.Second, nil
}

// assertionClaims are the assertion's claims, in the order they are sent.
type assertionClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

// assertion signs a new client assertion (a compact JWS, EdDSA): iss = sub =
// the client id, aud the issuer string, iat now, exp 60 seconds later, jti
// 16 random bytes in hex.
func (c *Client) assertion() (string, error) {
	var jti [16]byte
	if _, err := io.ReadFull(c.rand, jti[:]); err != nil {
		return "", fmt.Errorf("assertion jti: %w", err)
	}
	now := c.now().Unix()
	claims, err := json.Marshal(assertionClaims{
		Iss: c.clientID, Sub: c.clientID, Aud: c.issuer,
		Iat: now, Exp: now + assertionLifetime,
		Jti: hex.EncodeToString(jti[:]),
	})
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	input := b64.EncodeToString([]byte(assertionHeader)) + "." + b64.EncodeToString(claims)
	return input + "." + b64.EncodeToString(ed25519.Sign(c.key, []byte(input))), nil
}
