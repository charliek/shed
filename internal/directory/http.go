package directory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/config"
)

// Size limits (rule 1), counted on the bytes read.
const (
	// MaxDirectoryBody is the largest directory answer a client reads.
	MaxDirectoryBody = 8 << 20
	// MaxAnswerBody is the largest token or enrolment answer.
	MaxAnswerBody = 64 << 10
	// MaxErrorBody is how much of an error answer a client reads.
	MaxErrorBody = 4 << 10
)

// HTTP timeouts and limits of the client's transport.
const (
	tlsHandshakeTimeout    = 10 * time.Second
	responseHeaderTimeout  = 15 * time.Second
	maxResponseHeaderBytes = 64 << 10
)

// errTooLarge is an answer over its limit.
var errTooLarge = errors.New("the answer is over its size limit")

// transportConfig is what the HTTP client is built from. A caller outside
// this package sets only the exported fields of ClientConfig and
// EnrolRequest that carry these; it never supplies a client or a transport.
type transportConfig struct {
	// issuer is the service's issuer: requests go only to its scheme and
	// host.
	issuer string
	// allowPlainHTTP allows an http issuer (auth.directory.allow_plain_http).
	allowPlainHTTP bool
	// rootCAs, when set, are the root certificates an https service is
	// checked against, in place of the system's (for tests against a TLS
	// test server).
	rootCAs *x509.CertPool
	// base replaces the transport under the issuer and redirect rules. Tests
	// in this package only.
	base http.RoundTripper
}

// newHTTPClient returns the HTTP client the contract's requests go through,
// for the service at cfg.issuer (rule 3 and 4):
//
//   - https: the standard library's certificate checks (against cfg.rootCAs
//     when set) and the environment's proxy settings;
//   - http: only when cfg.allowPlainHTTP, with no proxy, and its dialer
//     refuses every address that is not loopback, checked on the address
//     actually dialled (after the host name resolved);
//   - no redirect is followed: a 3xx is returned as it is, and the caller
//     treats it as a failure;
//   - requests go only to the issuer's scheme and host;
//   - compression is off, so a body's size is the bytes read.
func newHTTPClient(cfg transportConfig) (*http.Client, error) {
	issuer := cfg.issuer
	if err := config.CheckDirectoryIssuer(issuer); err != nil {
		return nil, err
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return nil, fmt.Errorf("issuer %q: %w", issuer, err)
	}
	tr := &http.Transport{
		TLSHandshakeTimeout:    tlsHandshakeTimeout,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		DisableCompression:     true,
		MaxIdleConns:           4,
		IdleConnTimeout:        90 * time.Second,
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	switch u.Scheme {
	case "https":
		tr.Proxy = http.ProxyFromEnvironment
		tr.ForceAttemptHTTP2 = true
		if cfg.rootCAs != nil {
			tr.TLSClientConfig = &tls.Config{RootCAs: cfg.rootCAs, MinVersion: tls.VersionTLS12}
		}
	case "http":
		if !cfg.allowPlainHTTP {
			return nil, fmt.Errorf("issuer %q is plain HTTP, which needs allow_plain_http", issuer)
		}
		dialer.Control = loopbackOnly
	default:
		return nil, fmt.Errorf("issuer %q is neither https nor http", issuer)
	}
	tr.DialContext = dialer.DialContext
	var next http.RoundTripper = tr
	if cfg.base != nil {
		next = cfg.base
	}
	return &http.Client{
		Transport:     &issuerOnly{scheme: u.Scheme, host: u.Host, next: next},
		CheckRedirect: noRedirect,
	}, nil
}

// noRedirect makes the client return a 3xx as it is (rule 3).
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// loopbackOnly is the plain-HTTP dialer's check, run on the address being
// dialled: it refuses any address that is not a loopback IP.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("plain HTTP is allowed only to a loopback address, not %s", host)
	}
	return nil
}

// issuerOnly refuses any request to another scheme or host than the
// issuer's.
type issuerOnly struct {
	scheme, host string
	next         http.RoundTripper
}

func (t *issuerOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.scheme || req.URL.Host != t.host {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("refusing a request to %s://%s: not the issuer", req.URL.Scheme, req.URL.Host)
	}
	return t.next.RoundTrip(req)
}

