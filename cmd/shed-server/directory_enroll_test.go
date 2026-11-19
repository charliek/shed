package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/directory/refserver"
)

// The tests of `shed-server directory enroll` run the command's own code
// (directoryEnroll, loading the config from --config as the command does)
// against the reference server of the directory contract, in-process, over
// loopback plain HTTP (auth.directory.allow_plain_http: true).

const enrolTestName = "build-1.example.test"

// refService is a reference server on a loopback port: its control
// directory (or in-memory inputs), the client ids it enrolled, and the
// enrolment requests that reached it.
type refService struct {
	t        *testing.T
	issuer   string
	control  string
	mu       sync.Mutex
	enrolled []string
	requests atomic.Int32
}

type refOptions struct {
	// host is the issuer's host as the service names it; the server always
	// listens on 127.0.0.1. Default 127.0.0.1.
	host string
	// inputs replace the control directory.
	inputs refserver.Inputs
	// wrap wraps the server's handler.
	wrap   func(http.Handler) http.Handler
	limits refserver.Limits
}

func newRefService(t *testing.T, o refOptions) *refService {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := o.host
	if host == "" {
		host = "127.0.0.1"
	}
	r := &refService{t: t, control: t.TempDir()}
	r.issuer = fmt.Sprintf("http://%s:%d", host, ln.Addr().(*net.TCPAddr).Port)
	inputs := o.inputs
	if inputs == nil {
		cd, err := refserver.OpenControlDir(r.control)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cd.Close() })
		inputs = cd
	}
	srv, err := refserver.New(refserver.Config{
		Issuer: r.issuer, Organization: "example", Inputs: inputs, Limits: o.limits,
		OnEnrol: func(_, clientID string) {
			r.mu.Lock()
			r.enrolled = append(r.enrolled, clientID)
			r.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler = srv
	if o.wrap != nil {
		h = o.wrap(h)
	}
	counted := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/servers/enroll" {
			r.requests.Add(1)
		}
		h.ServeHTTP(w, req)
	})
	ts := httptest.NewUnstartedServer(counted)
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return r
}

// write writes a file of the control directory.
func (r *refService) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.control, name), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *refService) enrolledIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.enrolled...)
}

// memInputs are in-memory inputs: codes with the states a control directory
// cannot express, and an outage switch.
type memInputs struct {
	codes  map[string]refserver.Code
	outage bool
}

func (m *memInputs) Users() ([]refserver.User, error) {
	if m.outage {
		return nil, refserver.ErrOutage
	}
	return []refserver.User{}, nil
}

func (m *memInputs) Code(code string) (refserver.Code, bool, error) {
	if m.outage {
		return refserver.Code{}, false, refserver.ErrOutage
	}
	c, ok := m.codes[code]
	return c, ok, nil
}

func (m *memInputs) Disabled(string) (bool, error) {
	if m.outage {
		return false, refserver.ErrOutage
	}
	return false, nil
}

// enrolConfig writes a server config whose auth.directory names issuer
// (plain HTTP allowed), with organization org when not "", and a state
// directory that does not exist yet; it points --config at it and returns
// the state directory.
func enrolConfig(t *testing.T, issuer, org string) string {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	enrolConfigAt(t, stateDir, issuer, org)
	return stateDir
}

func enrolConfigAt(t *testing.T, stateDir, issuer, org string) {
	t.Helper()
	adminLine, _ := wiringKey(t)
	auth := "state_dir: " + stateDir + "\n" +
		"auth:\n  mode: mtls\n  users:\n    admin:\n      admin: true\n      keys:\n        - {key: \"" + adminLine + "\", label: laptop}\n" +
		"  directory:\n    url: " + issuer + "\n    allow_plain_http: true\n"
	if org != "" {
		auth += "    organization: " + org + "\n"
	}
	writeServeConfig(t, auth)
}

// countingReader counts the bytes read from it.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// enrolRun is one run of the command: what it printed and its error.
type enrolRun struct {
	out, errOut string
	err         error
}

// text is everything the run showed: its output, its prompt stream and its
// error.
func (r enrolRun) text() string {
	s := r.out + r.errOut
	if r.err != nil {
		s += r.err.Error()
	}
	return s
}

// runEnrol runs the command with stdin and --name name (unset when "").
func runEnrol(stdin io.Reader, name string) enrolRun {
	return runEnrolCtx(context.Background(), stdin, name)
}

// runEnrolCtx is runEnrol under ctx (an interrupt cancels it).
func runEnrolCtx(ctx context.Context, stdin io.Reader, name string) enrolRun {
	var out, errOut bytes.Buffer
	err := directoryEnroll(ctx, enrolStreams{in: stdin, out: &out, errOut: &errOut}, name, name != "")
	return enrolRun{out: out.String(), errOut: errOut.String(), err: err}
}

// enrolBody is the code an enrolment request carried.
func enrolBody(t *testing.T, req *http.Request) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Errorf("an enrolment request: %v", err)
	}
	return body.Code
}

