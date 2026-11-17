package directory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// good201 is the recorded enroll-ok answer, its client id filled.
const good201 = `{"client_id":"` + enrolledID + `","contract":1,"issuer":"https://accounts.example.test","organization":"example","token_endpoint":"https://accounts.example.test/oauth2/token"}`

func enrolAgainst(t *testing.T, fs *fakeService, org string) (*Enrolment, error) {
	t.Helper()
	clock := worldClock
	return Enroll(context.Background(), EnrolRequest{
		Issuer: worldIssuer, Organization: org,
		Code: "pending-code-for-srv-c-0000000000000", Name: "build-1.example.test",
		baseTransport: worldTransport(t, fs.srv), Now: fixedClock(&clock),
		Rand: bytes.NewReader(fixtureKey(t, "enrol-ed25519").Seed()),
	})
}

// TestEnrolRequest enrols with the fixture enrolment key's seed as the
// randomness: the request is the recorded enroll-ok request (the code, the
// name and the public key exactly as test-keys/enrol-ed25519.jwk.json
// holds it), and the enrolment carries that key.
func TestEnrolRequest(t *testing.T) {
	fs := newFakeService(t)
	fs.enroll = []served{{status: 201, header: http.Header{"Content-Type": {"application/json"}}, body: []byte(good201)}}
	e, err := enrolAgainst(t, fs, "")
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Code      string          `json:"code"`
		Name      string          `json:"name"`
		PublicKey json.RawMessage `json:"public_key"`
	}
	if err := json.Unmarshal(fs.enrolBody[0], &sent); err != nil {
		t.Fatal(err)
	}
	jwkText := bytes.TrimSpace(readFixture(t, "fixtures/test-keys/enrol-ed25519.jwk.json"))
	if !bytes.Equal(sent.PublicKey, jwkText) {
		t.Errorf("public_key %s, want %s", sent.PublicKey, jwkText)
	}
	var recorded struct {
		Request struct {
			Body json.RawMessage `json:"body"`
		} `json:"request"`
	}
	decodeFixture(t, "fixtures/exchanges/enroll-ok.json", &recorded)
	var a, b any
	_ = json.Unmarshal(fs.enrolBody[0], &a)
	_ = json.Unmarshal(recorded.Request.Body, &b)
	if ja, _ := json.Marshal(a); string(ja) != string(must(json.Marshal(b))) {
		t.Errorf("request %s, want the recorded %s", fs.enrolBody[0], recorded.Request.Body)
	}
	want := fixtureKey(t, "enrol-ed25519")
	if !e.PrivateKey.Equal(want) || e.ClientID != enrolledID ||
		e.Issuer != worldIssuer || e.TokenEndpoint != worldIssuer+"/oauth2/token" || e.Organization != worldOrg ||
		e.Name != "build-1.example.test" || !e.EnrolledAt.Equal(worldClock) {
		t.Errorf("enrolment %+v", e)
	}
	// The key the enrolment returns signs assertions that verify under the
	// key sent.
	c, err := NewClient(ClientConfig{Enrolment: e, Organization: e.Organization})
	if err != nil {
		t.Fatal(err)
	}
	as, _ := c.assertion()
	if _, _, _, ok := parseAssertion(t, as, fixtureJWKKey(t, "enrol-ed25519")); !ok {
		t.Error("the enrolled key's assertion does not verify under the key sent")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// Test201Checks holds a 201 to section 1's checks: each failure is a
// refused answer naming the client id the service created (so the operator
// can disable it), and no key is returned.
func Test201Checks(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, org string
		clientID            string // the id the failure names; "" is a lost answer
	}{
		{"contract 2", `"contract":1`, `"contract":2`, "", enrolledID},
		{"contract missing", `"contract":1,`, ``, "", enrolledID},
		// A client id that is not unambiguous and well-formed is never
		// offered as the one to disable: the answer is a lost one.
		{"client id upper case", `srv_0c0c`, `srv_0C0c`, "", ""},
		{"client id short", `srv_0c0c`, `srv_0c`, "", ""},
		{"client id with a control character", `srv_0c0c`, `srv_\u00070c`, "", ""},
		{"client id twice, different", `"contract":1`, `"contract":1,"client_id":"srv_0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d"`, "", ""},
		{"client id twice, the same", `"contract":1`, `"contract":1,"client_id":"` + enrolledID + `"`, "", ""},
		{"another issuer", `"issuer":"https://accounts.example.test"`, `"issuer":"https://other.example.test"`, "", enrolledID},
		{"issuer with a slash", `"issuer":"https://accounts.example.test"`, `"issuer":"https://accounts.example.test/"`, "", enrolledID},
		{"another token endpoint", `/oauth2/token`, `/token`, "", enrolledID},
		{"organization not one", `"organization":"example"`, `"organization":"Example"`, "", enrolledID},
		{"organization missing", `"organization":"example",`, ``, "", enrolledID},
		{"another organization than configured", `"organization":"example"`, `"organization":"other"`, "example", enrolledID},
		{"a member twice", `"contract":1`, `"contract":1,"contract":1`, "", enrolledID},
		{"client id missing", `"client_id":"` + enrolledID + `",`, ``, "", ""},
		{"client id a number", `"` + enrolledID + `"`, `7`, "", ""},
		{"not JSON", good201, `{"client_id":`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(good201, tc.old, tc.new, 1)
			if body == good201 {
				t.Fatal("the case changes nothing")
			}
			fs := newFakeService(t)
			fs.enroll = []served{{status: 201, body: []byte(body)}}
			e, err := enrolAgainst(t, fs, tc.org)
			if err == nil {
				t.Fatalf("enrolled: %+v", e)
			}
			f := asFailure(t, err)
			if f.Kind != KindBadAnswer || f.ClientID != tc.clientID || f.Lost() != (tc.clientID == "") {
				t.Errorf("%v (client id %q, lost %v)", f, f.ClientID, f.Lost())
			}
		})
	}
	// The controls: the recorded 201 enrols, with or without the
	// organization configured.
	for _, org := range []string{"", "example"} {
		fs := newFakeService(t)
		fs.enroll = []served{{status: 201, body: []byte(good201)}}
		if _, err := enrolAgainst(t, fs, org); err != nil {
			t.Errorf("organization %q: %v", org, err)
		}
	}
}

