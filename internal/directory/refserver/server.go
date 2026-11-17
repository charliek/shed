// Package refserver is a reference implementation of the service side of
// the directory contract (tests/directory-contract/v1/CONTRACT.md): the
// enrolment, token and directory endpoints, held to every recorded exchange
// of the contract by this package's tests.
//
// It is for tests, never for production. All of its state (enrolled
// servers, spent codes, the replay ledger) is in memory, so a restart is a
// clean start; its users, codes and disabled servers come from Inputs,
// which it only reads; nothing about it can be changed over HTTP. Its
// command, cmd/shed-directory-ref, serves plain HTTP on a loopback address
// only.
//
// Its replay ledger trusts the latest time it has seen: after the clock
// moves back, an assertion whose acceptance window ended before the latest
// time the server has seen is refused until the clock catches up.
//
// Its verification shares no code with shed's client of the contract
// (internal/directory): each side uses the standard library's crypto on its
// own, so the two cannot agree on one mistake.
package refserver

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// Banner is what every start of the reference server prints.
const Banner = "reference implementation of the directory contract, for tests; not for production"

// The contract's fixed values.
const (
	contractNumber   = 1
	leeway           = 30 * time.Second
	accessTokenTTL   = 600 // seconds
	scopeDirectory   = "directory:read"
	grantClientCreds = "client_credentials"
	assertionTypeJWT = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	rateWindow       = time.Minute
)

// The paths the server answers.
const (
	pathEnroll    = "/v1/servers/enroll"
	pathDirectory = "/v1/directory"
	pathToken     = "/oauth2/token"
	pathJWKS      = "/.well-known/jwks.json"
	pathMetadata  = "/.well-known/oauth-authorization-server"
)

var organizationRx = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Limits are the requests allowed per minute: enrolments and token
// requests per calling address, directory requests per server. Zero takes
// the default.
type Limits struct {
	Enroll, Token, Directory int
}

// Config is a reference server's configuration.
type Config struct {
	// Issuer is the service's public URL: an origin with no path and no
	// trailing slash. The token endpoint is <Issuer>/oauth2/token.
	Issuer string
	// Organization is the organization's slug (contract rule 5).
	Organization string
	// Inputs are the users, codes and disabled servers.
	Inputs Inputs
	// SigningKey signs access tokens, under KeyID. Nil makes a new 2048-bit
	// key at start, under a new key id.
	SigningKey *rsa.PrivateKey
	KeyID      string
	// Now is the service's clock; nil is time.Now.
	Now func() time.Time
	// Limits are the rate limits.
	Limits Limits
	// OnEnrol is called with each new server's name and client id.
	OnEnrol func(serverName, clientID string)
	// Logf logs why a request was refused, and never a code, an assertion,
	// a token or a key. Nil logs nothing.
	Logf func(format string, args ...any)
}

// Server is a reference server: an http.Handler.
type Server struct {
	issuer  string
	org     string
	inputs  Inputs
	signer  *rsa.PrivateKey
	kid     string
	now     func() time.Time
	onEnrol func(serverName, clientID string)
	logf    func(format string, args ...any)

	// mu guards the enrolled servers and the spent codes, so that checking
	// a code and spending it, and checking a key and enrolling it, are each
	// one step.
	mu         sync.Mutex
	servers    map[string]*server // by client id
	spentCodes map[string]string  // code -> the client id it enrolled

	ledger ledger
	limits map[limitKind]*limiter
}

// server is an enrolled server.
type server struct {
	name     string
	clientID string
	key      *clientKey
}