// writeJSON writes status and v as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// linesWith is the lines of s that contain sub.
func linesWith(s, sub string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// captureEnrolLog sends the standard logger to a buffer for the test.
func captureEnrolLog(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return b
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// checkNoSecrets fails when a code or a private key appears in any text
// given, or in any file under the state directory other than
// enrolment.json, or when a code appears in enrolment.json.
func checkNoSecrets(t *testing.T, stateDir string, codes []string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		for _, c := range codes {
			if strings.Contains(s, c) {
				t.Errorf("the code %q appears in %q", c, s)
			}
		}
		if strings.Contains(s, "PRIVATE KEY") {
			t.Errorf("a private key appears in %q", s)
		}
	}
	err := filepath.WalkDir(stateDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == stateDir {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, c := range codes {
			if bytes.Contains(data, []byte(c)) {
				t.Errorf("the code %q appears in %s", c, path)
			}
		}
		if path != filepath.Join(stateDir, directory.StateDirName, directory.EnrolmentFileName) && bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Errorf("a private key appears in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// dirEntries lists <state>/directory, nil when it does not exist.
func dirEntries(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, directory.StateDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// noEnrolment fails unless <state>/directory holds no enrolment.json and no
// temporary file: nothing was written.
func noEnrolment(t *testing.T, stateDir string) {
	t.Helper()
	for _, n := range dirEntries(t, stateDir) {
		if n != enrolLockName {
			t.Errorf("%s holds %s; want nothing but the lock", filepath.Join(stateDir, directory.StateDirName), n)
		}
	}
}

// fileUID is the owner fi names, -1 when it names none.
func fileUID(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

func fileMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func readEnrolmentFile(t *testing.T, stateDir string) *directory.Enrolment {
	t.Helper()
	data, err := directory.ReadStateFile(filepath.Join(stateDir, directory.StateDirName), directory.EnrolmentFileName, directory.MaxEnrolmentFileSize)
	if err != nil {
		t.Fatal(err)
	}
	e, err := directory.ParseEnrolmentFile(data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustContain(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s %q does not contain %q", what, s, w)
		}
	}
}

func mustNotContain(t *testing.T, what, s string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(s, w) {
			t.Errorf("%s %q contains %q", what, s, w)
		}
	}
}

// TestDirectoryEnrollEnrols: an enrolment with a code on standard input
// prints the state directory, makes it (0700) and <state>/directory (0700),
// and writes enrolment.json (0600) holding the client id the service
// enrolled, the configured issuer, the organization and the name, and the
// key the service enrolled (a pull with it succeeds). A second run, with a
// new code, is refused naming the client id, before reading the code or
// sending anything. No code or key is printed, and no file but
// enrolment.json holds a key.
func TestDirectoryEnrollEnrols(t *testing.T) {
	logs := captureEnrolLog(t)
	r := newRefService(t, refOptions{})
	r.write("users.json", "[]")
	r.write("codes", "build-1 code-one-a1b2c3\nbuild-1b code-two-d4e5f6\n")
	stateDir := enrolConfig(t, r.issuer, "")

	run := runEnrol(strings.NewReader("code-one-a1b2c3\n"), enrolTestName)
	if run.err != nil {
		t.Fatalf("enrol: %v\n%s", run.err, run.out)
	}
	ids := r.enrolledIDs()
	if len(ids) != 1 {
		t.Fatalf("the service enrolled %v", ids)
	}
	id := ids[0]
	dir := filepath.Join(stateDir, directory.StateDirName)
	path := filepath.Join(dir, directory.EnrolmentFileName)
	mustContain(t, "output", run.out, "State directory: "+stateDir+"\n", "as "+id, "Wrote "+path+" (mode 0600)")
	// After the state directory, exactly three lines: the enrolment, the
	// file, and what a server already running does with it.
	lines := strings.Split(strings.TrimSuffix(run.out, "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "State directory: ") ||
		!strings.HasPrefix(lines[1], "Enrolled with the account service at ") ||
		!strings.HasPrefix(lines[2], "Wrote "+path+" ") || lines[3] != runningServerNote() {
		t.Errorf("output %q; want the state directory, then the enrolment, the file and the running-server note %q",
			run.out, runningServerNote())
	}
	if run.errOut != "" {
		t.Errorf("standard error %q; want nothing (standard input is not a terminal)", run.errOut)
	}

	for _, c := range []struct {
		path string
		want fs.FileMode
	}{{stateDir, fs.ModeDir | 0o700}, {dir, fs.ModeDir | 0o700}, {path, 0o600}, {filepath.Join(dir, enrolLockName), 0o600}} {
		if m := fileMode(t, c.path); m != c.want {
			t.Errorf("%s has mode %v, want %v", c.path, m, c.want)
		}
	}
	if got := dirEntries(t, stateDir); !slices.Equal(got, []string{enrolLockName, directory.EnrolmentFileName}) {
		t.Errorf("%s holds %v", dir, got)
	}
	e := readEnrolmentFile(t, stateDir)
	if e.ClientID != id || e.Issuer != r.issuer || e.TokenEndpoint != r.issuer+"/oauth2/token" ||
		e.Organization != "example" || e.Name != enrolTestName {
		t.Errorf("enrolment.json: %+v", e)
	}
	// The key written is the one the service enrolled.
	cl, err := directory.NewClient(directory.ClientConfig{Enrolment: e, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cl.Pull(ctx); err != nil {
		t.Errorf("a pull with the key written: %v", err)
	}

	// A second run is refused, naming the client id, before reading the code
	// or sending anything; the file is unchanged.
	before, _ := os.ReadFile(path)
	stdin := &countingReader{r: strings.NewReader("code-two-d4e5f6\n")}
	second := runEnrol(stdin, enrolTestName)
	if second.err == nil {
		t.Fatal("a second enrolment succeeded")
	}
	mustContain(t, "the second run's error", second.err.Error(), "already enrolled, as "+id, "remove "+path)
	if n := stdin.n.Load(); n != 0 {
		t.Errorf("the second run read %d bytes of standard input", n)
	}
	if n := r.requests.Load(); n != 1 {
		t.Errorf("%d enrolment requests reached the service, want 1", n)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the second run changed enrolment.json")
	}
	checkNoSecrets(t, stateDir, []string{"code-one-a1b2c3", "code-two-d4e5f6"}, run.text(), second.text(), logs.String())
}

// TestEnrolNoteInGuide: the running-server note is what the code does, and
// every copy the docs show of it is the line the command prints. A server
// that found no enrolment tries again RetryInterval later (notEnrolled),
// which the note states in minutes.
func TestEnrolNoteInGuide(t *testing.T) {
	note := runningServerNote()
	if want := fmt.Sprintf("at most %d minutes from now", int(directory.RetryInterval/time.Minute)); !strings.Contains(note, want) {
		t.Errorf("note %q does not contain %q", note, want)
	}
	if directory.RetryInterval%time.Minute != 0 {
		t.Errorf("RetryInterval %s is not whole minutes; the note would understate it", directory.RetryInterval)
	}
	// The note's opening words find every copy, a corrupted one included.
	const opening = "If the server is already running with this config,"
	if !strings.HasPrefix(note, opening) {
		t.Fatalf("note %q does not start with %q; update this test", note, opening)
	}
	for _, doc := range []struct {
		path   []string
		copies int // the guide shows it under both tabs of "Enrol"
	}{
		{[]string{"docs", "guides", "user-directory.md"}, 2},
		{[]string{"docs", "reference", "cli.md"}, 1},
	} {
		rel := filepath.Join(doc.path...)
		data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, doc.path...)...))
		if err != nil {
			t.Fatal(err)
		}
		// A line of its own, indented or not (an output block inside a tab
		// is indented).
		found := 0
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, opening) {
				continue
			}
			found++
			if line != note {
				t.Errorf("%s:%d quotes the enrolment's last line as\n%s\nbut the command prints\n%s", rel, i+1, line, note)
			}
		}
		if found != doc.copies {
			t.Errorf("%s has %d copies of the enrolment's last line, want %d:\n%s", rel, found, doc.copies, note)
		}
	}
}

// TestDirectoryEnrollRefused: every 4xx the service gives a well-formed
// enrolment is a refusal: the error says so with the service's code,
// nothing is written, no client id is offered to disable, and no code
// appears anywhere.
func TestDirectoryEnrollRefused(t *testing.T) {
	logs := captureEnrolLog(t)
	in := &memInputs{codes: map[string]refserver.Code{
		"code-spent-111111":     {ServerName: "build-a"},
		"code-taken-a-222222":   {ServerName: "build-b"},
		"code-taken-b-333333":   {ServerName: "build-b"},
		"code-cancelled-444444": {ServerName: "build-c", Cancelled: true},
		"code-expired-555555":   {ServerName: "build-d", ExpiresAt: time.Now().Add(-time.Hour)},
	}}
	r := newRefService(t, refOptions{inputs: in})
	var codes, texts []string

	// Set-up enrolments, each from a state directory of its own.
	for _, code := range []string{"code-spent-111111", "code-taken-a-222222"} {
		enrolConfig(t, r.issuer, "")
		run := runEnrol(strings.NewReader(code+"\n"), enrolTestName)
		if run.err != nil {
			t.Fatalf("set-up enrolment with %s: %v", code, run.err)
		}
		codes, texts = append(codes, code), append(texts, run.text())
	}

	for _, tc := range []struct {
		name, code, want string
	}{
		{"unknown code", "code-unknown-666666", `(HTTP 401); nothing was written.` + "\n" + `The service's words: code "ENROLMENT_CODE_INVALID"`},
		{"spent code", "code-spent-111111", `(HTTP 409); nothing was written.` + "\n" + `The service's words: code "ENROLMENT_CODE_SPENT"`},
		{"name in use", "code-taken-b-333333", `(HTTP 409); nothing was written.` + "\n" + `The service's words: code "SERVER_NAME_IN_USE"`},
		{"cancelled code", "code-cancelled-444444", `(HTTP 410); nothing was written.` + "\n" + `The service's words: code "ENROLMENT_CODE_CANCELLED"`},
		{"expired code", "code-expired-555555", `(HTTP 410); nothing was written.` + "\n" + `The service's words: code "ENROLMENT_CODE_EXPIRED"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := enrolConfig(t, r.issuer, "")
			before := len(r.enrolledIDs())
			run := runEnrol(strings.NewReader(tc.code+"\n"), enrolTestName)
			if run.err == nil {
				t.Fatal("enrolled")
			}
			mustContain(t, "error", run.err.Error(), "the account service refused the enrolment", tc.want)
			mustNotContain(t, "error", run.err.Error(), "disable", "srv_")
			noEnrolment(t, stateDir)
			if len(r.enrolledIDs()) != before {
				t.Error("the service enrolled a server")
			}
			checkNoSecrets(t, stateDir, []string{tc.code}, run.text())
			codes, texts = append(codes, tc.code), append(texts, run.text())
		})
	}

	// Rate limited: a 429 is a refusal too.
	t.Run("rate limited", func(t *testing.T) {
		limited := newRefService(t, refOptions{inputs: in, limits: refserver.Limits{Enroll: 1}})
		enrolConfig(t, limited.issuer, "")
		first := runEnrol(strings.NewReader("code-unknown-777777\n"), enrolTestName)
		if first.err == nil {
			t.Fatal("enrolled with an unknown code")
		}
		stateDir := enrolConfig(t, limited.issuer, "")
		run := runEnrol(strings.NewReader("code-cancelled-444444\n"), enrolTestName)
		if run.err == nil {
			t.Fatal("enrolled")
		}
		mustContain(t, "error", run.err.Error(), "refused the enrolment (HTTP 429)", `The service's words: code "RATE_LIMITED"`)
		mustNotContain(t, "error", run.err.Error(), "disable")
		noEnrolment(t, stateDir)
		codes, texts = append(codes, "code-unknown-777777"), append(texts, first.text(), run.text())
	})
	checkNoSecrets(t, t.TempDir(), codes, append(texts, logs.String())...)
}

// TestDirectoryEnrollLostAnswer: a 503, a timeout, and a connection lost
// after the service enrolled the server are lost answers: the key is
// discarded, nothing is written, and the error gives the recovery in
// neutral words.
func TestDirectoryEnrollLostAnswer(t *testing.T) {
	logs := captureEnrolLog(t)
	prev := enrolTimeout
	enrolTimeout = 500 * time.Millisecond
	t.Cleanup(func() { enrolTimeout = prev })
	const code = "code-lost-9a9a9a"
	recovery := []string{
		"answer to the enrolment was lost", "Nothing was written and the key was discarded",
		"find the server enrolled with this code that this server does not know and disable it",
		"issue a new code", "run 'shed-server directory enroll' again",
	}

	// release ends the hanging handler once the command has given up, so
	// the test server can close.
	release := make(chan struct{})
	stopHanging := sync.OnceFunc(func() { close(release) })
	t.Cleanup(stopHanging)
	for _, tc := range []struct {
		name     string
		opts     refOptions
		want     string
		enrolled int // servers the service enrolled
	}{
		{"503", refOptions{inputs: &memInputs{outage: true}}, `was lost (HTTP 503).`, 0},
		{"timeout", refOptions{wrap: func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/v1/servers/enroll" {
					_, _ = io.Copy(io.Discard, req.Body)
					select {
					case <-req.Context().Done():
					case <-release:
					}
					return
				}
				h.ServeHTTP(w, req)
			})
		}}, "deadline exceeded", 0},
		{"connection lost after the service enrolled", refOptions{wrap: func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				// The service enrols, then the connection drops before the
				// answer is sent.
				h.ServeHTTP(httptest.NewRecorder(), req)
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					conn.Close()
				}
			})
		}}, "The service's words: error ", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefService(t, tc.opts)
			r.write("codes", "build-1 "+code+"\n")
			stateDir := enrolConfig(t, r.issuer, "")
			run := runEnrol(strings.NewReader(code+"\n"), enrolTestName)
			if tc.name == "timeout" {
				stopHanging()
			}
			if run.err == nil {
				t.Fatal("enrolled")
			}
			mustContain(t, "error", run.err.Error(), append([]string{tc.want}, recovery...)...)
			mustNotContain(t, "error", run.err.Error(), "srv_")
			// Each case reached the service: a failure before it (a refused
			// connection) would read the same.
			if n := r.requests.Load(); n != 1 {
				t.Errorf("%d enrolment requests reached the service, want 1", n)
			}
			noEnrolment(t, stateDir)
			if n := len(r.enrolledIDs()); n != tc.enrolled {
				t.Errorf("the service enrolled %d servers, want %d", n, tc.enrolled)
			}
			checkNoSecrets(t, stateDir, []string{code}, run.text(), logs.String())
		})
	}
}

