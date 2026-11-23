// Package refsource is the provision contract's reference source, for tests:
// a machine that holds a user's delivery stream on a shed-server and answers
// each request from canned items, or misbehaves on purpose (stays silent,
// answers late, answers twice, answers from a second connection or as
// another machine, or sends any refuse fixture of the contract).
//
// It is the fake machine of the transport tests and of the integration
// suite, run as bin/test/shed-provision-ref (make provision-ref). It is part
// of no release, and a test checks that no other main package imports it.
package refsource

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/delivery"
)

// Canned is what the source answers every request with: the answer's items
// and problems, in their wire form, sent as they are (nothing is checked, so
// a canned file can hold what the server must refuse). A nil list is sent
// as [].
type Canned struct {
	Items    json.RawMessage `json:"items,omitempty"`
	Problems json.RawMessage `json:"problems,omitempty"`
}

// ParseCanned reads a canned file: a JSON object with items and problems,
// both optional, and no other member.
func ParseCanned(raw []byte) (Canned, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Canned
	if err := dec.Decode(&c); err != nil {
		return Canned{}, fmt.Errorf("canned items: %w", err)
	}
	return c, nil
}

// Config is one reference source.
type Config struct {
	// Server is the shed-server's base URL (https://127.0.0.1:18443, or
	// http://127.0.0.1:8080 for a server in open mode on loopback).
	Server string
	// Client makes the stream and the answers. It carries the machine's
	// client certificate for an mtls server. nil is a plain client.
	Client *http.Client
	// AnswerClient, when set, makes the answers instead of Client: its own
	// connections and, for the other-certificate case, its own certificate.
	// The answers still name the stream's machine and stream ids (unless
	// AnswerMachineID or SecondConnection change them), so a server that
	// compares the certificate is what refuses them.
	AnswerClient *http.Client
	// MachineID and MachineName name the machine (32 lowercase hex digits;
	// 1 to 63 bytes of UTF-8).
	MachineID   string
	MachineName string
	// AnswerMachineID, when set, is the machine id the answers name instead
	// of MachineID: an answer from another machine.
	AnswerMachineID string

	// Canned is the answer's items and problems.
	Canned Canned
	// Refuse, when set, replaces every answer with this fixture's body.
	Refuse *AnswerFixture

	// Silent receives requests and answers none.
	Silent bool
	// Delay waits this long before answering.
	Delay time.Duration
	// Twice posts every answer a second time, after the first's response.
	Twice bool
	// SecondConnection posts answers as a second connection that did not
	// open the stream would: on connections of their own (AnswerClient's,
	// or a copy of Client's transport) and with a random stream id, not the
	// stream's.
	SecondConnection bool

	// Report, when set, is told every event.
	Report func(Event)
}

