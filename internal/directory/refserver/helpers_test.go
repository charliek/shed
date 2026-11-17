package refserver

import (
	"crypto"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Helpers shared by the verification tests: a world as the exchanges start
// from it, and requests built by hand.

const (
	srvA       = "srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
	srvB       = "srv_0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
	worldIss   = "https://accounts.example.test"
	srvCCode   = "pending-code-for-srv-c-0000000000000"
	enrolName  = "build-1.example.test"
	formPrefix = "grant_type=client_credentials&client_assertion_type=urn%3Aietf%3Aparams%3Aoauth%3Aclient-assertion-type%3Ajwt-bearer&client_assertion="
)

// newWorld is a reference server loaded with the contract's world, at its
// clock.
func newWorld(t *testing.T) *scenario {
	t.Helper()
	return newHarness(t).newScenario(t, &exchange{Name: t.Name()})
}

// do sends a request to the scenario's server, from the harness's caller.
func (s *scenario) do(req *http.Request) *httptest.ResponseRecorder {
	req.RemoteAddr = callerAddr
	rec := httptest.NewRecorder()
	s.srv.ServeHTTP(rec, req)
	return rec
}

// post sends a body with a content type.
func (s *scenario) post(path, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return s.do(req)
}

// tokenRequest sends a form to the token endpoint.
func (s *scenario) tokenRequest(form string) *httptest.ResponseRecorder {
	return s.post(pathToken, "application/x-www-form-urlencoded", form)
}

// withAssertion is the standard form carrying an assertion.
func withAssertion(a string) string { return formPrefix + url.QueryEscape(a) }

// oauthError decodes a token endpoint error's code.
func oauthError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an OAuth error: %q", rec.Body.String())
	}
	return e.Error
}

// v1Error decodes a /v1 error's code.
func v1Error(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a /v1 error: %q", rec.Body.String())
	}
	return e.Error.Code
}

// assertionParts are an assertion's header and claims, in order, so that a
// test can change, drop or repeat one member.
type assertionParts struct {
	header, claims []member
}

// defaultAssertion is a valid assertion's parts for a client at the
// scenario's clock, with the given algorithm.
func (s *scenario) defaultAssertion(clientID, alg string) assertionParts {
	iat := s.clock.Unix()
	s.jtiSeq++
	return assertionParts{
		header: []member{{"alg", rawJSON(alg)}, {"typ", rawJSON("client-authentication+jwt")}},
		claims: []member{
			{"iss", rawJSON(clientID)},
			{"sub", rawJSON(clientID)},
			{"aud", rawJSON(worldIss)},
			{"iat", rawJSON(iat)},
			{"exp", rawJSON(iat + 60)},
			{"jti", rawJSON("hand-" + randomHex(8))},
		},
	}
}

// set replaces a member's value (raw JSON), or adds it; drop removes it.
func set(ms []member, name, raw string) []member {
	out := append([]member(nil), ms...)
	for i := range out {
		if out[i].name == name {
			out[i].value = json.RawMessage(raw)
			return out
		}
	}
	return append(out, member{name, json.RawMessage(raw)})
}

func drop(ms []member, name string) []member {
	var out []member
	for _, m := range ms {
		if m.name != name {
			out = append(out, m)
		}
	}
	return out
}

// sign makes the compact JWS of the parts with a client key.
func (p assertionParts) sign(key crypto.Signer) string {
	input := b64(objectText(p.header)) + "." + b64(objectText(p.claims))
	return input + "." + b64(signClient(key, input))
}

// key is the client test key test-keys/<stem>.pem.
func (s *scenario) key(stem string) crypto.Signer { return s.h.clientKeys[stem] }

// bearerParts are an access token's header and claims, valid for a client
// at the scenario's clock.
func (s *scenario) bearerParts(clientID string) assertionParts {
	iat := s.clock.Unix()
	return assertionParts{
		header: []member{{"alg", rawJSON("RS256")}, {"typ", rawJSON("at+jwt")}, {"kid", rawJSON(fixtureKeyID)}},
		claims: []member{
			{"iss", rawJSON(worldIss)},
			{"sub", rawJSON(clientID)},
			{"client_id", rawJSON(clientID)},
			{"aud", rawJSON([]string{worldIss + "/v1"})},
			{"gty", rawJSON("client_credentials")},
			{"scp", rawJSON([]string{"directory:read"})},
			{"jti", rawJSON("hand-" + randomHex(8))},
			{"iat", rawJSON(iat)},
			{"nbf", rawJSON(iat)},
			{"exp", rawJSON(iat + 600)},
		},
	}
}

// signBearer signs access token parts with the fixture signing key.
func (s *scenario) signBearer(p assertionParts) string {
	input := b64(objectText(p.header)) + "." + b64(objectText(p.claims))
	return input + "." + b64(s.signRS256(input))
}

// directory sends GET /v1/directory with the given Authorization values.
func (s *scenario) directory(auth ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, pathDirectory, nil)
	for _, a := range auth {
		req.Header.Add("Authorization", a)
	}
	return s.do(req)
}

// at moves the scenario's clock.
func (s *scenario) at(t time.Time) { s.clock = t }
