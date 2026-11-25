package refserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// The hosted source (the provision contract, version 2, section 3b): the
// delivery scope and POST /v1/deliveries. The reference service answers
// from canned items, or is told to send any reply a server must classify
// (the contract's hosted/replies): silence, a late answer, a status and
// body, an oversize or truncated body, an answer that echoes the bearer, or
// any body at all (a refuse fixture). It parses only what it must to answer:
// the request's id (substituted into the canned JSON), its user and its
// deadline. It imports no shed package.

const (
	scopeDeliveries = "deliveries:request"
	// maxDeliveryRequest is the largest request body (section 3b).
	maxDeliveryRequest = 64 << 10
	// maxDeliveryAnswer is the answer limit (section 2), which oversize
	// passes by one byte.
	maxDeliveryAnswer = 6 << 20
	// fixtureRequestID is the id every answer fixture answers; a body sent
	// as it is has it replaced by the request's id.
	fixtureRequestID = "0123456789abcdef0123456789abcdef"
	// deliveryVersion is the provision contract's version this service
	// speaks.
	deliveryVersion = "2"
	// silentHold bounds how long a silent reply holds the connection past
	// the request's deadline, so a client that never gives up does not hold
	// a handler for ever.
	silentHold = time.Minute
	// DeliveryCallLine is logged once for every POST /v1/deliveries, before
	// anything of it is read (the integration suite counts it).
	DeliveryCallLine = "delivery call"
)

// The delivery modes (DeliveryInputs.Delivery).
const (
	// ModeAnswer (the empty mode) answers 200 with the canned items and
	// problems.
	ModeAnswer = ""
	// ModeSilent sends nothing until the client gives up (or a minute past
	// the deadline, when the connection is closed with no reply).
	ModeSilent = "silent"
	// ModeLate answers the canned items one second after the deadline.
	ModeLate = "late"
	// ModeStatus answers Status with Headers and Body (JSON) or Text.
	ModeStatus = "status"
	// ModeOversize answers 200 with a JSON body of 6 MiB and one byte.
	ModeOversize = "oversize"
	// ModeTruncated declares the canned answer's length, sends half of it
	// and closes the connection.
	ModeTruncated = "truncated"
	// ModeEcho answers 200 with a valid answer whose one env item's value is
	// the bearer token presented.
	ModeEcho = "echo"
	// ModeBody answers Status (200 when zero) with the bytes of BodyBase64,
	// the fixtures' request id replaced by the request's: any answer
	// fixture, refused ones included.
	ModeBody = "body"
)

// Delivery is what the service answers one person's delivery request with.
type Delivery struct {
	Mode string `json:"mode,omitempty"`
	// Items and Problems are the canned answer's lists (ModeAnswer,
	// ModeLate, ModeTruncated), JSON arrays sent as they are; absent is
	// empty.
	Items    json.RawMessage `json:"items,omitempty"`
	Problems json.RawMessage `json:"problems,omitempty"`
	// Status, Headers, Body and Text are ModeStatus's reply (Status is also
	// ModeBody's).
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Text    *string           `json:"text,omitempty"`
	// BodyBase64 is ModeBody's body.
	BodyBase64 string `json:"body_base64,omitempty"`
}

// DeliveryInputs is what an Inputs offers when the service delivers
// credentials. Inputs without it offer no delivery: the delivery scope is
// invalid_scope for every server.
type DeliveryInputs interface {
	// DeliveriesDenied reports whether the operator withdrew the server's
	// permission to ask for deliveries (every server may, until then). It
	// is read on every token request for the scope and every call.
	DeliveriesDenied(clientID string) (bool, error)
	// Delivery is the person's source: found is false for a person with no
	// source here (NO_SOURCE).
	Delivery(user string) (d Delivery, found bool, err error)
}

var (
	requestIDRx = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reasonsV2   = map[string]bool{"create": true, "provision": true, "start": true, "refresh": true}
)

// deliveryRequest is what the service reads of a request.
type deliveryRequest struct {
	id       string
	user     string
	deadline time.Time
}

// errNotARequest is every reason a body is not a request: one fixed
// classification, so nothing of the body (a member name, a value) reaches an
// error or a log line.
var errNotARequest = errors.New("the body is not a request of the provision contract's version 2")

