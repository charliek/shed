package roostprovider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// roost's op names (`roost_ipc::messages::ops`). Three, and no more: the
// provider gates on identify, reads projects, and opens a tab. Everything
// interactive on roost's wire is lease-gated (`session.connect` mints one, and
// holding it would depose whoever already had it), and the provider deliberately
// never takes a lease — `tab.open` is mutating but NOT lease-gated, which is
// what makes a leaseless provider possible at all.
const (
	opSessionIdentify = "session.identify"
	opTabList         = "tab.list"
	opTabOpen         = "tab.open"
)

// SpokenProtocol is the roost session protocol this build speaks —
// `roost_ipc::messages::SESSION_PROTOCOL_VERSION` at the rev crates/Cargo.toml
// pins. It appears in the protocol-mismatch row's copy ("… this shed speaks
// 4"), and it is the gate `Remote.Identify` applies.
//
// Hand-carried into Go the same way the exec chain is, and pinned the same way:
// the Rust twin test asserts this number against the real constant through
// crates/fixtures/roost-vectors/session.identify.response.v4.json, whose
// filename generation is the version. A roost-ipc bump that moves the protocol
// renames that vector, which breaks the twin test, which is the signal to
// change this.
const SpokenProtocol = 4

// wireRequestID is the correlation id every request carries.
//
// A constant, not a counter, because each request gets its OWN connection: the
// provider writes one request line, closes stdin so the far side sees EOF and
// exits on its own, and reads until the answer. That is roost's own
// `call_over` shape (roost-ipc/src/ssh.rs), which also hardcodes 1.
//
// It is a STRING on the wire. roost's envelope serializes every int64 id
// through `string_int64`, so `{"id":1,…}` — the obvious Go spelling — is a
// shape roost's own client never emits.
const wireRequestID = "1"

// maxWireBytes caps everything read while waiting for one answer, matching the
// order of roost's own `IpcClient` line cap (16 MiB). A `tab.list` from a
// session with many projects is the only response here that is not tiny; the
// cap is what keeps a far side that answers with an unterminated stream — or
// with an endless run of event frames — from being an unbounded allocation on
// this side.
const maxWireBytes = 16 << 20

// wireRequest is the request envelope (`roost_ipc::messages::RawRequest`),
// which is `deny_unknown_fields` on roost's side — these three keys, no others.
type wireRequest struct {
	ID     string `json:"id"`
	Op     string `json:"op"`
	Params any    `json:"params"`
}

// emptyParams is the params value for an op that takes none.
//
// `{}`, never `null`: `RawRequest.params` defaults to an empty object when a
// client omits the field, so `{}` is exactly "omitted", while `null` is a value
// that each op's own `deny_unknown_fields` struct would have to accept — and
// `SessionIdentifyParams` does not. An empty struct marshals to `{}`.
type emptyParams struct{}

// encodeRequest renders one request line, newline included.
//
// HTML escaping is turned OFF. Go's default would render a cwd containing `&`
// as `&`, which is valid JSON and decodes identically — but the request
// line is a contract asserted byte-for-byte against roost's own serde output in
// wire_test.go, and serde emits the character. Matching it means the two sides'
// wire logs are diffable rather than merely equivalent.
func encodeRequest(op string, params any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(wireRequest{ID: wireRequestID, Op: op, Params: params}); err != nil {
		return nil, err
	}
	// Encoder.Encode already appends the newline that frames the line.
	return buf.Bytes(), nil
}

