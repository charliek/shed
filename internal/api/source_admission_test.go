package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/source"
)

// Bus admission by machine per user, at the API door: the machine headers,
// the three refusals and their bodies, the respond check, the rate limit,
// the stream's write deadline and shutdown.

const subPath = "/api/plugins/listeners/ssh-agent/messages"
const respondPath = "/api/plugins/listeners/ssh-agent/respond"

// subscribeOnce runs a subscribe as leaf (nil: open mode) with the given
// header pairs (none: the key's own machine) on a context that ends after a
// moment, and returns what it answered: an admitted subscribe answers 200
// and streams until the context ends.
func subscribeOnce(h http.Handler, leaf *x509.Certificate, path string, hdr ...string) apiResp {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	fp := ""
	if leaf != nil {
		withClientCert(req, leaf)
		fp = leaf.Subject.CommonName
	}
	withTestMachine(req, fp)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return apiResp{rec.Code, rec.Body.Bytes(), rec.Header()}
}

// refusalSource is the "source" object of a SOURCE_NOT_BOUND body.
func refusalSource(t *testing.T, r apiResp) (id, name string) {
	t.Helper()
	var body struct {
		Error  config.APIErrorDetail `json:"error"`
		Source *struct {
			MachineID   string `json:"machine_id"`
			MachineName string `json:"machine_name"`
		} `json:"source"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("refusal body is not JSON: %v: %s", err, r.body)
	}
	if body.Source == nil {
		t.Fatalf("refusal has no source object: %s", r.body)
	}
	return body.Source.MachineID, body.Source.MachineName
}

// assertRefusal checks a pin refusal: status, code, never 401, a body under
// 1024 bytes in the usual error shape with the message first.
func assertRefusal(t *testing.T, what string, r apiResp, status int, code string) {
	t.Helper()
	if r.code != status || r.errCode(t) != code {
		t.Fatalf("%s = %d %s, want %d %s", what, r.code, r.body, status, code)
	}
	if len(r.body) >= 1024 {
		t.Fatalf("%s body is %d bytes, want under 1024", what, len(r.body))
	}
	if !strings.HasPrefix(string(r.body), `{"error":{"code":"`+code+`","message":"`) {
		t.Fatalf("%s body does not lead with the error object: %s", what, r.body)
	}
}

// versionLike matches a release version such as 0.10.0.
var versionLike = regexp.MustCompile(`\d+\.\d+\.\d+`)

// retiredDaemon matches the retired daemon's name in any spelling.
var retiredDaemon = regexp.MustCompile(`(?i)host.?agent`)

// assertMachineIDRequiredBody checks the refusal of a request with no valid
// machine headers: a JSON body with its code whose message is the whole
// message (not a shortened one), tells the person to install the desktop app,
// names both headers an SDK listener must send, and names neither the retired
// daemon nor any version, within the 1000 bytes a broker reads.
func assertMachineIDRequiredBody(t *testing.T, what string, r apiResp) {
	t.Helper()
	data := bytes.TrimSuffix(r.body, []byte("\n"))
	var body struct {
		Error config.APIErrorDetail `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("%s: body is not JSON: %v: %s", what, err, r.body)
	}
	if body.Error.Code != config.ErrSourceMachineIDRequired {
		t.Errorf("%s: code = %q, want %q", what, body.Error.Code, config.ErrSourceMachineIDRequired)
	}
	msg := body.Error.Message
	// The whole message, written out here and not read from the constant, so
	// a clause dropped from either copy fails.
	const whole = "this server lets one machine per user answer credential requests and needs to know which machine is asking " +
		"(headers X-Shed-Machine-Id and X-Shed-Machine-Name), but this request named no valid machine; " +
		"install the current shed desktop app"
	if msg != whole {
		t.Errorf("%s: message is not the whole refusal:\n got %q\nwant %q", what, msg, whole)
	}
	for _, want := range []string{"install the current shed desktop app", "X-Shed-Machine-Id", "X-Shed-Machine-Name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s: message does not contain %q: %q", what, want, msg)
		}
	}
	if retiredDaemon.MatchString(msg) {
		t.Errorf("%s: message names the retired daemon: %q", what, msg)
	}
	if v := versionLike.FindString(msg); v != "" {
		t.Errorf("%s: message names a version (%s): %q", what, v, msg)
	}
	if len(data) > 1000 || len(data) > maxRefusalBody {
		t.Errorf("%s: body is %d bytes, want at most 1000: %s", what, len(data), data)
	}
}

// TestMachineIDRequiredRefusal: the refusal itself, as writeMachineIDRequired
// writes it: 403 with the whole message.
func TestMachineIDRequiredRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMachineIDRequired(rec)
	r := apiResp{rec.Code, rec.Body.Bytes(), rec.Header()}
	assertRefusal(t, "no machine headers", r, http.StatusForbidden, config.ErrSourceMachineIDRequired)
	assertMachineIDRequiredBody(t, "no machine headers", r)
}

// TestMissingMachineIDLoggedOncePerKey: the server's log line for a request
// with no machine headers says what to install, names no version and no
// retired daemon, quotes the key and the user, and is written once per key.
func TestMissingMachineIDLoggedOncePerKey(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) }()

	var l missingIDLog
	l.logOnce("alice", "SHA256:alice")
	l.logOnce("alice", "SHA256:alice")
	l.logOnce("", "SHA256:odd\nkey")

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %d lines, want 2 (once per key, a key's newline escaped): %q", len(lines), buf.String())
	}
	if want := `credential bus: refused a request with no valid machine id for user "alice" from key "SHA256:alice"; ` +
		`install the current shed desktop app on that machine; logged once per key`; lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
	if !strings.Contains(lines[1], `with no valid machine id from key "SHA256:odd\nkey";`) {
		t.Errorf("the implicit user's line = %q, want the key quoted and no user", lines[1])
	}
	for _, line := range lines {
		if retiredDaemon.MatchString(line) || versionLike.MatchString(line) {
			t.Errorf("line names the retired daemon or a version: %q", line)
		}
	}
}

// TestSourceAdmissionTable is the admission table at the API door (mtls,
// users alice, bob and the admin root; each key its own machine unless a
// case names one).
func TestSourceAdmissionTable(t *testing.T) {
	s := busUsersServer(t)
	router := s.Router()
	alice := credsAs(aliceFP)
	aliceM := testMachineFor(aliceFP)
	other := source.Machine{ID: strings.Repeat("ab", 16), Name: "other ’laptop"}
	hdr := func(m source.Machine) []string {
		return []string{HeaderMachineID, m.ID, HeaderMachineName, url.PathEscape(m.Name)}
	}

	// The first machine subscribes and is bound; the same machine takes
	// every namespace.
	for _, ns := range []string{"ssh-agent", "docker-credential", "aws-credentials"} {
		if r := subscribeOnce(router, alice, "/api/plugins/listeners/"+ns+"/messages"); r.code != http.StatusOK {
			t.Fatalf("alice's machine on %s = %d %s, want 200", ns, r.code, r.body)
		}
	}
	if bd, ok := s.plugins.Bindings().Get("alice"); !ok || bd.MachineID() != aliceM.ID {
		t.Fatalf("binding = %+v %v, want alice's machine", bd, ok)
	}
	if ms := s.plugins.Bindings().Machines("alice"); len(ms) != 1 || ms[0].KeyFingerprint != aliceFP {
		t.Fatalf("machines = %+v, want one with the certificate's fingerprint", ms)
	}

	required := func(name string, hdrs ...string) {
		t.Helper()
		r := subscribeOnce(router, alice, subPath, hdrs...)
		assertRefusal(t, name, r, http.StatusForbidden, config.ErrSourceMachineIDRequired)
		assertMachineIDRequiredBody(t, name, r)
	}
	for _, tc := range []struct {
		name string
		hdr  []string
	}{
		{"no headers", []string{HeaderMachineID, "", HeaderMachineName, ""}},
		{"no name", []string{HeaderMachineID, aliceM.ID, HeaderMachineName, ""}},
		{"no id", []string{HeaderMachineID, "", HeaderMachineName, "x"}},
		{"uppercase id", []string{HeaderMachineID, strings.ToUpper(strings.Repeat("ab", 16)), HeaderMachineName, "x"}},
		{"hyphenated id", []string{HeaderMachineID, "00000000-0000-0000-0000-000000000001", HeaderMachineName, "x"}},
		{"short id", []string{HeaderMachineID, aliceM.ID[:31], HeaderMachineName, "x"}},
		{"two ids", []string{HeaderMachineID, aliceM.ID, HeaderMachineID, aliceM.ID, HeaderMachineName, "x"}},
		{"name over 63 bytes", []string{HeaderMachineID, aliceM.ID, HeaderMachineName, url.PathEscape(strings.Repeat("é", 32))}},
		{"control character in the name", []string{HeaderMachineID, aliceM.ID, HeaderMachineName, "a%0Ab"}},
		{"NUL in the name", []string{HeaderMachineID, aliceM.ID, HeaderMachineName, "a%00b"}},
		{"raw UTF-8 in the header", []string{HeaderMachineID, aliceM.ID, HeaderMachineName, "caf\xc3\xa9"}},
		{"malformed escape", []string{HeaderMachineID, aliceM.ID, HeaderMachineName, "a%zzb"}},
		{"invalid UTF-8 once decoded", []string{HeaderMachineID, aliceM.ID, HeaderMachineName, "a%ffb"}},
	} {
		t.Run(tc.name, func(t *testing.T) { required(tc.name, tc.hdr...) })
	}

	// Another machine of alice's is refused, naming alice's machine; the
	// refusal is neither 409 nor 401. A percent-encoded non-ASCII name is
	// decoded and remembered.
	r := subscribeOnce(router, alice, subPath, hdr(other)...)
	assertRefusal(t, "alice's second machine", r, http.StatusForbidden, config.ErrSourceNotBound)
	if id, name := refusalSource(t, r); id != aliceM.ID || name != aliceM.Name {
		t.Fatalf("refusal names %s %q, want alice's machine %s %q", id, name, aliceM.ID, aliceM.Name)
	}
	var found bool
	for _, m := range s.plugins.Bindings().Machines("alice") {
		found = found || (m.ID == other.ID && m.Name == other.Name)
	}
	if !found {
		t.Fatalf("the refused machine was not remembered under its decoded name: %+v", s.plugins.Bindings().Machines("alice"))
	}

	// The bound machine already holding the namespace: 409 with its code,
	// and the live stream stays.
	l := listen(t, s, "alice", "x-held", aliceFP)
	r = subscribeOnce(router, alice, "/api/plugins/listeners/x-held/messages")
	assertRefusal(t, "the same machine twice", r, http.StatusConflict, config.ErrSourceDuplicateStream)
	if got, ok := s.plugins.Get(l.Key()); !ok || got != l {
		t.Fatal("a duplicate subscribe replaced the live listener")
	}

	// Bob is his own user: his first machine is bound, independently.
	if r := subscribeOnce(router, credsAs(bobFP), subPath); r.code != http.StatusOK {
		t.Fatalf("bob's first machine = %d %s, want 200", r.code, r.body)
	}
	// Bob presenting alice's machine id is refused naming bob's machine.
	r = subscribeOnce(router, credsAs(bobFP), "/api/plugins/listeners/docker/messages", hdr(aliceM)...)
	assertRefusal(t, "alice's machine id under bob's key", r, http.StatusForbidden, config.ErrSourceNotBound)
	if id, _ := refusalSource(t, r); id != testMachineFor(bobFP).ID {
		t.Fatalf("refusal names %s, want bob's machine", id)
	}
}

// TestSourceRespondOnlyFromTheBoundMachine: an answer from a machine that is
// not the user's source is 403 SOURCE_NOT_BOUND and leaves the request
// pending; the bound machine then answers it. Without machine headers an
// answer is 403 SOURCE_MACHINE_ID_REQUIRED.
func TestSourceRespondOnlyFromTheBoundMachine(t *testing.T) {
	s := busUsersServer(t)
	router := s.Router()
	box := &sendCounter{}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: box.send})
	listen(t, s, "alice", "ssh-agent", aliceFP)
	id := publishFromShed(t, s, "box", gen)

	other := testMachineFor("another machine")
	r := do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", id), HeaderMachineID, other.ID, HeaderMachineName, other.Name)
	assertRefusal(t, "an answer from another machine", r, http.StatusForbidden, config.ErrSourceNotBound)
	if got, _ := refusalSource(t, r); got != testMachineFor(aliceFP).ID {
		t.Fatalf("refusal names %s", got)
	}
	r = do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", id), HeaderMachineID, "", HeaderMachineName, "")
	assertRefusal(t, "an answer with no machine", r, http.StatusForbidden, config.ErrSourceMachineIDRequired)
	assertMachineIDRequiredBody(t, "an answer with no machine", r)
	if box.count() != 0 {
		t.Fatal("a refused answer reached the shed")
	}
	if r := do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", id)); r.code != http.StatusNoContent || box.count() != 1 {
		t.Fatalf("the bound machine's answer = %d %s (sent %d), want 204", r.code, r.body, box.count())
	}
}

// TestSourceTwoUsersHoldTheSameNamespace: alice and bob each bind their own
// machine and hold ssh-agent at once over real streams; each is sent only
// their own shed's request.
func TestSourceTwoUsersHoldTheSameNamespace(t *testing.T) {
	s := busUsersServer(t)
	boxGen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	bobsGen := s.bridge.RegisterShed("bobs", &plugin.ShedConn{User: "bob", CreatedAt: busT1, Send: (&sendCounter{}).send})
	ts := sseServer(t, s, map[string]*x509.Certificate{"alice": credsAs(aliceFP), "bob": credsAs(bobFP)})
	alice := openStream(t, ts, "alice", subPath)
	bob := openStream(t, ts, "bob", subPath)
	waitListener(t, s, plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	waitListener(t, s, plugin.ListenerKey{User: "bob", Namespace: "ssh-agent"})
	for _, u := range []struct{ user, fp string }{{"alice", aliceFP}, {"bob", bobFP}} {
		l, _ := s.plugins.Get(plugin.ListenerKey{User: u.user, Namespace: "ssh-agent"})
		if l.MachineID != testMachineFor(u.fp).ID {
			t.Fatalf("%s's listener is machine %s, want %s's own", u.user, l.MachineID, u.user)
		}
	}
	boxReq := publishFromShed(t, s, "box", boxGen)
	bobsReq := publishFromShed(t, s, "bobs", bobsGen)
	if got := nextData(t, "bob", bob); !strings.Contains(got, bobsReq) {
		t.Fatalf("bob got %s, want %s", got, bobsReq)
	}
	if got := nextData(t, "alice", alice); !strings.Contains(got, boxReq) {
		t.Fatalf("alice got %s, want %s", got, boxReq)
	}
}

// worstNames are 63-byte machine names made of what JSON (with its HTML
// escaping) and percent-encoding expand most.
var worstNames = map[string]string{
	"angle brackets":  strings.Repeat("<", 63),
	"ampersands":      strings.Repeat("&", 63),
	"quotes":          strings.Repeat(`"`, 63),
	"backslashes":     strings.Repeat(`\`, 63),
	"line separators": strings.Repeat(" ", 21),
	"mixed":           strings.Repeat(`<"&\`, 15) + "<>&",
}

// TestSourceRefusalBodiesStayUnder1024Bytes: whatever the bound machine's
// name, the SOURCE_NOT_BOUND body (on subscribe and on respond) is under
// 1024 bytes, is valid JSON and names the machine exactly.
func TestSourceRefusalBodiesStayUnder1024Bytes(t *testing.T) {
	for what, name := range worstNames {
		t.Run(what, func(t *testing.T) {
			if len(name) != 63 || !source.ValidMachineName(name) {
				t.Fatalf("test set-up: name is %d bytes, valid %v", len(name), source.ValidMachineName(name))
			}
			s := busUsersServer(t)
			router := s.Router()
			bound := source.Machine{ID: strings.Repeat("cd", 16), Name: name}
			// The bound machine presents itself over the header, as a broker
			// would: percent-encoded.
			if r := subscribeOnce(router, credsAs(aliceFP), subPath, HeaderMachineID, bound.ID, HeaderMachineName, url.PathEscape(name)); r.code != http.StatusOK {
				t.Fatalf("bound machine = %d %s", r.code, r.body)
			}
			check := func(how string, r apiResp) {
				t.Helper()
				assertRefusal(t, how, r, http.StatusForbidden, config.ErrSourceNotBound)
				if id, got := refusalSource(t, r); id != bound.ID || got != name {
					t.Fatalf("%s names %s %q, want %q", how, id, got, name)
				}
				if strings.ContainsAny(string(r.body), "<>&") {
					t.Fatalf("%s body carries raw HTML characters: %s", how, r.body)
				}
			}
			check("subscribe", subscribeOnce(router, credsAs(aliceFP), subPath))
			check("respond", do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", "x")))
		})
	}
}

// TestSourceStoreFailureIs500AndRegistersNothing and the fail-closed case
// with no bindings.
func TestSourceStoreFailureAndNoBindings(t *testing.T) {
	s := busUsersServer(t)
	b := pinBus(s)
	router := s.Router()
	b.FailWrites(errors.New("disk full"))
	r := subscribeOnce(router, credsAs(aliceFP), subPath)
	if r.code != http.StatusInternalServerError {
		t.Fatalf("subscribe with a failing store = %d %s, want 500", r.code, r.body)
	}
	if _, ok := s.plugins.Get(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"}); ok {
		t.Fatal("a failed admission registered a listener")
	}
	if _, ok := b.Get("alice"); ok {
		t.Fatal("a failed admission bound a machine")
	}

	reg := plugin.NewRegistry()
	bare := NewServer(nil, &config.ServerConfig{Name: "t", HTTPPort: 8080}, "", reg, plugin.NewBridge(reg, false))
	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, subPath, ""},
		{http.MethodPost, respondPath, respondBody("box", "x")},
	} {
		if r := do(bare.Router(), nil, req.method, req.path, req.body); r.code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s with no bindings = %d %s, want 503", req.method, req.path, r.code, r.body)
		}
	}
}

// TestSourceAnswerBehindABlockedWriteIs503: while a durable write of the
// binding store holds alice's gate (a new machine's admission, its save
// blocked), her bound machine's answer gives up at the response deadline
// with the retryable 503 RESPONSE_NOT_DELIVERED and Retry-After, and nothing
// is consumed: once the write ends the same answer is accepted. It waits out
// the bus's real response deadline (5 s), which this package cannot shorten.
func TestSourceAnswerBehindABlockedWriteIs503(t *testing.T) {
	s := busUsersServer(t)
	b := pinBus(s)
	router := s.Router()
	box := &sendCounter{}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: box.send})
	listen(t, s, "alice", "ssh-agent", aliceFP)
	id := publishFromShed(t, s, "box", gen)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.BeforeWrite(func() { once.Do(func() { close(entered); <-release }) })
	slow := make(chan error, 1)
	go func() {
		_, err := s.plugins.Subscribe(context.Background(), plugin.ListenerKey{User: "alice", Namespace: "docker"}, testMachineFor("another machine"))
		slow <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the admission never reached its store write")
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	r := do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", id))
	if r.code != http.StatusServiceUnavailable || r.errCode(t) != "RESPONSE_NOT_DELIVERED" || r.header.Get("Retry-After") != "1" {
		t.Fatalf("an answer waiting for the gate = %d %s (Retry-After %q), want 503 RESPONSE_NOT_DELIVERED with Retry-After 1", r.code, r.body, r.header.Get("Retry-After"))
	}
	if box.count() != 0 {
		t.Fatal("an answer that gave up at the gate reached the shed")
	}

	released = true
	close(release)
	select {
	case <-slow:
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked admission never ended")
	}
	if r := do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", id)); r.code != http.StatusNoContent || box.count() != 1 {
		t.Fatalf("the same answer once the gate is free = %d %s (sent %d), want 204", r.code, r.body, box.count())
	}
}