// TestDirectoryEnrollAnswerFailsItsChecks: a 201 naming another issuer than
// the configured one, or another organization than the configured one, is
// refused after the service enrolled the server: nothing is written, and the
// error names the client id the service enrolled, to disable.
func TestDirectoryEnrollAnswerFailsItsChecks(t *testing.T) {
	logs := captureEnrolLog(t)
	const code = "code-check-7b7b7b"
	for _, tc := range []struct {
		name, host, org, want string
	}{
		{"another issuer", "localhost", "", "issuer"},
		{"another organization", "", "other", "organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefService(t, refOptions{host: tc.host})
			r.write("codes", "build-1 "+code+"\n")
			// The config names the service by 127.0.0.1; with host
			// localhost the service names itself otherwise.
			issuer := strings.Replace(r.issuer, "localhost", "127.0.0.1", 1)
			stateDir := enrolConfig(t, issuer, tc.org)
			run := runEnrol(strings.NewReader(code+"\n"), enrolTestName)
			if run.err == nil {
				t.Fatal("enrolled")
			}
			ids := r.enrolledIDs()
			if len(ids) != 1 {
				t.Fatalf("the service enrolled %v", ids)
			}
			mustContain(t, "error", run.err.Error(),
				"enrolled this server as "+ids[0]+", but its answer failed a check", "\nThe service's words: error \"", tc.want,
				"\nTo recover: disable "+ids[0]+" on the account service", "Nothing was written")
			noEnrolment(t, stateDir)
			checkNoSecrets(t, stateDir, []string{code}, run.text(), logs.String())
		})
	}
}

