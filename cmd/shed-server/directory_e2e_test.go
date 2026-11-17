package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/sshd"
)

// The end-to-end test of a directory server against the reference server of
// the directory contract: the enrolment command, serve's lookup and runtime,
// the real SSH server and the real API (mtls, with serve's handshake check),
// in-process, the reference server over loopback plain HTTP. The server's
// clock (its lookup, its runtime and the reference server) is the wall clock
// moved forward by the test, so the trust time can end without waiting for
// it; the SSH server and the certificates keep real time.

// e2eKey is a fresh Ed25519 SSH key: its signer and authorized_keys line.
func e2eKey(t *testing.T) (gossh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, strings.TrimSpace(string(gossh.MarshalAuthorizedKey(s.PublicKey())))
}

// e2eRecorder keeps every assertion sent to the token endpoint and every
// access token it gave out, to search the logs for. It can also hold the
// directory's answers (setHold).
type e2eRecorder struct {
	mu      sync.Mutex
	secrets []string
	// hold, when set, holds every directory request until it is closed (or
	// the request ends); held is told of each one it holds.
	hold, held chan struct{}
	// pulls counts the directory requests.
	pulls int
}

// setHold makes the directory's answers wait until hold is closed (nil: no
// wait); held is told of each request held.
func (r *e2eRecorder) setHold(hold, held chan struct{}) {
	r.mu.Lock()
	r.hold, r.held = hold, held
	r.mu.Unlock()
}

// pullCount is how many directory requests the service has had.
func (r *e2eRecorder) pullCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pulls
}

func (r *e2eRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/directory" {
			r.mu.Lock()
			r.pulls++
			hold, held := r.hold, r.held
			r.mu.Unlock()
			if hold != nil {
				// Neither the telling nor the wait can block past the hold's
				// release or the request's end.
				select {
				case held <- struct{}{}:
				case <-hold:
				case <-req.Context().Done():
					return
				}
				select {
				case <-hold:
				case <-req.Context().Done():
					return
				}
			}
		}
		if req.URL.Path != "/oauth2/token" {
			next.ServeHTTP(w, req)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		req.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, req)
		form, _ := url.ParseQuery(string(body))
		var answer struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		r.mu.Lock()
		r.secrets = append(r.secrets, form.Get("client_assertion"), answer.AccessToken)
		r.mu.Unlock()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

