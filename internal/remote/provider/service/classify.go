package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// The status matrix (the contract's section "Status and error matrix")
// and the code list, as fixtures/mapping.json gives them. A test holds
// this table equal to that file.

// ShedAPI is the shed API's answer to a code, when it reaches a caller.
type ShedAPI struct {
	Status int
	Code   string
	Reason string
}

// CodeInfo is one code of the contract.
type CodeInfo struct {
	// Status is the only status the code is honoured with; at another, it
	// is read as that status's default.
	Status int
	// Kind is the provider error kind the code means.
	Kind error
	// ShedAPI is what the shed API answers when the refusal reaches a
	// caller.
	ShedAPI ShedAPI
}

const (
	apiNotFound      = "SHED_NOT_FOUND"
	apiExists        = "SHED_ALREADY_EXISTS"
	apiBackend       = "BACKEND_ERROR"
	apiInvalid       = "INVALID_REQUEST"
	apiLimit         = "COMPUTE_LIMIT_REACHED"
	apiPolicy        = "COMPUTE_POLICY_REFUSED"
	apiDenied        = "COMPUTE_DENIED"
	apiUnavailable   = "COMPUTE_UNAVAILABLE"
	apiCloudQuota    = "CLOUD_QUOTA_EXCEEDED"
	apiCloudCapacity = "ZONE_CAPACITY_UNAVAILABLE"
	apiFirewall      = "FIREWALL_CHECK_FAILED"
	apiDeleting      = "COMPUTE_DELETING"
)

// Codes is every code of the contract (its section "Codes"), with the two
// the directory contract's 403s use (INSUFFICIENT_SCOPE, SERVER_DISABLED).
var Codes = map[string]CodeInfo{
	"NOT_FOUND":            {404, provider.ErrNotFound, ShedAPI{404, apiNotFound, ""}},
	"EXISTS":               {409, provider.ErrExists, ShedAPI{409, apiExists, ""}},
	"SPEC_MISMATCH":        {409, provider.ErrSpecMismatch, ShedAPI{500, apiBackend, ""}},
	"BUSY":                 {409, provider.ErrBusy, ShedAPI{503, apiUnavailable, ""}},
	"INVALID":              {422, provider.ErrInvalid, ShedAPI{500, apiBackend, ""}},
	"LABEL_TAKEN":          {409, provider.ErrLabelTaken, ShedAPI{500, apiBackend, ""}},
	"RESERVED_LABEL":       {422, provider.ErrInvalid, ShedAPI{500, apiBackend, ""}},
	"QUOTA_PERSON":         {409, provider.ErrComputeLimit, ShedAPI{503, apiLimit, "person"}},
	"QUOTA_ORG":            {409, provider.ErrComputeLimit, ShedAPI{503, apiLimit, "organization"}},
	"SIZE_NOT_ALLOWED":     {403, provider.ErrPolicy, ShedAPI{422, apiPolicy, "size"}},
	"SIZE_UNKNOWN":         {422, provider.ErrPolicy, ShedAPI{422, apiPolicy, "size"}},
	"IMAGE_UNKNOWN":        {422, provider.ErrPolicy, ShedAPI{422, apiPolicy, "image"}},
	"LEASE_TOO_LONG":       {422, provider.ErrPolicy, ShedAPI{422, apiPolicy, "lease"}},
	"EXTEND_NOT_ALLOWED":   {403, provider.ErrLease, ShedAPI{403, "EXTEND_NOT_ALLOWED", ""}},
	"LEASE_AT_CAP":         {409, provider.ErrLease, ShedAPI{409, "LEASE_AT_CAP", ""}},
	"LEASE_ENDED":          {409, provider.ErrLease, ShedAPI{409, "LEASE_ENDED", ""}},
	"DELETING":             {409, provider.ErrDeleting, ShedAPI{409, apiDeleting, ""}},
	"PERSON_INACTIVE":      {403, provider.ErrLease, ShedAPI{403, "PERSON_INACTIVE", ""}},
	"PERSON_UNKNOWN":       {403, provider.ErrPolicy, ShedAPI{422, apiPolicy, "person"}},
	"CLIENT_UNKNOWN":       {403, provider.ErrPolicy, ShedAPI{422, apiPolicy, "client"}},
	"CLOUD_QUOTA":          {409, provider.ErrQuota, ShedAPI{503, apiCloudQuota, ""}},
	"CLOUD_CAPACITY":       {409, provider.ErrCapacity, ShedAPI{503, apiCloudCapacity, ""}},
	"ACCESS_UNSUPPORTED":   {422, provider.ErrPolicy, ShedAPI{422, apiPolicy, "access"}},
	"INGRESS_NOT_WRITABLE": {403, provider.ErrIngress, ShedAPI{503, apiFirewall, ""}},
	"INGRESS_TOO_WIDE":     {422, provider.ErrIngress, ShedAPI{503, apiFirewall, ""}},
	"COMPUTE_DENIED":       {403, provider.ErrDenied, ShedAPI{403, apiDenied, ""}},
	"INSUFFICIENT_SCOPE":   {403, provider.ErrDenied, ShedAPI{403, apiDenied, ""}},
	"SERVER_DISABLED":      {403, provider.ErrDenied, ShedAPI{403, apiDenied, ""}},
	"RATE_LIMITED":         {429, provider.ErrUnavailable, ShedAPI{503, apiUnavailable, ""}},
	"UNAVAILABLE":          {503, provider.ErrUnavailable, ShedAPI{503, apiUnavailable, ""}},
}