// TestDirectoryEnrollTwoRunsAtOnce: while one run holds the lock, a second
// is refused before it reads its code or sends anything, and the first
// enrols; of runs started together exactly one enrols, and the service
// receives exactly one enrolment request.
func TestDirectoryEnrollTwoRunsAtOnce(t *testing.T) {
	logs := captureEnrolLog(t)
	t.Run("one waits inside the lock", func(t *testing.T) {
		r := newRefService(t, refOptions{})
		r.write("codes", "build-1 code-first-1c1c1c\nbuild-2 code-second-2d2d2d\n")
		stateDir := enrolConfig(t, r.issuer, "")

		var calls atomic.Int32
		secondDone := make(chan struct{})
		var second enrolRun
		secondIn := &countingReader{r: strings.NewReader("code-second-2d2d2d\n")}
		enrolHookLocked = func() {
			if calls.Add(1) != 1 {
				return
			}
			// The first run, inside the lock: the second runs to its end.
			go func() {
				second = runEnrol(secondIn, "build-2.example.test")
				close(secondDone)
			}()
			select {
			case <-secondDone:
			case <-time.After(20 * time.Second):
				t.Error("the second run did not end while the first held the lock")
			}
		}
		t.Cleanup(func() { enrolHookLocked = nil })
		first := runEnrol(strings.NewReader("code-first-1c1c1c\n"), enrolTestName)
		// The second run was started only if the first reached the hook.
		if calls.Load() == 0 {
			t.Fatalf("the first run ended before it held the lock: %v", first.err)
		}
		select {
		case <-secondDone:
		case <-time.After(20 * time.Second):
			t.Fatal("the second run did not end")
		}
		if first.err != nil {
			t.Fatalf("the first run: %v", first.err)
		}
		if second.err == nil {
			t.Fatal("the second run enrolled too")
		}
		mustContain(t, "the second run's error", second.err.Error(), "another enrolment is running", "nothing was sent")
		if n := secondIn.n.Load(); n != 0 {
			t.Errorf("the second run read %d bytes of standard input", n)
		}
		ids := r.enrolledIDs()
		if n := r.requests.Load(); n != 1 || len(ids) != 1 {
			t.Fatalf("%d enrolment requests, enrolled %v; want one", n, ids)
		}
		if e := readEnrolmentFile(t, stateDir); e.ClientID != ids[0] {
			t.Errorf("enrolment.json holds %s, the service enrolled %s", e.ClientID, ids[0])
		}
		checkNoSecrets(t, stateDir, []string{"code-first-1c1c1c", "code-second-2d2d2d"}, first.text(), second.text(), logs.String())
	})

	t.Run("started together", func(t *testing.T) {
		r := newRefService(t, refOptions{})
		const n = 6
		var codes []string
		var lines strings.Builder
		for i := range n {
			c := fmt.Sprintf("code-together-%d-%06d", i, i)
			codes = append(codes, c)
			fmt.Fprintf(&lines, "build-t%d %s\n", i, c)
		}
		r.write("codes", lines.String())
		stateDir := enrolConfig(t, r.issuer, "")
		runs := make([]enrolRun, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				runs[i] = runEnrol(strings.NewReader(codes[i]+"\n"), fmt.Sprintf("build-t%d.example.test", i))
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		var texts []string
		for i, run := range runs {
			texts = append(texts, run.text())
			if run.err == nil {
				won++
				continue
			}
			if !strings.Contains(run.err.Error(), "another enrolment is running") && !strings.Contains(run.err.Error(), "already enrolled") {
				t.Errorf("run %d: %v", i, run.err)
			}
		}
		ids := r.enrolledIDs()
		if won != 1 || len(ids) != 1 || r.requests.Load() != 1 {
			t.Fatalf("%d runs enrolled, the service enrolled %v from %d requests; want one", won, ids, r.requests.Load())
		}
		if e := readEnrolmentFile(t, stateDir); e.ClientID != ids[0] {
			t.Errorf("enrolment.json holds %s, the service enrolled %s", e.ClientID, ids[0])
		}
		checkNoSecrets(t, stateDir, codes, append(texts, logs.String())...)
	})
}

// TestDirectoryEnrollStateDirectory: the command refuses, before sending
// anything, a state directory owned by another user, a <state>/directory
// whose mode is not 0700, and an enrolment.json that is not one; it prints
// the state directory's path each time.
func TestDirectoryEnrollStateDirectory(t *testing.T) {
	r := newRefService(t, refOptions{})
	r.write("codes", "build-1 code-state-3e3e3e\n")
	stdin := func() io.Reader { return strings.NewReader("code-state-3e3e3e\n") }

	t.Run("owned by another user", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("this case does not run as root: root owns /, and no other user's directory is at hand to refuse")
		}
		fi, err := os.Stat("/")
		if err != nil {
			t.Fatal(err)
		}
		if uid := fileUID(fi); uid == os.Geteuid() {
			t.Skip("/ is owned by this user")
		}
		enrolConfigAt(t, "/", r.issuer, "")
		run := runEnrol(stdin(), enrolTestName)
		if run.err == nil {
			t.Fatal("enrolled into /")
		}
		mustContain(t, "output", run.out, "State directory: /\n")
		mustContain(t, "error", run.err.Error(), "state dir / is owned by uid", "not by the user running this command", "run it as the user the server runs as")
	})

	t.Run("directory mode 0750", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		dir := filepath.Join(stateDir, directory.StateDirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		enrolConfigAt(t, stateDir, r.issuer, "")
		run := runEnrol(stdin(), enrolTestName)
		if run.err == nil {
			t.Fatal("enrolled")
		}
		mustContain(t, "output", run.out, "State directory: "+stateDir+"\n")
		mustContain(t, "error", run.err.Error(), dir+": has mode 0750, not 0700")
		noEnrolment(t, stateDir)
	})

	t.Run("enrolment.json not an enrolment", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		dir := filepath.Join(stateDir, directory.StateDirName)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := directory.EnsureStateDir(dir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, directory.EnrolmentFileName)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		enrolConfigAt(t, stateDir, r.issuer, "")
		run := runEnrol(stdin(), enrolTestName)
		if run.err == nil {
			t.Fatal("enrolled")
		}
		mustContain(t, "error", run.err.Error(), path+" is there but cannot be used: it is not a complete enrolment",
			"Nothing was removed and no code was read", "remove the file by hand")
	})

	t.Run("enrolment.json readable by the group", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		dir := filepath.Join(stateDir, directory.StateDirName)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := directory.EnsureStateDir(dir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, directory.EnrolmentFileName)
		plantEnrolment(t, dir, directory.EnrolmentFileName, r.issuer, "srv_0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d")
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		enrolConfigAt(t, stateDir, r.issuer, "")
		in := &countingReader{r: stdin()}
		run := runEnrol(in, enrolTestName)
		if run.err == nil {
			t.Fatal("enrolled")
		}
		mustContain(t, "error", run.err.Error(), path+" is there but cannot be used", "has mode 0640", "no code was read")
		if n := in.n.Load(); n != 0 {
			t.Errorf("read %d bytes of standard input", n)
		}
		if m := fileMode(t, path); m != 0o640 {
			t.Errorf("enrolment.json's mode was changed to %v", m)
		}
	})

	if n := r.requests.Load(); n != 0 {
		t.Errorf("%d enrolment requests reached the service, want none", n)
	}
}

// TestDirectoryEnrollLeavesAnExistingStateDirectory: a state directory
// that exists is used as it is: its mode is not changed.
func TestDirectoryEnrollLeavesAnExistingStateDirectory(t *testing.T) {
	r := newRefService(t, refOptions{})
	r.write("codes", "build-1 code-keep-1f1f1f\n")
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o751); err != nil {
		t.Fatal(err)
	}
	enrolConfigAt(t, stateDir, r.issuer, "")
	if run := runEnrol(strings.NewReader("code-keep-1f1f1f\n"), enrolTestName); run.err != nil {
		t.Fatalf("enrol: %v", run.err)
	}
	if m := fileMode(t, stateDir); m != fs.ModeDir|0o751 {
		t.Errorf("the state directory's mode is %v, want it left as 0751", m)
	}
}

// TestDirectoryEnrollTakesNoCodeArgument: the command takes no argument
// and no flag but --name (and the global --config): there is no way to put
// the code on its command line.
func TestDirectoryEnrollTakesNoCodeArgument(t *testing.T) {
	if err := directoryEnrollCmd.Args(directoryEnrollCmd, []string{"code-arg-2a2a2a"}); err == nil {
		t.Error("an argument was accepted")
	}
	for _, line := range strings.Split(strings.TrimSpace(directoryEnrollCmd.LocalNonPersistentFlags().FlagUsages()), "\n") {
		if f := strings.Fields(line); len(f) == 0 || (f[0] != "--name" && f[0] != "-h," && f[0] != "--help") {
			t.Errorf("a flag other than --name: %q", line)
		}
	}
	if directoryEnrollCmd.Flags().Lookup("name") == nil {
		t.Error("no --name")
	}
}

// TestDirectoryEnrollCodeBounds: a code of 64 bytes enrols; one of 65 bytes
// or more is refused having read 65 bytes, before anything is sent; the
// line is trimmed, and nothing past it is read.
func TestDirectoryEnrollCodeBounds(t *testing.T) {
	logs := captureEnrolLog(t)
	code64 := "c" + strings.Repeat("0123456789abcdef", 4)[1:]
	code65 := code64 + "x"
	r := newRefService(t, refOptions{})
	r.write("codes", "build-1 "+code64+"\nbuild-2 "+code65+"\n")

	stateDir := enrolConfig(t, r.issuer, "")
	run := runEnrol(strings.NewReader(code64+"\n"), enrolTestName)
	if run.err != nil {
		t.Fatalf("a 64-byte code: %v", run.err)
	}
	checkNoSecrets(t, stateDir, []string{code64}, run.text(), logs.String())

	for _, tc := range []struct {
		name  string
		stdin string
	}{
		{"65 bytes and a newline", code65 + "\n"},
		{"65 bytes, no newline", code65},
		{"much longer", strings.Repeat(code65, 8) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := enrolConfig(t, r.issuer, "")
			in := &countingReader{r: strings.NewReader(tc.stdin)}
			run := runEnrol(in, enrolTestName)
			if !errors.Is(run.err, errCodeTooLong) {
				t.Fatalf("err %v, want the code refused as too long", run.err)
			}
			if n := in.n.Load(); n != maxCodeRead {
				t.Errorf("read %d bytes of standard input, want %d", n, maxCodeRead)
			}
			noEnrolment(t, stateDir)
			checkNoSecrets(t, stateDir, []string{code65}, run.text())
		})
	}
	if n := r.requests.Load(); n != 1 {
		t.Errorf("%d enrolment requests reached the service, want 1", n)
	}
}

