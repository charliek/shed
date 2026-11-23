package delivery

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
)

// The Service on a fake pin: what Request decides, the binding an answer is
// judged against, the limits of the pending-request table, and Write against
// a fake agent. The transport over real handlers is internal/api's
// delivery_transport_test.go.

const (
	testMachine = "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b"
	testSerial  = "1a2b3c"
)

// fakeStream is a delivery stream that records what it is offered.
type fakeStream struct {
	id     string
	gen    uint64
	serial string
	done   chan struct{}
	once   sync.Once
	full   bool
	out    chan Outgoing
}

func newFakeStream() *fakeStream {
	return &fakeStream{id: newRequestID(), gen: 1, serial: testSerial, done: make(chan struct{}), out: make(chan Outgoing, 32)}
}

func (s *fakeStream) ID() string { return s.id }
func (s *fakeStream) Machine() SourceMachine {
	return SourceMachine{MachineID: testMachine, MachineName: "ada-laptop"}
}
func (s *fakeStream) Generation() uint64    { return s.gen }
func (s *fakeStream) Serial() string        { return s.serial }
func (s *fakeStream) Done() <-chan struct{} { return s.done }
func (s *fakeStream) Offer(o Outgoing) bool {
	if s.full || closed(s.done) {
		return false
	}
	s.out <- o
	return true
}
func (s *fakeStream) end() { s.once.Do(func() { close(s.done) }) }

// next is the next request offered on the stream.
func (s *fakeStream) next(t *testing.T) Outgoing {
	t.Helper()
	select {
	case o := <-s.out:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("no request was offered")
	}
	return Outgoing{}
}

// fakePin binds users to the test machine, at generation 1, and gives a
// user the stream set for them.
type fakePin struct {
	mu      sync.Mutex
	bound   map[string]bool
	streams map[string]*fakeStream
	gen     uint64
}

func newFakePin() *fakePin {
	return &fakePin{bound: map[string]bool{}, streams: map[string]*fakeStream{}, gen: 1}
}

func (p *fakePin) DeliveryBound(user string) (SourceMachine, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return SourceMachine{MachineID: testMachine, MachineName: "ada-laptop"}, p.bound[user]
}

func (p *fakePin) DeliveryStream(user string) (Stream, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.streams[user]
	if s == nil || closed(s.done) || s.gen != p.gen {
		return nil, false
	}
	return s, true
}

func (p *fakePin) DeliveryCurrent(user, machine string, gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bound[user] && machine == testMachine && gen == p.gen
}

// connect binds user and gives them an open stream.
func (p *fakePin) connect(user string) *fakeStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bound[user] = true
	s := newFakeStream()
	s.gen = p.gen
	p.streams[user] = s
	return s
}

func shedFor(name string) RequestShed {
	return RequestShed{Name: name, ID: "id" + strings.ReplaceAll(name, "-", ""), Backend: BackendFirecracker, ProjectDir: "/home/shed/" + name}
}

func params(user, shed string) Params {
	return Params{Reason: ReasonCreate, User: user, Shed: shedFor(shed)}
}

// requestAsync runs Request in the background.
func requestAsync(s *Service, p Params) <-chan Result {
	ch := make(chan Result, 1)
	go func() {
		r, err := s.Request(context.Background(), p)
		if err != nil {
			r = Result{Reason: StepReason("error: " + err.Error())}
		}
		ch <- r
	}()
	return ch
}

func resultOf(t *testing.T, ch <-chan Result) Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the request did not end")
	}
	return Result{}
}

func owner(st *fakeStream, user string) Answerer {
	return Answerer{User: user, Machine: testMachine, Serial: st.serial, Stream: st.id}
}

func answerBody(id, items string) []byte {
	return []byte(fmt.Sprintf(`{"version":1,"id":%q,"items":[%s],"problems":[]}`, id, items))
}

func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// TestRequestNotApplicable: a service client's shed, a script key's create,
// a snapshot restore and a user with no bound machine make no request.
func TestRequestNotApplicable(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Minute)
	for _, p := range []Params{
		{Reason: ReasonCreate, User: "ada", Shed: shedFor("a"), ServiceClient: true},
		{Reason: ReasonCreate, User: "ada", Shed: shedFor("a"), ScriptKey: true},
		{Reason: ReasonCreate, User: "ada", Shed: shedFor("a"), FromSnapshot: true},
		params("nobody", "a"),
	} {
		r, err := svc.Request(context.Background(), p)
		if err != nil || r.Outcome != OutcomeNotRequested || r.Reason != ReasonNotApplicable || r.Failed() || r.Source != nil {
			t.Errorf("Request(%+v) = %+v, %v; want not_applicable", p, r, err)
		}
	}
	select {
	case o := <-st.out:
		t.Fatalf("a not applicable delivery sent %s", o.ID)
	default:
	}
}

