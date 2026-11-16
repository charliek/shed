package sdk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/shed/sdk/authfail"
)

const (
	defaultServerURL    = "http://localhost:8080"
	maxReconnectBackoff = 30 * time.Second
	initialBackoff      = 1 * time.Second
	// maxRetryAfterWait bounds how long a subscription waits on a server's
	// Retry-After before trying again.
	maxRetryAfterWait = 10 * time.Minute
)

// Subscription connection states reported by HostClient.Status.
const (
	ConnConnected    = "connected"
	ConnReconnecting = "reconnecting"
	ConnStopped      = "stopped"
	// ConnRejected is terminal: the server refused the subscription with 409
	// (another listener already owns the namespace), so the loop stops instead
	// of hot-looping a retry. Distinct from ConnStopped (a clean ctx cancel).
	ConnRejected = "rejected"
)

// errSubscribeConflict is returned by streamMessages when the server answers the
// subscribe with 409 Conflict — the namespace already has an active listener.
// subscribeLoop treats it as terminal: a second broker must be observably
// rejected, not silently retry forever. A 409 with the code
// SOURCE_DUPLICATE_STREAM is not this: it is a *SourceError, and retried.
var errSubscribeConflict = errors.New("namespace already has an active subscriber")

// SubStatus is a snapshot of one namespace subscription's connection state, so
// callers can report health (e.g. `shed-host-agent status --live`) without
// scraping logs.
//
// Fields may be added to SubStatus: build one with field names, since a
// positional literal of it is not something this module keeps compiling.
type SubStatus struct {
	Namespace string
	State     string    // ConnConnected | ConnReconnecting | ConnStopped | ConnRejected
	LastError string    // last connection error (empty while connected)
	Since     time.Time // when the current state began
	// Err is the last connection error as a value (nil while connected), so
	// a caller can tell a credential-source refusal apart with errors.Is or
	// errors.As (see SourceError). It is not part of the JSON encoding
	// (LastError carries its text there).
	Err error `json:"-"`
}

// Machine identity on the credential bus. A server with credential sources
// (shed-server 0.10.0 and newer) takes each user's credential requests from
// one machine of that user, and requires every subscribe and respond to name
// the machine it comes from in two headers.
const (
	// HeaderMachineID carries the machine's id: 32 lowercase hexadecimal
	// digits, stable for the machine.
	HeaderMachineID = "X-Shed-Machine-Id"
	// HeaderMachineName carries the machine's display name, percent-encoded
	// UTF-8.
	HeaderMachineName = "X-Shed-Machine-Name"
	// MaxMachineNameBytes is the longest machine name, in bytes of UTF-8.
	MaxMachineNameBytes = 63
)

// The error codes of a server's credential-source refusals (error.code in
// the response body).
const (
	// CodeSourceNotBound (403): another machine is the user's credential
	// source; the refusal names it.
	CodeSourceNotBound = "SOURCE_NOT_BOUND"
	// CodeSourceMachineIDRequired (403): the request named no machine, or
	// named one the server could not read.
	CodeSourceMachineIDRequired = "SOURCE_MACHINE_ID_REQUIRED"
	// CodeSourceDuplicateStream (409): this machine already holds the
	// namespace's subscription for its user.
	CodeSourceDuplicateStream = "SOURCE_DUPLICATE_STREAM"
	// CodeSourceRateLimited (429, with Retry-After): too many new or changed
	// machines for the user lately.
	CodeSourceRateLimited = "SOURCE_RATE_LIMITED"
)

// Sentinels for errors.Is on a *SourceError, one per refusal code.
var (
	ErrSourceNotBound          = errors.New("another machine is the credential source")
	ErrSourceMachineIDRequired = errors.New("the server requires a machine identity")
	ErrSourceDuplicateStream   = errors.New("this machine already holds the subscription")
	ErrSourceRateLimited       = errors.New("too many new or changed machines lately")
)

// ErrInvalidMachine is returned (wrapped, with the reason) by ValidateMachine,
// and by Subscribe's loop and Respond for a client built with an invalid
// WithMachine identity: nothing is sent.
var ErrInvalidMachine = errors.New("invalid machine identity")