// TestReadEnrolmentCode: one line, trimmed, read a byte at a time so nothing
// after the newline is consumed; empty, blank, over-long and invalid UTF-8
// codes refused.
func TestReadEnrolmentCode(t *testing.T) {
	for _, tc := range []struct {
		name, in, want, err string
		read                int
	}{
		{"a line", "abc\nrest", "abc", "", 4},
		{"no newline", "abc", "abc", "", 3},
		{"trimmed", " \tabc \r\n", "abc", "", 8},
		{"64 bytes with spaces", strings.Repeat("a", 62) + "  \n", strings.Repeat("a", 62), "", 65},
		{"empty", "", "", "no enrolment code", 0},
		{"blank line", "   \n", "", "no enrolment code", 4},
		{"invalid UTF-8", "ab\xffcd\n", "", "1 to 64 bytes of UTF-8", 6},
		{"65 bytes", strings.Repeat("a", 65) + "\n", "", "longer than 64 bytes", 65},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &countingReader{r: strings.NewReader(tc.in)}
			var prompt bytes.Buffer
			got, err := readEnrolmentCode(context.Background(), in, &prompt)
			switch {
			case tc.err == "" && (err != nil || got != tc.want):
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("got %q, %v; want an error with %q", got, err, tc.err)
			}
			if n := in.n.Load(); n != int64(tc.read) {
				t.Errorf("read %d bytes, want %d", n, tc.read)
			}
			if prompt.Len() != 0 {
				t.Errorf("prompted %q though standard input is not a terminal", prompt.String())
			}
		})
	}

	// A reader that gives no byte and no error, again and again, is given up
	// on rather than read forever.
	t.Run("nothing, again and again", func(t *testing.T) {
		in := &emptyReader{}
		done := make(chan error, 1)
		go func() {
			_, err := readEnrolmentCode(context.Background(), in, io.Discard)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, errNoInput) {
				t.Errorf("err %v, want errNoInput", err)
			}
			if n := in.reads.Load(); n != maxEmptyReads {
				t.Errorf("%d reads, want %d", n, maxEmptyReads)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the read never ended")
		}
	})
}

// emptyReader gives no byte and no error, and counts its reads.
type emptyReader struct{ reads atomic.Int32 }

func (e *emptyReader) Read([]byte) (int, error) {
	e.reads.Add(1)
	return 0, nil
}

// TestDirectoryEnrollName: --name defaults to the host name when that is 1
// to 255 printable ASCII characters, and is required otherwise; a --name
// given must be such a name. A refused name sends nothing.
func TestDirectoryEnrollName(t *testing.T) {
	r := newRefService(t, refOptions{})
	r.write("codes", "build-1 code-name-4f4f4f\n")
	setHost := func(name string, err error) {
		prev := enrolHostname
		enrolHostname = func() (string, error) { return name, err }
		t.Cleanup(func() { enrolHostname = prev })
	}

	for _, tc := range []struct {
		name, flag, host string
		set              bool
		hostErr          error
		want             string
	}{
		{"host name not ASCII", "", "bühne", false, nil, "--name is required"},
		{"host name empty", "", "", false, nil, "--name is required"},
		{"host name too long", "", strings.Repeat("h", 256), false, nil, "--name is required"},
		{"host name unreadable", "", "", false, errors.New("no host name"), "--name is required"},
		{"flag empty", "", "build-1.example.test", true, nil, "is not 1 to 255 printable ASCII"},
		{"flag with a tab", "build\t1", "build-1.example.test", true, nil, "is not 1 to 255 printable ASCII"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHost(tc.host, tc.hostErr)
			stateDir := enrolConfig(t, r.issuer, "")
			var out, errOut bytes.Buffer
			err := directoryEnroll(context.Background(), enrolStreams{in: strings.NewReader("code-name-4f4f4f\n"), out: &out, errOut: &errOut}, tc.flag, tc.set)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			noEnrolment(t, stateDir)
		})
	}
	if n := r.requests.Load(); n != 0 {
		t.Fatalf("%d enrolment requests reached the service, want none", n)
	}

	setHost("build-host.example.test", nil)
	stateDir := enrolConfig(t, r.issuer, "")
	if run := runEnrol(strings.NewReader("code-name-4f4f4f\n"), ""); run.err != nil {
		t.Fatalf("enrol under the host name: %v", run.err)
	}
	if e := readEnrolmentFile(t, stateDir); e.Name != "build-host.example.test" {
		t.Errorf("enrolled under %q, want the host name", e.Name)
	}
}