// TestRequestSourceOffline: a bound machine with no stream is source_offline
// at once, naming the machine.
func TestRequestSourceOffline(t *testing.T) {
	pin := newFakePin()
	pin.connect("ada").end()
	svc := NewService(pin, time.Hour)
	start := time.Now()
	r, err := svc.Request(context.Background(), params("ada", "a"))
	if err != nil || r.Outcome != OutcomeNoAnswer || r.Reason != ReasonSourceOffline || !r.Failed() ||
		r.Source == nil || r.Source.MachineID != testMachine {
		t.Fatalf("Request = %+v, %v; want source_offline", r, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("an offline source was waited for")
	}
}

// TestRequestAccepted: the request goes out on the stream as a valid request
// line; the owner's answer is claimed and judged; the bundle is the answer.
func TestRequestAccepted(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Minute)
	ch := requestAsync(svc, Params{Reason: ReasonProvision, User: "ada", Shed: shedFor("box")})
	o := st.next(t)
	req, refusal := ParseRequest(o.Line)
	if refusal != nil || req.ID != o.ID || req.Reason != ReasonProvision || req.User != "ada" || req.Shed.Name != "box" {
		t.Fatalf("offered %s (%v)", o.Line, refusal)
	}
	dl, _ := ParseTime(req.Deadline)
	if d := time.Until(dl); d < 58*time.Second || d > 62*time.Second {
		t.Fatalf("deadline %s is %s away, want about a minute", req.Deadline, d)
	}
	if !o.Live() {
		t.Fatal("a waiting request is not live")
	}
	c, err := svc.Claim(o.ID, owner(st, "ada"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Live() {
		t.Fatal("a claimed request is still live")
	}
	file := `{"name":"key","provider":"ssh","kind":"file","stage":1,"base":"home","path":".ssh/id_ed25519","mode":"0600","content":"a2V5","fingerprint":"f1"}`
	env := `{"name":"tok","provider":"gh","kind":"env","variable":"GH_TOKEN","value":"v","fingerprint":"f2"}`
	if r, err := c.Judge(answerBody(o.ID, file+","+env)); r != nil || err != nil {
		t.Fatal(r, err)
	}
	res := resultOf(t, ch)
	if res.Outcome != OutcomeAccepted || res.Reason != "" || res.Failed() || res.RequestID != o.ID || res.Source.MachineID != testMachine {
		t.Fatalf("result = %+v", res)
	}
	if items := res.Bundle.Items(); len(items) != 2 || items[0].Name != "key" || string(items[0].Content) != "key" || items[1].Value != "v" {
		t.Fatalf("bundle items = %+v", items)
	}
	if n := len(res.Bundle.StageItems(1)); n != 2 {
		t.Fatalf("stage 1 items = %d", n)
	}
	// The answer is remembered: a repeat is already_answered, from its owner.
	if _, err := svc.Claim(o.ID, owner(st, "ada")); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("a repeated answer = %v, want already_answered", err)
	}
}

// TestRequestRefused: an answer that breaks a rule ends the request,
// refused under the rule, and is remembered as answered.
func TestRequestRefused(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Minute)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	c, err := svc.Claim(o.ID, owner(st, "ada"))
	if err != nil {
		t.Fatal(err)
	}
	bad := `{"name":"k","provider":"p","kind":"file","stage":1,"base":"home","path":".ssh/authorized_keys","mode":"0600","content":"","fingerprint":"f"}`
	r, jerr := c.Judge(answerBody(o.ID, bad))
	if jerr != nil || r == nil || r.Rule != RuleRefusedTarget {
		t.Fatalf("judge = %v, want refused_target", r)
	}
	res := resultOf(t, ch)
	if res.Outcome != OutcomeRefused || res.Reason != ReasonAnswerRefused || res.Rule != RuleRefusedTarget || res.Bundle != nil {
		t.Fatalf("result = %+v", res)
	}
	if _, err := svc.Claim(o.ID, owner(st, "ada")); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("an answer after a refusal = %v, want already_answered", err)
	}
}

