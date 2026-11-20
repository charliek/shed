package api

// The machine a credential-bus request comes from, and the pin's refusals.
// Every subscribe and respond names its machine in two headers; the bus
// (plugin.Registry.Subscribe, plugin.Bridge.BeginResponseFrom) admits only
// the user's bound machine. See the note at the top of
// internal/plugin/registry.go.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/source"
)

const (
	// HeaderMachineID carries the machine's id: 32 lowercase hex digits.
	HeaderMachineID = "X-Shed-Machine-Id"
	// HeaderMachineName carries the machine's display name, percent-encoded
	// UTF-8 (the header holds only printable ASCII); decoded it is 1 to 63
	// bytes with no control character.
	HeaderMachineName = "X-Shed-Machine-Name"
)

// machineIDRequiredMessage is the refusal of a request with no valid machine
// headers. It names the two headers (an SDK listener must send them) and
// tells a person what to do; it names no version.
const machineIDRequiredMessage = "this server lets one machine per user answer credential requests and needs to know which machine is asking " +
	"(headers " + HeaderMachineID + " and " + HeaderMachineName + "), but this request named no valid machine; install the current shed desktop app"

// maxRefusalBody is the most bytes a pin refusal body may have. A broker
// classifies a refusal only from a body it read completely within 1024
// bytes; this leaves room for the encoder's trailing newline.
const maxRefusalBody = 1000

// singleHeader returns the one value of header name, and false when it is
// missing, empty or given more than once.
func singleHeader(r *http.Request, name string) (string, bool) {
	vals := r.Header.Values(name)
	if len(vals) != 1 || vals[0] == "" {
		return "", false
	}
	return vals[0], true
}

// machineFromRequest reads the machine the request names. The id must be 32
// lowercase hex digits; the name must be printable ASCII that percent-decodes
// to a valid machine name (a raw UTF-8 or control byte, or a malformed
// escape, is refused). The key fingerprint is the caller's certificate
// subject (empty in open mode): recorded and shown, never matched.
func machineFromRequest(r *http.Request) (source.Machine, bool) {
	id, ok := singleHeader(r, HeaderMachineID)
	if !ok || !source.ValidMachineID(id) {
		return source.Machine{}, false
	}
	raw, ok := singleHeader(r, HeaderMachineName)
	if !ok {
		return source.Machine{}, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return source.Machine{}, false
		}
	}
	name, err := url.PathUnescape(raw)
	if err != nil || !source.ValidMachineName(name) {
		return source.Machine{}, false
	}
	m := source.Machine{ID: id, Name: name}
	if c := callerFrom(r.Context()); c.Kind == CallerUser {
		m.KeyFingerprint = c.Name
	}
	return m, true
}

// sourceRefusal is a pin refusal body: the API's usual error object first,
// then (for SOURCE_NOT_BOUND) the machine that is the source.
type sourceRefusal struct {
	Error  config.APIErrorDetail `json:"error"`
	Source *sourceRefusalMachine `json:"source,omitempty"`
}

type sourceRefusalMachine struct {
	MachineID   string `json:"machine_id"`
	MachineName string `json:"machine_name"`
}

// encodeRefusal renders a refusal body JSON-encoded (so a machine name holding
// quotes or HTML characters is inert) with message, or with short when the
// whole body would be over maxRefusalBody bytes.
func encodeRefusal(code, message, short string, src *sourceRefusalMachine) []byte {
	body := sourceRefusal{Error: config.APIErrorDetail{Code: code, Message: message}, Source: src}
	data, err := json.Marshal(body)
	if err == nil && len(data) <= maxRefusalBody {
		return data
	}
	body.Error.Message = short
	data, err = json.Marshal(body)
	if err == nil && len(data) <= maxRefusalBody {
		return data
	}
	// Unreachable for a valid machine name (at most 63 bytes, which JSON
	// expands at most sixfold); without the name the body is a few hundred.
	body.Source = nil
	data, _ = json.Marshal(body)
	return data
}

// writeRefusal writes a refusal body with status.
func writeRefusal(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// writeMachineIDRequired answers a request with no valid machine headers:
// what a broker released before the pin gets. 403, never 401.
func writeMachineIDRequired(w http.ResponseWriter) {
	writeRefusal(w, http.StatusForbidden,
		encodeRefusal(config.ErrSourceMachineIDRequired, machineIDRequiredMessage, machineIDRequiredMessage, nil))
}

// writeSourceNotBound answers a machine that is not its user's credential
// source, naming the one that is.
func writeSourceNotBound(w http.ResponseWriter, bound source.Machine) {
	if bound.ID == "" {
		// No machine is the source (a binding dropped since): the machine's
		// next subscribe binds it.
		msg := "no machine is your credential source on this server; this machine becomes it when it next subscribes"
		writeRefusal(w, http.StatusForbidden, encodeRefusal(config.ErrSourceNotBound, msg, msg, nil))
		return
	}
	src := &sourceRefusalMachine{MachineID: bound.ID, MachineName: bound.Name}
	name := bound.Name
	if name == "" {
		name = bound.ID
	}
	msg := fmt.Sprintf("this server takes your credential requests only from your machine %q; "+
		"to use this machine instead, run 'shed source use' naming this machine", name)
	short := "this server takes your credential requests only from another machine of yours (source.machine_name); " +
		"to use this machine instead, run 'shed source use' naming this machine"
	writeRefusal(w, http.StatusForbidden, encodeRefusal(config.ErrSourceNotBound, msg, short, src))
}

// writeDuplicateStream answers the bound machine subscribing to a namespace
// it already holds.
func writeDuplicateStream(w http.ResponseWriter, namespace string) {
	msg := fmt.Sprintf("this machine already holds the %q subscription on this server: another process may be using this machine's id "+
		"(a copied state directory), or the previous stream has not ended yet; it is retried", namespace)
	short := "this machine already holds this subscription on this server; it is retried"
	writeRefusal(w, http.StatusConflict, encodeRefusal(config.ErrSourceDuplicateStream, msg, short, nil))
}

// writeAdmissionThrottled answers an admission refused by the user's rate
// limit: 429 with Retry-After in whole seconds.
func writeAdmissionThrottled(w http.ResponseWriter, retry time.Duration) {
	secs := retryAfterSeconds(retry)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	msg := fmt.Sprintf("too many new or changed machines for your user on this server lately; retry in %d s", secs)
	writeRefusal(w, http.StatusTooManyRequests, encodeRefusal(config.ErrSourceRateLimited, msg, msg, nil))
}

// retryAfterSeconds is d in whole seconds, rounded up, at least 1.
func retryAfterSeconds(d time.Duration) int {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

// missingIDLog remembers the callers whose request without machine headers
// has been logged, so a released broker retrying every few seconds is
// logged once (until the set fills and starts over).
type missingIDLog struct {
	mu   sync.Mutex
	seen map[string]bool
}

const maxMissingIDLogged = 1024

// logOnce logs a request with no valid machine headers from user's key, once
// per (user, key).
func (l *missingIDLog) logOnce(user, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := user + "\x00" + key
	if l.seen[k] {
		return
	}
	if l.seen == nil || len(l.seen) >= maxMissingIDLogged {
		l.seen = map[string]bool{}
	}
	l.seen[k] = true
	log.Printf("credential bus: refused a request with no valid machine id%s from key %q; install the current shed desktop app on that machine; logged once per key",
		forBusUser(user), key)
}

// forBusUser names a named user in a log line; the implicit user is not
// named.
func forBusUser(user string) string {
	if user == "" {
		return ""
	}
	return fmt.Sprintf(" for user %q", user)
}