// TestDirectoryEnrollNotConfigured: without auth.directory the command
// refuses, before anything.
func TestDirectoryEnrollNotConfigured(t *testing.T) {
	adminLine, _ := wiringKey(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	writeServeConfig(t, "state_dir: "+stateDir+"\nauth:\n  mode: mtls\n  users:\n    admin:\n      admin: true\n      keys:\n        - {key: \""+adminLine+"\", label: laptop}\n")
	run := runEnrol(strings.NewReader("code\n"), enrolTestName)
	if run.err == nil || !strings.Contains(run.err.Error(), "auth.directory is not configured") {
		t.Fatalf("err %v", run.err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the state directory was made: %v", err)
	}
}

// plantEnrolment writes an enrolment for clientID, as enrolment.json's
// content, to dir/name with mode 0600.
func plantEnrolment(t *testing.T, dir, name, issuer, clientID string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e := &directory.Enrolment{
		ClientID: clientID, Issuer: issuer, TokenEndpoint: issuer + "/oauth2/token",
		Organization: "example", Name: enrolTestName, EnrolledAt: time.Now().UTC().Truncate(time.Second), PrivateKey: priv,
	}
	data, err := e.MarshalFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// TestDirectoryEnrollRemovesLeftovers: a temporary file of enrolment.json
// an earlier run left (it holds a key) that reads as a complete enrolment is
// removed by the next run, under the lock, which says so and names the
// client id it held when this server keeps no key for it (enrolment.json is
// absent or holds another); one that is enrolment.json's own is removed
// naming nothing; files that only resemble one are left alone.
func TestDirectoryEnrollRemovesLeftovers(t *testing.T) {
	logs := captureEnrolLog(t)
	const leftID = "srv_0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e"
	const storedID = "srv_0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c"
	const code = "code-left-5a5a5a"
	r := newRefService(t, refOptions{})
	r.write("codes", "build-1 "+code+"\n")
	left := ".enrolment.json.0123456789abcdef.tmp"
	// The look-alike differs by a digit that is not hex, not by case: on a
	// file system that ignores case (macOS by default) an upper-case spelling
	// of the leftover's name is the leftover itself.
	alike := ".enrolment.json.0123456789abcdeg.tmp"

	// Before a first enrolment: a leftover holding an enrolment (the run
	// stopped after the service answered) and a look-alike.
	stateDir, dir := leftoverStateDir(t, r.issuer)
	plantEnrolment(t, dir, left, r.issuer, leftID)
	if err := os.WriteFile(filepath.Join(dir, alike), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := runEnrol(strings.NewReader(code+"\n"), enrolTestName)
	if run.err != nil {
		t.Fatalf("enrol: %v", run.err)
	}
	mustContain(t, "output", run.out,
		"Removed "+filepath.Join(dir, left)+", a temporary file an earlier enrolment left",
		"It held an enrolment as "+leftID+", for which this server keeps no key: disable "+leftID+" on the account service")
	if got := dirEntries(t, stateDir); !slices.Equal(got, []string{alike, enrolLockName, directory.EnrolmentFileName}) {
		t.Errorf("%s holds %v", dir, got)
	}
	if err := os.Remove(filepath.Join(dir, alike)); err != nil {
		t.Fatal(err)
	}
	checkNoSecrets(t, stateDir, []string{code}, run.text(), logs.String())

	// After an enrolment whose run stopped after the link: the leftover is
	// the enrolment itself. It is removed, nothing is named to disable, and
	// the run is refused as enrolled.
	stateDir, dir = leftoverStateDir(t, r.issuer)
	data := plantEnrolment(t, dir, directory.EnrolmentFileName, r.issuer, storedID)
	if err := os.WriteFile(filepath.Join(dir, left), data, 0o600); err != nil {
		t.Fatal(err)
	}
	run = runEnrol(strings.NewReader(code+"\n"), enrolTestName)
	if run.err == nil {
		t.Fatal("enrolled again")
	}
	mustContain(t, "error", run.err.Error(), "already enrolled, as "+storedID)
	mustContain(t, "output", run.out, "Removed "+filepath.Join(dir, left))
	mustNotContain(t, "output", run.out, "It held an enrolment", "disable "+storedID+" on")
	if got := dirEntries(t, stateDir); !slices.Equal(got, []string{enrolLockName, directory.EnrolmentFileName}) {
		t.Errorf("%s holds %v", dir, got)
	}

	// Beside an enrolment, a leftover holding another: removed, and its
	// client id (not the stored one) named to disable.
	stateDir, dir = leftoverStateDir(t, r.issuer)
	plantEnrolment(t, dir, directory.EnrolmentFileName, r.issuer, storedID)
	plantEnrolment(t, dir, left, r.issuer, leftID)
	run = runEnrol(strings.NewReader(code+"\n"), enrolTestName)
	if run.err == nil {
		t.Fatal("enrolled again")
	}
	mustContain(t, "error", run.err.Error(), "already enrolled, as "+storedID)
	mustContain(t, "output", run.out, "Removed "+filepath.Join(dir, left), "disable "+leftID+" on the account service")
	mustNotContain(t, "output", run.out, "disable "+storedID+" on")
	if got := dirEntries(t, stateDir); !slices.Equal(got, []string{enrolLockName, directory.EnrolmentFileName}) {
		t.Errorf("%s holds %v", dir, got)
	}
	if n := r.requests.Load(); n != 1 {
		t.Errorf("%d enrolment requests reached the service, want 1", n)
	}
}

// leftoverStateDir is a state directory with <state>/directory made, and
// the config pointing at it.
func leftoverStateDir(t *testing.T, issuer string) (stateDir, dir string) {
	t.Helper()
	stateDir = filepath.Join(t.TempDir(), "state")
	dir = filepath.Join(stateDir, directory.StateDirName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := directory.EnsureStateDir(dir); err != nil {
		t.Fatal(err)
	}
	enrolConfigAt(t, stateDir, issuer, "")
	return stateDir, dir
}

// TestDirectoryEnrollKeepsUnreadableLeftovers: a temporary file of
// enrolment.json that cannot be read as a complete enrolment (incomplete,
// a mode that lets others read it, a symbolic link, a directory, a FIFO,
// one holding enrolment.json's client id with another key), or any
// leftover when enrolment.json cannot be read, is not removed: the run
// stops before it reads a code, names the file and says what to do.
func TestDirectoryEnrollKeepsUnreadableLeftovers(t *testing.T) {
	const leftID = "srv_0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e"
	const storedID = "srv_0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c"
	r := newRefService(t, refOptions{})
	left := ".enrolment.json.0123456789abcdef.tmp"
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, dir string)
		want  []string
	}{
		{"incomplete", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, left), []byte(`{"format": 1, "client_id": "`+leftID), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"it is not a complete enrolment"}},
		{"readable by others", func(t *testing.T, dir string) {
			plantEnrolment(t, dir, left, r.issuer, leftID)
			if err := os.Chmod(filepath.Join(dir, left), 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"has mode 0644"}},
		{"a symbolic link", func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "target")
			plantEnrolment(t, filepath.Dir(target), "target", r.issuer, leftID)
			if err := os.Symlink(target, filepath.Join(dir, left)); err != nil {
				t.Fatal(err)
			}
		}, []string{"is a symbolic link"}},
		{"a directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, left), 0o700); err != nil {
				t.Fatal(err)
			}
		}, []string{"is not a regular file"}},
		{"a FIFO", func(t *testing.T, dir string) {
			if err := syscall.Mkfifo(filepath.Join(dir, left), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"is not a regular file"}},
		{"enrolment.json's client id with another key", func(t *testing.T, dir string) {
			plantEnrolment(t, dir, directory.EnrolmentFileName, r.issuer, storedID)
			plantEnrolment(t, dir, left, r.issuer, storedID)
		}, []string{"holds the client id " + storedID + " of enrolment.json, with another key"}},
		{"enrolment.json cannot be read", func(t *testing.T, dir string) {
			plantEnrolment(t, dir, directory.EnrolmentFileName, r.issuer, storedID)
			if err := os.Chmod(filepath.Join(dir, directory.EnrolmentFileName), 0o640); err != nil {
				t.Fatal(err)
			}
			plantEnrolment(t, dir, left, r.issuer, leftID)
		}, []string{"enrolment.json is there but cannot be used", "has mode 0640"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir, dir := leftoverStateDir(t, r.issuer)
			tc.plant(t, dir)
			before := dirEntries(t, stateDir)
			in := &countingReader{r: strings.NewReader("code-never-read-1\n")}
			done := make(chan enrolRun, 1)
			go func() { done <- runEnrol(in, enrolTestName) }()
			var run enrolRun
			select {
			case run = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the run did not end")
			}
			if run.err == nil {
				t.Fatal("enrolled")
			}
			msg := run.err.Error()
			mustContain(t, "error", msg, tc.want...)
			mustContain(t, "error", msg, "Nothing was removed and no code was read", "remove the file by hand")
			if tc.name != "enrolment.json cannot be read" {
				mustContain(t, "error", msg, filepath.Join(dir, left), "find the server on the account service that this server does not know and disable it")
			}
			mustNotContain(t, "output and error", run.text(), "Removed", "disable "+leftID+" on", "disable "+storedID+" on")
			if n := in.n.Load(); n != 0 {
				t.Errorf("read %d bytes of standard input", n)
			}
			after := dirEntries(t, stateDir)
			after = slices.DeleteFunc(after, func(n string) bool { return n == enrolLockName })
			if !slices.Equal(before, after) {
				t.Errorf("%s held %v and now holds %v", dir, before, after)
			}
		})
	}
	if n := r.requests.Load(); n != 0 {
		t.Errorf("%d enrolment requests reached the service, want none", n)
	}
}

