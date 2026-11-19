package refserver

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The conformance harness: every exchange of the contract, run against the
// reference server in-process, with the world's set-up steps as hooks.
// It signs its own assertions and tokens with the fixture keys using the
// standard library, sharing no code with shed's client of the contract.

// contractDir is tests/directory-contract/v1 from this package's directory.
const contractDir = "../../../tests/directory-contract/v1"

// callerAddr is the address every harness request comes from: the caller
// limits.exhaust names for the enrolment and token limits.
const callerAddr = "192.0.2.1:40000"

func readContract(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

// decodeClosed decodes a contract file into v, refusing any member v does
// not have: the harness language is closed.
func decodeClosed(t *testing.T, rel string, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

// The world (fixtures/world.json).
type worldFile struct {
	Contract     int    `json:"contract"`
	Organization string `json:"organization"`
	Issuer       string `json:"issuer"`
	Clock        string `json:"clock"`
	Users        []User `json:"users"`
	Servers      []struct {
		Name       string `json:"name"`
		ClientID   string `json:"client_id"`
		Key        string `json:"key"`
		EnrolledAt string `json:"enrolled_at"`
		DisabledAt string `json:"disabled_at"`
		Note       string `json:"note"`
	} `json:"servers"`
	Codes []struct {
		Code        string `json:"code"`
		ServerName  string `json:"server_name"`
		CreatedAt   string `json:"created_at"`
		ExpiresAt   string `json:"expires_at"`
		State       string `json:"state"`
		CancelledAt string `json:"cancelled_at"`
		Server      string `json:"server"`
		Note        string `json:"note"`
	} `json:"codes"`
}

// worldInputs is the world as Inputs, changed by the set-up steps.
type worldInputs struct {
	mu       sync.Mutex
	users    []User
	codes    map[string]Code
	disabled map[string]bool
	fail     bool
}

func (w *worldInputs) Users() ([]User, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		return nil, fmt.Errorf("lookups.fail")
	}
	out := make([]User, len(w.users))
	copy(out, w.users)
	return out, nil
}

func (w *worldInputs) Code(code string) (Code, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		return Code{}, false, fmt.Errorf("lookups.fail")
	}
	c, ok := w.codes[code]
	return c, ok, nil
}

func (w *worldInputs) Disabled(clientID string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		return false, fmt.Errorf("lookups.fail")
	}
	return w.disabled[clientID], nil
}

// The exchange language (CONTRACT.md, "Exchanges").
type exStep struct {
	Before    []map[string]json.RawMessage `json:"before"`
	Assertion json.RawMessage              `json:"assertion"`
	Token     json.RawMessage              `json:"token"`
	Request   struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int                `json:"status"`
		Headers map[string]*string `json:"headers"`
		Body    json.RawMessage    `json:"body"`
		Text    *string            `json:"text"`
	} `json:"response"`
}

type exchange struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	exStep
	Then []exStep `json:"then"`
}

type assertionSpec struct {
	Reuse          bool            `json:"reuse"`
	Key            string          `json:"key"`
	ClientID       string          `json:"client_id"`
	Typ            json.RawMessage `json:"typ"`
	Aud            json.RawMessage `json:"aud"`
	Lifetime       *int64          `json:"lifetime"`
	IatOffset      int64           `json:"iat_offset"`
	NbfOffset      *int64          `json:"nbf_offset"`
	DuplicateClaim string          `json:"duplicate_claim"`
}

type tokenSpec struct {
	ClientID         string          `json:"client_id"`
	Scope            []string        `json:"scope"`
	NbfOffset        int64           `json:"nbf_offset"`
	Sub              *string         `json:"sub"`
	Aud              json.RawMessage `json:"aud"`
	Gty              *string         `json:"gty"`
	Iss              *string         `json:"iss"`
	Typ              *string         `json:"typ"`
	Kid              *string         `json:"kid"`
	CorruptSignature *bool           `json:"corrupt_signature"`
}