// SourceError is a server's credential-source refusal of a subscribe or a
// respond, read from a complete response body of at most 1024 bytes. Use
// errors.Is with ErrSourceNotBound, ErrSourceMachineIDRequired,
// ErrSourceDuplicateStream or ErrSourceRateLimited to tell them apart, and
// errors.As for the fields. Any other non-success answer is reported as
// before: an "unexpected status" error, except that a subscribe's other 409
// (a legacy namespace conflict) ends the subscription as ConnRejected.
type SourceError struct {
	StatusCode int    // 403, 409 or 429
	Code       string // one of the CodeSource* constants
	Message    string // the server's error.message
	// MachineID and MachineName name the user's credential source, for
	// CodeSourceNotBound; both are empty when no machine is the source.
	MachineID   string
	MachineName string
	// RetryAfter is the server's Retry-After, for CodeSourceRateLimited, at
	// most ten minutes (zero when absent, negative, or not a whole number of
	// seconds that fits an int).
	RetryAfter time.Duration
}

// Error implements error.
func (e *SourceError) Error() string {
	s := fmt.Sprintf("credential source refusal (status %d, %s)", e.StatusCode, e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is matches the sentinel of e's code.
func (e *SourceError) Is(target error) bool {
	switch e.Code {
	case CodeSourceNotBound:
		return target == ErrSourceNotBound
	case CodeSourceMachineIDRequired:
		return target == ErrSourceMachineIDRequired
	case CodeSourceDuplicateStream:
		return target == ErrSourceDuplicateStream
	case CodeSourceRateLimited:
		return target == ErrSourceRateLimited
	}
	return false
}

// maxErrorBody is how much of an error response body is read and reported.
const maxErrorBody = 1024

// errorBodyReadTimeout bounds the time a non-success body may take to arrive
// (subscribe and respond alike, as the Rust broker's ERROR_BODY_READ_TIMEOUT):
// a body that never ends must not hold a subscription or a respond for ever.
// A variable so tests can shorten it.
var errorBodyReadTimeout = 3 * time.Second

// errorBodyAbortGrace bounds the wait, after an aborted read, for the read
// to give back what it had.
const errorBodyAbortGrace = time.Second

// readErrorBody reads at most maxErrorBody bytes of a non-success body, for
// at most errorBodyReadTimeout, and reports whether that is the whole body
// (only a complete body is classified as a refusal). A read that runs out of
// time is ended by abort (the request's context) and by closing the body,
// and is never complete.
func readErrorBody(body io.ReadCloser, abort context.CancelFunc) (raw []byte, complete bool) {
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(body, maxErrorBody+1))
		done <- result{raw, err}
	}()
	timer := time.NewTimer(errorBodyReadTimeout)
	defer timer.Stop()
	var r result
	select {
	case r = <-done:
	case <-timer.C:
		abort()
		_ = body.Close()
		select {
		case r = <-done:
		case <-time.After(errorBodyAbortGrace):
		}
		r.err = errors.New("the body did not end in time")
	}
	if len(r.raw) > maxErrorBody {
		return r.raw[:maxErrorBody], false
	}
	return r.raw, r.err == nil
}

