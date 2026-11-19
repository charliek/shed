package directory

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Step names the contract step a request belonged to.
type Step string

// The three steps of the contract.
const (
	StepEnroll    Step = "enrolment"
	StepToken     Step = "token"
	StepDirectory Step = "directory"
)

// Kind classifies a failed request. Every kind is a failed refresh (or a
// failed enrolment); the kind says what to log and when to try again.
type Kind string

// The kinds of failure.
const (
	// KindTransport: no answer arrived (a refused connection, a timeout, a
	// certificate the client does not trust, a connection lost).
	KindTransport Kind = "transport"
	// KindRedirect: a 3xx, never followed (rule 3).
	KindRedirect Kind = "redirect"
	// KindTooLarge: an answer over its size limit (rule 1).
	KindTooLarge Kind = "too-large"
	// KindInvalidClient: the token endpoint refused the client's login: a
	// 400 or 401 whose error is exactly invalid_client. Logged loudly with
	// both clocks. Any other token 4xx is KindRefused (with a code) or
	// KindStatus (without one).
	KindInvalidClient Kind = "invalid-client"
	// KindInvalidToken: the directory refused the access token (401).
	KindInvalidToken Kind = "invalid-token"
	// KindServerDisabled: 403 SERVER_DISABLED.
	KindServerDisabled Kind = "server-disabled"
	// KindRateLimited: 429, with the Retry-After rule 2 accepts.
	KindRateLimited Kind = "rate-limited"
	// KindUnavailable: 503, with the Retry-After rule 2 accepts.
	KindUnavailable Kind = "unavailable"
	// KindServerError: another 5xx (on the token endpoint, the assertion
	// may have been spent: the next attempt signs a new one, as every
	// attempt does).
	KindServerError Kind = "server-error"
	// KindRefused: another 4xx the contract names (Code says which): an
	// enrolment code's state, INSUFFICIENT_SCOPE, a token request the
	// service found malformed (a client bug).
	KindRefused Kind = "refused"
	// KindStatus: a status the contract does not give for this request.
	KindStatus Kind = "status"
	// KindBadAnswer: an answer of the expected status whose body the client
	// refuses (rules 5 and 6, a token answer without its members, a 201
	// that fails its checks).
	KindBadAnswer Kind = "bad-answer"
	// KindCredentialEchoed: a directory 200 whose body holds the access
	// token the request carried (anywhere: a member a reader ignores, an
	// entry's name, a string written with escapes). Not a good pull: nothing
	// of the answer is kept, published or logged, so the token reaches
	// neither the kept copy nor a skipped entry's log line. The failure's
	// text never holds the token.
	KindCredentialEchoed Kind = "credential-echoed"
)

// Failure is one failed request of the contract.
type Failure struct {
	Step Step
	Kind Kind
	// Status is the answer's status; 0 when no answer arrived.
	Status int
	// Code is the error code the answer's body carried: a /v1 error's
	// error.code or an OAuth error. The service's text: printed quoted.
	Code string
	// Message is the answer's error.message or error_description, for logs
	// only (never compared). The service's text: printed quoted.
	Message string
	// RetryAfter is the least wait before the next attempt rule 2 allows,
	// when HasRetryAfter (a 429 or 503 carrying an acceptable Retry-After).
	RetryAfter    time.Duration
	HasRetryAfter bool
	// Date is the answer's Date header, zero when it had none that parses;
	// Local is the client's clock when the answer arrived.
	Date  time.Time
	Local time.Time
	// ClientID is, for an enrolment whose 201 failed its checks, the client
	// id the service said it created, so the operator can disable it. It is
	// set only when the 201 named it unambiguously (one client_id member)
	// in a client id's form; otherwise it is "" and the failure is Lost.
	ClientID string
	// Err is the underlying error, if any.
	Err error
}

func (f *Failure) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", f.Step, f.Kind)
	if f.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", f.Status)
	}
	if f.Code != "" {
		fmt.Fprintf(&b, " %q", f.Code)
	}
	if f.Message != "" {
		fmt.Fprintf(&b, ": %q", f.Message)
	}
	if f.Err != nil {
		fmt.Fprintf(&b, ": %v", f.Err)
	}
	if f.HasRetryAfter {
		fmt.Fprintf(&b, " (retry after %s)", f.RetryAfter)
	}
	if f.Kind == KindInvalidClient {
		fmt.Fprintf(&b, " (the service's clock %s, this server's %s)", formatDate(f.Date), f.Local.UTC().Format(time.RFC3339))
	}
	return b.String()
}

func (f *Failure) Unwrap() error { return f.Err }

// CredentialMarker stands, in a Failure, where text taken from an answer
// held a credential the client had just sent (scrub).
const CredentialMarker = "[credential removed]"