// shedIDRx is a shed id: 1 to 64 lowercase letters and digits.
var shedIDRx = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// canonicalAbsolute reports whether p is an absolute, canonical path: / or
// slash-separated segments after one leading slash, with no empty, . or ..
// segment, no trailing slash and no backslash.
func canonicalAbsolute(p string) bool {
	if p == "/" {
		return true
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') {
		return false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// backendsV2 are the backends a request's shed may be on.
var backendsV2 = map[string]bool{"firecracker": true, "vz": true, "remote": true}

// boundedText reports whether s is 1 to max bytes with no control
// character.
func boundedText(s string, max int) bool {
	return s != "" && len(s) <= max && strings.IndexFunc(s, unicode.IsControl) < 0
}

// members decodes a JSON object into its members, and checks it names only
// allowed members and every one of required.
func members(raw json.RawMessage, allowed, required []string) (map[string]json.RawMessage, bool) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	for k := range m {
		if !containsString(allowed, k) {
			return nil, false
		}
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, false
		}
	}
	return m, true
}

// parseDeliveryRequest reads the request document as the contract's
// section 1 shapes it: one strict JSON object of the request's members,
// all present; version 2; a request id; one of the four reasons; a deadline
// in the contract's time form; a user of 1 to 64 bytes with no control
// character; and a shed object with its required name, id and backend and
// its optional repo {url, name?}, local_dir and project_dir, each of its
// type and under the contract's value rules (internal/delivery/request.go's,
// written again here: this package imports no shed package). Anything else
// is errNotARequest.
//
// It checks the document's structure and these value rules, and refuses an
// unpaired UTF-16 surrogate escape (which Go's decoder would repair) as the
// contract's parser does; it does not check every text rule of the
// contract's conventions. Its tests hold that every request it refuses is
// one the contract's parser refuses too.
func parseDeliveryRequest(b []byte) (deliveryRequest, error) {
	var none deliveryRequest
	if checkStrictJSON(b, 16) != nil || unpairedSurrogate(b) {
		return none, errNotARequest
	}
	top := []string{"version", "id", "reason", "deadline", "user", "shed"}
	raw, ok := members(b, top, top)
	if !ok || string(raw["version"]) != deliveryVersion {
		return none, errNotARequest
	}
	str := func(m map[string]json.RawMessage, name string) (string, bool) { return jsonString(m[name]) }
	id, ok := str(raw, "id")
	if !ok || !requestIDRx.MatchString(id) {
		return none, errNotARequest
	}
	if reason, ok := str(raw, "reason"); !ok || !reasonsV2[reason] {
		return none, errNotARequest
	}
	dl, ok := str(raw, "deadline")
	if !ok {
		return none, errNotARequest
	}
	deadline, err := time.Parse(time.RFC3339, dl)
	if err != nil || deadline.UTC().Format(timeLayout) != dl {
		return none, errNotARequest
	}
	user, ok := str(raw, "user")
	if !ok || !boundedText(user, 64) {
		return none, errNotARequest
	}
	shed, ok := members(raw["shed"], []string{"name", "id", "backend", "repo", "local_dir", "project_dir"}, []string{"name", "id", "backend"})
	if !ok {
		return none, errNotARequest
	}
	if name, ok := str(shed, "name"); !ok || !boundedText(name, 64) {
		return none, errNotARequest
	}
	if sid, ok := str(shed, "id"); !ok || !shedIDRx.MatchString(sid) {
		return none, errNotARequest
	}
	if backend, ok := str(shed, "backend"); !ok || !backendsV2[backend] {
		return none, errNotARequest
	}
	if v, present := shed["local_dir"]; present {
		t, ok := jsonString(v)
		if !ok || !boundedText(t, 255) || strings.Contains(t, "/") || t == "." || t == ".." {
			return none, errNotARequest
		}
	}
	if v, present := shed["project_dir"]; present {
		t, ok := jsonString(v)
		if !ok || !boundedText(t, 1024) || !canonicalAbsolute(t) {
			return none, errNotARequest
		}
	}
	if rv, present := shed["repo"]; present {
		repo, ok := members(rv, []string{"url", "name"}, []string{"url"})
		if !ok {
			return none, errNotARequest
		}
		if u, ok := str(repo, "url"); !ok || !boundedText(u, 2048) {
			return none, errNotARequest
		}
		if n, present := repo["name"]; present {
			t, ok := jsonString(n)
			owner, rest, cut := strings.Cut(t, "/")
			if !ok || !cut || owner == "" || rest == "" || strings.Contains(rest, "/") || !boundedText(t, 255) {
				return none, errNotARequest
			}
		}
	}
	return deliveryRequest{id: id, user: user, deadline: deadline}, nil
}

