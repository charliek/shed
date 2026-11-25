// Package deliverytest has test doubles for delivery: a source (a pin with
// one bound machine per user, whose stream answers each request from a
// canned answer, or does not), and an agent whose writer keeps what it is
// given in memory. They let a backend's or the API's tests run a real
// delivery.Service and delivery.Run end to end without a machine or a VM.
package deliverytest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/guest"
)

// Machine is the bound machine every user of a Source has.
var Machine = delivery.SourceMachine{MachineID: "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b", MachineName: "ada-laptop"}

// Mode is how a user's source behaves.
type Mode int

const (
	// Unbound: the user has no bound machine (not_applicable).
	Unbound Mode = iota
	// Offline: bound, with no stream (source_offline).
	Offline
	// Answer: bound and connected; each request is answered with the
	// user's answer.
	Answer
	// Silent: bound and connected; requests are never answered
	// (source_timeout at the service's timeout).
	Silent
)

// Source is a fake pin and stream. Create it, build the service over it,
// then Attach the service.
type Source struct {
	mu      sync.Mutex
	svc     *delivery.Service
	modes   map[string]Mode
	answers map[string]func(req *delivery.Request) []byte
	streams map[string]*stream
	gates   map[string]<-chan struct{}
	// Requests is every request offered, in order.
	requests []*delivery.Request
}

// SetGate makes user's answers wait until gate is closed (nil: no wait).
func (s *Source) SetGate(user string, gate <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gates == nil {
		s.gates = map[string]<-chan struct{}{}
	}
	s.gates[user] = gate
}

// NewSource returns a source on which every user is unbound.
func NewSource() *Source {
	return &Source{modes: map[string]Mode{}, answers: map[string]func(*delivery.Request) []byte{}, streams: map[string]*stream{}}
}

// Attach gives the source the service whose requests it answers.
func (s *Source) Attach(svc *delivery.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.svc = svc
}

// Set sets user's mode, and for Answer the answer body made from each
// request (nil answers with no items).
func (s *Source) Set(user string, m Mode, answer func(req *delivery.Request) []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modes[user] = m
	if answer == nil {
		answer = func(req *delivery.Request) []byte { return EmptyAnswer(req.ID) }
	}
	s.answers[user] = answer
	if old := s.streams[user]; old != nil {
		old.end()
		delete(s.streams, user)
	}
	if m == Answer || m == Silent {
		s.streams[user] = &stream{src: s, user: user, id: "0123456789abcdef0123456789abcdef", done: make(chan struct{})}
	}
}

// Requests is every request offered so far.
func (s *Source) Requests() []*delivery.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*delivery.Request(nil), s.requests...)
}

// DeliveryBound implements delivery.Pin.
func (s *Source) DeliveryBound(user string) (delivery.SourceMachine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Machine, s.modes[user] != Unbound
}

// DeliveryStream implements delivery.Pin.
func (s *Source) DeliveryStream(user string) (delivery.Stream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[user]
	if st == nil {
		return nil, false
	}
	return st, true
}

// DeliveryCurrent implements delivery.Pin.
func (s *Source) DeliveryCurrent(user, machine string, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modes[user] != Unbound && machine == Machine.MachineID && gen == 1
}

type stream struct {
	src  *Source
	user string
	id   string
	done chan struct{}
	once sync.Once
}

func (st *stream) ID() string                      { return st.id }
func (st *stream) Machine() delivery.SourceMachine { return Machine }
func (st *stream) Generation() uint64              { return 1 }
func (st *stream) Serial() string                  { return "" }
func (st *stream) Done() <-chan struct{}           { return st.done }
func (st *stream) end()                            { st.once.Do(func() { close(st.done) }) }

func (st *stream) Offer(o delivery.Outgoing) bool {
	req, refusal := delivery.ParseRequest(o.Line)
	if refusal != nil {
		return false
	}
	s := st.src
	s.mu.Lock()
	s.requests = append(s.requests, req)
	mode, answer, svc, gate := s.modes[st.user], s.answers[st.user], s.svc, s.gates[st.user]
	s.mu.Unlock()
	if mode != Answer || svc == nil {
		return true
	}
	go func() {
		if gate != nil {
			<-gate
		}
		// The answer goes after the request is in the table's hands.
		time.Sleep(time.Millisecond)
		c, err := svc.Claim(o.ID, delivery.Answerer{User: st.user, Machine: Machine.MachineID, Stream: st.id})
		if err != nil {
			return
		}
		_, _ = c.Judge(answer(req))
	}()
	return true
}

// EmptyAnswer is an answer with no items and no problems.
func EmptyAnswer(id string) []byte {
	return []byte(fmt.Sprintf(`{"version":2,"id":%q,"items":[],"problems":[]}`, id))
}

// FileItem is a file item for an answer.
type FileItem struct {
	Name, Provider string
	Stage          int
	Base           delivery.Base
	Path, Mode     string
	Content        []byte
	ExpiresAt      string
}