// scrub replaces, in every text of f taken from an answer (its code, its
// message, and its error's text, which may quote the answer: a body
// fragment, a Location, a value it refused), every occurrence of each of
// sent, the credentials the request carried (the assertion, the access
// token), by CredentialMarker, so that nothing logging f can show them. An
// error whose text changed is replaced by its scrubbed text, which still
// answers errors.Is for what the original did but no longer unwraps to it.
// It returns f.
func scrub(f *Failure, sent ...string) *Failure {
	if f == nil {
		return nil
	}
	f.Code = scrubText(f.Code, sent)
	f.Message = scrubText(f.Message, sent)
	if f.Err != nil {
		if text := f.Err.Error(); scrubText(text, sent) != text {
			f.Err = &scrubbedError{text: scrubText(text, sent), was: f.Err}
		}
	}
	return f
}

// scrubText is s with every occurrence of each non-empty secret replaced by
// CredentialMarker.
func scrubText(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, CredentialMarker)
		}
	}
	return s
}

// scrubbedError is an error whose text held a credential: its text with the
// credential replaced. It matches (errors.Is) what the original matched,
// and does not unwrap to it.
type scrubbedError struct {
	text string
	was  error
}

func (e *scrubbedError) Error() string { return e.text }

func (e *scrubbedError) Is(target error) bool { return errors.Is(e.was, target) }

// Lost reports whether an enrolment's outcome is unknown: the service may
// have enrolled the server under a client id the client never learned. It
// is false for a 201 that named one well-formed client id unambiguously
// (ClientID is then the one to disable) and for a 4xx (the service refused
// the enrolment). Everything else is lost: no answer, a 5xx, a 3xx, a 2xx
// other than 201, and a 201 whose client id is missing, named twice or not
// of a client id's form.
func (f *Failure) Lost() bool {
	if f.Step != StepEnroll {
		return false
	}
	switch {
	case f.Status == http.StatusCreated:
		return !ValidClientID(f.ClientID)
	case f.Status >= 400 && f.Status < 500:
		return false
	}
	return true
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "unknown (no Date)"
	}
	return t.UTC().Format(time.RFC3339)
}

// MaxRetryAfter is the longest Retry-After a client honours (rule 2).
const MaxRetryAfter = time.Hour

// RetryAfter reads an answer's Retry-After by rule 2: only on a 429 or a
// 503; the header given once; after spaces and tabs are trimmed from both
// ends, one or more ASCII digits and nothing else, a number of seconds, of
// which at most an hour is honoured. Anything else is absent (ok false).
func RetryAfter(status int, h http.Header) (time.Duration, bool) {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return 0, false
	}
	values := h.Values("Retry-After")
	if len(values) != 1 {
		return 0, false
	}
	v := strings.Trim(values[0], " \t")
	if v == "" {
		return 0, false
	}
	for _, c := range []byte(v) {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	v = strings.TrimLeft(v, "0")
	if len(v) > 4 {
		return MaxRetryAfter, true
	}
	n := 0
	if v != "" {
		n, _ = strconv.Atoi(v)
	}
	return min(time.Duration(n)*time.Second, MaxRetryAfter), true
}

// AnswerDate is an answer's Date header, ok false when it has none that
// parses.
func AnswerDate(h http.Header) (time.Time, bool) {
	v := h.Get("Date")
	if v == "" {
		return time.Time{}, false
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// errorBody reads an error answer's code and message, in either shape: a
// /v1 error, {"error":{"code","message"}}, or an OAuth error,
// {"error","error_description"}. Members are matched by their exact names,
// and an answer naming a member twice (at its top level or inside error)
// is not recognised, so a code is never picked from two candidates.
// Anything else gives "", "".
func errorBody(body []byte) (code, message string) {
	code, message, _ = errorBodyShape(body)
	return code, message
}

// errorBodyShape is errorBody, and whether the code came from an OAuth
// error (error a string) rather than a /v1 error.
func errorBodyShape(body []byte) (code, message string, oauth bool) {
	top, err := parseDocument(body)
	if err != nil || top.duplicate() {
		return "", "", false
	}
	errRaw, ok := top.one("error")
	if !ok {
		return "", "", false
	}
	if s, isString := rawString(errRaw); isString {
		desc, _ := top.one("error_description")
		message, _ = rawString(desc)
		return s, message, s != ""
	}
	code, message = v1Error(errRaw)
	return code, message, false
}

// v1Error reads a /v1 error's object: its code (non-empty) and message.
func v1Error(errRaw []byte) (code, message string) {
	inner, isObject := parseObject(errRaw)
	if !isObject || inner.duplicate() {
		return "", ""
	}
	c, _ := inner.one("code")
	code, _ = rawString(c)
	if code == "" {
		return "", ""
	}
	m, _ := inner.one("message")
	message, _ = rawString(m)
	return code, message
}