// The six compared headers.
var comparedHeaders = []string{"Content-Type", "Cache-Control", "Pragma", "WWW-Authenticate", "Retry-After", "ETag"}

const fixtureKeyID = "test-signing-1"

// harness is what every scenario of one run shares.
type harness struct {
	world      worldFile
	worldRaw   []byte
	clientKeys map[string]crypto.Signer // test-keys/<stem>.pem
	jwks       map[string][]byte        // test-keys/<stem>.jwk.json
	signing    *rsa.PrivateKey

	mu       sync.Mutex
	seenJTIs map[string]bool // access tokens' jti across the run
}

func loadPEM(t *testing.T, rel string) any {
	t.Helper()
	block, _ := pem.Decode(readContract(t, rel))
	if block == nil {
		t.Fatalf("%s is not PEM", rel)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return k
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		clientKeys: map[string]crypto.Signer{},
		jwks:       map[string][]byte{},
		seenJTIs:   map[string]bool{},
	}
	h.worldRaw = readContract(t, "fixtures/world.json")
	decodeClosed(t, "world.json", h.worldRaw, &h.world)
	entries, err := os.ReadDir(filepath.Join(contractDir, "fixtures", "test-keys"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".pem")
		if !ok {
			continue
		}
		k := loadPEM(t, "fixtures/test-keys/"+e.Name())
		if stem == "token-signing-key" {
			h.signing = k.(*rsa.PrivateKey)
			continue
		}
		h.clientKeys[stem] = k.(crypto.Signer)
		h.jwks[stem] = bytes.TrimSpace(readContract(t, "fixtures/test-keys/"+stem+".jwk.json"))
	}
	if h.signing == nil || len(h.clientKeys) == 0 {
		t.Fatal("the test keys are missing")
	}
	return h
}

// scenario is one exchange's run against a fresh world.
type scenario struct {
	t        *testing.T
	h        *harness
	name     string
	srv      *Server
	in       *worldInputs
	clock    time.Time
	clientID string // the bound {{client_id}}
	current  string // the client id of the step's assertion or token
	last     string // the previous assertion, for reuse
	server   string // the scenario's server, for limits.exhaust directory
	token    string // the bound {{access_token}}
	listGen  int
	versions map[int]string
	jtiSeq   int
}