// TestDirectoryEnrollWriteFails: when enrolment.json cannot be written
// after the service enrolled the server, the error names the client id to
// disable and nothing is left; when the write failed after the file was
// created holding the enrolment, the error says the server is enrolled.
func TestDirectoryEnrollWriteFails(t *testing.T) {
	logs := captureEnrolLog(t)
	r := newRefService(t, refOptions{})
	r.write("codes", "build-1 code-w1-6b6b6b\nbuild-2 code-w2-7c7c7c\nbuild-3 code-w3-8d8d8d\n")
	setCreate := func(f func(d *directory.StateDir, name string, data []byte) (bool, error)) {
		prev := createEnrolmentFile
		createEnrolmentFile = f
		t.Cleanup(func() { createEnrolmentFile = prev })
	}

	// Before the link: nothing written.
	setCreate(func(*directory.StateDir, string, []byte) (bool, error) {
		return false, errors.New("no space left on device")
	})
	stateDir := enrolConfig(t, r.issuer, "")
	run := runEnrol(strings.NewReader("code-w1-6b6b6b\n"), enrolTestName)
	ids := r.enrolledIDs()
	if run.err == nil || len(ids) != 1 {
		t.Fatalf("err %v, enrolled %v", run.err, ids)
	}
	mustContain(t, "error", run.err.Error(), "enrolled this server as "+ids[0], "was not written (no space left on device)",
		"keeps no key for "+ids[0], "disable "+ids[0]+" on the account service")
	noEnrolment(t, stateDir)
	checkNoSecrets(t, stateDir, []string{"code-w1-6b6b6b"}, run.text(), logs.String())

	// Another file in the way (the link found one): it is left, and the
	// client id is named to disable.
	var other []byte
	setCreate(func(d *directory.StateDir, name string, _ []byte) (bool, error) {
		other = plantEnrolment(t, d.Path(), name, r.issuer, "srv_0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f")
		return false, fs.ErrExist
	})
	stateDir = enrolConfig(t, r.issuer, "")
	run = runEnrol(strings.NewReader("code-w2-7c7c7c\n"), enrolTestName)
	ids = r.enrolledIDs()
	if run.err == nil || len(ids) != 2 {
		t.Fatalf("err %v, enrolled %v", run.err, ids)
	}
	mustContain(t, "error", run.err.Error(), "enrolled this server as "+ids[1], "disable "+ids[1]+" on the account service")
	if got, _ := os.ReadFile(filepath.Join(stateDir, directory.StateDirName, directory.EnrolmentFileName)); !bytes.Equal(got, other) {
		t.Error("the file in the way was changed")
	}

	// After the link: the file holds the enrolment, and a step after the
	// link failed. The command says the server is enrolled with the write
	// not fully confirmed and names nothing to disable, from the link alone:
	// here the file is then made one a read would refuse (mode 0644).
	setCreate(func(d *directory.StateDir, name string, data []byte) (bool, error) {
		if linked, err := d.CreateOnce(name, data); err != nil || !linked {
			return linked, err
		}
		if err := os.Chmod(d.Join(name), 0o644); err != nil {
			t.Fatal(err)
		}
		return true, errors.New("sync directory: input/output error")
	})
	stateDir = enrolConfig(t, r.issuer, "")
	run = runEnrol(strings.NewReader("code-w3-8d8d8d\n"), enrolTestName)
	ids = r.enrolledIDs()
	if run.err == nil || len(ids) != 3 {
		t.Fatalf("err %v, enrolled %v", run.err, ids)
	}
	mustContain(t, "error", run.err.Error(), "enrolled with the account service as "+ids[2]+", and ", "was created holding the enrolment",
		"the write is not fully confirmed (sync directory: input/output error)", "Nothing needs disabling")
	mustNotContain(t, "error", run.err.Error(), "disable "+ids[2], "keeps no key")
	path := filepath.Join(stateDir, directory.StateDirName, directory.EnrolmentFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := directory.ParseEnrolmentFile(data); err != nil || e.ClientID != ids[2] {
		t.Errorf("enrolment.json holds %v, %v; want %s", e, err, ids[2])
	}
	checkNoSecrets(t, stateDir, []string{"code-w3-8d8d8d"}, run.text(), logs.String())
}

// TestDirectoryEnrollInterrupted: an interrupt ends the command promptly
// while it waits for a code on a pipe that never writes, saying nothing was
// sent, and releases the lock; an interrupt after the code is read and
// before the request is sent is "nothing was sent", never a lost answer;
// an interrupt while the request is under way is a lost answer.
func TestDirectoryEnrollInterrupted(t *testing.T) {
	logs := captureEnrolLog(t)
	t.Run("waiting for the code on a pipe", func(t *testing.T) {
		r := newRefService(t, refOptions{})
		r.write("codes", "build-1 code-after-1a1a1a\n")
		stateDir := enrolConfig(t, r.issuer, "")
		pr, pw := io.Pipe()
		t.Cleanup(func() { pw.Close() })
		in := &enteredReader{r: pr, entered: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan enrolRun, 1)
		go func() { done <- runEnrolCtx(ctx, in, enrolTestName) }()
		select {
		case <-in.entered:
		case <-time.After(20 * time.Second):
			t.Fatal("the code was never read")
		}
		cancel()
		var run enrolRun
		select {
		case run = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the interrupt did not end the command")
		}
		if !errors.Is(run.err, errInterrupted) {
			t.Fatalf("err %v, want %v", run.err, errInterrupted)
		}
		mustContain(t, "error", run.err.Error(), "interrupted; nothing was sent")
		noEnrolment(t, stateDir)
		// The lock was released: the next run enrols.
		if next := runEnrol(strings.NewReader("code-after-1a1a1a\n"), enrolTestName); next.err != nil {
			t.Fatalf("the next run: %v", next.err)
		}
		if n := r.requests.Load(); n != 1 {
			t.Errorf("%d enrolment requests reached the service, want 1 (the next run's)", n)
		}
	})

	t.Run("before the request is sent", func(t *testing.T) {
		r := newRefService(t, refOptions{})
		r.write("codes", "build-1 code-before-2b2b2b\n")
		stateDir := enrolConfig(t, r.issuer, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		enrolHookCodeRead = cancel
		t.Cleanup(func() { enrolHookCodeRead = nil })
		run := runEnrolCtx(ctx, strings.NewReader("code-before-2b2b2b\n"), enrolTestName)
		if !errors.Is(run.err, errInterrupted) {
			t.Fatalf("err %v, want %v", run.err, errInterrupted)
		}
		mustNotContain(t, "error", run.err.Error(), "lost", "disable")
		if n := r.requests.Load(); n != 0 {
			t.Errorf("%d enrolment requests reached the service, want none", n)
		}
		noEnrolment(t, stateDir)
		checkNoSecrets(t, stateDir, []string{"code-before-2b2b2b"}, run.text(), logs.String())
	})

	t.Run("while the request is under way", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := newRefService(t, refOptions{wrap: func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/v1/servers/enroll" {
					_, _ = io.Copy(io.Discard, req.Body)
					cancel()
					<-req.Context().Done()
					return
				}
				h.ServeHTTP(w, req)
			})
		}})
		r.write("codes", "build-1 code-during-3c3c3c\n")
		stateDir := enrolConfig(t, r.issuer, "")
		run := runEnrolCtx(ctx, strings.NewReader("code-during-3c3c3c\n"), enrolTestName)
		if run.err == nil {
			t.Fatal("enrolled")
		}
		mustContain(t, "error", run.err.Error(), "answer to the enrolment was lost", "find the server enrolled with this code that this server does not know and disable it")
		if n := r.requests.Load(); n != 1 {
			t.Errorf("%d enrolment requests reached the service, want 1", n)
		}
		noEnrolment(t, stateDir)
		checkNoSecrets(t, stateDir, []string{"code-during-3c3c3c"}, run.text(), logs.String())
	})
}

// enteredReader closes entered when its first Read begins.
type enteredReader struct {
	r       io.Reader
	entered chan struct{}
	once    sync.Once
}

func (e *enteredReader) Read(p []byte) (int, error) {
	e.once.Do(func() { close(e.entered) })
	return e.r.Read(p)
}