// sourceRefusal returns the credential-source refusal a non-success answer
// carries, or nil: the status and error.code must be one of the pairs the
// server sends (403 SOURCE_NOT_BOUND, 403 SOURCE_MACHINE_ID_REQUIRED, 409
// SOURCE_DUPLICATE_STREAM, 429 SOURCE_RATE_LIMITED) and the body complete.
func sourceRefusal(resp *http.Response, body []byte, complete bool) *SourceError {
	if !complete {
		return nil
	}
	var parsed struct {
		Error  map[string]json.RawMessage `json:"error"`
		Source map[string]json.RawMessage `json:"source"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return nil
	}
	str := func(m map[string]json.RawMessage, key string) string {
		var s string
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	e := &SourceError{StatusCode: resp.StatusCode, Code: str(parsed.Error, "code"), Message: str(parsed.Error, "message")}
	switch {
	case resp.StatusCode == http.StatusForbidden && e.Code == CodeSourceNotBound:
		e.MachineID, e.MachineName = str(parsed.Source, "machine_id"), str(parsed.Source, "machine_name")
	case resp.StatusCode == http.StatusForbidden && e.Code == CodeSourceMachineIDRequired,
		resp.StatusCode == http.StatusConflict && e.Code == CodeSourceDuplicateStream:
	case resp.StatusCode == http.StatusTooManyRequests && e.Code == CodeSourceRateLimited:
		// Clamped to the cap in seconds before it becomes a Duration: a huge
		// value must not overflow into a negative or tiny wait. A value Atoi
		// cannot read (an HTTP date, garbage, or one too large for an int)
		// is absent.
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(min(secs, int(maxRetryAfterWait/time.Second))) * time.Second
		}
	default:
		return nil
	}
	return e
}

// ValidateMachine checks a machine identity by the rules a server applies:
// the id is 32 lowercase hexadecimal digits; the name is 1 to
// MaxMachineNameBytes bytes of valid UTF-8 with no control character. It
// returns nil or an error wrapping ErrInvalidMachine.
func ValidateMachine(id, name string) error {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: the id must be 32 lowercase hexadecimal digits", ErrInvalidMachine)
	}
	if name == "" || len(name) > MaxMachineNameBytes {
		return fmt.Errorf("%w: the name must be 1 to %d bytes", ErrInvalidMachine, MaxMachineNameBytes)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: the name must be valid UTF-8", ErrInvalidMachine)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: the name must not contain a control character", ErrInvalidMachine)
		}
	}
	return nil
}

// encodeMachineName is name as it travels in HeaderMachineName: its UTF-8
// bytes, every byte that is not an RFC 3986 unreserved character
// (A-Z a-z 0-9 - . _ ~) written %XX (uppercase hex). The same encoding as
// the Rust broker's (crates/shed-broker/src/bus.rs: percent_encode_name), and
// the server decodes it with a path unescape.
func encodeMachineName(name string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// HostClient connects to shed-server's plugin API to receive and respond to messages.
// Used by host-side extension binaries.
//
// A server with credential sources (shed-server 0.10.0 and newer) needs to
// know which machine is subscribing and answering: give the client one with
// WithMachine. Without it the client sends no machine headers, and such a
// server refuses every subscribe and respond with 403
// SOURCE_MACHINE_ID_REQUIRED (ErrSourceMachineIDRequired).
type HostClient struct {
	serverURL  string
	httpClient *http.Client
	logger     *slog.Logger
	token      string
	// tokenProvider, when set, supplies a refreshing bearer token and takes
	// precedence over the static token. It is consulted per request (subscribe
	// reconnect + respond) so a re-minted token is picked up without rebuilding
	// the client; Invalidate is called after a 401.
	tokenProvider TokenProvider
	tlsPin        string // "sha256:<hex>" pin applied in NewHostClient, or ""
	// clientCerts, when set, supplies the client certificate for an mtls-mode
	// server. It is installed as tls.Config.GetClientCertificate, so it is
	// consulted per handshake — a rotated certificate is picked up without
	// rebuilding the client or its transport.
	clientCerts ClientCertProvider
	// certAuth records that clientCerts was actually WIRED into the transport
	// (which needs a pin — see applyTLSPin). A provider that was configured but
	// never installed leaves the token path exactly as it was.
	certAuth bool

	// The machine this client answers from (WithMachine): sent in
	// HeaderMachineID and HeaderMachineName when machineSet, unless
	// machineErr says the identity is invalid, in which case nothing is sent.
	machineSet  bool
	machineID   string
	machineName string
	machineErr  error

	mu     sync.Mutex
	states map[string]SubStatus
}

// TokenProvider supplies the bearer token for the bus client and refreshes it as
// needed. Implementations must be safe for concurrent use: the subscribe and
// respond paths call Token from different goroutines.
type TokenProvider interface {
	// Token returns the current bearer token, re-minting if it has expired. An
	// error means no token is currently available; the client sends unauthenticated.
	Token() (string, error)
	// Invalidate marks the current token stale so the next Token re-mints. The
	// client calls it after a 401 — the server rejected the cached token.
	Invalidate()
}

// ClientCertProvider supplies the client certificate presented to an mtls-mode
// shed-server. Implementations must be safe for concurrent use: it is called
// from the TLS stack on every handshake, on whichever goroutine is dialing.
//
// Returning nil means "no certificate": the provider is installed
// unconditionally alongside the pin, so a token-mode client simply has nothing
// to present and a later mode flip needs no new transport.
type ClientCertProvider interface {
	ClientCertificate() *tls.Certificate
}

// ClientCertFunc adapts a plain function to ClientCertProvider.
type ClientCertFunc func() *tls.Certificate

// ClientCertificate implements ClientCertProvider.
func (f ClientCertFunc) ClientCertificate() *tls.Certificate { return f() }

// HostClientOption configures a HostClient.
type HostClientOption func(*HostClient)

// WithServerURL sets the shed-server URL.
func WithServerURL(url string) HostClientOption {
	return func(c *HostClient) {
		c.serverURL = strings.TrimRight(url, "/")
	}
}

// WithHTTPClient sets a custom HTTP client. The client is copied, never
// changed. Whatever its own CheckRedirect allows, the HostClient follows a
// redirect only within the configured server's origin (scheme, host name and
// effective port; never from https to http) and at most three times; the
// client's CheckRedirect is then asked about each redirect that rule allows.
func WithHTTPClient(hc *http.Client) HostClientOption {
	return func(c *HostClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) HostClientOption {
	return func(c *HostClient) {
		c.logger = logger
	}
}

// WithTokenProvider sets a refreshing token source, used in preference to a
// static WithToken. The host-agent passes one backed by its SSH credential
// minter so the bus token is re-minted near expiry / on a 401 without rebuilding
// the client.
//
// A wired WithClientCertificates outranks it: no Authorization header is sent
// while the certificate is the credential.
func WithTokenProvider(tp TokenProvider) HostClientOption {
	return func(c *HostClient) {
		c.tokenProvider = tp
	}
}

// WithToken sets the bearer token sent on requests (the host-agent's
// credentials-scoped token). Sent when non-empty.
//
// A wired WithClientCertificates outranks it: no Authorization header is sent
// while the certificate is the credential.
func WithToken(token string) HostClientOption {
	return func(c *HostClient) {
		c.token = token
	}
}

// WithTLSPin pins shed-server's self-signed TLS certificate by the SHA-256
// fingerprint of its DER ("sha256:<hex>") — the trust model the HTTPS listener
// expects (no CA). Pass it alongside an https WithServerURL.
//
// Fail-closed: a pin only protects an https endpoint, so if the resolved
// serverURL is not https the client refuses to send (every request errors)
// rather than silently downgrading to unpinned plaintext.
//
// Composition with WithHTTPClient: order-independent. The pin is applied once,
// after all options, onto the resolved HTTP client — its Timeout and a custom
// *http.Transport's other settings (including any existing TLS config: client
// certs, ServerName, NextProtos) are preserved by cloning, never mutating the
// caller's client. If the custom client uses a non-*http.Transport
// RoundTripper, it owns its own TLS and the pin is skipped with a warning.
func WithTLSPin(fingerprint string) HostClientOption {
	return func(c *HostClient) {
		c.tlsPin = fingerprint
	}
}

// WithClientCertificates installs a client-certificate provider for an
// mtls-mode shed-server, consulted per TLS handshake. Pass it alongside
// WithTLSPin: mtls is only ever served over the pinned HTTPS listener, and the
// provider is only wired when a pin is set.
//
// It TAKES PRECEDENCE over WithToken/WithTokenProvider whenever it actually has
// a certificate: for those requests no Authorization header is sent at all,
// mirroring the in-tree CLI's mode gating. An mtls-mode server never reads the
// header, so a client that sent both would be shipping a live bearer token to
// an endpoint that ignores it — a credential on the wire with no upside.
//
// While the provider holds NOTHING the token options apply normally. That is
// what makes one client work against a server in either mode, and keep working
// across a flip: the provider and the token source are two faces of the same
// credential, only one of which is populated at a time, and the transport is
// never rebuilt when which one changes.
func WithClientCertificates(p ClientCertProvider) HostClientOption {
	return func(c *HostClient) {
		c.clientCerts = p
	}
}

// WithMachine names the machine this client subscribes and answers from: id
// is 32 lowercase hexadecimal digits that stay the same for the machine, and
// name is its display name (1 to MaxMachineNameBytes bytes of UTF-8, no
// control character). Both are sent on every subscribe and respond, the name
// percent-encoded. The server takes a user's credential requests from one
// machine, the first to connect, until the user moves it (`shed source use`);
// another machine is refused with a *SourceError naming the one that is.
//
// Which machine answers for a user is the caller's decision: keep the id
// stable (stored with the machine's other state), since a new id is a new
// machine to the server. An invalid identity (see ValidateMachine) is not
// sent: Subscribe's loop stops with ConnRejected and Respond returns an error
// wrapping ErrInvalidMachine.
func WithMachine(id, name string) HostClientOption {
	return func(c *HostClient) {
		c.machineSet, c.machineID, c.machineName = true, id, name
		c.machineErr = ValidateMachine(id, name)
	}
}

// setMachine adds the machine headers when the client has an identity.
func (c *HostClient) setMachine(req *http.Request) {
	if c.machineSet && c.machineErr == nil {
		req.Header.Set(HeaderMachineID, c.machineID)
		req.Header.Set(HeaderMachineName, encodeMachineName(c.machineName))
	}
}

// applyTLSPin installs cert pinning on the resolved HTTP client's transport.
// Called once after all options so it composes with WithHTTPClient regardless
// of option order.
func (c *HostClient) applyTLSPin() {
	if c.tlsPin == "" {
		return
	}
	// A pin only protects an https endpoint; with an http serverURL the TLS
	// config is never exercised and traffic would go plaintext. Fail closed.
	if !strings.HasPrefix(strings.ToLower(c.serverURL), "https://") {
		c.httpClient = &http.Client{
			Timeout: c.httpClient.Timeout,
			Transport: errorRoundTripper{fmt.Errorf(
				"WithTLSPin set but serverURL %q is not https; refusing to send unpinned plaintext", c.serverURL)},
		}
		return
	}
	var base *http.Transport
	switch t := c.httpClient.Transport.(type) {
	case *http.Transport:
		base = t.Clone()
	case nil:
		// Clone DefaultTransport to keep proxy/HTTP2 defaults; guard the
		// assertion in case it was globally replaced with another RoundTripper.
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			base = dt.Clone()
		} else {
			base = &http.Transport{}
		}
	default:
		c.logger.Warn("WithTLSPin ignored: custom HTTP client uses a non-*http.Transport RoundTripper that owns its own TLS")
		return
	}
	// Overlay the pin onto the caller's existing TLS settings rather than
	// replacing them wholesale, so client certs / ServerName / NextProtos /
	// a stricter MinVersion survive.
	tlsCfg := base.TLSClientConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	tlsCfg.InsecureSkipVerify = true
	tlsCfg.VerifyPeerCertificate = pinVerifier(c.tlsPin)
	// Install the client-certificate callback UNCONDITIONALLY when a provider is
	// configured, even if it currently has nothing to present. The callback is
	// what makes rotation — and a token↔mtls mode flip — invisible to the
	// transport: the TLS stack asks per handshake, so the credential can change
	// underneath a live *http.Transport without rebuilding it. See
	// internal/servertls.PinnedClientConfig, the in-tree twin of this.
	if c.clientCerts != nil {
		provider := c.clientCerts
		// From here on the certificate IS the credential: setAuth stops sending a
		// bearer token, so the two never travel together.
		c.certAuth = true
		tlsCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if cert := provider.ClientCertificate(); cert != nil {
				return cert, nil
			}
			// An EMPTY certificate, not an error: the handshake must proceed and
			// let the server decide. Returning an error aborts the connection
			// client-side, which would turn "this server wants a cert I don't
			// have yet" into an opaque dial failure instead of the server's own
			// certificate_required alert that the re-enrollment path keys on.
			return &tls.Certificate{}, nil
		}
	}
	base.TLSClientConfig = tlsCfg
	c.httpClient = &http.Client{
		Transport:     base,
		Timeout:       c.httpClient.Timeout,
		CheckRedirect: c.httpClient.CheckRedirect,
		Jar:           c.httpClient.Jar,
	}
}

// maxRedirects is the most redirects the client follows in one request, and
// only within the configured server's origin (the Rust broker's
// MAX_REDIRECTS).
const maxRedirects = 3

// effectivePort is u's port, or its scheme's default when it names none.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// redirectAllowed reports whether the client follows a redirect to next,
// hops redirects into the request: only to the configured server's own
// origin (scheme, host name and effective port), so never from https to
// http, and at most maxRedirects times. Every request carries this machine's
// identity headers and the bearer credential, and may present a client
// certificate: none of them may reach another origin. A nil configured URL
// (one that did not parse) follows nothing. As the Rust broker's
// redirect_allowed (crates/shed-broker/src/bus.rs).
func redirectAllowed(configured, next *url.URL, hops int) bool {
	if configured == nil || next == nil || hops > maxRedirects {
		return false
	}
	if configured.Scheme != "http" && configured.Scheme != "https" {
		return false
	}
	return next.Scheme == configured.Scheme &&
		strings.EqualFold(next.Hostname(), configured.Hostname()) &&
		effectivePort(next) == effectivePort(configured)
}

// restrictRedirects makes the resolved HTTP client follow only the
// redirects redirectAllowed allows. Any other redirect response is the
// answer (http.ErrUseLastResponse), reported as an unexpected status, and
// nothing is sent to its target. A CheckRedirect the caller's client had
// runs after this rule, on a redirect the rule allows, and can only refuse
// more. The caller's client is copied, never changed.
func (c *HostClient) restrictRedirects() {
	configured, err := url.Parse(c.serverURL)
	if err != nil {
		configured = nil
	}
	hc := *c.httpClient
	theirs := hc.CheckRedirect
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !redirectAllowed(configured, req.URL, len(via)) {
			return http.ErrUseLastResponse
		}
		if theirs != nil {
			return theirs(req, via)
		}
		return nil
	}
	c.httpClient = &hc
}

// invalidateOnAuthFailure asks the credential provider to re-mint when the
// server refused what we presented, and reports whether it did.
//
// The two shapes of refusal — an HTTP 401 and a peer TLS alert naming a
// certificate problem — are classified together (sdk/authfail) because they are
// the same event seen from the two modes, and because a client cannot know in
// advance which mode the server is in. A caller uses the bool to decide whether
// a single retry is worth attempting; false means "nothing changed, retrying
// would just repeat the failure".
func (c *HostClient) invalidateOnAuthFailure(status int, err error) bool {
	if c.tokenProvider == nil || !authfail.IsAuthFailure(status, err) {
		return false
	}
	c.tokenProvider.Invalidate()
	return true
}

// presentingCertificate reports whether the wired client-certificate provider
// currently HAS a certificate to present.
//
// It is a per-request question, not a per-client one, and that is the whole
// point. The provider is installed unconditionally alongside the pin so that a
// server flipping between token and mtls needs no new transport — which means
// "a provider is wired" says nothing about which mode we are actually in.
// Only "the provider is holding a certificate right now" does.
func (c *HostClient) presentingCertificate() bool {
	return c.certAuth && c.clientCerts != nil && c.clientCerts.ClientCertificate() != nil
}

// setAuth adds the bearer token header. With a tokenProvider it uses the current
// (possibly just-refreshed) token; otherwise the static token. Both no-op when
// empty (an open-mode, un-gated server).
//
// It adds NOTHING while a certificate is actually being presented: the
// credential travels in the handshake, an mtls-mode server never reads this
// header, and sending a bearer token alongside a certificate would put a second
// live credential on the wire for no benefit.
//
// The gate is on the certificate being HELD, not on the provider being wired.
// An adaptive client wires the provider always and lets the server decide the
// mode (see WithClientCertificates); gating on the wiring would mean a
// token-mode server never receives the token that authenticates the request,
// which is not "mutually exclusive credentials" but simply a broken client.
//
// The provider's Token() runs BEFORE the certificate gate, not behind it:
// Token is where the freshness machinery lives (proactive renewal, and the
// forced re-mint after a server rejection whose replacement mint failed), and
// it is mode-agnostic — in mtls state it returns "" with the machinery run.
// Gating it on "not presenting a certificate" would mean a cert-holding
// client never renews on the request path and never surfaces WHY its
// rejected certificate cannot be replaced. The gate is also re-read after
// Token() rather than before it, because Token() itself can change the
// answer — a token→mtls flip mints a certificate right here.
func (c *HostClient) setAuth(req *http.Request) {
	tok := c.token
	if c.tokenProvider != nil {
		t, err := c.tokenProvider.Token()
		if err != nil {
			// The provider returns the SURVIVING credential alongside the
			// error (a failed re-mint keeps the prior credential presentable
			// — the server might still accept it), so the log line names what
			// actually goes on the wire.
			switch {
			case c.presentingCertificate():
				c.logger.Warn("credential refresh failed; presenting the existing certificate", "error", err)
			case t != "":
				c.logger.Warn("credential refresh failed; presenting the existing token", "error", err)
			default:
				c.logger.Warn("token provider returned no token; sending unauthenticated", "error", err)
			}
		}
		tok = t
	}
	// Add NOTHING while a certificate is actually being presented: the
	// credential travels in the handshake, and a bearer token alongside it
	// would put a second live credential on the wire for no benefit.
	if c.presentingCertificate() {
		return
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// NewHostClient creates a new HostClient with the given options.
func NewHostClient(opts ...HostClientOption) *HostClient {
	c := &HostClient{
		serverURL:  defaultServerURL,
		httpClient: &http.Client{},
		logger:     slog.Default(),
		states:     make(map[string]SubStatus),
	}
	for _, opt := range opts {
		opt(c)
	}
	c.applyTLSPin()
	c.restrictRedirects()
	return c
}

// Subscribe connects to the SSE stream for the given namespace and returns a
// channel of envelopes. The channel is closed when the context is cancelled
// or the connection is permanently lost. Reconnects automatically on transient
// failures with exponential backoff.
func (c *HostClient) Subscribe(ctx context.Context, namespace string) <-chan *Envelope {
	ch := make(chan *Envelope, 32)
	go c.subscribeLoop(ctx, namespace, ch)
	return ch
}

// Status returns a snapshot of each namespace subscription's connection state,
// sorted by namespace. Safe for concurrent use.
func (c *HostClient) Status() []SubStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SubStatus, 0, len(c.states))
	for _, s := range c.states {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}

// setState records a namespace's connection state, stamping Since only when the
// state actually changes (so a snapshot shows how long it's been in that state).
func (c *HostClient) setState(namespace, state string, cause error) {
	es := ""
	if cause != nil {
		es = cause.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.states[namespace]; ok && prev.State == state {
		prev.LastError, prev.Err = es, cause
		c.states[namespace] = prev
		return
	}
	c.states[namespace] = SubStatus{Namespace: namespace, State: state, LastError: es, Since: time.Now(), Err: cause}
}

func (c *HostClient) subscribeLoop(ctx context.Context, namespace string, ch chan<- *Envelope) {
	defer close(ch)
	// Terminal state defaults to a clean stop (ctx cancel); a 409 rejection
	// overrides it to ConnRejected below so Status() reports it observably.
	stopState, stopCause := ConnStopped, error(nil)
	defer func() { c.setState(namespace, stopState, stopCause) }()
	c.setState(namespace, ConnReconnecting, nil) // "connecting" until the first attempt resolves

	backoff := initialBackoff
	downLogged := false
	for {
		start := time.Now()
		connected := false
		err := c.streamMessages(ctx, namespace, ch, func() {
			connected = true
			downLogged = false
			c.setState(namespace, ConnConnected, nil)
			c.logger.Info("SSE connected", "namespace", namespace)
		})
		if ctx.Err() != nil {
			return
		}
		// A 409 means another listener already owns this namespace. Retrying
		// would be a silent double-broker hot-loop, so stop terminally and let
		// Status() surface the rejection.
		if errors.Is(err, errSubscribeConflict) {
			stopState, stopCause = ConnRejected, err
			c.logger.Error("SSE subscription rejected; another listener owns this namespace — not retrying",
				"namespace", namespace, "error", err)
			return
		}
		// An invalid machine identity never becomes valid: nothing was sent,
		// and retrying would send nothing either.
		if errors.Is(err, ErrInvalidMachine) {
			stopState, stopCause = ConnRejected, err
			c.logger.Error("SSE subscription not started; the machine identity is invalid — not retrying",
				"namespace", namespace, "error", err)
			return
		}
		c.setState(namespace, ConnReconnecting, err)

		// Reset backoff only after a connection that held for a while, so a
		// flapping server doesn't keep resetting it.
		if connected && time.Since(start) > 60*time.Second {
			backoff = initialBackoff
		}

		// Log loudly on a down-transition / first failure, then quietly while it
		// stays down — a persistently-unreachable server must not flood the log
		// with an identical WARN every backoff cycle.
		if !downLogged {
			c.logger.Warn("SSE connection lost, reconnecting",
				"namespace", namespace, "error", err, "backoff", backoff)
			downLogged = true
		} else {
			c.logger.Debug("SSE still down, retrying",
				"namespace", namespace, "error", err, "backoff", backoff)
		}
		// A rate-limit refusal says when to come back: wait at least that
		// long (bounded), and otherwise the backoff.
		wait := backoff
		var se *SourceError
		if errors.As(err, &se) && se.RetryAfter > wait {
			wait = min(se.RetryAfter, maxRetryAfterWait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxReconnectBackoff)
	}
}

func (c *HostClient) streamMessages(ctx context.Context, namespace string, ch chan<- *Envelope, onConnect func()) error {
	if c.machineErr != nil {
		return c.machineErr
	}
	url := fmt.Sprintf("%s/api/plugins/listeners/%s/messages", c.serverURL, namespace)
	// reqCtx ends this request alone: readErrorBody cancels it when a
	// non-success body does not end in time.
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setMachine(req)
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A rejected CLIENT CERTIFICATE arrives here, not as a status: the server
		// refuses it in (TLS 1.2) or just after (TLS 1.3) the handshake, so there
		// is no response to read a 401 off. Invalidating on that error is what
		// makes the backoff-reconnect re-enroll instead of replaying the
		// credential the server just refused, forever.
		c.invalidateOnAuthFailure(0, err)
		return fmt.Errorf("connecting: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, complete := readErrorBody(resp.Body, cancelReq)
		// A credential-source refusal is retried on the backoff (a 409
		// SOURCE_DUPLICATE_STREAM included); none is a reason to re-mint.
		if se := sourceRefusal(resp, body, complete); se != nil {
			return se
		}
		if resp.StatusCode == http.StatusConflict {
			return fmt.Errorf("%w: %s", errSubscribeConflict, strings.TrimSpace(string(body)))
		}
		// Credential rejected — re-mint so the backoff-reconnect authenticates
		// fresh. In token mode that is the classic expired-token 401; in mtls it
		// is the per-request re-validation of a certificate that has expired or
		// whose identity has been de-allowlisted since the handshake.
		c.invalidateOnAuthFailure(resp.StatusCode, nil)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	onConnect()

	scanner := bufio.NewScanner(resp.Body)
	const maxSSEEventSize = 1 << 20 // 1 MiB — envelopes can carry large payloads
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSEEventSize)
	var dataBuf bytes.Buffer
	dataLines := 0

	for scanner.Scan() {
		line := scanner.Text()

		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			// Per SSE spec, multiple data: lines are joined with newlines
			if dataLines > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(data)
			dataLines++
			continue
		}

		// Empty line signals end of event
		if line == "" && dataBuf.Len() > 0 {
			var env Envelope
			if err := json.Unmarshal(dataBuf.Bytes(), &env); err != nil {
				c.logger.Warn("failed to parse SSE event", "error", err)
			} else {
				select {
				case ch <- &env:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			dataBuf.Reset()
			dataLines = 0
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stream: %w", err)
	}
	return errors.New("stream closed by server")
}

// Respond sends a response envelope back to shed-server for routing to the
// originating shed.
//
// A server with credential sources accepts an answer only from the machine the
// request was sent to; its refusals are returned as a *SourceError.
func (c *HostClient) Respond(ctx context.Context, namespace string, env *Envelope) error {
	if c.machineErr != nil {
		return c.machineErr
	}
	// cancel ends this respond's requests: readErrorBody uses it when a
	// non-success body does not end in time.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	url := fmt.Sprintf("%s/api/plugins/listeners/%s/respond", c.serverURL, namespace)

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling response: %w", err)
	}

	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		c.setMachine(req)
		c.setAuth(req)
		return c.httpClient.Do(req)
	}

	resp, err := send()
	// A credential can be refused mid-session — a token that expired, or a
	// certificate that expired / was revoked / lost its allowlist entry. Re-mint
	// once via the provider and retry a single time (mirrors the CLI client).
	// The certificate case has no response at all to carry a 401, so the
	// transport error is classified too. The bound is one re-mint / two sends
	// TOTAL: a TLS-level re-mint whose retry then 401s must surface that 401,
	// not spend a second re-mint on a third send — the fresh credential was
	// just refused, and a second mint would present the same identity again.
	reminted := false
	if err != nil && c.invalidateOnAuthFailure(0, err) {
		reminted = true
		resp, err = send()
	}
	if err != nil {
		return fmt.Errorf("sending response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized && !reminted && c.invalidateOnAuthFailure(resp.StatusCode, nil) {
		_ = resp.Body.Close()
		resp, err = send()
		if err != nil {
			return fmt.Errorf("sending response: %w", err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, complete := readErrorBody(resp.Body, cancel)
		if se := sourceRefusal(resp, respBody, complete); se != nil {
			return se
		}
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