func (h *harness) newScenario(t *testing.T, ex *exchange) *scenario {
	t.Helper()
	var w worldFile
	decodeClosed(t, "world.json", h.worldRaw, &w) // a fresh copy
	clock, err := time.Parse(time.RFC3339, w.Clock)
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario{t: t, h: h, name: ex.Name, clock: clock, versions: map[int]string{}}
	s.in = &worldInputs{users: w.Users, codes: map[string]Code{}, disabled: map[string]bool{}}
	s.srv, err = New(Config{
		Issuer:       w.Issuer,
		Organization: w.Organization,
		Inputs:       s.in,
		SigningKey:   h.signing,
		KeyID:        fixtureKeyID,
		Now:          func() time.Time { return s.clock },
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, sv := range w.Servers {
		if err := s.srv.addServer(sv.Name, sv.ClientID, h.jwks[sv.Key]); err != nil {
			t.Fatalf("world server %s: %v", sv.Name, err)
		}
		if sv.DisabledAt != "" {
			s.in.disabled[sv.ClientID] = true
		}
		byName[sv.Name] = sv.ClientID
	}
	for _, c := range w.Codes {
		code := Code{ServerName: c.ServerName, Cancelled: c.State == "cancelled"}
		if c.ExpiresAt != "" {
			if code.ExpiresAt, err = time.Parse(time.RFC3339, c.ExpiresAt); err != nil {
				t.Fatal(err)
			}
		}
		s.in.codes[c.Code] = code
		if c.State == "spent" {
			s.srv.spendCode(c.Code, byName[c.Server])
		}
	}
	// The scenario's server: the first client id an assertion or token
	// names.
	for _, st := range append([]exStep{ex.exStep}, ex.Then...) {
		var named struct {
			ClientID string `json:"client_id"`
		}
		for _, raw := range []json.RawMessage{st.Assertion, st.Token} {
			if len(raw) > 0 && json.Unmarshal(raw, &named) == nil && named.ClientID != "" && s.server == "" {
				s.server = named.ClientID
			}
		}
	}
	return s
}

// resolve fills {{client_id}} with the bound client id.
func (s *scenario) resolve(v string) string {
	if strings.Contains(v, "{{client_id}}") {
		if s.clientID == "" {
			s.t.Fatalf("{{client_id}} is used before it is bound")
		}
		v = strings.ReplaceAll(v, "{{client_id}}", s.clientID)
	}
	return v
}

// before runs one step's set-up steps against the world.
func (s *scenario) before(steps []map[string]json.RawMessage) {
	for _, st := range steps {
		if len(st) != 1 {
			s.t.Fatalf("a set-up step is not an object with one member: %v", st)
		}
		for name, raw := range st {
			switch name {
			case "users.remove":
				var who string
				mustDecode(s.t, raw, &who)
				s.in.mu.Lock()
				i := s.userIndex(who)
				s.in.users = append(s.in.users[:i:i], s.in.users[i+1:]...)
				s.in.mu.Unlock()
				s.listGen++
			case "users.set_admin":
				var args [2]json.RawMessage
				mustDecode(s.t, raw, &args)
				var who string
				var admin bool
				mustDecode(s.t, args[0], &who)
				mustDecode(s.t, args[1], &admin)
				s.in.mu.Lock()
				i := s.userIndex(who)
				if s.in.users[i].Admin != admin {
					s.listGen++
				}
				s.in.users[i].Admin = admin
				s.in.mu.Unlock()
			case "servers.disable":
				var who string
				mustDecode(s.t, raw, &who)
				found := false
				s.srv.mu.Lock()
				for id, sv := range s.srv.servers {
					if sv.name == who {
						s.in.mu.Lock()
						s.in.disabled[id] = true
						s.in.mu.Unlock()
						found = true
					}
				}
				s.srv.mu.Unlock()
				if !found {
					s.t.Fatalf("servers.disable: no server %q", who)
				}
			case "clock.set":
				var at string
				mustDecode(s.t, raw, &at)
				c, err := time.Parse(time.RFC3339, at)
				if err != nil {
					s.t.Fatal(err)
				}
				s.clock = c
			case "lookups.fail":
				s.in.mu.Lock()
				s.in.fail = true
				s.in.mu.Unlock()
			case "limits.exhaust":
				var kind string
				mustDecode(s.t, raw, &kind)
				caller, err := s.limitCaller(kind)
				if err != nil {
					s.t.Fatalf("limits.exhaust: %v", err)
				}
				s.srv.limits[limitKind(kind)].exhaust(caller, s.clock)
			default:
				s.t.Fatalf("the set-up step %q is not one the contract names", name)
			}
		}
	}
}

// limitCaller is the caller whose allowance limits.exhaust spends: the
// harness's address for the enrolment and token limits, the scenario's
// server for the directory limit. A directory limit in a scenario that names
// no server is an error, never the empty caller.
func (s *scenario) limitCaller(kind string) (string, error) {
	switch limitKind(kind) {
	case limitEnroll, limitToken:
		return strings.Split(callerAddr, ":")[0], nil
	case limitDirectory:
		if s.server == "" {
			return "", fmt.Errorf("the directory limit, but no step names the scenario's server")
		}
		id := s.server
		if strings.Contains(id, "{{client_id}}") {
			if s.clientID == "" {
				return "", fmt.Errorf("the scenario's server is {{client_id}}, not yet bound")
			}
			id = strings.ReplaceAll(id, "{{client_id}}", s.clientID)
		}
		return id, nil
	}
	return "", fmt.Errorf("no limit %q", kind)
}

// userIndex finds a world user; the caller holds s.in.mu.
func (s *scenario) userIndex(name string) int {
	for i, u := range s.in.users {
		if u.Name == name {
			return i
		}
	}
	s.t.Fatalf("no world user %q", name)
	return -1
}

func mustDecode(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

var b64 = base64.RawURLEncoding.EncodeToString

// member is one member of a JSON object built in order, so that a member
// can be named twice on purpose.
type member struct {
	name  string
	value json.RawMessage
}

func objectText(ms []member) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(m.name)
		b.Write(name)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// signClient signs a JWS with a client test key: EdDSA for Ed25519, ES256
// (r || s, 32 bytes each) for P-256.
func signClient(key crypto.Signer, input string) []byte {
	switch k := key.(type) {
	case ed25519.PrivateKey:
		return ed25519.Sign(k, []byte(input))
	case *ecdsa.PrivateKey:
		sum := sha256.Sum256([]byte(input))
		r, sv, err := ecdsa.Sign(rand.Reader, k, sum[:])
		if err != nil {
			panic(err)
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		sv.FillBytes(sig[32:])
		return sig
	}
	panic(fmt.Sprintf("a test key of type %T", key))
}

func algOf(key crypto.Signer) string {
	if _, ok := key.(ed25519.PrivateKey); ok {
		return "EdDSA"
	}
	return "ES256"
}

// assertion makes the step's {{assertion}}, as CONTRACT.md's table says.
func (s *scenario) assertion(raw json.RawMessage) string {
	var spec assertionSpec
	mustDecode(s.t, raw, &spec)
	if spec.Reuse {
		if s.last == "" {
			s.t.Fatal("a reuse assertion with no assertion before it")
		}
		return s.last
	}
	key := s.h.clientKeys[spec.Key]
	if key == nil {
		s.t.Fatalf("no test key %q", spec.Key)
	}
	clientID := s.resolve(spec.ClientID)
	s.current = clientID

	header := []member{{"alg", rawJSON(algOf(key))}}
	switch {
	case spec.Typ == nil:
		header = append(header, member{"typ", rawJSON("JWT")})
	case string(spec.Typ) == "null":
	default:
		header = append(header, member{"typ", spec.Typ})
	}
	aud := spec.Aud
	if aud == nil {
		aud = rawJSON(s.h.world.Issuer)
	}
	lifetime := int64(60)
	if spec.Lifetime != nil {
		lifetime = *spec.Lifetime
	}
	iat := s.clock.Unix() + spec.IatOffset
	s.jtiSeq++
	claims := []member{
		{"iss", rawJSON(clientID)},
		{"sub", rawJSON(clientID)},
		{"aud", aud},
		{"iat", rawJSON(iat)},
		{"exp", rawJSON(iat + lifetime)},
		{"jti", rawJSON(fmt.Sprintf("%s-%d-%s", s.name, s.jtiSeq, randomHex(8)))},
	}
	if spec.NbfOffset != nil {
		claims = append(claims, member{"nbf", rawJSON(iat + *spec.NbfOffset)})
	}
	if spec.DuplicateClaim != "" {
		found := false
		for _, m := range claims {
			if m.name == spec.DuplicateClaim {
				claims = append(claims, m)
				found = true
				break
			}
		}
		if !found {
			s.t.Fatalf("duplicate_claim %q names no claim", spec.DuplicateClaim)
		}
	}
	s.last = assertionParts{header, claims}.sign(key)
	return s.last
}

// signRS256 signs a JWS signing input with the fixture signing key.
func (s *scenario) signRS256(input string) []byte {
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(nil, s.h.signing, crypto.SHA256, sum[:])
	if err != nil {
		s.t.Fatal(err)
	}
	return sig
}

// accessToken signs the step's {{access_token}} with the fixture signing
// key, as CONTRACT.md's token member says.
func (s *scenario) accessToken(raw json.RawMessage) string {
	var spec tokenSpec
	mustDecode(s.t, raw, &spec)
	if spec.CorruptSignature != nil && !*spec.CorruptSignature {
		s.t.Fatal("corrupt_signature is only ever true")
	}
	clientID := s.resolve(spec.ClientID)
	s.current = clientID
	str := func(p *string, def string) string {
		if p != nil {
			return *p
		}
		return def
	}
	header := []member{
		{"alg", rawJSON("RS256")},
		{"typ", rawJSON(str(spec.Typ, "at+jwt"))},
		{"kid", rawJSON(str(spec.Kid, fixtureKeyID))},
	}
	aud := spec.Aud
	if aud == nil {
		aud = rawJSON([]string{s.h.world.Issuer + "/v1"})
	}
	scope := spec.Scope
	if scope == nil {
		scope = []string{}
	}
	iat := s.clock.Unix()
	claims := []member{
		{"iss", rawJSON(str(spec.Iss, s.h.world.Issuer))},
		{"sub", rawJSON(str(spec.Sub, clientID))},
		{"client_id", rawJSON(clientID)},
		{"aud", aud},
		{"gty", rawJSON(str(spec.Gty, "client_credentials"))},
		{"scp", rawJSON(scope)},
		{"jti", rawJSON("harness-" + randomHex(8))},
		{"iat", rawJSON(iat)},
		{"nbf", rawJSON(iat + spec.NbfOffset)},
		{"exp", rawJSON(iat + 600)},
	}
	input := b64(objectText(header)) + "." + b64(objectText(claims))
	sig := s.signRS256(input)
	if spec.CorruptSignature != nil {
		sig[0] ^= 1
	}
	return input + "." + b64(sig)
}

// request builds a step's request, filling its placeholders.
func (s *scenario) request(st *exStep, assertion string) *http.Request {
	var body io.Reader
	raw := st.Request.Body
	switch {
	case raw == nil || string(raw) == "null":
	case raw[0] == '"':
		var str string
		mustDecode(s.t, raw, &str)
		str = strings.ReplaceAll(str, "{{assertion}}", assertion)
		body = strings.NewReader(s.resolve(str))
	default:
		body = strings.NewReader(s.resolve(string(raw)))
	}
	req := httptest.NewRequest(st.Request.Method, st.Request.Path, body)
	req.RemoteAddr = callerAddr
	for k, v := range st.Request.Headers {
		if strings.Contains(v, "{{access_token}}") {
			if s.token == "" {
				s.t.Fatal("{{access_token}} is used before it is bound")
			}
			v = strings.ReplaceAll(v, "{{access_token}}", s.token)
		}
		req.Header.Set(k, s.resolve(v))
	}
	return req
}

// run runs every step of the scenario.
func (s *scenario) run(ex *exchange) {
	for i, st := range append([]exStep{ex.exStep}, ex.Then...) {
		s.step(i, &st)
	}
}

func (s *scenario) step(i int, st *exStep) {
	t := s.t
	s.before(st.Before)
	assertion := ""
	if st.Assertion != nil {
		assertion = s.assertion(st.Assertion)
	}
	if st.Token != nil {
		s.token = s.accessToken(st.Token)
	}
	rec := httptest.NewRecorder()
	s.srv.ServeHTTP(rec, s.request(st, assertion))
	res := rec.Result()
	got, _ := io.ReadAll(res.Body)
	where := fmt.Sprintf("step %d (%s %s)", i, st.Request.Method, st.Request.Path)

	if res.StatusCode != st.Response.Status {
		t.Fatalf("%s: status %d, want %d; body %s", where, res.StatusCode, st.Response.Status, got)
	}
	for _, name := range comparedHeaders {
		want, listed := st.Response.Headers[name]
		if !listed {
			t.Fatalf("%s: the fixture does not list %s", where, name)
		}
		vals := res.Header.Values(name)
		switch {
		case want == nil && len(vals) != 0:
			t.Errorf("%s: %s is %q, want absent", where, name, vals)
		case want != nil && strings.Join(vals, ", ") != *want:
			t.Errorf("%s: %s is %q, want %q", where, name, strings.Join(vals, ", "), *want)
		}
	}
	switch {
	case st.Response.Text != nil:
		if string(got) != *st.Response.Text {
			t.Errorf("%s: body %q, want %q", where, got, *st.Response.Text)
		}
	case st.Response.Body == nil || string(st.Response.Body) == "null":
		if len(got) != 0 {
			t.Errorf("%s: body %q, want none", where, got)
		}
	default:
		want, err := decodeNumbers(st.Response.Body)
		if err != nil {
			t.Fatal(err)
		}
		// The answer is held to strict JSON before it is compared: the
		// standard decoder keeps the last of two same-named members, and a
		// stray closing bracket after the value would otherwise pass.
		if err := strictBody(got); err != nil {
			t.Fatalf("%s: the body is not one strict JSON value: %v: %q", where, err, got)
		}
		act, err := decodeNumbers(got)
		if err != nil {
			t.Fatalf("%s: the body is not JSON: %v: %q", where, err, got)
		}
		s.match(where+": body", want, act)
	}
}

func decodeNumbers(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("data after the JSON value")
	}
	return v, nil
}

// strictBody checks a response body is exactly one JSON value in valid
// UTF-8, with no member named twice in any object and a real end of input
// after it. It is the harness's own walk, sharing no code with the server's
// strict reader, so the two cannot agree on one mistake.
func strictBody(raw []byte) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := strictValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("no end of input after the JSON value (%v)", err)
	}
	return nil
}

// strictValue reads one value from dec, refusing a member named twice.
func strictValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := k.(string)
			if !ok {
				return fmt.Errorf("a member name is not a string")
			}
			if seen[name] {
				return fmt.Errorf("the member %q is named twice", name)
			}
			seen[name] = true
			if err := strictValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := strictValue(dec); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("an unexpected %q", d)
	}
	end, err := dec.Token()
	if err != nil {
		return err
	}
	if e, ok := end.(json.Delim); !ok || (d == '{' && e != '}') || (d == '[' && e != ']') {
		return fmt.Errorf("%q is not closed", d)
	}
	return nil
}