// StatusDefaults is the code a refusal status stands for when its body
// names no code, a code outside the list, or a listed code at another
// status.
var StatusDefaults = map[int]string{403: "COMPUTE_DENIED", 404: "NOT_FOUND", 409: "INVALID", 422: "INVALID"}

// Failures are the outcomes that carry no code, with their kind and the
// shed API's answer.
var Failures = map[string]CodeInfo{
	// unavailable: a reply that never arrived whole (a timeout, a reset, a
	// truncated body), a 5xx, a 401 twice, a status outside the matrix.
	"unavailable": {0, provider.ErrUnavailable, ShedAPI{503, apiUnavailable, ""}},
	// invalid_reply: a 2xx the contract refuses.
	"invalid_reply": {0, provider.ErrInvalidReply, ShedAPI{503, apiUnavailable, ""}},
	// payload_too_large: a 413, a server's own bug.
	"payload_too_large": {0, provider.ErrInvalid, ShedAPI{500, apiBackend, ""}},
}

// Result is what a reply means for the call.
type Result string

// The results.
const (
	// ResultOK: the reply is the call's answer, validated (Outcome.Value).
	ResultOK Result = "ok"
	// ResultRefused: a definite refusal with a code; nothing was changed.
	ResultRefused Result = "refused"
	// ResultUnknown: a call that changes something got no reply it can
	// read; its outcome is unknown. Retry it with the same request_id, or
	// read the machine.
	ResultUnknown Result = "unknown"
	// ResultFailed: a read failed (Kind says why), or a reply that can be
	// neither read nor retried (413).
	ResultFailed Result = "failed"
	// ResultRetry: a 429; try again after RetryAfter, inside the deadline.
	ResultRetry Result = "retry"
	// ResultReauth: a 401; get a new token and send the call once more.
	ResultReauth Result = "reauth"
)

// Reply is one reply as it arrived: its status, headers and body read as
// ReadBody reads it (a 2xx to the call's limit, an error body to 4 KiB),
// or the transport error that ended it (a timeout, a reset, a body cut
// short). TooLarge says the body passed its limit (Body then holds the
// bytes read, the limit and one): a 2xx so is invalid, and an error body
// so is read as its status alone, once its bytes are checked for a
// credential. Bearer and Assertion are the credentials this attempt
// presented: the bearer token, and the client assertion that token was
// obtained with. A reply that holds either is refused, whatever its
// status; each attempt is checked against its own.
type Reply struct {
	Status    int
	Header    http.Header
	Body      []byte
	TooLarge  bool
	Err       error
	Bearer    string
	Assertion string
}