// TestSourceAdmissionThrottled: past the user's admission budget a writing
// admission is 429 with a whole-seconds Retry-After (a retryable class),
// never a refusal the broker treats as final.
func TestSourceAdmissionThrottled(t *testing.T) {
	s := busUsersServer(t)
	router := s.Router()
	var r apiResp
	for i := 0; i < 20; i++ {
		m := testMachineFor(fmt.Sprintf("cycling-%d", i))
		r = subscribeOnce(router, credsAs(aliceFP), subPath, HeaderMachineID, m.ID, HeaderMachineName, m.Name)
		if r.code == http.StatusTooManyRequests {
			break
		}
	}
	assertRefusal(t, "a broker cycling machine ids", r, http.StatusTooManyRequests, config.ErrSourceRateLimited)
	secs, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q, want whole seconds", r.header.Get("Retry-After"))
	}
	// Bob is not held back.
	if r := subscribeOnce(router, credsAs(bobFP), subPath); r.code != http.StatusOK {
		t.Fatalf("bob = %d %s", r.code, r.body)
	}
}

// TestSourceFeatureAdvertised: a server with a credential bus advertises
// source_binding; one without does not.
func TestSourceFeatureAdvertised(t *testing.T) {
	if !sliceHas(newTestServerWithPlugins().serverFeatures(), FeatureSourceBinding) {
		t.Fatal("a server with a bus does not advertise source_binding")
	}
	plain := NewServer(nil, &config.ServerConfig{Name: "t", HTTPPort: 8080}, "", nil, nil)
	if sliceHas(plain.serverFeatures(), FeatureSourceBinding) {
		t.Fatal("a server with no bus advertises source_binding")
	}
}