var (
	placeholderRx = regexp.MustCompile(`^\{\{([a-z_]+)\}\}$`)
	clientIDRx    = regexp.MustCompile(`^srv_[0-9a-f]{32}$`)
	versionRx     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// match compares a response body with the fixture's, value for value, a
// placeholder by its rule.
func (s *scenario) match(where string, want, got any) {
	t := s.t
	if str, ok := want.(string); ok {
		if m := placeholderRx.FindStringSubmatch(str); m != nil {
			s.placeholder(where, m[1], got)
			return
		}
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: %v, want an object", where, got)
			return
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !wok:
				t.Errorf("%s.%s: present (%v), want absent", where, k, gv)
			case !gok:
				t.Errorf("%s.%s: absent, want %v", where, k, wv)
			default:
				s.match(where+"."+k, wv, gv)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			t.Errorf("%s: %v, want %v", where, got, want)
			return
		}
		for i := range w {
			s.match(fmt.Sprintf("%s[%d]", where, i), w[i], g[i])
		}
	default:
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: %v, want %v", where, got, want)
		}
	}
}

// placeholder applies one placeholder's rule (CONTRACT.md's table).
func (s *scenario) placeholder(where, name string, got any) {
	t := s.t
	str, ok := got.(string)
	if !ok {
		t.Errorf("%s: %v for {{%s}}, want a string", where, got, name)
		return
	}
	switch name {
	case "message", "description":
		if str == "" {
			t.Errorf("%s: empty for {{%s}}", where, name)
		}
	case "client_id":
		if !clientIDRx.MatchString(str) {
			t.Errorf("%s: %q does not match %s", where, str, clientIDRx)
		}
		if s.clientID != "" && s.clientID != str {
			t.Errorf("%s: %q, but {{client_id}} is bound to %q", where, str, s.clientID)
		}
		s.clientID = str
	case "version":
		if !versionRx.MatchString(str) {
			t.Errorf("%s: %q is not 64 lowercase hex digits", where, str)
		}
		if prev, ok := s.versions[s.listGen]; ok && prev != str {
			t.Errorf("%s: version %s, but the list has not changed since %s", where, str, prev)
		}
		for gen, v := range s.versions {
			if gen != s.listGen && v == str {
				t.Errorf("%s: version %s, the same as before the list changed", where, str)
			}
		}
		s.versions[s.listGen] = str
	case "access_token":
		s.checkAccessToken(where, str)
		s.token = str
	default:
		t.Errorf("%s: {{%s}} is not a placeholder the contract names", where, name)
	}
}

