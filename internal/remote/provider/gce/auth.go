package gce

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Scope is the one OAuth scope the client asks for. The service account's
// role, not the scope, is what limits it.
const Scope = "https://www.googleapis.com/auth/cloud-platform"

// DefaultTokenURL is where a signed JWT is traded for an access token when
// the key file names none.
const DefaultTokenURL = "https://oauth2.googleapis.com/token"

// tokenRefreshMargin is how long before expiry a cached token is replaced.
const tokenRefreshMargin = 5 * time.Minute

// TokenSource yields OAuth access tokens for the Compute API. A source that
// also has an Invalidate method has it called when the API answers 401, so
// the next Token mints a fresh one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type invalidator interface{ Invalidate() }

// Credentials is a service account key and the access token cached from it.
// It mints a token by signing a JWT with the key and trading it at the token
// endpoint (RFC 7523, Google's service account flow), and reuses the token
// until five minutes before it expires. Safe for concurrent use.
//
// Neither Credentials nor any error it returns ever contains the private key
// or a token.
type Credentials struct {
	key *serviceAccountKey

	// TokenURL overrides the key file's token_uri (tests).
	TokenURL string
	// HTTP is the client for the token exchange. Nil means a client with a
	// 30 s timeout.
	HTTP *http.Client
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// mu guards tok, exp and minting. It is never held across the token
	// exchange, so a caller whose context ends is not kept waiting on one.
	mu  sync.Mutex
	tok string
	exp time.Time
	// minting is non-nil while one caller is at the token endpoint; it is
	// closed when that exchange ends, whatever its outcome.
	minting chan struct{}
}

// String keeps the cached token and the key out of anything that prints a
// Credentials with %v, %+v or %s.
func (c *Credentials) String() string {
	if c == nil || c.key == nil {
		return "gce.Credentials(none)"
	}
	return "gce.Credentials(" + c.key.ClientEmail + ")"
}

// GoString does the same for %#v.
func (c *Credentials) GoString() string { return c.String() }

type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
	ProjectID    string `json:"project_id"`
	rsa          *rsa.PrivateKey
}

// ParseCredentials reads a service account JSON key, given as the JSON itself
// or as its base64 encoding (standard or URL alphabet, padded or not).
func ParseCredentials(raw []byte) (*Credentials, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, errors.New("service account key is empty")
	}
	data := []byte(text)
	if !strings.HasPrefix(text, "{") {
		decoded, err := decodeBase64(text)
		if err != nil {
			return nil, errors.New("service account key is neither JSON nor base64")
		}
		data = decoded
	}
	var k serviceAccountKey
	if err := json.Unmarshal(data, &k); err != nil {
		return nil, errors.New("service account key is not a JSON key file")
	}
	if k.Type != "service_account" || k.ClientEmail == "" || k.PrivateKey == "" {
		return nil, errors.New("service account key is not a service_account key file")
	}
	blk, _ := pem.Decode([]byte(k.PrivateKey))
	if blk == nil {
		return nil, fmt.Errorf("service account key for %s: the private key is not PEM", k.ClientEmail)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		rk, err1 := x509.ParsePKCS1PrivateKey(blk.Bytes)
		if err1 != nil {
			return nil, fmt.Errorf("service account key for %s: the private key is not PKCS #8 or PKCS #1", k.ClientEmail)
		}
		parsed = rk
	}
	rk, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("service account key for %s: the private key is not RSA", k.ClientEmail)
	}
	k.rsa = rk
	k.PrivateKey = "" // keep the PEM text out of memory dumps of the struct
	if k.TokenURI == "" {
		k.TokenURI = DefaultTokenURL
	}
	return &Credentials{key: &k}, nil
}

func decodeBase64(s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}