// Event is what the source reports: one JSON line each from the command.
type Event struct {
	// Kind is stream (a stream opened), refused (the stream was refused),
	// request (a request arrived), invalid_request (one that did not
	// parse; not answered), answer (an answer's response), dropped (a
	// request whose stream ended before its answer was sent) or end (the
	// stream ended).
	Kind    string `json:"event"`
	Stream  string `json:"stream,omitempty"`
	ID      string `json:"id,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Shed    string `json:"shed,omitempty"`
	User    string `json:"user,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Status  int    `json:"status,omitempty"`
	Code    string `json:"code,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RefusedError is a stream the server did not open: its status, code and
// Retry-After.
type RefusedError struct {
	Status     int
	Code       string
	RetryAfter time.Duration
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("stream refused: %d %s", e.Status, e.Code)
}

// Source is a configured reference source.
type Source struct {
	cfg     Config
	answers *http.Client
	// held is how long the last RunStream's stream stayed open (zero when
	// none opened). Run alone reads it, between streams.
	held time.Duration
}

// New checks cfg and returns its source.
func New(cfg Config) (*Source, error) {
	if cfg.Server == "" {
		return nil, errors.New("no server")
	}
	if _, err := url.Parse(cfg.Server); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	if !delivery.ValidID(cfg.MachineID) {
		return nil, errors.New("the machine id is 32 lowercase hex digits")
	}
	if cfg.MachineName == "" || len(cfg.MachineName) > 63 {
		return nil, errors.New("the machine name is 1 to 63 bytes")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	s := &Source{cfg: cfg, answers: cfg.Client}
	switch {
	case cfg.AnswerClient != nil:
		s.answers = cfg.AnswerClient
	case cfg.SecondConnection:
		s.answers = &http.Client{Transport: cloneTransport(cfg.Client.Transport)}
	}
	return s, nil
}

// cloneTransport is a transport with connections of its own.
func cloneTransport(rt http.RoundTripper) http.RoundTripper {
	if t, ok := rt.(*http.Transport); ok {
		return t.Clone()
	}
	if rt == nil {
		return http.DefaultTransport.(*http.Transport).Clone()
	}
	return rt
}

func (s *Source) report(e Event) {
	if s.cfg.Report != nil {
		s.cfg.Report(e)
	}
}

// The reconnect schedule, as CONTRACT.md gives it.
const (
	minBackoff  = time.Second
	maxBackoff  = 30 * time.Second
	resetHeldAt = 60 * time.Second
)

// nextBackoff is the wait before the next stream: back to the minimum
// after a stream that held resetHeldAt, else double the last wait (the
// minimum at first), at most maxBackoff; and never less than a
// Retry-After.
func nextBackoff(last, held, retryAfter time.Duration) time.Duration {
	wait := minBackoff
	if held < resetHeldAt && last > 0 {
		wait = min(last*2, maxBackoff)
	}
	return max(wait, retryAfter)
}

// Run holds a stream and opens a new one each time it ends, until ctx is
// done, waiting nextBackoff between streams. Requests of an ended stream
// are never answered on the next.
func (s *Source) Run(ctx context.Context) error {
	var wait time.Duration
	for {
		s.held = 0
		err := s.RunStream(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var retryAfter time.Duration
		var refused *RefusedError
		if errors.As(err, &refused) {
			retryAfter = refused.RetryAfter
		}
		wait = nextBackoff(wait, s.held, retryAfter)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// RunStream opens one stream and answers its requests until the stream
// ends (or ctx is done). An answer not yet sent when the stream ends is
// dropped, not sent: the server failed that request when its stream ended.
// It returns once every answer it started has its response.
func (s *Source) RunStream(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(delivery.StreamPath), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	s.machineHeaders(req.Header, s.cfg.MachineID)
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		s.report(Event{Kind: "refused", Error: err.Error()})
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		code, _ := errorCode(resp.Body)
		rerr := &RefusedError{Status: resp.StatusCode, Code: code}
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			rerr.RetryAfter = time.Duration(secs) * time.Second
		}
		s.report(Event{Kind: "refused", Status: resp.StatusCode, Code: code})
		return rerr
	}
	stream := resp.Header.Get(delivery.HeaderStream)
	s.report(Event{Kind: "stream", Stream: stream})
	opened := time.Now()

	err = readSSE(resp.Body, func(ev sseEvent) bool {
		if ev.Type != delivery.EventRequest {
			return true
		}
		r, refusal := delivery.ParseRequest([]byte(ev.Data))
		if refusal != nil {
			s.report(Event{Kind: "invalid_request", Stream: stream, Rule: string(refusal.Rule), Error: refusal.Error()})
			return true
		}
		s.report(Event{Kind: "request", Stream: stream, ID: r.ID, Reason: string(r.Reason), Shed: r.Shed.Name, User: r.User})
		if s.cfg.Silent {
			return true
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.answer(ctx, stream, r.ID)
		}()
		return true
	})
	// How long the stream itself held, for Run's backoff: from its 200 to
	// its end, not counting the connect or the answers still finishing.
	s.held = time.Since(opened)
	// The stream has ended: answers still waiting are dropped, answers on
	// their way finish, and only then is the end reported, so a report never
	// shows a stream's end before its own answers.
	cancel()
	wg.Wait()
	s.report(Event{Kind: "end", Stream: stream})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// answer waits Delay, then posts the answer to id (twice when Twice).
func (s *Source) answer(ctx context.Context, stream, id string) {
	if s.cfg.Delay > 0 {
		select {
		case <-ctx.Done():
			s.report(Event{Kind: "dropped", Stream: stream, ID: id})
			return
		case <-time.After(s.cfg.Delay):
		}
	}
	if ctx.Err() != nil {
		s.report(Event{Kind: "dropped", Stream: stream, ID: id})
		return
	}
	body, err := s.body(id)
	if err != nil {
		s.report(Event{Kind: "answer", Stream: stream, ID: id, Error: err.Error()})
		return
	}
	attempts := 1
	if s.cfg.Twice {
		attempts = 2
	}
	for a := 1; a <= attempts; a++ {
		ev := s.post(stream, id, body)
		ev.Attempt = a
		s.report(ev)
	}
}

// body is the answer to id: the refuse fixture's, or the canned items'.
func (s *Source) body(id string) ([]byte, error) {
	if s.cfg.Refuse != nil {
		return s.cfg.Refuse.Body(id)
	}
	items, problems := s.cfg.Canned.Items, s.cfg.Canned.Problems
	if len(items) == 0 {
		items = json.RawMessage("[]")
	}
	if len(problems) == 0 {
		problems = json.RawMessage("[]")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"version":%d,"id":%q,"items":`, delivery.Version, id)
	b.Write(items)
	b.WriteString(`,"problems":`)
	b.Write(problems)
	b.WriteString("}")
	return b.Bytes(), nil
}

// post sends one answer. It is not bound to the stream's context: an answer
// already on its way is sent whole, and the server judges it.
func (s *Source) post(stream, id string, body []byte) Event {
	ev := Event{Kind: "answer", Stream: stream, ID: id}
	client := s.answers
	if s.cfg.SecondConnection {
		stream = randomID()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url(delivery.AnswerPathPrefix+url.PathEscape(id)), bytes.NewReader(body))
	if err != nil {
		ev.Error = err.Error()
		return ev
	}
	req.Header.Set("Content-Type", "application/json")
	machine := s.cfg.MachineID
	if s.cfg.AnswerMachineID != "" {
		machine = s.cfg.AnswerMachineID
	}
	s.machineHeaders(req.Header, machine)
	req.Header.Set(delivery.HeaderStream, stream)
	resp, err := client.Do(req)
	if err != nil {
		ev.Error = err.Error()
		return ev
	}
	defer func() { _ = resp.Body.Close() }()
	ev.Status = resp.StatusCode
	if resp.StatusCode != http.StatusNoContent {
		ev.Code, ev.Rule = errorCode(resp.Body)
	}
	return ev
}

func (s *Source) url(path string) string {
	return strings.TrimSuffix(s.cfg.Server, "/") + path
}

func (s *Source) machineHeaders(h http.Header, machineID string) {
	h.Set(delivery.HeaderMachineID, machineID)
	h.Set(delivery.HeaderMachineName, url.PathEscape(s.cfg.MachineName))
}

// errorCode reads an error body's code and, for answer_refused, its rule.
func errorCode(r io.Reader) (code, rule string) {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Rule string `json:"rule"`
	}
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || json.Unmarshal(b, &body) != nil {
		return "", ""
	}
	return body.Error.Code, body.Rule
}

// randomID is 32 lowercase hex digits.
func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewMachineID is a fresh machine id.
func NewMachineID() string { return randomID() }