// handleDeliveries answers POST /v1/deliveries. It checks, in order: the
// token and its scope, the rate limit, the media type, the size and the
// request, the deadline, then one read of the server's record, its grant
// and the person's source, and answers as that source says.
func (s *Server) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	// One fixed line per call, before anything is checked: what a test
	// counts to show a server did, or did not, ask (nothing of the request
	// is in it).
	s.logf("%s", DeliveryCallLine)
	now := s.now()
	clientID, scopes, err := s.verifyBearer(r, now)
	if err != nil {
		s.logf("deliveries refused: %v", err)
		writeInvalidToken(w)
		return
	}
	if !containsString(scopes, scopeDeliveries) {
		writeV1Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "the token lacks deliveries:request", 0)
		return
	}
	if ok, ra := s.allow(limitDeliveries, clientID); !ok {
		writeV1Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many delivery requests", ra)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeV1Error(w, http.StatusBadRequest, "BAD_REQUEST", "the body is not application/json", 0)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDeliveryRequest+1))
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "BAD_REQUEST", "the body did not arrive whole", 0)
		return
	}
	if len(body) > maxDeliveryRequest {
		writeV1Error(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "the request is over 64 KiB", 0)
		return
	}
	req, err := parseDeliveryRequest(body)
	if err != nil {
		// A fixed classification: nothing of the body is logged.
		s.logf("deliveries refused: %v", errNotARequest)
		writeV1Error(w, http.StatusBadRequest, "BAD_REQUEST", "the body is not a request of the provision contract's version 2", 0)
		return
	}
	// The clock after the body arrived: a body finished after the deadline
	// is past it, whenever the request began.
	if !s.now().Before(req.deadline) {
		writeV1Error(w, http.StatusGone, "DEADLINE_PASSED", "the request's deadline has passed", 0)
		return
	}

	// Every reply from here is judged against the deadline once more, on
	// the clock read just before it is written (after the lookups below and
	// after a reply's body is built): past it, the reply is 410
	// DEADLINE_PASSED. Only the two faults late on purpose (silent, late)
	// write past it, on raw.
	raw := w
	w = &deadlineWriter{ResponseWriter: raw, past: func() bool { return !s.now().Before(req.deadline) }}

	// One read of the server's record, its grant and the person's source.
	unavailable := func(err error) {
		s.logf("deliveries: a lookup failed: %v", err)
		writeUnavailable(w)
	}
	disabled, err := s.inputs.Disabled(clientID)
	if err != nil {
		unavailable(err)
		return
	}
	s.mu.Lock()
	srv := s.servers[clientID]
	s.mu.Unlock()
	if srv == nil {
		s.logf("deliveries refused: no server has the token's client id")
		writeInvalidToken(w)
		return
	}
	if disabled {
		writeV1Error(w, http.StatusForbidden, "SERVER_DISABLED", "the server is disabled", 0)
		return
	}
	di, ok := s.inputs.(DeliveryInputs)
	if !ok {
		writeV1Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "this service delivers no credentials", 0)
		return
	}
	denied, err := di.DeliveriesDenied(clientID)
	if err != nil {
		unavailable(err)
		return
	}
	if denied {
		writeV1Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "this server may not ask for deliveries", 0)
		return
	}
	d, found, err := di.Delivery(req.user)
	if err != nil {
		unavailable(err)
		return
	}
	if !found {
		writeV1Error(w, http.StatusNotFound, "NO_SOURCE", "no source for this person here", 0)
		return
	}
	s.answerDelivery(w, raw, r, req, d)
}

// cannedAnswer is the answer to req from d's items and problems.
func cannedAnswer(req deliveryRequest, d Delivery) []byte {
	list := func(raw json.RawMessage) json.RawMessage {
		if len(bytes.TrimSpace(raw)) == 0 {
			return json.RawMessage("[]")
		}
		return raw
	}
	return []byte(`{"version":` + deliveryVersion + `,"id":"` + req.id + `","items":` + string(list(d.Items)) + `,"problems":` + string(list(d.Problems)) + `}`)
}