// checkAccessToken holds an issued token to the {{access_token}} rule.
func (s *scenario) checkAccessToken(where, tok string) {
	t := s.t
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Errorf("%s: not a compact JWS", where)
		return
	}
	dec := func(p string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			t.Fatalf("%s: a part is not base64url: %v", where, err)
		}
		return b
	}
	var header map[string]any
	if err := json.Unmarshal(dec(parts[0]), &header); err != nil {
		t.Fatalf("%s: header: %v", where, err)
	}
	if want := map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": fixtureKeyID}; !reflect.DeepEqual(header, want) {
		t.Errorf("%s: header %v, want %v", where, header, want)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&s.h.signing.PublicKey, crypto.SHA256, sum[:], dec(parts[2])); err != nil {
		t.Errorf("%s: the signature does not verify under the fixture key", where)
	}
	var c struct {
		Iss      string   `json:"iss"`
		Sub      string   `json:"sub"`
		ClientID string   `json:"client_id"`
		Aud      []string `json:"aud"`
		Gty      string   `json:"gty"`
		Scp      []string `json:"scp"`
		Jti      string   `json:"jti"`
		Iat      int64    `json:"iat"`
		Nbf      int64    `json:"nbf"`
		Exp      int64    `json:"exp"`
	}
	if err := json.Unmarshal(dec(parts[1]), &c); err != nil {
		t.Fatalf("%s: claims: %v", where, err)
	}
	issuer := s.h.world.Issuer
	if c.Iss != issuer || c.Sub != s.current || c.ClientID != s.current {
		t.Errorf("%s: iss %q sub %q client_id %q, want %q and %q", where, c.Iss, c.Sub, c.ClientID, issuer, s.current)
	}
	if !reflect.DeepEqual(c.Aud, []string{issuer + "/v1"}) || c.Gty != "client_credentials" || !reflect.DeepEqual(c.Scp, []string{"directory:read"}) {
		t.Errorf("%s: aud %v gty %q scp %v", where, c.Aud, c.Gty, c.Scp)
	}
	if c.Nbf != c.Iat || c.Exp-c.Iat != 600 {
		t.Errorf("%s: iat %d nbf %d exp %d", where, c.Iat, c.Nbf, c.Exp)
	}
	if d := c.Iat - s.clock.Unix(); d < -1 || d > 1 {
		t.Errorf("%s: iat %d is not within a second of the scenario's clock %d", where, c.Iat, s.clock.Unix())
	}
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if c.Jti == "" || s.h.seenJTIs[c.Jti] {
		t.Errorf("%s: jti %q is empty or seen before", where, c.Jti)
	}
	s.h.seenJTIs[c.Jti] = true
}

// TestConformance runs every exchange of the contract against the
// reference server.
func TestConformance(t *testing.T) {
	h := newHarness(t)
	entries, err := os.ReadDir(filepath.Join(contractDir, "fixtures", "exchanges"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		n++
		rel := "fixtures/exchanges/" + e.Name()
		raw := readContract(t, rel)
		t.Run(strings.TrimSuffix(e.Name(), ".json"), func(t *testing.T) {
			var ex exchange
			decodeClosed(t, rel, raw, &ex)
			s := h.newScenario(t, &ex)
			s.run(&ex)
		})
	}
	if n == 0 {
		t.Fatal("no exchanges")
	}
}