// TestSourceCloseEndsStreams: closing the bus (shutdown) ends an open
// stream, and a later subscribe is refused with 503.
func TestSourceCloseEndsStreams(t *testing.T) {
	srv := newTestServerWithPlugins()
	rec := newStreamRecorder()
	done := subscribeInBackground(t, srv, "op", rec)
	srv.plugins.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end when the bus closed")
	}
	if r := subscribeOnce(srv.Router(), nil, "/api/plugins/listeners/op/messages"); r.code != http.StatusServiceUnavailable {
		t.Fatalf("subscribe after Close = %d %s, want 503", r.code, r.body)
	}
}

// TestSourceStalledStreamIsReaped: a subscriber that stops reading has its
// stream closed by the write deadline and its listener unregistered, so the
// same machine's next subscribe is admitted (while the stalled stream is
// still registered, it is a duplicate). Over a real socket, with the
// deadline shortened.
func TestSourceStalledStreamIsReaped(t *testing.T) {
	prev := sseWriteTimeout
	sseWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sseWriteTimeout = prev })

	srv := newTestServerWithPlugins()
	gen := srv.bridge.RegisterShed("dev", &plugin.ShedConn{Send: func(*plugin.Envelope) error { return nil }})
	ts := httptest.NewUnstartedServer(srv.Router())
	ts.Config.ConnContext = ConnContext
	ts.Start()
	t.Cleanup(ts.Close)
	m := testMachineFor("")
	key := plugin.ListenerKey{Namespace: "op"}

	// A raw subscriber with a small receive buffer that reads the response
	// head and then nothing.
	d := net.Dialer{Control: smallReceiveBuffer}
	conn, err := d.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET /api/plugins/listeners/op/messages HTTP/1.1\r\nHost: x\r\n%s: %s\r\n%s: %s\r\n\r\n", HeaderMachineID, m.ID, HeaderMachineName, m.Name)
	head, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(head, "200") {
		t.Fatalf("subscribe = %q, %v", head, err)
	}
	waitListener(t, srv, key)

	// While it is registered, the same machine is a duplicate.
	if r := subscribeOnce(srv.Router(), nil, "/api/plugins/listeners/op/messages"); r.code != http.StatusConflict {
		t.Fatalf("a second stream of the machine = %d %s, want 409", r.code, r.body)
	}

	// Fill the stream until a write blocks past its deadline.
	payload := json.RawMessage(`"` + strings.Repeat("x", 256<<10) + `"`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := srv.plugins.Get(key); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stalled stream was never reaped")
		}
		env := plugin.NewEnvelope("op", plugin.MessageTypeEvent, payload)
		_ = srv.bridge.PublishToHost(context.Background(), "dev", gen, env)
	}

	// The machine reconnects: admitted.
	if r := subscribeOnce(srv.Router(), nil, "/api/plugins/listeners/op/messages"); r.code != http.StatusOK {
		t.Fatalf("the machine's reconnect after the reap = %d %s, want 200", r.code, r.body)
	}
}