// answerDelivery sends d's reply to req: on w, which judges the deadline
// just before the reply is written, or, for the two faults late on purpose
// (silent, late: what a server must survive), on raw.
func (s *Server) answerDelivery(w, raw http.ResponseWriter, r *http.Request, req deliveryRequest, d Delivery) {
	switch d.Mode {
	case ModeAnswer:
		writeBody(w, http.StatusOK, cannedAnswer(req, d), true)
	case ModeLate:
		// On the wall clock: a late answer is late for the client.
		select {
		case <-time.After(max(time.Until(req.deadline.Add(time.Second)), 0)):
		case <-r.Context().Done():
			return
		}
		writeBody(raw, http.StatusOK, cannedAnswer(req, d), true)
	case ModeSilent:
		select {
		case <-r.Context().Done():
		case <-time.After(max(time.Until(req.deadline), 0) + silentHold):
		}
		panic(http.ErrAbortHandler) // close with no reply
	case ModeStatus:
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		for k, v := range d.Headers {
			h.Set(k, v)
		}
		status := d.Status
		if status == 0 {
			status = http.StatusOK
		}
		switch {
		case d.Text != nil:
			w.WriteHeader(status)
			_, _ = io.WriteString(w, *d.Text)
		case len(d.Body) > 0:
			if h.Get("Content-Type") == "" {
				h.Set("Content-Type", "application/json")
			}
			w.WriteHeader(status)
			w.Write(d.Body)
		default:
			w.WriteHeader(status)
		}
	case ModeOversize:
		head := []byte(`{"version":` + deliveryVersion + `,"id":"` + req.id + `","items":[],"problems":[],"pad":"`)
		tail := []byte(`"}`)
		pad := maxDeliveryAnswer + 1 - len(head) - len(tail)
		body := make([]byte, 0, maxDeliveryAnswer+1)
		body = append(append(append(body, head...), bytes.Repeat([]byte("a"), pad)...), tail...)
		writeBody(w, http.StatusOK, body, true)
	case ModeTruncated:
		body := cannedAnswer(req, d)
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		if dw, ok := w.(*deadlineWriter); ok && dw.gone {
			return // the 410 is whole
		}
		w.Write(body[:len(body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // the rest never comes
	case ModeEcho:
		_, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		item, _ := json.Marshal(map[string]string{
			"name": "echo", "provider": "echo", "kind": "env",
			"variable": "ECHO_TOKEN", "value": tok, "fingerprint": "echo",
		})
		writeBody(w, http.StatusOK, cannedAnswer(req, Delivery{Items: json.RawMessage("[" + string(item) + "]")}), true)
	case ModeBody:
		b, err := base64.StdEncoding.DecodeString(d.BodyBase64)
		if err != nil {
			s.logf("deliveries: body_base64 is not base64: %v", err)
			writeUnavailable(w)
			return
		}
		status := d.Status
		if status == 0 {
			status = http.StatusOK
		}
		writeBody(w, status, bytes.ReplaceAll(b, []byte(fixtureRequestID), []byte(req.id)), true)
	default:
		s.logf("deliveries: the mode %q is not one this service knows", d.Mode)
		writeUnavailable(w)
	}
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// decodeDelivery reads one person's entry of deliveries.json: the string
// NO_SOURCE (no source: found is false), or a Delivery object, strictly.
func decodeDelivery(raw json.RawMessage) (Delivery, bool, error) {
	if s, ok := jsonString(raw); ok {
		if s == "NO_SOURCE" {
			return Delivery{}, false, nil
		}
		return Delivery{}, false, fmt.Errorf("the string %q is not NO_SOURCE", s)
	}
	var d Delivery
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Delivery{}, false, err
	}
	switch d.Mode {
	case ModeAnswer, ModeSilent, ModeLate, ModeStatus, ModeOversize, ModeTruncated, ModeEcho, ModeBody:
	default:
		return Delivery{}, false, fmt.Errorf("the mode %q is not one this service knows", d.Mode)
	}
	for _, l := range []json.RawMessage{d.Items, d.Problems} {
		if t := bytes.TrimSpace(l); len(t) > 0 && t[0] != '[' {
			return Delivery{}, false, errors.New("items and problems are JSON arrays")
		}
	}
	if d.Mode == ModeStatus && (d.Status < 100 || d.Status > 599) {
		return Delivery{}, false, errors.New("a status mode needs a status from 100 to 599")
	}
	return d, true, nil
}

// deadlineWriter judges a reply against the request's deadline at the
// moment its status is written: past it, the reply becomes 410
// DEADLINE_PASSED (whatever headers were set for it are dropped) and
// what was meant to follow is discarded.
type deadlineWriter struct {
	http.ResponseWriter
	past    func() bool
	decided bool
	gone    bool
}

func (d *deadlineWriter) WriteHeader(status int) {
	if d.decided {
		return
	}
	d.decided = true
	if d.past() {
		d.gone = true
		h := d.Header()
		for k := range h {
			delete(h, k)
		}
		writeV1Error(d.ResponseWriter, http.StatusGone, "DEADLINE_PASSED", "the request's deadline has passed", 0)
		return
	}
	d.ResponseWriter.WriteHeader(status)
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	if !d.decided {
		d.WriteHeader(http.StatusOK)
	}
	if d.gone {
		return len(b), nil
	}
	return d.ResponseWriter.Write(b)
}

func (d *deadlineWriter) Flush() {
	if f, ok := d.ResponseWriter.(http.Flusher); ok && !d.gone {
		f.Flush()
	}
}