// New makes a reference server.
func New(cfg Config) (*Server, error) {
	if err := checkIssuer(cfg.Issuer); err != nil {
		return nil, err
	}
	if !organizationRx.MatchString(cfg.Organization) {
		return nil, fmt.Errorf("organization %q does not match %s", cfg.Organization, organizationRx)
	}
	if cfg.Inputs == nil {
		return nil, errors.New("no inputs")
	}
	s := &Server{
		issuer:     cfg.Issuer,
		org:        cfg.Organization,
		inputs:     cfg.Inputs,
		signer:     cfg.SigningKey,
		kid:        cfg.KeyID,
		now:        cfg.Now,
		onEnrol:    cfg.OnEnrol,
		logf:       cfg.Logf,
		servers:    map[string]*server{},
		spentCodes: map[string]string{},
		ledger:     ledger{spent: map[ledgerKey]int64{}},
	}
	if s.signer == nil {
		if cfg.KeyID != "" {
			return nil, errors.New("a key id without a signing key")
		}
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		s.signer = k
		s.kid = "ref-" + randomHex(4)
	} else if s.kid == "" {
		return nil, errors.New("a signing key needs its key id")
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.onEnrol == nil {
		s.onEnrol = func(string, string) {}
	}
	l := cfg.Limits
	s.limits = map[limitKind]*limiter{
		limitEnroll:    newLimiter(orDefault(l.Enroll, 30)),
		limitToken:     newLimiter(orDefault(l.Token, 120)),
		limitDirectory: newLimiter(orDefault(l.Directory, 120)),
	}
	return s, nil
}

func orDefault(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// checkIssuer checks the issuer is an origin: a scheme and a host, with no
// user info, path, query or fragment.
func checkIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("issuer: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.String() != issuer {
		return fmt.Errorf("issuer %q is not an origin (a scheme and a host, no path or trailing slash)", issuer)
	}
	return nil
}

// KeyID is the key id the server signs access tokens under.
func (s *Server) KeyID() string { return s.kid }

// Issuer is the server's issuer.
func (s *Server) Issuer() string { return s.issuer }

// ServeHTTP routes a request by its exact path: no path is cleaned or
// redirected (rule 3).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := map[string]struct {
		method  string
		handler func(http.ResponseWriter, *http.Request)
	}{
		pathEnroll:    {http.MethodPost, s.handleEnroll},
		pathToken:     {http.MethodPost, s.handleToken},
		pathDirectory: {http.MethodGet, s.handleDirectory},
		pathJWKS:      {http.MethodGet, s.handleJWKS},
		pathMetadata:  {http.MethodGet, s.handleMetadata},
	}[r.URL.Path]
	switch {
	case route.handler == nil:
		writeV1Error(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint", 0)
	case r.Method != route.method:
		w.Header().Set("Allow", route.method)
		writeV1Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use "+route.method, 0)
	default:
		route.handler(w, r)
	}
}

// writeJSON writes a JSON answer. Every answer on /v1 and /oauth2 carries
// Cache-Control: no-store; the documents under /.well-known do not.
func writeJSON(w http.ResponseWriter, status int, v any, noStore bool) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeBody(w, status, b, noStore)
}

// writeBody writes an answer already encoded as JSON.
func writeBody(w http.ResponseWriter, status int, b []byte, noStore bool) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if noStore {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	w.Write(b)
}

// setRetryAfter sets Retry-After in delta-seconds, on a 429 (rule 2).
func setRetryAfter(w http.ResponseWriter, seconds int) {
	if seconds > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(seconds))
	}
}

// writeV1Error writes a /v1 error: {"error":{"code","message"}}.
func writeV1Error(w http.ResponseWriter, status int, code, message string, retryAfter int) {
	setRetryAfter(w, retryAfter)
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}}, true)
}

// writeUnavailable writes the /v1 answer to a failed lookup, as in a
// storage outage.
func writeUnavailable(w http.ResponseWriter) {
	writeV1Error(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the service cannot reach its storage", 0)
}

// writeOAuthError writes a token endpoint error (RFC 6749, section 5.2).
func writeOAuthError(w http.ResponseWriter, status int, code, description string, retryAfter int) {
	setRetryAfter(w, retryAfter)
	writeJSON(w, status, map[string]string{"error": code, "error_description": description}, true)
}

// callerOf is the address a request came from, for the rate limits.
func callerOf(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// The rate limits: a fixed window of one minute per caller.

type limitKind string

const (
	limitEnroll    limitKind = "enroll"
	limitToken     limitKind = "token"
	limitDirectory limitKind = "directory"
)

type limiter struct {
	mu      sync.Mutex
	max     int
	windows map[string]*rateWindowState
}

type rateWindowState struct {
	start time.Time
	count int
}

func newLimiter(perWindow int) *limiter {
	return &limiter{max: perWindow, windows: map[string]*rateWindowState{}}
}

// allow counts one request of the caller at now, and when the caller has
// used its allowance says how many whole seconds remain of the window.
func (l *limiter) allow(caller string, now time.Time) (ok bool, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[caller]
	if w == nil || !now.Before(w.start.Add(rateWindow)) || now.Before(w.start) {
		if len(l.windows) > 4096 {
			for k, old := range l.windows {
				if !now.Before(old.start.Add(rateWindow)) {
					delete(l.windows, k)
				}
			}
		}
		w = &rateWindowState{start: now}
		l.windows[caller] = w
	}
	if w.count >= l.max {
		left := w.start.Add(rateWindow).Sub(now).Seconds()
		return false, max(1, int(math.Ceil(left)))
	}
	w.count++
	return true, 0
}

// exhaust spends the caller's whole allowance for a window that starts now.
func (l *limiter) exhaust(caller string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.windows[caller] = &rateWindowState{start: now, count: l.max}
}

// allow applies one of the limits.
func (s *Server) allow(kind limitKind, caller string) (bool, int) {
	return s.limits[kind].allow(caller, s.now())
}