// ResponseError is the `ok:false` body: a kebab-case stable code and a human
// message.
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// wireResponse is the response envelope. `id` is decoded as raw JSON rather
// than into a string field so that a frame carrying a NON-string id is a
// mismatch to be reported (with the offending value quoted back) instead of a
// whole-envelope decode failure — see idMatches.
type wireResponse struct {
	ID     json.RawMessage `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *ResponseError  `json:"error"`
}

// idMatches reports whether a response's raw `id` is the JSON STRING this
// connection's one request carried.
//
// Type-safe on purpose. roost serializes every envelope id through
// `string_int64`, so `"1"` is the only shape roost's own session emits; a bare
// `1` is a different type from a different speaker, and treating the two as
// equal is how a frame that is not our reply gets decoded as one. Anything that
// is not a JSON string — a number, a float, null, an object — fails here, and
// a mismatched id is a hard error rather than a skipped frame: on a connection
// that carries exactly one request, an answer to a different one means the
// stream is not what it claims to be.
func idMatches(raw json.RawMessage) bool {
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return false
	}
	return id == wireRequestID
}

// readReply reads response lines until the one answering wireRequestID.
//
// **Event frames are skipped.** A roost session pushes `{"event":…}` frames at
// a connection that never subscribed — a tab opening elsewhere, a driver
// change — and they are not this request's answer. roost's own client does the
// same thing at the same place (`IpcClient::call_raw`, `ssh::call_over`); a
// reader that did not would mistake the first event for a malformed response
// and fail every call made while anything else was happening on the far side.
//
// A clean EOF before the answer is an error naming the op: that is what the far
// side closing without answering looks like, and it is how a bridge that
// started and immediately died reads from here.
func readReply(r io.Reader, op string) (*wireResponse, error) {
	br := bufio.NewReader(io.LimitReader(r, maxWireBytes))
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) == 0 {
			if err != nil {
				return nil, fmt.Errorf("the far side closed without answering %s", op)
			}
			// A blank keepalive line is not a frame. Nothing in roost emits
			// one, but a line-oriented reader that treats one as EOF would be
			// wrong in a way that only shows up in production.
			continue
		}

		// A frame carrying an `event` key is a push, not an answer. Decoded
		// into a one-field probe struct rather than a full map: the interesting
		// frame here is the response, and decoding every event's whole payload
		// twice would be the only cost of doing it the obvious way.
		var probe struct {
			Event *json.RawMessage `json:"event"`
		}
		if jsonErr := json.Unmarshal(line, &probe); jsonErr != nil {
			return nil, fmt.Errorf("far side sent a line that is not JSON while answering %s: %w", op, jsonErr)
		}
		if probe.Event != nil {
			if err != nil {
				return nil, fmt.Errorf("the far side closed without answering %s", op)
			}
			continue
		}

		var resp wireResponse
		if jsonErr := json.Unmarshal(line, &resp); jsonErr != nil {
			return nil, fmt.Errorf("far side sent a malformed response to %s: %w", op, jsonErr)
		}
		if !idMatches(resp.ID) {
			return nil, fmt.Errorf("answer to %s carried id %s, not the string %q", op, resp.ID, wireRequestID)
		}
		return &resp, nil
	}
}

// result unwraps a response into its `result` payload, turning an `ok:false`
// envelope into an error.
func (r *wireResponse) result() (json.RawMessage, error) {
	if r.OK {
		return r.Result, nil
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return nil, fmt.Errorf("ok=false with no error body (internal)")
}

// IdentifyResult is the subset of `session.identify`'s reply this package
// reads. roost's reply carries app_version, payload_kinds, features, a
// libghostty build string, a session id and a start time as well; the provider
// gates on exactly one number and has no use for the rest.
type IdentifyResult struct {
	SessionProtocol int `json:"session_protocol"`
}

// Project is one far-side project, as `tab.list` reports it. `id` is a string
// on the wire (string_int64) and is carried as one all the way to `tab.open`'s
// `project_id`, so it is never parsed into a number here — parsing it would
// add a failure mode and gain nothing.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}

// TabListResult is the subset of `tab.list`'s reply this package reads.
// `tab.list` returns EVERY project including tab-less ones, which is what makes
// it the right source for the workdir step.
type TabListResult struct {
	Projects []Project `json:"projects"`
}

// TabOpenParams are the `tab.open` params for a completed token.
//
// `project_id` is a JSON STRING (roost serializes every int64 id that way), and
// `"0"` is roost's "no project named" sentinel: it reuses the session's first
// existing project, or creates `Default` seeded with cwd. Either is correct
// here because cwd is explicit and absolute — the project only decides which
// group the tab is filed under, not where it starts.
//
// The params struct on roost's side is `deny_unknown_fields`, so this must
// carry these keys and no others; wire_test.go pins the exact request JSON.
type TabOpenParams struct {
	ProjectID string   `json:"project_id"`
	Cwd       string   `json:"cwd"`
	Title     string   `json:"title"`
	Argv      []string `json:"argv"`
}

// TabOpenResult is the subset of `tab.open`'s reply this package reads: the new
// tab's id, for the confirmation line.
type TabOpenResult struct {
	Tab struct {
		ID string `json:"id"`
	} `json:"tab"`
}