// shortStreamTimers shortens the stream's write deadline and keepalive for
// the rest of the test: the keepalive comes well after a deadline would have
// passed.
func shortStreamTimers(t *testing.T, write, keepalive time.Duration) {
	t.Helper()
	prevW, prevK := sseWriteTimeout, sseKeepaliveInterval
	sseWriteTimeout, sseKeepaliveInterval = write, keepalive
	t.Cleanup(func() { sseWriteTimeout, sseKeepaliveInterval = prevW, prevK })
}

// TestSourceIdleHTTP2StreamSurvivesTheWriteDeadline: over HTTP/2 (which the
// HTTPS listener negotiates) a stream's write deadline resets the stream when
// it passes, so it must be armed only while a write is in progress. A healthy
// subscription that stays idle well past the deadline still receives its
// keepalives and a later event, and stays registered.
func TestSourceIdleHTTP2StreamSurvivesTheWriteDeadline(t *testing.T) {
	shortStreamTimers(t, 150*time.Millisecond, 600*time.Millisecond)
	srv := newTestServerWithPlugins()
	gen := srv.bridge.RegisterShed("dev", &plugin.ShedConn{Send: func(*plugin.Envelope) error { return nil }})
	ts := httptest.NewUnstartedServer(srv.Router())
	ts.EnableHTTP2 = true
	ts.Config.ConnContext = ConnContext
	ts.StartTLS()
	t.Cleanup(ts.Close)
	key := plugin.ListenerKey{Namespace: "op"}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/plugins/listeners/op/messages", nil)
	setMachineHeaders(req, testMachineFor(""))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		t.Fatalf("subscribe = %d over %s, want 200 over HTTP/2", resp.StatusCode, resp.Proto)
	}
	lines := make(chan string, 16)
	readErr := make(chan error, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				readErr <- err
				return
			}
			if line = strings.TrimSpace(line); line != "" {
				lines <- line
			}
		}
	}()
	next := func(what string) string {
		t.Helper()
		select {
		case l := <-lines:
			return l
		case err := <-readErr:
			t.Fatalf("waiting for %s: the stream ended: %v", what, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("waiting for %s: nothing came", what)
		}
		return ""
	}
	// Idle from the headers to the first keepalive, and between keepalives:
	// each gap is four times the write deadline.
	for i := 0; i < 2; i++ {
		if got := next("a keepalive"); got != ": keepalive" {
			t.Fatalf("keepalive %d = %q", i+1, got)
		}
	}
	waitListener(t, srv, key)
	env := plugin.NewEnvelope("op", plugin.MessageTypeEvent, json.RawMessage(`"after-idle"`))
	if err := srv.bridge.PublishToHost(context.Background(), "dev", gen, env); err != nil {
		t.Fatal(err)
	}
	for {
		got := next("the event")
		if got == ": keepalive" || got == "event: message" {
			continue
		}
		if !strings.Contains(got, "after-idle") {
			t.Fatalf("event = %q", got)
		}
		break
	}
}