// TestLostEnrolment checks every answer that leaves the outcome unknown is
// a lost one: a 503, another 5xx, a connection lost, a timeout, a 201 too
// large to read. A refusal (4xx) is not.
func TestLostEnrolment(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    served
		lost bool
	}{
		{"503", served{status: 503, body: []byte(`{"error":{"code":"UNAVAILABLE","message":"m"}}`)}, true},
		{"500", served{status: 500}, true},
		{"201 too large", served{status: 201, body: bytes.Repeat([]byte(" "), MaxAnswerBody+1)}, true},
		{"409 spent", served{status: 409, body: []byte(`{"error":{"code":"ENROLMENT_CODE_SPENT","message":"m"}}`)}, false},
		// Another 2xx than 201: the service may have enrolled the server,
		// and its client id is not taken from an answer the contract does
		// not give.
		{"200 with a good body", served{status: 200, body: []byte(good201)}, true},
		{"202 with a good body", served{status: 202, body: []byte(good201)}, true},
		{"204", served{status: 204}, true},
		{"201 with a lone surrogate", served{status: 201, body: []byte(strings.Replace(good201, `"contract":1`, `"contract":1,"x":"\udc00"`, 1))}, true},
		{"429", served{status: 429, header: http.Header{"Retry-After": {"60"}}, body: []byte(`{"error":{"code":"RATE_LIMITED","message":"m"}}`)}, false},
	} {
		fs := newFakeService(t)
		fs.enroll = []served{tc.s}
		_, err := enrolAgainst(t, fs, "")
		if err == nil {
			t.Errorf("%s: enrolled", tc.name)
			continue
		}
		if f := asFailure(t, err); f.Lost() != tc.lost || f.ClientID != "" {
			t.Errorf("%s: %v lost=%v (client id %q), want %v and none", tc.name, f, f.Lost(), f.ClientID, tc.lost)
		}
	}

	// The connection lost after the request was read.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()
	_, err := Enroll(context.Background(), EnrolRequest{Issuer: worldIssuer, Code: "c", Name: "n", baseTransport: worldTransport(t, srv)})
	if f := asFailure(t, err); !f.Lost() || f.Kind != KindTransport {
		t.Errorf("connection lost: %v lost=%v", f, f.Lost())
	}

	// A timeout.
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = Enroll(ctx, EnrolRequest{Issuer: worldIssuer, Code: "c", Name: "n", baseTransport: worldTransport(t, slow)})
	if f := asFailure(t, err); !f.Lost() || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v lost=%v", f, f.Lost())
	}
}