// TestClaimBinding: an answer is judged against the user, machine,
// certificate serial and stream the request went out on, in the contract's
// order, and a refused claim consumes nothing.
func TestClaimBinding(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	pin.connect("bob")
	svc := NewService(pin, time.Minute)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	good := owner(st, "ada")
	for _, tc := range []struct {
		name string
		id   string
		a    Answerer
		want error
	}{
		{"not an id", "XYZ", good, ErrUnknownRequest},
		{"never made", newRequestID(), good, ErrUnknownRequest},
		{"another user's", o.ID, Answerer{User: "bob", Machine: testMachine, Serial: testSerial, Stream: st.id}, ErrUnknownRequest},
		{"another machine", o.ID, Answerer{User: "ada", Machine: strings.Repeat("0", 32), Serial: testSerial, Stream: st.id}, ErrNotStreamOwner},
		{"another certificate", o.ID, Answerer{User: "ada", Machine: testMachine, Serial: "ffff", Stream: st.id}, ErrNotStreamOwner},
		{"open mode, no serial", o.ID, Answerer{User: "ada", Machine: testMachine, Serial: "", Stream: st.id}, ErrNotStreamOwner},
		{"another stream", o.ID, Answerer{User: "ada", Machine: testMachine, Serial: testSerial, Stream: newRequestID()}, ErrNotStreamOwner},
		{"no stream", o.ID, Answerer{User: "ada", Machine: testMachine, Serial: testSerial}, ErrNotStreamOwner},
	} {
		if _, err := svc.Claim(tc.id, tc.a); !errors.Is(err, tc.want) {
			t.Errorf("%s: Claim = %v, want %v", tc.name, err, tc.want)
		}
	}
	// Nothing was consumed: the owner's answer is accepted.
	c, err := svc.Claim(o.ID, good)
	if err != nil {
		t.Fatalf("the owner's answer after the refusals = %v", err)
	}
	// While it is being judged, a second answer is already_answered.
	if _, err := svc.Claim(o.ID, good); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("an answer during the judgement = %v", err)
	}
	// After acceptance, another stream is still not_stream_owner (403
	// before 409).
	if r, err := c.Judge(answerBody(o.ID, "")); r != nil || err != nil {
		t.Fatal("an empty answer was refused")
	}
	if _, err := svc.Claim(o.ID, Answerer{User: "ada", Machine: testMachine, Serial: testSerial, Stream: newRequestID()}); !errors.Is(err, ErrNotStreamOwner) {
		t.Fatalf("another stream after acceptance = %v", err)
	}
	if res := resultOf(t, ch); res.Outcome != OutcomeAccepted {
		t.Fatalf("result = %+v", res)
	}
	for _, err := range []error{ErrUnknownRequest, ErrNotStreamOwner, ErrAlreadyAnswered, ErrDeadlinePassed} {
		if _, _, ok := ClaimStatus(err); !ok {
			t.Errorf("ClaimStatus(%v) unknown", err)
		}
	}
	if st, code, _ := ClaimStatus(ErrDeadlinePassed); st != 410 || code != CodeDeadlinePassed {
		t.Errorf("deadline_passed = %d %s", st, code)
	}
}

// TestRequestTimeout: no answer by the deadline is source_timeout; a late
// answer is deadline_passed, then and on a repeat; nothing is sent once the
// request has ended.
func TestRequestTimeout(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Second)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	res := resultOf(t, ch)
	if res.Outcome != OutcomeNoAnswer || res.Reason != ReasonSourceTimeout {
		t.Fatalf("result = %+v, want source_timeout", res)
	}
	if o.Live() {
		t.Fatal("an expired request is live")
	}
	for range 2 {
		if _, err := svc.Claim(o.ID, owner(st, "ada")); !errors.Is(err, ErrDeadlinePassed) {
			t.Fatalf("a late answer = %v, want deadline_passed", err)
		}
	}
	// The slot is free again.
	ch = requestAsync(svc, params("ada", "box"))
	st.next(t)
	if res := resultOf(t, ch); res.Reason != ReasonSourceTimeout {
		t.Fatalf("second request = %+v", res)
	}
}

// TestRequestStreamEnds: a stream that ends fails its pending request at
// once (source_disconnected); an answer to it is then unknown_request.
func TestRequestStreamEnds(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Hour)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	start := time.Now()
	st.end()
	res := resultOf(t, ch)
	if res.Outcome != OutcomeNoAnswer || res.Reason != ReasonSourceDisconnected || time.Since(start) > 5*time.Second {
		t.Fatalf("result = %+v after %s, want source_disconnected at once", res, time.Since(start))
	}
	if _, err := svc.Claim(o.ID, owner(st, "ada")); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("an answer after the stream ended = %v, want unknown_request", err)
	}
}

// TestClaimAfterAMove: the binding moved (its generation changed) before its
// stream was seen to end: the answer is unknown_request, and the request
// fails when its stream ends.
func TestClaimAfterAMove(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Hour)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	pin.mu.Lock()
	pin.gen++
	pin.mu.Unlock()
	if o.Live() {
		t.Fatal("a request of a moved binding is live")
	}
	if _, err := svc.Claim(o.ID, owner(st, "ada")); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("an answer after a move = %v, want unknown_request", err)
	}
	st.end()
	if res := resultOf(t, ch); res.Reason != ReasonSourceDisconnected {
		t.Fatalf("result = %+v", res)
	}
}

// TestClaimedAnswerLosesToTheStreamsEnd: an answer is claimed and its body
// is still on its way when its stream ends: the request fails at once
// (source_disconnected, never the answer's bundle), and the body, when it
// comes, is refused as unknown_request and accepted nowhere.
func TestClaimedAnswerLosesToTheStreamsEnd(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Hour)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	c, err := svc.Claim(o.ID, owner(st, "ada"))
	if err != nil {
		t.Fatal(err)
	}
	st.end()
	if res := resultOf(t, ch); res.Outcome != OutcomeNoAnswer || res.Reason != ReasonSourceDisconnected || res.Bundle != nil {
		t.Fatalf("result = %+v, want source_disconnected", res)
	}
	if r, err := c.Judge(answerBody(o.ID, "")); r != nil || !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("the body after the stream ended = %v, %v; want unknown_request", r, err)
	}
	if err := c.Refuse(RuleNotJSON); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("Refuse after the end = %v", err)
	}
	if _, err := svc.Claim(o.ID, owner(st, "ada")); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("a repeated answer = %v, want unknown_request", err)
	}
}