// TestSourceKeepAliveConnectionReusableAfterAStream: over HTTP/1.1, a stream
// that was idle past its write deadline and then ends (here its listener is
// unregistered, as a move or shutdown does) ends cleanly, and the keep-alive
// connection serves another request: no deadline is left armed on it.
func TestSourceKeepAliveConnectionReusableAfterAStream(t *testing.T) {
	shortStreamTimers(t, 150*time.Millisecond, time.Minute)
	srv := newTestServerWithPlugins()
	ts := httptest.NewUnstartedServer(srv.Router())
	ts.Config.ConnContext = ConnContext
	ts.Start()
	t.Cleanup(ts.Close)
	key := plugin.ListenerKey{Namespace: "op"}
	m := testMachineFor("")

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	br := bufio.NewReader(conn)
	get := func(path string, hdr ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://x"+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if err := req.Write(conn); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			t.Fatalf("reading the answer to %s on the reused connection: %v", path, err)
		}
		return resp
	}

	resp := get("/api/plugins/listeners/op/messages", HeaderMachineID, m.ID, HeaderMachineName, m.Name)
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("subscribe = %d (close %v), want 200 on a keep-alive connection", resp.StatusCode, resp.Close)
	}
	waitListener(t, srv, key)
	// The stream stays idle past the write deadline its last write (the
	// headers) was given, then ends: the end of its body is written after
	// that deadline, and net/http clears the connection's deadline only once
	// it has written it.
	time.Sleep(3 * sseWriteTimeout)
	l, _ := srv.plugins.Get(key)
	srv.plugins.Unregister(key, l.Token)
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("the stream did not end cleanly: %v", err)
	}
	resp = get("/api/plugins/listeners")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the next request on the connection = %d %s, want 200", resp.StatusCode, body)
	}
}