func (r *e2eRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.secrets {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// e2eIdentity is a person or the local admin: an SSH key and, after
// enrolment, a client certificate for the API.
type e2eIdentity struct {
	name   string
	signer gossh.Signer
	line   string
	cert   tls.Certificate
}

// e2eServer is the directory server under test.
type e2eServer struct {
	t        *testing.T
	sshAddr  string
	apiURL   string
	control  string
	stateDir string
	offset   *atomic.Int64
	ticks    chan time.Time
	rt       *directory.Runtime
	recorder *e2eRecorder
}

// dial logs in over SSH as user with id's key.
func (s *e2eServer) dial(id *e2eIdentity, user string) (*gossh.Client, error) {
	return gossh.Dial("tcp", s.sshAddr, &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(id.signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
}

// enrol logs id in as _bootstrap, asks for a client certificate, and keeps
// both: the connection stays open (held) and is returned.
func (s *e2eServer) enrol(id *e2eIdentity) *gossh.Client {
	s.t.Helper()
	client, err := s.dial(id, "_bootstrap")
	if err != nil {
		s.t.Fatalf("%s: _bootstrap login: %v", id.name, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		s.t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		s.t.Fatal(err)
	}
	sess, err := client.NewSession()
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	if err := sess.Run("control cli csr=" + base64.StdEncoding.EncodeToString(csr)); err != nil {
		s.t.Fatalf("%s: _bootstrap: %v (%s)", id.name, err, errOut.String())
	}
	var bundle struct {
		ClientCert string `json:"client_cert"`
	}
	if err := json.Unmarshal(out.Bytes(), &bundle); err != nil || bundle.ClientCert == "" {
		s.t.Fatalf("%s: bundle %q: %v", id.name, out.String(), err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		s.t.Fatal(err)
	}
	id.cert, err = tls.X509KeyPair([]byte(bundle.ClientCert), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	if err != nil {
		s.t.Fatal(err)
	}
	return client
}

// api sends method path with id's certificate, on a new connection, and
// returns the status (0 when the TLS handshake refused it).
func (s *e2eServer) api(id *e2eIdentity, method, path string) int {
	s.t.Helper()
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{id.cert}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // the test pins nothing; the server's check is what is tested
	}}
	req, err := http.NewRequest(method, s.apiURL+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// refresh is the local admin's `shed users refresh`, over the API.
func (s *e2eServer) refresh(admin *e2eIdentity) {
	s.t.Helper()
	if code := s.api(admin, http.MethodPost, "/api/users/refresh"); code != http.StatusOK {
		s.t.Fatalf("the local admin's refresh: HTTP %d", code)
	}
}

// writeControl writes a file of the reference server's control directory.
func (s *e2eServer) writeControl(name, content string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.control, name), []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// listUsers serves these users (name: admin), each with their key.
func (s *e2eServer) listUsers(people map[*e2eIdentity]bool) {
	s.t.Helper()
	us := []refserver.User{}
	for id, admin := range people {
		us = append(us, refserver.User{Name: id.name, Admin: admin, Keys: []refserver.Key{{Key: id.line, Label: "laptop"}}})
	}
	b, err := json.Marshal(us)
	if err != nil {
		s.t.Fatal(err)
	}
	s.writeControl("users.json", string(b))
}

// closed reports whether client's connection ends within ten seconds.
func closed(client *gossh.Client) bool {
	done := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// startE2EServer builds and starts the directory server: the config, the
// enrolment through the command, serve's lookup, the SSH server and the API.
func startE2EServer(t *testing.T, ops *e2eIdentity) (*e2eServer, string) {
	t.Helper()
	s := &e2eServer{t: t, control: t.TempDir(), offset: &atomic.Int64{}, ticks: make(chan time.Time), recorder: &e2eRecorder{}}
	now := func() time.Time { return time.Now().Add(time.Duration(s.offset.Load())) }

	// The reference server, on its own loopback port.
	refLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	issuer := "http://" + refLn.Addr().String()
	cd, err := refserver.OpenControlDir(s.control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cd.Close() })
	ref, err := refserver.New(refserver.Config{Issuer: issuer, Organization: "example", Inputs: cd, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	refTS := httptest.NewUnstartedServer(s.recorder.wrap(ref))
	refTS.Listener.Close()
	refTS.Listener = refLn
	refTS.Start()
	t.Cleanup(refTS.Close)

	// The server's config: ops the local admin, the directory with plain
	// HTTP to loopback, a minute's refresh and two minutes' trust.
	stateDir := filepath.Join(t.TempDir(), "state")
	s.stateDir = stateDir
	writeServeConfig(t, "state_dir: "+stateDir+"\n"+
		"auth:\n  mode: mtls\n  users:\n    ops:\n      admin: true\n      keys:\n        - {key: \""+ops.line+"\", label: desk}\n"+
		"  directory:\n    url: "+issuer+"\n    allow_plain_http: true\n    refresh: 1m\n    trust: 2m\n")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	// Enrolment, through the command, with a code from the control
	// directory on standard input.
	code := "e2e-" + base64.RawURLEncoding.EncodeToString([]byte(t.Name()))[:16]
	s.writeControl("codes", "e2e-server "+code+"\n")
	var out, errOut bytes.Buffer
	if err := directoryEnroll(context.Background(), enrolStreams{in: strings.NewReader(code + "\n"), out: &out, errOut: &errOut}, "e2e.example.test", true); err != nil {
		t.Fatalf("enrol: %v\n%s%s", err, out.String(), errOut.String())
	}

	// serve's lookup, on the test's clock and trust ticks; its schedule
	// pulls at start and when asked (its timers never fire).
	prevNow, prevTicks, prevTimer := directoryNow, directoryTrustTicks, directoryTimer
	directoryNow = now
	directoryTrustTicks = func() (<-chan time.Time, func()) { return s.ticks, func() {} }
	directoryTimer = func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	t.Cleanup(func() { directoryNow, directoryTrustTicks, directoryTimer = prevNow, prevTicks, prevTimer })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lookup, err := newUserLookup(ctx, cfg, serverStateDir(cfg))
	if err != nil {
		t.Fatal(err)
	}
	s.rt = lookup.(*directory.Runtime)

	// The SSH server, as serve builds it.
	sshLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.sshAddr = sshLn.Addr().String()
	sshLn.Close()
	guard := backend.NewGuard(newListOnlyBackend())
	sshServer, err := sshd.NewServer(guard, filepath.Join(serverStateDir(cfg), "host_key"), s.sshAddr, nil, sshKeyAuth(lookup))
	if err != nil {
		t.Fatal(err)
	}
	closeOnPublication(s.rt, sshServer)
	lookup.StartRefresh(ctx)

	caCert, caKey := caCertPaths(serverStateDir(cfg))
	ca, err := servertls.LoadOrGenerateCA(caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := tlsCertPaths(cfg, serverStateDir(cfg))
	cert, der, err := servertls.LoadOrGenerate(certPath, keyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	apiTS := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(apiTS.Listener.Addr().String())
	var httpsPort int
	_, _ = fmt.Sscan(port, &httpsPort)
	sshServer.SetBootstrap(sshd.BootstrapInfo{HTTPSPort: httpsPort, TLSFingerprint: servertls.Fingerprint(der), TokenTTL: time.Hour, CA: &ca})
	go func() { _ = sshServer.Start() }()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = sshServer.Shutdown(sctx)
	})

	apiServer := api.NewServer(guard, cfg, sshServer.GetHostPublicKey(), plugin.NewRegistry(), newPluginBridge(cfg, plugin.NewRegistry()))
	apiServer.SetKeyRefresher(lookup)
	apiServer.SetUserLookup(lookup)
	apiServer.SetClientCAInfo(ca.Fingerprint(), ca.NotAfter())
	apiTS.Config.Handler = apiServer.Router()
	apiTS.TLS = buildHTTPSTLSConfig(cfg, cert, &ca, certificateAuthorized(lookup))
	apiTS.StartTLS()
	t.Cleanup(apiTS.Close)
	s.apiURL = apiTS.URL

	// Wait for the SSH listener.
	deadline := time.After(10 * time.Second)
	for {
		c, err := s.dial(ops, "_bootstrap")
		if err == nil {
			_ = c.Close()
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the SSH server did not start: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	return s, code + "\n" + out.String() + errOut.String()
}

// TestEndToEndAgainstReferenceServer: enrol through the command; the first
// pull; a person's _bootstrap (a held SSH connection) and API requests with
// the certificate it gave; an admin flip seen at the next request with the
// same certificate; a removal that closes the held connection and refuses
// the next SSH login and API request; the person back; an outage, through
// which the list held still works; the trust time ending, which closes the
// person's connection and leaves only the local admin; the service back.
// The local admin works at every step. No log line holds the code, an
// assertion, an access token or a private key.
func TestEndToEndAgainstReferenceServer(t *testing.T) {
	logs := captureEnrolLog(t)
	opsSigner, opsLine := e2eKey(t)
	adaSigner, adaLine := e2eKey(t)
	ops := &e2eIdentity{name: "ops", signer: opsSigner, line: opsLine}
	ada := &e2eIdentity{name: "ada", signer: adaSigner, line: adaLine}

	s, enrolText := startE2EServer(t, ops)
	opsHeld := s.enrol(ops)
	defer opsHeld.Close()
	opsWorks := func(step string) {
		t.Helper()
		if code := s.api(ops, http.MethodGet, "/api/ssh-host-key"); code != http.StatusOK {
			t.Fatalf("%s: the local admin's API request: HTTP %d", step, code)
		}
		c, err := s.dial(ops, "_bootstrap")
		if err != nil {
			t.Fatalf("%s: the local admin's SSH login: %v", step, err)
		}
		_ = c.Close()
		if _, _, err := opsHeld.SendRequest("keepalive@example.test", true, nil); err != nil {
			t.Fatalf("%s: the local admin's held connection was closed: %v", step, err)
		}
	}
	opsWorks("enrolled, no list yet")

	// The person is listed; the admin pulls.
	s.listUsers(map[*e2eIdentity]bool{ada: false})
	s.refresh(ops)
	if st := s.rt.Status(); st.State != directory.StateTrusted || st.Copy != directory.CopyKept {
		t.Fatalf("after the first pull: %+v", st)
	}
	adaHeld := s.enrol(ada)
	if code := s.api(ada, http.MethodGet, "/api/ssh-host-key"); code != http.StatusOK {
		t.Fatalf("ada's API request: HTTP %d", code)
	}
	if code := s.api(ada, http.MethodPost, "/api/users/refresh"); code != http.StatusForbidden {
		t.Fatalf("ada (not an admin) refreshing: HTTP %d, want 403", code)
	}
	opsWorks("ada listed")

	// Admin flip: the same certificate is an admin's at the next request.
	s.listUsers(map[*e2eIdentity]bool{ada: true})
	s.refresh(ops)
	if code := s.api(ada, http.MethodPost, "/api/users/refresh"); code != http.StatusOK {
		t.Fatalf("ada (now an admin) refreshing: HTTP %d, want 200", code)
	}
	s.listUsers(map[*e2eIdentity]bool{ada: false})
	s.refresh(ops)
	if code := s.api(ada, http.MethodPost, "/api/users/refresh"); code != http.StatusForbidden {
		t.Fatalf("ada (demoted) refreshing: HTTP %d, want 403", code)
	}

	// Removal: her held connection is closed; SSH and the API refuse her.
	s.listUsers(map[*e2eIdentity]bool{})
	s.refresh(ops)
	if !closed(adaHeld) {
		t.Fatal("ada's held SSH connection was not closed when she left the list")
	}
	if c, err := s.dial(ada, "_bootstrap"); err == nil {
		_ = c.Close()
		t.Fatal("ada logged in over SSH after she left the list")
	}
	if code := s.api(ada, http.MethodGet, "/api/ssh-host-key"); code == http.StatusOK {
		t.Fatal("ada's API request was answered after she left the list")
	}
	opsWorks("ada removed")

	// Back, then an outage: the list held still works.
	s.listUsers(map[*e2eIdentity]bool{ada: false})
	s.refresh(ops)
	adaHeld = s.enrol(ada)
	defer adaHeld.Close()
	s.writeControl("outage", "")
	s.refresh(ops)
	if st := s.rt.Status(); st.State != directory.StateTrusted || !strings.Contains(st.LastError, "503") {
		t.Fatalf("during the outage: %+v", st)
	}
	if code := s.api(ada, http.MethodGet, "/api/ssh-host-key"); code != http.StatusOK {
		t.Fatalf("ada's API request during the outage: HTTP %d", code)
	}
	if c, err := s.dial(ada, "_bootstrap"); err != nil {
		t.Fatalf("ada's SSH login during the outage: %v", err)
	} else {
		_ = c.Close()
	}
	opsWorks("outage")

	// The trust time ends (two minutes on the server's clock): her held
	// connection is closed, and only the local admin can log in.
	s.offset.Store(int64(3 * time.Minute))
	s.ticks <- time.Now()
	s.ticks <- time.Now() // the second tick waits for the first to be handled
	if !closed(adaHeld) {
		t.Fatal("ada's held SSH connection was not closed when the trust time ended")
	}
	if c, err := s.dial(ada, "_bootstrap"); err == nil {
		_ = c.Close()
		t.Fatal("ada logged in over SSH after the trust time ended")
	}
	if code := s.api(ada, http.MethodGet, "/api/ssh-host-key"); code == http.StatusOK {
		t.Fatal("ada's API request was answered after the trust time ended")
	}
	if st := s.rt.Status(); st.State != directory.StateExpired {
		t.Fatalf("after the trust time: %+v", st)
	}
	opsWorks("trust ended")

	// The service is back: the local admin's refresh restores the list.
	if err := os.Remove(filepath.Join(s.control, "outage")); err != nil {
		t.Fatal(err)
	}
	s.refresh(ops)
	if code := s.api(ada, http.MethodGet, "/api/ssh-host-key"); code != http.StatusOK {
		t.Fatalf("ada's API request after the service came back: HTTP %d", code)
	}
	opsWorks("restored")

	// Nothing secret in the log, the enrolment's output, or the copy.
	secrets := s.recorder.all()
	if len(secrets) < 2 {
		t.Fatalf("recorded %d assertions and tokens", len(secrets))
	}
	code, output, _ := strings.Cut(enrolText, "\n")
	kept, err := os.ReadFile(filepath.Join(s.stateDir, directory.StateDirName, directory.CopyFileName))
	if err != nil {
		t.Fatal(err)
	}
	text := logs.String() + output + string(kept)
	for _, sec := range append(secrets, code) {
		if strings.Contains(text, sec) {
			t.Errorf("the log, the enrolment's output or the kept copy holds an assertion, a token or the code")
		}
	}
	for _, bad := range []string{"PRIVATE KEY", "client_assertion"} {
		if strings.Contains(text, bad) {
			t.Errorf("the log, the enrolment's output or the kept copy holds %q", bad)
		}
	}
	if jwt := regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.`); jwt.MatchString(text) {
		t.Error("the log, the enrolment's output or the kept copy holds a JWT")
	}
	for _, want := range []string{"good pull", `removed "ada"`, "TRUST ENDED", "trust restored", "SSH: closed 1 connection(s) of user \"ada\""} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log has no line holding %q", want)
		}
	}
}