// TestEnrolInputs checks a code or a name out of bounds is refused before
// anything is sent.
func TestEnrolInputs(t *testing.T) {
	for _, tc := range []struct{ code, name string }{
		{"", "n"},
		{strings.Repeat("c", 65), "n"},
		{"\xff", "n"},
		{"c", ""},
		{"c", strings.Repeat("n", 256)},
		{"c", "tab\there"},
		{"c", "café"},
	} {
		fs := newFakeService(t)
		_, err := Enroll(context.Background(), EnrolRequest{Issuer: worldIssuer, Code: tc.code, Name: tc.name, baseTransport: worldTransport(t, fs.srv)})
		if err == nil || len(fs.paths) != 0 {
			t.Errorf("code %q name %q: %v, %d requests", tc.code, tc.name, err, len(fs.paths))
		}
	}
	for _, tc := range []struct{ code, name string }{{strings.Repeat("c", 64), strings.Repeat("n", 255)}, {"c", "n ~"}} {
		if !ValidCode(tc.code) || !ValidServerName(tc.name) {
			t.Errorf("code %q name %q refused", tc.code, tc.name)
		}
	}
}

// TestEnrolmentFile round-trips enrolment.json and holds its decoding to
// strictness: an unknown member (in any case), a member twice, data after
// it, another format or a field out of form is refused.
func TestEnrolmentFile(t *testing.T) {
	e := worldEnrolment(t, worldIssuer)
	data, err := e.MarshalFile()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnrolmentFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != e.ClientID || got.Issuer != e.Issuer || got.TokenEndpoint != e.TokenEndpoint ||
		got.Organization != e.Organization || got.Name != e.Name || !got.EnrolledAt.Equal(e.EnrolledAt) ||
		!got.PrivateKey.Equal(e.PrivateKey) {
		t.Fatalf("round trip: %+v, want %+v", got, e)
	}
	compact := string(compactJSON(t, data))
	p256 := string(must(json.Marshal(string(readFixture(t, "fixtures/test-keys/server-p256.pem")))))
	for name, tc := range map[string][2]string{
		"unknown member":      {`"format":1`, `"format":1,"extra":true`},
		"member in caps":      {`"client_id"`, `"Client_ID"`},
		"member twice":        {`"format":1`, `"format":1,"format":1`},
		"format 2":            {`"format":1`, `"format":2`},
		"client id":           {worldClientID, "srv_x"},
		"token endpoint":      {`/oauth2/token"`, `/token"`},
		"organization":        {`"organization":"example"`, `"organization":"Example"`},
		"name":                {`"name":"build-1.example.test"`, `"name":""`},
		"enrolled at":         {`"enrolled_at":"2026-11-16T06:00:00Z"`, `"enrolled_at":"0001-01-01T00:00:00Z"`},
		"a P-256 key":         {compact[strings.Index(compact, `"private_key":`)+len(`"private_key":`) : len(compact)-1], p256},
		"no private key":      {`"private_key":"`, `"private_key":"x`},
		"data after the file": {compact[len(compact)-1:], "} {}"},
	} {
		broken := strings.Replace(compact, tc[0], tc[1], 1)
		if broken == compact {
			t.Errorf("%s: changes nothing", name)
			continue
		}
		if _, err := ParseEnrolmentFile([]byte(broken)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseEnrolmentFile([]byte(compact)); err != nil {
		t.Fatalf("the control: %v", err)
	}
	// An issuer that is not an origin is refused, even with a token
	// endpoint that agrees with it.
	for _, issuer := range []string{"https://accounts.example.test/prefix", "https://accounts.example.test/", "https://accounts.example.test?x", "https://user@accounts.example.test"} {
		// Both the issuer and the token endpoint change, so they agree.
		broken := strings.ReplaceAll(compact, `"https://accounts.example.test`, `"`+issuer)
		if _, err := ParseEnrolmentFile([]byte(broken)); err == nil {
			t.Errorf("issuer %q: accepted", issuer)
		}
		if _, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, issuer)}); err == nil {
			t.Errorf("issuer %q: a client was made", issuer)
		}
		fs := newFakeService(t)
		if _, err := Enroll(context.Background(), EnrolRequest{Issuer: issuer, Code: "c", Name: "n", baseTransport: worldTransport(t, fs.srv)}); err == nil || len(fs.paths) != 0 {
			t.Errorf("issuer %q: enrolment %v, %d requests", issuer, err, len(fs.paths))
		}
	}

	if err := e.CheckConfig(worldIssuer, ""); err != nil {
		t.Error(err)
	}
	if err := e.CheckConfig(worldIssuer, "example"); err != nil {
		t.Error(err)
	}
	if e.CheckConfig("https://other.example.test", "") == nil || e.CheckConfig(worldIssuer, "other") == nil {
		t.Error("an enrolment matched another issuer or organization")
	}
	if e.AcceptedOrganization("") != "example" || e.AcceptedOrganization("other") != "other" {
		t.Error("AcceptedOrganization")
	}
}