// EnvItem is an env item for an answer.
type EnvItem struct {
	Name, Provider, Variable, Value string
	ExpiresAt                       string
}

// AnswerOf is an answer to id holding files and envs, in that order, with
// problems.
func AnswerOf(id string, files []FileItem, envs []EnvItem, problems []delivery.Problem) []byte {
	items := []map[string]any{}
	for _, f := range files {
		m := map[string]any{
			"name": f.Name, "provider": f.Provider, "kind": "file", "stage": f.Stage, "base": string(f.Base),
			"path": f.Path, "mode": f.Mode, "content": base64.StdEncoding.EncodeToString(f.Content),
			"fingerprint": "fp-" + f.Name,
		}
		if f.ExpiresAt != "" {
			m["expires_at"] = f.ExpiresAt
		}
		items = append(items, m)
	}
	for _, e := range envs {
		m := map[string]any{
			"name": e.Name, "provider": e.Provider, "kind": "env", "variable": e.Variable, "value": e.Value,
			"fingerprint": "fp-" + e.Name,
		}
		if e.ExpiresAt != "" {
			m["expires_at"] = e.ExpiresAt
		}
		items = append(items, m)
	}
	if problems == nil {
		problems = []delivery.Problem{}
	}
	b, err := json.Marshal(map[string]any{"version": delivery.Version, "id": id, "items": items, "problems": problems})
	if err != nil {
		panic(err)
	}
	return b
}

// WriterMode is how an Agent's writer answers.
type WriterMode int

const (
	// WriteOK writes every item and reports each ok.
	WriteOK WriterMode = iota
	// WriteFailAt fails at the item named FailItem (is_directory); the
	// items after it are not_written.
	WriteFailAt
	// WriteMalformed answers garbage that echoes the input back, and
	// exits 0.
	WriteMalformed
)

// Agent is a fake shed agent: its probe reports Protocol, and its writer
// keeps each stage's input in Writes (the shed's "disk", in memory).
type Agent struct {
	mu       sync.Mutex
	Protocol int
	Mode     WriterMode
	FailItem string
	// ProbeErr, when set, fails the probe.
	ProbeErr error
	writes   []delivery.WriterInput
}

// NewAgent returns an agent that takes items and writes them all.
func NewAgent() *Agent { return &Agent{Protocol: guest.ApplyItemsProtocol} }

// Probe implements delivery.Agent.
func (a *Agent) Probe(context.Context) (guest.AgentInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ProbeErr != nil {
		return guest.AgentInfo{}, a.ProbeErr
	}
	return guest.AgentInfo{Version: "test", Protocol: a.Protocol, Layout: guest.Layout}, nil
}

// Run implements delivery.Agent: argv must be the writer's.
func (a *Agent) Run(_ context.Context, argv []string, stdin io.Reader, stdout io.Writer, _ int) (int, error) {
	if strings.Join(argv, " ") != strings.Join(delivery.WriterArgv, " ") {
		return 127, nil
	}
	body, err := io.ReadAll(stdin)
	if err != nil {
		return 0, err
	}
	var in delivery.WriterInput
	if err := json.Unmarshal(body, &in); err != nil {
		return delivery.WriterExitRefused, nil
	}
	a.mu.Lock()
	mode, fail := a.Mode, a.FailItem
	a.writes = append(a.writes, in)
	a.mu.Unlock()
	if mode == WriteMalformed {
		_, _ = stdout.Write([]byte("not json " + string(body) + "\n{\"name\":\"x\"}\n"))
		return delivery.WriterExitOK, nil
	}
	code := delivery.WriterExitOK
	failed := false
	line := func(name string, r delivery.ItemResult) {
		b, _ := json.Marshal(delivery.WriterLine{Name: name, Result: r})
		_, _ = stdout.Write(append(b, '\n'))
	}
	names := []string{}
	for _, f := range in.Files {
		names = append(names, f.Name)
	}
	for _, e := range in.Env {
		names = append(names, e.Name)
	}
	for _, n := range names {
		switch {
		case failed:
			line(n, delivery.ResultNotWritten)
		case mode == WriteFailAt && n == fail:
			line(n, delivery.ResultIsDirectory)
			failed, code = true, delivery.WriterExitFailed
		default:
			line(n, delivery.ResultOK)
		}
	}
	return code, nil
}

// Writes is each stage's input the writer was given, in order.
func (a *Agent) Writes() []delivery.WriterInput {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]delivery.WriterInput(nil), a.writes...)
}

// Wrote reports whether any write carried value (in a file's content or an
// env value).
func (a *Agent) Wrote(value string) bool {
	for _, w := range a.Writes() {
		for _, f := range w.Files {
			if bytes.Contains(f.Content, []byte(value)) {
				return true
			}
		}
		for _, e := range w.Env {
			if strings.Contains(e.Value, value) {
				return true
			}
		}
	}
	return false
}