// BodyLimit is how much of a reply with this status a server reads.
func BodyLimit(call Call, status int) int64 {
	if status >= 200 && status < 300 {
		return ReplyLimit(call)
	}
	return MaxErrorBody
}

// Outcome is a reply classified.
type Outcome struct {
	Result Result
	// Code is the code the server acts on, for a refusal.
	Code string
	// Kind is the provider error kind, for every result but ok, retry and
	// reauth.
	Kind error
	// RetryAfter is a 429's wait, capped at MaxRetryAfter.
	RetryAfter time.Duration
	// Value is the decoded answer of an ok reply: *Catalog, *Accepted,
	// *Operation, *Machine, *List, *Usage or *Ingress.
	Value any
}

// ErrTooLarge is ReadBody's error for a body over its limit.
var ErrTooLarge = errors.New("the body is over its limit")

// ReadBody reads a body to at most limit bytes: it reads one byte past
// the limit to learn that a body is larger, and no further. A larger body
// is ErrTooLarge, with the bytes read (limit and one), which a classifier
// still checks for a credential.
func ReadBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > limit {
		return b, ErrTooLarge
	}
	return b, nil
}

// ReplyLimit is the most a 2xx of the call may hold.
func ReplyLimit(call Call) int64 {
	if call == CallList {
		return MaxListReply
	}
	return MaxReply
}

// okStatuses are the 2xx each call answers.
func okStatus(call Call, status int) bool {
	switch call {
	case CallCreate, CallStart, CallStop:
		return status == 200 || status == 202
	case CallDelete:
		return status == 202 || status == 204
	}
	return status == 200
}

// errorCode reads an error body's code: {"error":{"code":"<CODE>"}} in a
// body of at most 4 KiB that meets the strict JSON checks; "" otherwise.
// The message is never read.
func errorCode(b []byte) string {
	if len(b) > MaxErrorBody {
		return ""
	}
	o, err := decodeTop(b)
	if err != nil {
		return ""
	}
	raw, ok := o["error"]
	if !ok {
		return ""
	}
	eo, err := asObject(raw, "error")
	if err != nil {
		return ""
	}
	code, err := eo.str("code")
	if err != nil || !codeRx.MatchString(code) {
		return ""
	}
	return code
}

// retryAfter reads a 429's Retry-After as delta-seconds given once, capped
// at MaxRetryAfter; absent or malformed, DefaultRetryAfter.
func retryAfter(h http.Header) time.Duration {
	vals := h.Values("Retry-After")
	if len(vals) != 1 {
		return DefaultRetryAfter
	}
	v := strings.Trim(vals[0], " \t")
	if v == "" {
		return DefaultRetryAfter
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return DefaultRetryAfter
		}
	}
	v = strings.TrimLeft(v, "0")
	if v == "" {
		return 0
	}
	if len(v) > 4 {
		return MaxRetryAfter
	}
	n, _ := strconv.Atoi(v)
	return min(time.Duration(n)*time.Second, MaxRetryAfter)
}

// lost is the outcome of a reply that cannot be read: unknown for a call
// that changes something, failed for a read.
func lost(call Call, kind error) Outcome {
	if call.Mutation() {
		return Outcome{Result: ResultUnknown, Kind: kind}
	}
	return Outcome{Result: ResultFailed, Kind: kind}
}

// refused is a refusal by its code.
func refused(code string) Outcome {
	return Outcome{Result: ResultRefused, Code: code, Kind: Codes[code].Kind}
}