// CloseIdleConnections closes the idle connections of the transport under
// it, so http.Client.CloseIdleConnections reaches them.
func (t *issuerOnly) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// answer is one HTTP answer, read within its limits.
type answer struct {
	status int
	header http.Header
	body   []byte
	date   time.Time
	local  time.Time
}

// fail is a failure of step, of kind, about a.
func (a *answer) fail(step Step, kind Kind, err error) *Failure {
	return &Failure{Step: step, Kind: kind, Status: a.status, Date: a.date, Local: a.local, Err: err}
}

// exchange sends req and reads its answer: up to okLimit bytes when the
// status is okStatus (more is a failure, KindTooLarge), and at most
// MaxErrorBody bytes of any other. A 3xx is a failure (KindRedirect). now is
// the client's clock.
func exchange(hc *http.Client, req *http.Request, step Step, okStatus int, okLimit int64, now func() time.Time) (*answer, *Failure) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &Failure{Step: step, Kind: KindTransport, Err: err, Local: wallClock(now)}
	}
	defer resp.Body.Close()
	return readAnswer(resp, step, okStatus, okLimit, wallClock(now))
}

// readAnswer reads resp, which arrived at local, as exchange says.
func readAnswer(resp *http.Response, step Step, okStatus int, okLimit int64, local time.Time) (*answer, *Failure) {
	a := &answer{status: resp.StatusCode, header: resp.Header, local: local}
	a.date, _ = AnswerDate(resp.Header)
	if a.status >= 300 && a.status < 400 {
		return a, a.fail(step, KindRedirect, fmt.Errorf("redirected to %q, not followed", resp.Header.Get("Location")))
	}
	if a.status != okStatus {
		// An error answer is read to at most MaxErrorBody; the rest is
		// left unread.
		a.body, _ = io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
		return a, nil
	}
	if resp.ContentLength > okLimit {
		return a, a.fail(step, KindTooLarge, errTooLarge)
	}
	var err error
	a.body, err = readLimited(resp.Body, okLimit)
	switch {
	case errors.Is(err, errTooLarge):
		return a, a.fail(step, KindTooLarge, err)
	case err != nil:
		return a, a.fail(step, KindTransport, err)
	}
	return a, nil
}

// readLimited reads r to its end, refusing more than limit bytes. It reads
// at most limit+1 bytes: the one byte past the limit is read only to tell a
// body of exactly limit bytes from a longer one (whose length may not be
// known beforehand), and is then discarded with the rest unread.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

// statusFailure classifies an answer whose status is not the one the step
// succeeds with.
func statusFailure(step Step, a *answer) *Failure {
	f := a.fail(step, "", nil) // its kind is decided below
	var oauth bool
	f.Code, f.Message, oauth = errorBodyShape(a.body)
	f.RetryAfter, f.HasRetryAfter = RetryAfter(a.status, a.header)
	switch {
	case a.status == http.StatusTooManyRequests:
		f.Kind = KindRateLimited
	case a.status == http.StatusServiceUnavailable:
		f.Kind = KindUnavailable
	case a.status >= 500:
		f.Kind = KindServerError
	case step == StepToken && (a.status == http.StatusUnauthorized || a.status == http.StatusBadRequest) && oauth && f.Code == "invalid_client":
		// Only the OAuth error invalid_client is a refused login; another
		// token 4xx (another code, none, or a body that is not an OAuth
		// error) is classified below like any other.
		f.Kind = KindInvalidClient
	case step == StepDirectory && a.status == http.StatusUnauthorized:
		f.Kind = KindInvalidToken
	case step == StepDirectory && a.status == http.StatusForbidden && f.Code == "SERVER_DISABLED":
		f.Kind = KindServerDisabled
	case a.status >= 400 && a.status < 500 && f.Code != "":
		f.Kind = KindRefused
	default:
		f.Kind = KindStatus
	}
	return f
}

// wallClock is now() as a wall-clock reading only: no monotonic reading, so
// comparisons count time the host spent asleep.
func wallClock(now func() time.Time) time.Time { return now().Round(0) }

// newRequest builds a request with ctx. The body, if any, is never rewound:
// GetBody is cleared so that no transport can send it a second time.
func newRequest(ctx context.Context, method, target string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	return req, nil
}
