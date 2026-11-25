package refsource

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/delivery"
)

// v2 is the version 2 contract, from this package's directory.
var v2 = filepath.Join("..", "..", "..", "tests", "provision-contract", "v2")

const (
	testMachine  = "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b"
	otherMachine = "00000000000000000000000000000001"
)

func transcript(t *testing.T, name string) *Transcript {
	t.Helper()
	tr, err := LoadTranscript(filepath.Join(v2, "fixtures", "stream", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func answerFixture(t *testing.T, rel string) *AnswerFixture {
	t.Helper()
	f, err := LoadAnswerFixture(filepath.Join(v2, "fixtures", "answers", filepath.FromSlash(rel)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// post is one answer the fake server received, and what it answered.
type post struct {
	id, stream, machine, machineName, contentType string
	body                                          []byte
	status                                        int
	code, rule                                    string
	at                                            time.Time
}

// fakeServer serves a stream transcript as the contract's server would:
// the transcript's response and chunks (each at_ms scaled by scale), then
// the end of the stream, or, with hold, the end once release is closed.
// It judges answers as the contract's server does: the stream's id, a
// request the transcript carried, answered once, and ParseAnswer.
type fakeServer struct {
	tr      *Transcript
	scale   time.Duration
	hold    bool
	release chan struct{}

	mu       sync.Mutex
	posts    []post
	answered map[string]bool
	started  time.Time
	streams  int
	// The machine and certificate serial the stream was opened with.
	streamMachine, streamSerial string
	refuse                      func(w http.ResponseWriter) bool // refuses the stream instead, when set and true
}

func newFake(t *testing.T, tr *Transcript, hold bool) (*fakeServer, *httptest.Server) {
	f := &fakeServer{tr: tr, scale: 10 * time.Microsecond, hold: hold, release: make(chan struct{}), answered: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(func() {
		f.end()
		srv.Close()
	})
	return f, srv
}

func (f *fakeServer) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.release:
	default:
		close(f.release)
	}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == delivery.StreamPath:
		f.stream(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, delivery.AnswerPathPrefix):
		f.answer(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeServer) stream(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.streams++
	refuse := f.refuse
	f.mu.Unlock()
	if refuse != nil && refuse(w) {
		return
	}
	if r.Header.Get(delivery.HeaderMachineID) == "" || r.Header.Get(delivery.HeaderMachineName) == "" {
		writeErr(w, 403, "SOURCE_MACHINE_ID_REQUIRED", "")
		return
	}
	f.mu.Lock()
	f.streamMachine = r.Header.Get(delivery.HeaderMachineID)
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		f.streamSerial = r.TLS.PeerCertificates[0].SerialNumber.String()
	}
	f.mu.Unlock()
	for k, v := range f.tr.Response.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(f.tr.Response.Status)
	fl := w.(http.Flusher)
	fl.Flush()
	start := time.Now()
	f.mu.Lock()
	f.started = start
	f.mu.Unlock()
	for _, c := range f.tr.Chunks {
		select {
		case <-time.After(time.Until(start.Add(time.Duration(c.AtMS) * f.scale))):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, c.Text)
		fl.Flush()
	}
	if f.hold {
		select {
		case <-f.release:
		case <-r.Context().Done():
		}
	}
}

func (f *fakeServer) answer(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, delivery.AnswerPathPrefix)
	body, _ := io.ReadAll(io.LimitReader(r.Body, delivery.MaxBodyBytes+1))
	p := post{
		id: id, stream: r.Header.Get(delivery.HeaderStream), machine: r.Header.Get(delivery.HeaderMachineID),
		machineName: r.Header.Get(delivery.HeaderMachineName), contentType: r.Header.Get("Content-Type"),
		body: body, at: time.Now(),
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	req := f.requests()[id]
	serial := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		serial = r.TLS.PeerCertificates[0].SerialNumber.String()
	}
	switch {
	case req == nil:
		p.status, p.code = 404, delivery.CodeUnknownRequest
	case p.stream != f.tr.Response.Headers[delivery.HeaderStream] || p.machine != f.streamMachine || serial != f.streamSerial:
		p.status, p.code = 403, delivery.CodeNotStreamOwner
	case f.answered[id]:
		p.status, p.code = 409, delivery.CodeAlreadyAnswered
	default:
		f.answered[id] = true
		if _, refusal := delivery.ParseAnswer(body, req); refusal != nil {
			p.status, p.code, p.rule = refusal.Rule.Status(), delivery.CodeAnswerRefused, string(refusal.Rule)
		} else {
			p.status = 204
		}
	}
	f.posts = append(f.posts, p)
	if p.status == 204 {
		w.WriteHeader(204)
		return
	}
	body, _ = json.Marshal(map[string]any{"error": map[string]string{"code": p.code, "message": "refused"}, "rule": p.rule})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(p.status)
	_, _ = w.Write(body)
}

// requests are the transcript's requests, by id.
func (f *fakeServer) requests() map[string]*delivery.Request {
	out := map[string]*delivery.Request{}
	for _, c := range f.tr.Chunks {
		data, ok := strings.CutPrefix(c.Text, "event: request\ndata: ")
		if !ok {
			continue
		}
		if req, r := delivery.ParseRequest([]byte(strings.TrimSpace(data))); r == nil {
			out[req.ID] = req
		}
	}
	return out
}

func writeErr(w http.ResponseWriter, status int, code, rule string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": "refused"}, "rule": rule})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (f *fakeServer) received() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]post(nil), f.posts...)
}

// waitPosts waits until n answers have arrived, then ends the stream.
func (f *fakeServer) waitPosts(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.received()) < n {
		if time.Now().After(deadline) {
			t.Errorf("%d answers after 5 s, want %d", len(f.received()), n)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.end()
}

// events collects a source's reports.
type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) report(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func (e *events) of(kind string) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Event
	for _, ev := range e.list {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func newSource(t *testing.T, srv *httptest.Server, cfg Config, ev *events) *Source {
	t.Helper()
	cfg.Server = srv.URL
	cfg.MachineID = testMachine
	cfg.MachineName = "ada's laptop"
	cfg.Report = ev.report
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runStream(t *testing.T, s *Source) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.RunStream(ctx)
}

// TestTranscriptsReadBySource holds the source to every stream transcript:
// silent, it reports exactly the requests the transcript expects, in order,
// and answers none.
func TestTranscriptsReadBySource(t *testing.T) {
	for _, name := range []string{"request-event", "keepalive", "ends-with-pending"} {
		t.Run(name, func(t *testing.T) {
			tr := transcript(t, name)
			fake, srv := newFake(t, tr, false)
			var ev events
			s := newSource(t, srv, Config{Silent: true}, &ev)
			if err := runStream(t, s); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, e := range ev.of("request") {
				ids = append(ids, e.ID)
				if e.Stream != tr.Response.Headers[delivery.HeaderStream] {
					t.Errorf("request %s reported on stream %q", e.ID, e.Stream)
				}
			}
			if fmt.Sprint(ids) != fmt.Sprint(tr.Expect.Requests) {
				t.Errorf("requests %v, want %v", ids, tr.Expect.Requests)
			}
			if p := fake.received(); len(p) != 0 {
				t.Errorf("a silent source answered %d times", len(p))
			}
			if len(ev.of("end")) != 1 || len(ev.of("stream")) != 1 {
				t.Errorf("events %v", ev.list)
			}
		})
	}
}

// TestAnswers drives each way the source answers against the fake server.
func TestAnswers(t *testing.T) {
	canned, err := ParseCanned([]byte(`{"items":[{"name":"k","provider":"api-keys","kind":"env","variable":"EXAMPLE_KEY","value":"fixture","fingerprint":"f"}],` +
		`"problems":[{"provider":"github","code":"signed_out","message":"m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		status     int
		code, rule string
	}
	cases := []struct {
		name       string
		transcript string
		cfg        Config
		want       []want
		check      func(t *testing.T, tr *Transcript, posts []post, f *fakeServer)
	}{
		{
			name: "canned items", transcript: "request-event", cfg: Config{Canned: canned},
			want: []want{{204, "", ""}},
			check: func(t *testing.T, tr *Transcript, posts []post, _ *fakeServer) {
				p := posts[0]
				a, r := delivery.ParseAnswer(p.body, &delivery.Request{ID: p.id})
				if r != nil || len(a.Items) != 1 || a.Items[0].Variable != "EXAMPLE_KEY" || len(a.Problems) != 1 {
					t.Errorf("answer %s: %v", p.body, r)
				}
				if p.machine != testMachine || p.contentType != "application/json" {
					t.Errorf("machine %q, content type %q", p.machine, p.contentType)
				}
				if name, err := url.PathUnescape(p.machineName); err != nil || name != "ada's laptop" {
					t.Errorf("machine name %q", p.machineName)
				}
			},
		},
		{
			name: "empty answer", transcript: "keepalive", cfg: Config{},
			want: []want{{204, "", ""}},
			check: func(t *testing.T, _ *Transcript, posts []post, _ *fakeServer) {
				if a, r := delivery.ParseAnswer(posts[0].body, &delivery.Request{ID: posts[0].id}); r != nil || len(a.Items) != 0 || len(a.Problems) != 0 {
					t.Errorf("answer %s: %v", posts[0].body, r)
				}
			},
		},
		{
			name: "twice", transcript: "request-event", cfg: Config{Twice: true},
			want: []want{{204, "", ""}, {409, delivery.CodeAlreadyAnswered, ""}},
		},
		{
			name: "second connection", transcript: "request-event", cfg: Config{SecondConnection: true},
			want: []want{{403, delivery.CodeNotStreamOwner, ""}},
			check: func(t *testing.T, tr *Transcript, posts []post, _ *fakeServer) {
				if posts[0].stream == tr.Response.Headers[delivery.HeaderStream] || !delivery.ValidID(posts[0].stream) {
					t.Errorf("answered with stream id %q", posts[0].stream)
				}
			},
		},
		{
			name: "another machine", transcript: "request-event", cfg: Config{AnswerMachineID: otherMachine},
			want: []want{{403, delivery.CodeNotStreamOwner, ""}},
			check: func(t *testing.T, _ *Transcript, posts []post, _ *fakeServer) {
				if posts[0].machine != otherMachine {
					t.Errorf("answered as machine %q", posts[0].machine)
				}
			},
		},
		{
			name: "late", transcript: "request-event", cfg: Config{Delay: 100 * time.Millisecond},
			want: []want{{204, "", ""}},
			check: func(t *testing.T, _ *Transcript, posts []post, f *fakeServer) {
				f.mu.Lock()
				started := f.started
				f.mu.Unlock()
				if d := posts[0].at.Sub(started); d < 100*time.Millisecond {
					t.Errorf("answered %s after the request, want at least 100ms", d)
				}
			},
		},
		{
			name: "refuse fixture nested-target", transcript: "request-event", cfg: Config{Refuse: answerFixture(t, "refuse/nested-target")},
			want: []want{{422, delivery.CodeAnswerRefused, "nested_target"}},
		},
		{
			name: "refuse fixture not-json", transcript: "request-event", cfg: Config{Refuse: answerFixture(t, "refuse/not-json-truncated")},
			want: []want{{400, delivery.CodeAnswerRefused, "not_json"}},
		},
		{
			name: "refuse fixture with fill", transcript: "request-event", cfg: Config{Refuse: answerFixture(t, "refuse/file-too-large")},
			want: []want{{422, delivery.CodeAnswerRefused, "file_too_large"}},
		},
		{
			name: "refuse fixture id-mismatch", transcript: "request-event", cfg: Config{Refuse: answerFixture(t, "refuse/id-mismatch")},
			want: []want{{422, delivery.CodeAnswerRefused, "id_mismatch"}},
		},
		{
			name: "accept fixture", transcript: "request-event", cfg: Config{Refuse: answerFixture(t, "accept/typical")},
			want: []want{{204, "", ""}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := transcript(t, c.transcript)
			fake, srv := newFake(t, tr, true)
			var ev events
			s := newSource(t, srv, c.cfg, &ev)
			done := make(chan error, 1)
			go func() { done <- runStream(t, s) }()
			fake.waitPosts(t, len(c.want))
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			posts := fake.received()
			if len(posts) != len(c.want) {
				t.Fatalf("%d answers, want %d", len(posts), len(c.want))
			}
			reported := ev.of("answer")
			if len(reported) != len(c.want) {
				t.Fatalf("%d answers reported, want %d", len(reported), len(c.want))
			}
			for i, w := range c.want {
				p := posts[i]
				if p.id != tr.Expect.Requests[0] {
					t.Errorf("answer %d to %s, want %s", i, p.id, tr.Expect.Requests[0])
				}
				if p.status != w.status || p.code != w.code || p.rule != w.rule {
					t.Errorf("answer %d: %d %s %s, want %d %s %s", i, p.status, p.code, p.rule, w.status, w.code, w.rule)
				}
				r := reported[i]
				if r.Attempt != i+1 || r.Status != w.status || r.Code != w.code || r.Rule != w.rule {
					t.Errorf("answer %d reported %+v, want %d %s %s", i, r, w.status, w.code, w.rule)
				}
			}
			if c.check != nil {
				c.check(t, tr, posts, fake)
			}
		})
	}
}

// TestAnswerWithAnotherCertificate: AnswerClient answers with another
// client certificate but the stream's own machine and stream ids, so only a
// server that compares the certificate refuses it.
func TestAnswerWithAnotherCertificate(t *testing.T) {
	tr := transcript(t, "request-event")
	f := &fakeServer{tr: tr, scale: 10 * time.Microsecond, hold: true, release: make(chan struct{}), answered: map[string]bool{}}
	srv := httptest.NewUnstartedServer(f)
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	t.Cleanup(func() { f.end(); srv.Close() })

	client := func(serial int64) *http.Client {
		tr := srv.Client().Transport.(*http.Transport).Clone()
		tr.TLSClientConfig.Certificates = []tls.Certificate{selfSigned(t, serial)}
		return &http.Client{Transport: tr}
	}
	cases := []struct {
		name   string
		answer *http.Client
		want   int
		code   string
	}{
		{"the stream's certificate", nil, 204, ""},
		{"another certificate", client(2), 403, delivery.CodeNotStreamOwner},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.mu.Lock()
			f.posts, f.answered, f.release = nil, map[string]bool{}, make(chan struct{})
			f.mu.Unlock()
			var ev events
			cfg := Config{Server: srv.URL, Client: client(1), AnswerClient: c.answer, MachineID: testMachine, MachineName: "m", Report: ev.report}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- runStream(t, s) }()
			f.waitPosts(t, 1)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			p := f.received()[0]
			if p.status != c.want || p.code != c.code {
				t.Fatalf("answer: %d %s, want %d %s", p.status, p.code, c.want, c.code)
			}
			if p.stream != tr.Response.Headers[delivery.HeaderStream] || p.machine != testMachine {
				t.Fatalf("answered with stream %q machine %q: not the stream's own", p.stream, p.machine)
			}
		})
	}
}

// selfSigned is a throwaway client certificate with serial, made for the
// test and never written to disk.
func selfSigned(t *testing.T, serial int64) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "provision-ref test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestNextBackoff is the reconnect schedule CONTRACT.md gives.
func TestNextBackoff(t *testing.T) {
	cases := []struct {
		name                    string
		last, held, retry, want time.Duration
	}{
		{"first", 0, 0, 0, time.Second},
		{"doubles after a short stream", time.Second, time.Second, 0, 2 * time.Second},
		{"doubles again", 8 * time.Second, 0, 0, 16 * time.Second},
		{"at most 30 s", 16 * time.Second, 0, 0, 30 * time.Second},
		{"stays at 30 s", 30 * time.Second, 0, 0, 30 * time.Second},
		{"resets after a 60 s hold", 30 * time.Second, 60 * time.Second, 0, time.Second},
		{"never less than Retry-After", time.Second, 0, 45 * time.Second, 45 * time.Second},
		{"Retry-After below the backoff", 8 * time.Second, 0, 2 * time.Second, 16 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nextBackoff(c.last, c.held, c.retry); got != c.want {
				t.Errorf("nextBackoff = %s, want %s", got, c.want)
			}
		})
	}
}

// TestEndedStreamDropsPending: requests whose stream ends before their
// answer is sent are dropped, never answered.
func TestEndedStreamDropsPending(t *testing.T) {
	tr := transcript(t, "ends-with-pending")
	fake, srv := newFake(t, tr, false)
	var ev events
	s := newSource(t, srv, Config{Delay: 300 * time.Millisecond}, &ev)
	if err := runStream(t, s); err != nil {
		t.Fatal(err)
	}
	if p := fake.received(); len(p) != 0 {
		t.Fatalf("%d answers to requests of an ended stream", len(p))
	}
	var dropped []string
	for _, e := range ev.of("dropped") {
		dropped = append(dropped, e.ID)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %v, want both of %v", dropped, tr.Expect.Requests)
	}
}

// TestStreamRefused: a refused stream is reported with its status, code and
// Retry-After, and nothing is answered.
func TestStreamRefused(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		retry  string
		wantRA time.Duration
	}{
		{"duplicate stream", 409, "SOURCE_DUPLICATE_STREAM", "", 0},
		{"not bound", 403, "SOURCE_NOT_BOUND", "", 0},
		{"throttled", 429, "SOURCE_RATE_LIMITED", "7", 7 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake, srv := newFake(t, transcript(t, "request-event"), false)
			fake.refuse = func(w http.ResponseWriter) bool {
				if c.retry != "" {
					w.Header().Set("Retry-After", c.retry)
				}
				writeErr(w, c.status, c.code, "")
				return true
			}
			var ev events
			s := newSource(t, srv, Config{}, &ev)
			err := runStream(t, s)
			re, ok := err.(*RefusedError)
			if !ok || re.Status != c.status || re.Code != c.code || re.RetryAfter != c.wantRA {
				t.Fatalf("got %v, want %d %s", err, c.status, c.code)
			}
			if r := ev.of("refused"); len(r) != 1 || r[0].Code != c.code {
				t.Errorf("reported %v", ev.list)
			}
		})
	}
}

// TestRunReconnects: Run opens a new stream after one ends, and answers a
// request of the new stream only.
func TestRunReconnects(t *testing.T) {
	fake, srv := newFake(t, transcript(t, "request-event"), false)
	var ev events
	s := newSource(t, srv, Config{Silent: true}, &ev)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	streams := fake.streams
	fake.mu.Unlock()
	if streams < 2 {
		t.Fatalf("%d streams in 1.5 s, want a reconnect after a second", streams)
	}
}

func TestNewChecks(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no server", Config{MachineID: testMachine, MachineName: "m"}},
		{"bad machine id", Config{Server: "http://127.0.0.1:1", MachineID: "XYZ", MachineName: "m"}},
		{"no machine name", Config{Server: "http://127.0.0.1:1", MachineID: testMachine}},
		{"long machine name", Config{Server: "http://127.0.0.1:1", MachineID: testMachine, MachineName: strings.Repeat("m", 64)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestParseCanned(t *testing.T) {
	if _, err := ParseCanned([]byte(`{"items":[],"extra":1}`)); err == nil {
		t.Error("an unknown member was accepted")
	}
	c, err := ParseCanned([]byte(`{}`))
	if err != nil || c.Items != nil || c.Problems != nil {
		t.Errorf("empty: %+v %v", c, err)
	}
}