// TestClaimedAnswerLosesToAMove: an answer is claimed and its body is on
// its way when the binding moves (its generation changes) before the
// stream is seen to end: the judgement refuses it (unknown_request) and the
// request is source_disconnected at once.
func TestClaimedAnswerLosesToAMove(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Hour)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	c, err := svc.Claim(o.ID, owner(st, "ada"))
	if err != nil {
		t.Fatal(err)
	}
	pin.mu.Lock()
	pin.gen++
	pin.mu.Unlock()
	if r, err := c.Judge(answerBody(o.ID, "")); r != nil || !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("the body after the move = %v, %v; want unknown_request", r, err)
	}
	if res := resultOf(t, ch); res.Outcome != OutcomeNoAnswer || res.Reason != ReasonSourceDisconnected || res.Bundle != nil {
		t.Fatalf("result = %+v, want source_disconnected", res)
	}
}

// TestClaimedAnswerOutlivesItsDeadline: an answer claimed before the
// deadline is judged after it, and is accepted.
func TestClaimedAnswerOutlivesItsDeadline(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Second)
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	c, err := svc.Claim(o.ID, owner(st, "ada"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	if r, err := c.Judge(answerBody(o.ID, "")); r != nil || err != nil {
		t.Fatalf("judge = %v, %v", r, err)
	}
	if res := resultOf(t, ch); res.Outcome != OutcomeAccepted {
		t.Fatalf("result = %+v", res)
	}
}

// TestRequestStreamRefusesOffer: a stream that takes no request fails the
// request at once.
func TestRequestStreamRefusesOffer(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	st.full = true
	svc := NewService(pin, time.Hour)
	r, err := svc.Request(context.Background(), params("ada", "box"))
	if err != nil || r.Reason != ReasonSourceDisconnected {
		t.Fatalf("Request = %+v, %v", r, err)
	}
	// Its slot is free.
	st.full = false
	ch := requestAsync(svc, params("ada", "box"))
	st.next(t)
	st.end()
	resultOf(t, ch)
}

// TestRequestContextEnds: the caller giving up drops the request.
func TestRequestContextEnds(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := svc.Request(ctx, params("ada", "box")); errc <- err }()
	o := st.next(t)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Request = %v", err)
	}
	if _, err := svc.Claim(o.ID, owner(st, "ada")); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("an answer after the caller gave up = %v", err)
	}
}

// TestRequestLimits: one waiting request per shed (a second is refused at
// once), and maxActivePerUser per user: one more waits for a slot, is sent
// when one frees, and times out (source_timeout) when none does.
func TestRequestLimits(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	pin.connect("bob")
	svc := NewService(pin, 2*time.Second)

	first := requestAsync(svc, params("ada", "s0"))
	o0 := st.next(t)
	if _, err := svc.Request(context.Background(), params("ada", "s0")); !errors.Is(err, ErrShedPending) {
		t.Fatalf("a second request for the shed = %v, want ErrShedPending", err)
	}
	var rest []<-chan Result
	for i := 1; i < maxActivePerUser; i++ {
		rest = append(rest, requestAsync(svc, params("ada", fmt.Sprintf("s%d", i))))
		st.next(t)
	}
	// Bob is not held back by ada.
	bobs := requestAsync(svc, params("bob", "b0"))
	pin.mu.Lock()
	bobStream := pin.streams["bob"]
	pin.mu.Unlock()
	bobStream.next(t)

	// The ninth waits: nothing is offered.
	ninth := requestAsync(svc, params("ada", "s9"))
	select {
	case o := <-st.out:
		t.Fatalf("the ninth request was sent (%s) with eight waiting", o.ID)
	case <-time.After(200 * time.Millisecond):
	}
	// A slot frees: the ninth is sent.
	c, err := svc.Claim(o0.ID, owner(st, "ada"))
	if err != nil {
		t.Fatal(err)
	}
	c.Judge(answerBody(o0.ID, ""))
	if r := resultOf(t, first); r.Outcome != OutcomeAccepted {
		t.Fatalf("first = %+v", r)
	}
	st.next(t)
	for _, ch := range append(rest, ninth, bobs) {
		if r := resultOf(t, ch); r.Reason != ReasonSourceTimeout {
			t.Fatalf("a waiting request ended %+v, want source_timeout", r)
		}
	}

	// With eight waiting and none freeing, a ninth times out unsent.
	setVar(t, &maxActivePerUser, 1)
	held := requestAsync(svc, params("ada", "h0"))
	st.next(t)
	blocked := requestAsync(svc, params("ada", "h1"))
	if r := resultOf(t, blocked); r.Reason != ReasonSourceTimeout || r.RequestID != "" {
		t.Fatalf("a request that never had a slot = %+v, want source_timeout unsent", r)
	}
	resultOf(t, held)
}