// TestDirectoryEnrollRedactsTheCode: nothing the service sends is printed
// with the code in it. A service that reflects the code in a refusal's
// message and code, in a failed 201's issuer (a code with quote marks, so
// its quoted form differs), or as the client id of a failed 201, has it
// replaced by a fixed marker; a client id that holds the code is not named
// to disable. A service whose message carries the words of the guidance and
// a client id cannot make the command's own guidance name it: service text
// stays quoted on its own labelled line. (A 201 that would keep the code is
// TestDirectoryEnrollKeepsNoCode's.)
func TestDirectoryEnrollRedactsTheCode(t *testing.T) {
	logs := captureEnrolLog(t)
	const fakeID = "srv_0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name, code, org string
		answer          func(t *testing.T, issuer string) func(w http.ResponseWriter, code string)
		check           func(t *testing.T, run enrolRun)
	}{
		{name: "a refusal's message and code", code: "code-reflect-4d4d4d",
			answer: func(*testing.T, string) func(http.ResponseWriter, string) {
				return func(w http.ResponseWriter, code string) {
					writeJSON(w, 400, map[string]any{"error": map[string]any{"code": code, "message": "the code " + code + " is not known"}})
				}
			},
			check: func(t *testing.T, run enrolRun) {
				mustContain(t, "error", run.err.Error(), "refused the enrolment (HTTP 400)",
					`The service's words: code "`+codeMarker+`", message "the code `+codeMarker+` is not known"`)
			}},
		{name: "a failed 201's issuer", code: `code-"q"-5e5e5e`,
			answer: func(_ *testing.T, issuer string) func(http.ResponseWriter, string) {
				return func(w http.ResponseWriter, code string) {
					writeJSON(w, 201, map[string]any{"contract": 1, "client_id": fakeID, "issuer": "http://other.example.test/" + code,
						"token_endpoint": issuer + "/oauth2/token", "organization": "example"})
				}
			},
			check: func(t *testing.T, run enrolRun) {
				mustContain(t, "error", run.err.Error(), "enrolled this server as "+fakeID+", but its answer failed a check",
					codeMarker, "\nTo recover: disable "+fakeID+" on the account service")
			}},
		{name: "a failed 201's client id", code: fakeID,
			answer: func(_ *testing.T, issuer string) func(http.ResponseWriter, string) {
				return func(w http.ResponseWriter, code string) {
					writeJSON(w, 201, map[string]any{"contract": 1, "client_id": code, "issuer": "http://other.example.test",
						"token_endpoint": issuer + "/oauth2/token", "organization": "example"})
				}
			},
			check: func(t *testing.T, run enrolRun) {
				mustContain(t, "error", run.err.Error(), "answer to the enrolment was lost",
					"find the server enrolled with this code that this server does not know and disable it")
			}},
		{name: "a message with the guidance's words", code: "code-words-6f6f6f",
			answer: func(*testing.T, string) func(http.ResponseWriter, string) {
				return func(w http.ResponseWriter, _ string) {
					writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "BAD",
						"message": "ok.\nTo recover: disable " + fakeID + " on the account service, issue a new code and enrol again."}})
				}
			},
			check: func(t *testing.T, run enrolRun) {
				msg := run.err.Error()
				mustContain(t, "error", msg, "refused the enrolment (HTTP 400)")
				for _, l := range linesWith(msg, "srv_") {
					if !strings.HasPrefix(l, "The service's words: ") {
						t.Errorf("a line other than the service's words names a client id: %q", l)
					}
				}
				for _, l := range linesWith(msg, "To recover") {
					if strings.HasPrefix(l, "To recover") {
						t.Errorf("the command gave recovery for a refusal: %q", l)
					}
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r *refService
			wrap := func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path != "/v1/servers/enroll" {
						h.ServeHTTP(w, req)
						return
					}
					tc.answer(t, r.issuer)(w, enrolBody(t, req))
				})
			}
			r = newRefService(t, refOptions{wrap: wrap})
			r.write("codes", "build-1 "+tc.code+"\n")
			stateDir := enrolConfig(t, r.issuer, tc.org)
			run := runEnrol(strings.NewReader(tc.code+"\n"), enrolTestName)
			if run.err == nil {
				t.Fatal("enrolled")
			}
			tc.check(t, run)
			quoted := strconv.Quote(tc.code)
			for _, s := range []string{run.text(), logs.String()} {
				if strings.Contains(s, tc.code) || strings.Contains(s, quoted[1:len(quoted)-1]) {
					t.Errorf("the code appears in %q", s)
				}
			}
			noEnrolment(t, stateDir)
		})
	}
}

// TestDirectoryEnrollKeepsNoCode: the code never reaches enrolment.json. A
// 201 that would put it there (the reference server's 201 with a value the
// file keeps replaced by the code) fails its checks: nothing is written, the
// key is discarded, and the client id is named to disable only when it does
// not itself hold the code (else the recovery for an unknown server). A name
// to enrol under that holds the code (--name or the host name) is refused
// before anything is sent, in words that show neither. No file under the
// state directory, no output and no log holds the code afterwards.
func TestDirectoryEnrollKeepsNoCode(t *testing.T) {
	logs := captureEnrolLog(t)
	lost := []string{"answer to the enrolment was lost (HTTP 201)", "Nothing was written and the key was discarded",
		"find the server enrolled with this code that this server does not know and disable it"}
	for _, tc := range []struct {
		name, code, member string
		named              bool // the client id the service enrolled is named to disable
	}{
		// No organization is configured, and the code fits rule 5's grammar.
		{"the organization", "reflect-org-7a7a7a", "organization", true},
		// The code has a client id's form.
		{"the client id", "srv_" + strings.Repeat("7c", 16), "client_id", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrap := func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path != "/v1/servers/enroll" {
						h.ServeHTTP(w, req)
						return
					}
					// The reference server enrols; its 201 is passed on with
					// tc.member replaced by the code.
					body, _ := io.ReadAll(req.Body)
					var sent struct {
						Code string `json:"code"`
					}
					_ = json.Unmarshal(body, &sent)
					req.Body = io.NopCloser(bytes.NewReader(body))
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					var answer map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusCreated {
						t.Errorf("the reference server's answer: %d %s (%v)", rec.Code, rec.Body.Bytes(), err)
					}
					answer[tc.member] = sent.Code
					writeJSON(w, rec.Code, answer)
				})
			}
			r := newRefService(t, refOptions{wrap: wrap})
			r.write("codes", "build-1 "+tc.code+"\n")
			stateDir := enrolConfig(t, r.issuer, "")
			run := runEnrol(strings.NewReader(tc.code+"\n"), enrolTestName)
			checkNoSecrets(t, stateDir, []string{tc.code}, run.text(), logs.String())
			noEnrolment(t, stateDir)
			if run.err == nil {
				t.Fatalf("enrolled: %s", run.out)
			}
			ids := r.enrolledIDs()
			if len(ids) != 1 {
				t.Fatalf("the service enrolled %v", ids)
			}
			msg := run.err.Error()
			mustContain(t, "error", msg, `The service's words: error "its `+tc.member+` holds the enrolment code"`)
			if tc.named {
				mustContain(t, "error", msg, "enrolled this server as "+ids[0]+", but its answer failed a check",
					"Nothing was written and the key was discarded", "\nTo recover: disable "+ids[0]+" on the account service")
			} else {
				mustContain(t, "error", msg, lost...)
				mustNotContain(t, "error", msg, "disable srv_", ids[0])
			}
		})
	}

	t.Run("the name", func(t *testing.T) {
		const code = "build-code-8e8e8e"
		r := newRefService(t, refOptions{})
		r.write("codes", "build-1 "+code+"\n")
		for _, tc := range []struct {
			name, flag, host, want string
			set                    bool
		}{
			{"--name the code", code, "build-1.example.test", "--name holds the enrolment code", true},
			{"--name holding the code", "host-" + code + ".example.test", "build-1.example.test", "--name holds the enrolment code", true},
			{"the host name the code", "", code, "this host's name holds the enrolment code; give a --name", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				prev := enrolHostname
				enrolHostname = func() (string, error) { return tc.host, nil }
				t.Cleanup(func() { enrolHostname = prev })
				stateDir := enrolConfig(t, r.issuer, "")
				var out, errOut bytes.Buffer
				err := directoryEnroll(context.Background(), enrolStreams{in: strings.NewReader(code + "\n"), out: &out, errOut: &errOut}, tc.flag, tc.set)
				run := enrolRun{out: out.String(), errOut: errOut.String(), err: err}
				checkNoSecrets(t, stateDir, []string{code}, run.text(), logs.String())
				noEnrolment(t, stateDir)
				if err == nil {
					t.Fatal("enrolled")
				}
				mustContain(t, "error", err.Error(), tc.want, "Nothing was sent")
			})
		}
		if n := r.requests.Load(); n != 0 {
			t.Errorf("%d enrolment requests reached the service, want none", n)
		}
	})
}