// Classify reads one reply to a call, as the status matrix says.
func Classify(call Call, r Reply, c Context) Outcome {
	if r.Err != nil {
		return lost(call, provider.ErrUnavailable)
	}
	// A reply that holds a credential the server presented is refused
	// whole, whatever its status, before anything of it is used.
	if echoes(r.Body, r.Bearer, r.Assertion) {
		return lost(call, provider.ErrInvalidReply)
	}
	switch s := r.Status; {
	case s >= 200 && s < 300:
		if !okStatus(call, s) {
			return lost(call, provider.ErrInvalidReply)
		}
		if r.TooLarge || int64(len(r.Body)) > ReplyLimit(call) {
			return lost(call, provider.ErrInvalidReply)
		}
		v, err := decodeOK(call, s, r.Body, c)
		if err != nil {
			return lost(call, provider.ErrInvalidReply)
		}
		return Outcome{Result: ResultOK, Value: v}
	case s == 401:
		return Outcome{Result: ResultReauth}
	case s == 429:
		return Outcome{Result: ResultRetry, RetryAfter: retryAfter(r.Header)}
	case s == 413:
		return Outcome{Result: ResultFailed, Kind: Failures["payload_too_large"].Kind}
	case s == 403 || s == 404 || s == 409 || s == 422:
		code := ""
		if !r.TooLarge {
			code = errorCode(r.Body)
		}
		if info, ok := Codes[code]; !ok || info.Status != s {
			code = StatusDefaults[s]
		}
		return refused(code)
	}
	return lost(call, provider.ErrUnavailable)
}

// Resolve reads the replies to one call in order: a second only after a
// 401, to the call sent again with a new token. A second 401, or a 401
// with no second reply, is unavailable.
func Resolve(call Call, replies []Reply, c Context) Outcome {
	if len(replies) == 0 {
		return lost(call, provider.ErrUnavailable)
	}
	o := Classify(call, replies[0], c)
	if o.Result != ResultReauth {
		return o
	}
	if len(replies) < 2 {
		return lost(call, provider.ErrUnavailable)
	}
	o = Classify(call, replies[1], c)
	if o.Result == ResultReauth {
		return lost(call, provider.ErrUnavailable)
	}
	return o
}

// decodeOK decodes and validates a call's 2xx.
func decodeOK(call Call, status int, b []byte, c Context) (any, error) {
	switch call {
	case CallCatalog:
		return decodeCatalog(b)
	case CallCreate, CallStart, CallStop, CallDelete:
		return decodeAccepted(call, status, b, c)
	case CallOperation:
		return decodeOperation(b, c)
	case CallGet, CallLabels, CallExtend:
		o, err := decodeTop(b)
		if err != nil {
			return nil, err
		}
		return machineMember(o, c)
	case CallList:
		return decodeList(b, c)
	case CallUsage:
		return decodeUsage(b)
	case CallIngressGet, CallIngressPut:
		return decodeIngress(b)
	}
	return nil, errors.New("no such call")
}

// OperationKind is what a failed operation's code means: its kind in the
// code list, or unavailable for a code outside it.
func OperationKind(code string) error {
	if info, ok := Codes[code]; ok {
		return info.Kind
	}
	return provider.ErrUnavailable
}

// echoes reports whether a body holds one of the credentials: in its bytes
// (a body cut at its limit included), or, for a body the strict reader
// takes as JSON, in any string it decodes to (a member name or a value,
// escapes resolved), wherever it is. The scan keeps every number as its
// text (a number too large for a float64 is still valid JSON), and fails
// closed: a scan error on a body the strict reader took counts as an echo.
func echoes(body []byte, secrets ...string) bool {
	var want [][]byte
	for _, s := range secrets {
		if s != "" {
			want = append(want, []byte(s))
		}
	}
	if len(want) == 0 || len(body) == 0 {
		return false
	}
	holds := func(b []byte) bool {
		for _, w := range want {
			if bytes.Contains(b, w) {
				return true
			}
		}
		return false
	}
	if holds(body) {
		return true
	}
	if checkStrict(body) != nil {
		return false // not JSON: its bytes are all there is to check
	}
	found := false
	if err := scanStrings(body, func(s string) {
		if holds([]byte(s)) {
			found = true
		}
	}); err != nil {
		return true
	}
	return found
}

// scanStrings calls fn with every string of a JSON document, member names
// included, to its end. It is a variable so a test can make it fail.
var scanStrings = func(body []byte, fn func(string)) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if s, ok := tok.(string); ok {
			fn(s)
		}
	}
}