// TestRememberedOutcomes: an answered or expired outcome is kept at least
// RememberAnswered and then forgotten (unknown_request); at most
// maxRemembered are kept.
func TestRememberedOutcomes(t *testing.T) {
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	tb := newTable(func(string, string, uint64) bool { return true }, clock)
	add := func(id, shed string) *entry {
		e := &entry{id: id, user: "ada", machine: SourceMachine{MachineID: testMachine}, serial: testSerial, stream: "s", done: make(chan struct{}), shedID: shed, deadline: now.Add(time.Hour)}
		if _, err := tb.add(e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	a := Answerer{User: "ada", Machine: testMachine, Serial: testSerial, Stream: "s"}
	e := add(strings.Repeat("a", 32), "x")
	if _, err := tb.claim(e.id, a); err != nil {
		t.Fatal(err)
	}
	tb.judge(e, judged{rule: RuleNotJSON})
	mu.Lock()
	now = now.Add(RememberAnswered - time.Second)
	mu.Unlock()
	if _, err := tb.claim(e.id, a); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("just before %s = %v", RememberAnswered, err)
	}
	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()
	if _, err := tb.claim(e.id, a); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("after %s = %v, want forgotten", RememberAnswered, err)
	}

	setVar(t, &maxRemembered, 3)
	var ids []string
	for i := range 5 {
		e := add(fmt.Sprintf("%032x", i+1), fmt.Sprint(i))
		tb.endUnanswered(e, true)
		ids = append(ids, e.id)
	}
	if _, err := tb.claim(ids[0], a); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("the oldest beyond the cap = %v, want forgotten", err)
	}
	if _, err := tb.claim(ids[4], a); !errors.Is(err, ErrDeadlinePassed) {
		t.Fatalf("the newest = %v, want deadline_passed", err)
	}
}

// TestDeadlineAfter: a request's deadline is a whole second, never early.
func TestDeadlineAfter(t *testing.T) {
	base := time.Date(2026, 11, 23, 9, 0, 0, 400_000_000, time.UTC)
	if got := deadlineAfter(base, 30*time.Second); !got.Equal(time.Date(2026, 11, 23, 9, 0, 31, 0, time.UTC)) {
		t.Fatalf("deadlineAfter = %s", got)
	}
	whole := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	if got := deadlineAfter(whole, 30*time.Second); !got.Equal(whole.Add(30 * time.Second)) {
		t.Fatalf("deadlineAfter(whole) = %s", got)
	}
}

// TestRequestRefusesAnInvalidShed: a caller's params that make no valid
// request are an error, and nothing is sent.
func TestRequestRefusesAnInvalidShed(t *testing.T) {
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Minute)
	p := params("ada", "box")
	p.Shed.ID = "Not An Id"
	if _, err := svc.Request(context.Background(), p); err == nil {
		t.Fatal("an invalid shed id was sent")
	}
	select {
	case o := <-st.out:
		t.Fatalf("sent %s", o.ID)
	default:
	}
}

// --- Write ----------------------------------------------------------------

// fakeAgent is a shed's agent: its probe answers protocol, and Run answers
// with reply and exit code (or err), recording what it was given.
type fakeAgent struct {
	protocol int
	probeErr error
	reply    func(in *WriterInput) string
	code     int
	runErr   error

	probeHangs bool

	runs      int
	argv      []string
	stdin     []byte
	deadline  time.Duration
	maxOutput int
}

func (a *fakeAgent) Probe(ctx context.Context) (guest.AgentInfo, error) {
	if a.probeHangs {
		<-ctx.Done()
		return guest.AgentInfo{}, ctx.Err()
	}
	return guest.AgentInfo{Protocol: a.protocol}, a.probeErr
}

func (a *fakeAgent) Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer, maxOutput int) (int, error) {
	a.runs++
	a.argv = argv
	a.maxOutput = maxOutput
	a.stdin, _ = io.ReadAll(stdin)
	if dl, ok := ctx.Deadline(); ok {
		a.deadline = time.Until(dl)
	}
	if a.runErr != nil {
		return -1, a.runErr
	}
	in, refusal := ParseWriterInput(a.stdin, HomeDir)
	if refusal != nil {
		return WriterExitRefused, nil
	}
	if a.reply != nil {
		_, _ = io.WriteString(stdout, a.reply(in))
	}
	return a.code, nil
}

// okReply is a writer that writes everything.
func okReply(in *WriterInput) string {
	var b strings.Builder
	for _, f := range in.Files {
		fmt.Fprintf(&b, "{\"name\":%q,\"result\":\"ok\"}\n", f.Name)
	}
	for _, e := range in.Env {
		fmt.Fprintf(&b, "{\"name\":%q,\"result\":\"ok\"}\n", e.Name)
	}
	return b.String()
}

// bundleOf is an accepted bundle of items.
func bundleOf(items ...Item) *Bundle {
	return &Bundle{answer: &Answer{Version: 1, Items: items}}
}

func fileItem(name string, stage int, base Base, path string) Item {
	return Item{Name: name, Provider: "p", Kind: KindFile, Stage: stage, Base: base, Path: path, Mode: "0600", Content: []byte("c-" + name), Fingerprint: "f"}
}

func envItem(name, variable, value string) Item {
	return Item{Name: name, Provider: "p", Kind: KindEnv, Variable: variable, Value: value, Fingerprint: "f"}
}