// LoadCredentials reads the service account key the server config points at.
// b64Env names an environment variable holding the base64 key; fileEnv names
// one holding a path to the key file (JSON or base64). Exactly one of the two
// variables must be set and non-empty; the error names both otherwise. An
// empty name means that source is not offered. A leading "~/" in the path is
// the user's home directory.
func LoadCredentials(getenv func(string) string, b64Env, fileEnv string) (*Credentials, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var b64, path string
	if b64Env != "" {
		b64 = strings.TrimSpace(getenv(b64Env))
	}
	if fileEnv != "" {
		path = strings.TrimSpace(getenv(fileEnv))
	}
	names := describeEnvs(b64Env, fileEnv)
	switch {
	case b64 != "" && path != "":
		return nil, fmt.Errorf("gce credentials: both %s are set; set exactly one", names)
	case b64 == "" && path == "":
		return nil, fmt.Errorf("gce credentials: none of %s is set", names)
	case b64 != "":
		c, err := ParseCredentials([]byte(b64))
		if err != nil {
			return nil, fmt.Errorf("gce credentials from %s: %w", b64Env, err)
		}
		return c, nil
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("gce credentials from %s: %w", fileEnv, err)
		}
		path = filepath.Join(home, rest)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gce credentials from %s: %w", fileEnv, err)
	}
	c, err := ParseCredentials(data)
	if err != nil {
		return nil, fmt.Errorf("gce credentials from %s (%s): %w", fileEnv, path, err)
	}
	return c, nil
}

func describeEnvs(names ...string) string {
	var set []string
	for _, n := range names {
		if n != "" {
			set = append(set, n)
		}
	}
	if len(set) == 0 {
		return "(no variables configured)"
	}
	return strings.Join(set, " and ")
}

// Email returns the service account's address, which is safe to log.
func (c *Credentials) Email() string { return c.key.ClientEmail }

// ProjectID returns the project the key was issued in, as the key file says.
func (c *Credentials) ProjectID() string { return c.key.ProjectID }

// Token implements TokenSource. One caller at a time trades the key for a
// token; the others wait for it, each only as long as its own context lasts.
// A failed exchange is reported to the caller that made it, and the next
// waiter tries again.
func (c *Credentials) Token(ctx context.Context) (string, error) {
	for {
		c.mu.Lock()
		now := c.now()
		if c.tok != "" && c.exp.Sub(now) > tokenRefreshMargin {
			tok := c.tok
			c.mu.Unlock()
			return tok, nil
		}
		if wait := c.minting; wait != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-wait:
			}
			continue
		}
		done := make(chan struct{})
		c.minting = done
		c.mu.Unlock()
		return c.exchange(ctx, now, done)
	}
}

// exchange mints a token, stores it, and releases the waiters.
func (c *Credentials) exchange(ctx context.Context, now time.Time, done chan struct{}) (tok string, err error) {
	var exp time.Time
	defer func() {
		c.mu.Lock()
		if err == nil {
			c.tok, c.exp = tok, exp
		}
		c.minting = nil
		c.mu.Unlock()
		close(done)
	}()
	tok, exp, err = c.mint(ctx, now)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// Invalidate drops the cached token, so the next Token mints a new one.
func (c *Credentials) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tok, c.exp = "", time.Time{}
}

func (c *Credentials) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Credentials) mint(ctx context.Context, now time.Time) (string, time.Time, error) {
	tokenURL := c.key.TokenURI
	if c.TokenURL != "" {
		tokenURL = c.TokenURL
	}
	enc := base64.RawURLEncoding
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": c.key.PrivateKeyID})
	claims, _ := json.Marshal(map[string]any{
		"iss":   c.key.ClientEmail,
		"scope": Scope,
		"aud":   tokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	signing := enc.EncodeToString(hdr) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key.rsa, crypto.SHA256, sum[:])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token request for %s: %w", c.key.ClientEmail, err)
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signing + "." + enc.EncodeToString(sig)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token exchange for %s: %w", c.key.ClientEmail, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token exchange for %s: %w", c.key.ClientEmail, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("token exchange for %s: HTTP %d: %s", c.key.ClientEmail, resp.StatusCode, tokenErrorText(body))
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("token exchange for %s: the reply holds no access token", c.key.ClientEmail)
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 3600
	}
	return r.AccessToken, now.Add(time.Duration(r.ExpiresIn) * time.Second), nil
}

// tokenErrorText keeps the token endpoint's error and description, and
// nothing else from the reply.
func tokenErrorText(body []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		if e.Description != "" {
			return e.Error + ": " + e.Description
		}
		return e.Error
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}