func TestWrite(t *testing.T) {
	svc := NewService(newFakePin(), time.Minute)
	ctx := context.Background()
	full := bundleOf(
		fileItem("key", 1, BaseHome, ".ssh/id_ed25519"),
		envItem("tok", "GH_TOKEN", "v1"),
		fileItem("dotenv", 2, BaseProject, ".env"),
		fileItem("cfg", 1, BaseHome, ".config/x/c"),
	)

	t.Run("stage 1 written", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol, reply: okReply}
		r := svc.Write(ctx, a, 1, full, "/home/shed/box")
		if r.State != StepDone || r.Reason != "" || r.Failure != nil || fmt.Sprint(r.Items) != "[{key ok} {cfg ok} {tok ok}]" {
			t.Fatalf("result = %+v", r)
		}
		if strings.Join(a.argv, " ") != "/usr/local/bin/shed-agent apply-items" {
			t.Fatalf("argv = %v", a.argv)
		}
		if a.deadline <= 0 || a.deadline > writerTimeout {
			t.Fatalf("the writer ran with %s left, want at most %s", a.deadline, writerTimeout)
		}
		if a.maxOutput != maxWriterOutput {
			t.Fatalf("the writer's output bound = %d, want %d", a.maxOutput, maxWriterOutput)
		}
		in, refusal := ParseWriterInput(a.stdin, HomeDir)
		if refusal != nil || in.Stage != 1 || len(in.Files) != 2 || len(in.Env) != 1 || in.ProjectDir != "" {
			t.Fatalf("stdin = %+v (%v)", in, refusal)
		}
	})
	t.Run("stage 2 written", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol + 1, reply: okReply}
		r := svc.Write(ctx, a, 2, full, "/home/shed/box")
		if r.State != StepDone || fmt.Sprint(r.Items) != "[{dotenv ok}]" {
			t.Fatalf("result = %+v", r)
		}
		in, _ := ParseWriterInput(a.stdin, HomeDir)
		if in.ProjectDir != "/home/shed/box" {
			t.Fatalf("project dir = %q", in.ProjectDir)
		}
	})
	t.Run("stage 2 with no item asks nothing", func(t *testing.T) {
		a := &fakeAgent{probeErr: errors.New("must not probe")}
		r := svc.Write(ctx, a, 2, bundleOf(envItem("tok", "A", "v")), "/home/shed/box")
		if r.State != StepSkipped || r.Reason != ReasonNoItems || a.runs != 0 {
			t.Fatalf("result = %+v, runs %d", r, a.runs)
		}
	})
	t.Run("stage 1 with no item still runs, to remove the env file", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol, reply: okReply}
		r := svc.Write(ctx, a, 1, bundleOf(), "")
		if r.State != StepSkipped || r.Reason != ReasonNoItems || a.runs != 1 || len(r.Items) != 0 {
			t.Fatalf("result = %+v, runs %d", r, a.runs)
		}
	})
	t.Run("an env-only failure has no item", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol, code: WriterExitFailed}
		r := svc.Write(ctx, a, 1, bundleOf(), "")
		if r.State != StepFailed || r.Reason != ReasonWriteFailed || r.Failure == nil ||
			*r.Failure != (Failure{Reason: ReasonWriteFailed, Result: ResultIO}) {
			t.Fatalf("result = %+v", r)
		}
		a = &fakeAgent{protocol: guest.ApplyItemsProtocol, code: WriterExitFailed, reply: okReply}
		r = svc.Write(ctx, a, 1, bundleOf(fileItem("key", 1, BaseHome, "k")), "")
		if r.State != StepFailed || r.Failure.Item != "" || r.Failure.Result != ResultIO || fmt.Sprint(r.Items) != "[{key ok}]" {
			t.Fatalf("files written, env file not = %+v", r)
		}
	})
	t.Run("an old agent is image_too_old, never tried", func(t *testing.T) {
		for _, p := range []int{0, guest.ApplyItemsProtocol - 1} {
			a := &fakeAgent{protocol: p}
			r := svc.Write(ctx, a, 1, full, "/home/shed/box")
			if r.State != StepFailed || r.Reason != ReasonImageTooOld || a.runs != 0 || r.Failure.Reason != ReasonImageTooOld ||
				fmt.Sprint(r.Items) != "[{key not_written} {cfg not_written} {tok not_written}]" {
				t.Fatalf("protocol %d: result = %+v, runs %d", p, r, a.runs)
			}
			a = &fakeAgent{protocol: p}
			if r := svc.Write(ctx, a, 1, bundleOf(), ""); r.State != StepSkipped || r.Reason != ReasonNoItems || a.runs != 0 {
				t.Fatalf("protocol %d, no item: %+v", p, r)
			}
		}
	})
	t.Run("a probe that fails writes nothing", func(t *testing.T) {
		a := &fakeAgent{probeErr: errors.New("link down")}
		r := svc.Write(ctx, a, 1, full, "")
		if r.State != StepFailed || r.Reason != ReasonWriteFailed || a.runs != 0 || r.Failure.Item != "" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("a probe that never answers is bounded", func(t *testing.T) {
		setVar(t, &probeTimeout, 50*time.Millisecond)
		a := &fakeAgent{probeHangs: true}
		start := time.Now()
		r := svc.Write(ctx, a, 1, full, "")
		if r.State != StepFailed || a.runs != 0 || time.Since(start) > 5*time.Second {
			t.Fatalf("result = %+v after %s", r, time.Since(start))
		}
	})
	t.Run("the probe is inside the stage's bound", func(t *testing.T) {
		setVar(t, &writerTimeout, 50*time.Millisecond)
		a := &fakeAgent{probeHangs: true}
		start := time.Now()
		if r := svc.Write(ctx, a, 1, full, ""); r.State != StepFailed || time.Since(start) > 5*time.Second {
			t.Fatalf("result = %+v after %s", r, time.Since(start))
		}
	})
	t.Run("the writer's failure names its item", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol, code: WriterExitFailed, reply: func(*WriterInput) string {
			return `{"name":"key","result":"ok"}` + "\n" + `{"name":"cfg","result":"symlink"}` + "\n" + `{"name":"tok","result":"not_written"}` + "\n"
		}}
		r := svc.Write(ctx, a, 1, full, "")
		if r.State != StepFailed || *r.Failure != (Failure{Reason: ReasonWriteFailed, Item: "cfg", Result: ResultSymlink}) ||
			fmt.Sprint(r.Items) != "[{key ok} {cfg symlink} {tok not_written}]" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("nothing the writer says is trusted", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol, code: WriterExitOK, reply: func(*WriterInput) string {
			return strings.Join([]string{
				`{"name":"other","result":"ok"}`,           // not sent
				`{"name":"key","result":"pwned"}`,          // not a result
				`{"name":"key","result":"ok","extra":"x"}`, // an unknown member
				`{"name":"key","result":"ok"} trailing`,    // trailing text
				`not json`,
				`{"name":"key"}`,                 // no result
				`{"name":"cfg","result":"ok"}`,   // good
				`{"name":"cfg","result":"io"}`,   // a second line for cfg: ignored
				`["key","ok"]`,                   // not an object
				`{"name":"tok","result":null}`,   // a null
				`{"name":"tok","result":"ok"}  `, // good, trailing space
			}, "\n")
		}}
		r := svc.Write(ctx, a, 1, full, "")
		if r.State != StepFailed || fmt.Sprint(r.Items) != "[{key io} {cfg ok} {tok ok}]" ||
			*r.Failure != (Failure{Reason: ReasonWriteFailed, Item: "key", Result: ResultIO}) {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("exit 0 with a failed line is a failure", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol, code: WriterExitOK, reply: func(in *WriterInput) string {
			return strings.Replace(okReply(in), `"tok","result":"ok"`, `"tok","result":"permission"`, 1)
		}}
		r := svc.Write(ctx, a, 1, full, "")
		if r.State != StepFailed || r.Failure.Item != "tok" || r.Failure.Result != ResultPermission {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("only 64 KiB of output is read", func(t *testing.T) {
		a := &fakeAgent{protocol: guest.ApplyItemsProtocol, code: WriterExitOK, reply: func(in *WriterInput) string {
			return strings.Repeat(" ", maxWriterOutput) + "\n" + okReply(in)
		}}
		r := svc.Write(ctx, a, 1, full, "")
		if r.State != StepFailed || fmt.Sprint(r.Items) != "[{key io} {cfg io} {tok io}]" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("a refused input, an unknown exit and a broken exec are failures", func(t *testing.T) {
		for _, tc := range []struct {
			a           *fakeAgent
			items, item string
		}{
			{&fakeAgent{protocol: guest.ApplyItemsProtocol, code: WriterExitRefused}, "[{key io} {cfg io} {tok io}]", "key"},
			{&fakeAgent{protocol: guest.ApplyItemsProtocol, code: 127, reply: okReply}, "[{key ok} {cfg ok} {tok ok}]", ""},
			{&fakeAgent{protocol: guest.ApplyItemsProtocol, runErr: errors.New("link broke")}, "[{key io} {cfg io} {tok io}]", "key"},
		} {
			r := svc.Write(ctx, tc.a, 1, full, "")
			if r.State != StepFailed || r.Reason != ReasonWriteFailed || fmt.Sprint(r.Items) != tc.items ||
				*r.Failure != (Failure{Reason: ReasonWriteFailed, Item: tc.item, Result: ResultIO}) {
				t.Fatalf("result = %+v", r)
			}
		}
	})
}

// TestBundlePrintsNoValue: however a bundle is formatted, it shows counts.
func TestBundlePrintsNoValue(t *testing.T) {
	b := bundleOf(envItem("tok", "A", "SENTINEL-VALUE"), fileItem("k", 1, BaseHome, "k"))
	r := Result{Outcome: OutcomeAccepted, Bundle: b}
	for _, s := range []string{fmt.Sprint(b), fmt.Sprintf("%v %+v %#v %s", b, b, b, b), fmt.Sprintf("%v %+v %#v", r, r, r), r.describe()} {
		if strings.Contains(s, "SENTINEL") || strings.Contains(s, "c-k") {
			t.Fatalf("a bundle printed a value: %s", s)
		}
	}
}

// TestSentinelValuesNeverLeak sends sentinel values through Request (an
// accepted answer, a refused answer whose bad item holds one) and Write
// (a writer that echoes the values back as malformed lines, and one that
// fails), and requires that none of them appears in the log, in an error, in
// a refusal, or in a result as it prints.
func TestSentinelValuesNeverLeak(t *testing.T) {
	const sentinel = "SENTINEL-7f3a9c"
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	var texts []string
	pin := newFakePin()
	st := pin.connect("ada")
	svc := NewService(pin, time.Minute)
	content := base64.StdEncoding.EncodeToString([]byte(sentinel + "-file"))

	// Accepted.
	ch := requestAsync(svc, params("ada", "box"))
	o := st.next(t)
	c, err := svc.Claim(o.ID, owner(st, "ada"))
	if err != nil {
		t.Fatal(err)
	}
	items := fmt.Sprintf(`{"name":"key","provider":"p","kind":"file","stage":1,"base":"home","path":"k","mode":"0600","content":%q,"fingerprint":"f"},`+
		`{"name":"tok","provider":"p","kind":"env","variable":"TOK","value":"%s-env","fingerprint":"f"},`+
		`{"name":"dot","provider":"p","kind":"file","stage":2,"base":"project","path":"e","mode":"0600","content":%q,"fingerprint":"f"}`, content, sentinel, content)
	if r, err := c.Judge(answerBody(o.ID, items)); r != nil || err != nil {
		t.Fatal(r, err)
	}
	res := resultOf(t, ch)
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("accepted = %+v", res)
	}
	texts = append(texts, fmt.Sprintf("%v %+v %#v", res, res, res), res.describe())

	// Refused: the sentinel in a value with a trailing space, and in a
	// document that does not parse.
	for _, bad := range []string{
		fmt.Sprintf(`{"name":"tok","provider":"p","kind":"env","variable":"TOK","value":"%s ","fingerprint":"f"}`, sentinel),
		fmt.Sprintf(`{"name":"tok","provider":"p","kind":"env","variable":"PATH","value":"%s","fingerprint":"f"}`, sentinel),
	} {
		ch := requestAsync(svc, params("ada", "box"))
		o := st.next(t)
		c, err := svc.Claim(o.ID, owner(st, "ada"))
		if err != nil {
			t.Fatal(err)
		}
		r, _ := c.Judge(answerBody(o.ID, bad))
		if r == nil {
			t.Fatal("a bad answer was accepted")
		}
		texts = append(texts, r.Error(), string(r.Rule), r.Where)
		rr := resultOf(t, ch)
		texts = append(texts, fmt.Sprintf("%+v", rr), rr.describe())
	}
	ch = requestAsync(svc, params("ada", "box"))
	o = st.next(t)
	c, _ = svc.Claim(o.ID, owner(st, "ada"))
	r, _ := c.Judge([]byte(`{"version":1,"id":"` + o.ID + `","items":[{"value":"` + sentinel + `"`))
	texts = append(texts, r.Error())
	texts = append(texts, fmt.Sprintf("%+v", resultOf(t, ch)))

	// Write: a writer that echoes every value back, as lines that are not
	// its protocol and as names it was not sent; one that fails; one whose
	// exec errors with the value in its error.
	echo := func(in *WriterInput) string {
		var b strings.Builder
		for _, f := range in.Files {
			fmt.Fprintf(&b, "{\"name\":%q,\"result\":%q}\n%s\n", f.Name, string(f.Content), f.Content)
		}
		for _, e := range in.Env {
			fmt.Fprintf(&b, "{\"name\":%q,\"result\":\"ok\"}\n{\"name\":%q,\"result\":\"ok\"}\n", e.Name, e.Value)
		}
		return b.String()
	}
	for _, a := range []*fakeAgent{
		{protocol: guest.ApplyItemsProtocol, reply: echo, code: WriterExitOK},
		{protocol: guest.ApplyItemsProtocol, reply: echo, code: WriterExitFailed},
		{protocol: guest.ApplyItemsProtocol, runErr: errors.New("exec failed: " + sentinel)},
		{probeErr: errors.New("unexpected response namespace: " + sentinel)},
		{protocol: guest.ApplyItemsProtocol, reply: func(in *WriterInput) string {
			return strings.Repeat(sentinel, maxWriterOutput/len(sentinel)+1)
		}},
		{protocol: 1},
	} {
		for _, stage := range []int{1, 2} {
			sr := svc.Write(context.Background(), a, stage, res.Bundle, "/home/shed/box")
			texts = append(texts, fmt.Sprintf("%+v %#v", sr, sr))
			if sr.Failure != nil {
				texts = append(texts, fmt.Sprintf("%+v", *sr.Failure))
			}
		}
	}

	texts = append(texts, logged.String())
	for _, s := range texts {
		if strings.Contains(s, sentinel) || strings.Contains(s, content) {
			t.Fatalf("a delivered value leaked:\n%s", s)
		}
	}
	if !strings.Contains(logged.String(), "delivery: request") {
		t.Fatal("test set-up: nothing was logged")
	}
}
