package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// exchangeStep is one step of an exchange: its request's method and path,
// and the response recorded for it.
type exchangeStep struct {
	Request struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"request"`
	Response struct {
		Status  int                `json:"status"`
		Headers map[string]*string `json:"headers"`
		Body    json.RawMessage    `json:"body"`
		Text    *string            `json:"text"`
	} `json:"response"`
}

// served is the step's recorded answer with its placeholders filled.
func (s exchangeStep) served() served {
	out := served{status: s.Response.Status, header: http.Header{}}
	for name, v := range s.Response.Headers {
		if v != nil {
			out.header.Set(name, *v)
		}
	}
	switch {
	case s.Response.Text != nil:
		out.body = []byte(*s.Response.Text)
	case len(s.Response.Body) > 0 && string(s.Response.Body) != "null":
		out.body = fillPlaceholders(s.Response.Body)
	}
	return out
}

// errorCode is the recorded body's error code, in either error shape.
func (s exchangeStep) errorCode() string {
	code, _ := errorBody(s.Response.Body)
	return code
}

// exchangeExpectations is what the client makes of each recorded answer, by
// request, status and error code. A recorded answer this table does not
// cover fails the test, so a new exchange is classified on purpose.
var exchangeExpectations = map[string]Kind{
	"GET /v1/directory 200":                                "",
	"GET /v1/directory 401 invalid_token":                  KindInvalidToken,
	"GET /v1/directory 403 SERVER_DISABLED":                KindServerDisabled,
	"GET /v1/directory 403 INSUFFICIENT_SCOPE":             KindRefused,
	"GET /v1/directory 429 RATE_LIMITED":                   KindRateLimited,
	"POST /oauth2/token 200":                               "",
	"POST /oauth2/token 401 invalid_client":                KindInvalidClient,
	"POST /oauth2/token 429 temporarily_unavailable":       KindRateLimited,
	"POST /oauth2/token 503 server_error":                  KindUnavailable,
	"POST /v1/servers/enroll 201":                          "",
	"POST /v1/servers/enroll 400 INVALID_PUBLIC_KEY":       KindRefused,
	"POST /v1/servers/enroll 401 ENROLMENT_CODE_INVALID":   KindRefused,
	"POST /v1/servers/enroll 409 ENROLMENT_CODE_SPENT":     KindRefused,
	"POST /v1/servers/enroll 409 PUBLIC_KEY_IN_USE":        KindRefused,
	"POST /v1/servers/enroll 409 SERVER_NAME_IN_USE":       KindRefused,
	"POST /v1/servers/enroll 410 ENROLMENT_CODE_CANCELLED": KindRefused,
	"POST /v1/servers/enroll 410 ENROLMENT_CODE_EXPIRED":   KindRefused,
	"POST /v1/servers/enroll 429 RATE_LIMITED":             KindRateLimited,
}

// TestClientOnExchanges runs the client against every answer recorded in
// fixtures/exchanges: each directory answer through a pull, each token
// answer through a login, each enrolment answer through an enrolment, and
// holds the outcome to the table above (and, where the answer carries
// Retry-After, its wait). The well-known documents are never fetched by a
// client (rule 13), so their answers are not asked for.
func TestClientOnExchanges(t *testing.T) {
	ran := map[string]int{}
	for _, rel := range fixtureFiles(t, "fixtures/exchanges", ".json") {
		var ex struct {
			Name string         `json:"name"`
			Then []exchangeStep `json:"then"`
			exchangeStep
		}
		decodeFixture(t, rel, &ex)
		steps := append([]exchangeStep{ex.exchangeStep}, ex.Then...)
		for i, st := range steps {
			req := st.Request.Method + " " + st.Request.Path
			key := req + " " + strconv.Itoa(st.Response.Status)
			if code := st.errorCode(); code != "" {
				key += " " + code
			}
			if st.Request.Path == "/.well-known/jwks.json" || st.Request.Path == "/.well-known/oauth-authorization-server" {
				continue
			}
			want, ok := exchangeExpectations[key]
			if !ok {
				t.Errorf("%s step %d: no expectation for %q", ex.Name, i, key)
				continue
			}
			ran[key]++
			t.Run(fmt.Sprintf("%s/%d", ex.Name, i), func(t *testing.T) {
				err := runExchangeStep(t, req, st.served())
				checkExchangeOutcome(t, st, want, err)
			})
		}
	}
	for key := range exchangeExpectations {
		if ran[key] == 0 {
			t.Errorf("no exchange answers %q any more; drop it from the table", key)
		}
	}
}

// runExchangeStep runs the client operation the step's request belongs to
// against a service answering it with s.
func runExchangeStep(t *testing.T, req string, s served) error {
	t.Helper()
	fs := newFakeService(t)
	switch req {
	case "GET /v1/directory":
		fs.directory = []served{s}
		_, err := fs.client(t, worldOrg).Pull(context.Background())
		if s.status == http.StatusUnauthorized {
			// A 401 is answered by one new login and one more try.
			if d, tk := fs.count("GET /v1/directory"), fs.count("POST /oauth2/token"); d != 2 || tk != 2 {
				t.Errorf("after a 401: %d directory requests and %d logins, want 2 and 2", d, tk)
			}
		}
		return err
	case "POST /oauth2/token":
		fs.tokens = []served{s}
		fs.directory = []served{{status: 200, header: http.Header{"Content-Type": {"application/json"}}, body: worldListBody(t)}}
		_, err := fs.client(t, worldOrg).Pull(context.Background())
		if err != nil && fs.count("GET /v1/directory") != 0 {
			t.Errorf("the directory was asked after a failed login")
		}
		return err
	case "POST /v1/servers/enroll":
		fs.enroll = []served{s}
		e, err := Enroll(context.Background(), EnrolRequest{
			Issuer: worldIssuer, Code: "pending-code-for-srv-c-0000000000000", Name: "build-1.example.test",
			baseTransport: worldTransport(t, fs.srv),
		})
		if err == nil && e.ClientID != enrolledID {
			t.Errorf("enrolled as %q", e.ClientID)
		}
		return err
	}
	t.Fatalf("no client operation for %s", req)
	return nil
}

func checkExchangeOutcome(t *testing.T, st exchangeStep, want Kind, err error) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("failed, want success: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("succeeded, want %s", want)
	}
	f := asFailure(t, err)
	if f.Kind != want || f.Status != st.Response.Status {
		t.Errorf("%s (HTTP %d), want %s (HTTP %d): %v", f.Kind, f.Status, want, st.Response.Status, f)
	}
	if code := st.errorCode(); f.Code != code {
		t.Errorf("code %q, want %q", f.Code, code)
	}
	if f.Lost() {
		t.Errorf("an answer that arrived is a lost enrolment")
	}
	if ra := st.Response.Headers["Retry-After"]; ra != nil {
		secs, _ := strconv.Atoi(*ra)
		if !f.HasRetryAfter || f.RetryAfter != time.Duration(secs)*time.Second {
			t.Errorf("Retry-After %v (%v), want %ss", f.RetryAfter, f.HasRetryAfter, *ra)
		}
	} else if f.HasRetryAfter {
		t.Errorf("a Retry-After where the answer had none")
	}
}

// worldListBody is the recorded directory-ok answer, placeholders filled.
func worldListBody(t *testing.T) []byte {
	t.Helper()
	var ex struct {
		Then []exchangeStep `json:"then"`
	}
	decodeFixture(t, "fixtures/exchanges/directory-ok.json", &ex)
	return ex.Then[len(ex.Then)-1].served().body
}

// TestClientOnDirectoryOK checks the recorded directory-ok answer admits the
// world's users exactly, with every key used.
func TestClientOnDirectoryOK(t *testing.T) {
	p, err := classifyServed(served{status: 200, header: http.Header{}, body: worldListBody(t)}, worldOrg)
	if err != nil {
		t.Fatal(err)
	}
	var world struct {
		Users []struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
			Keys  []struct {
				Key     string `json:"key"`
				Label   string `json:"label"`
				Expires string `json:"expires"`
			} `json:"keys"`
		} `json:"users"`
	}
	decodeFixture(t, "fixtures/world.json", &world)
	want := clientFixture{}
	want.Expect.Users = world.Users
	if got, w := describeList(p.List), want.expectedList(); got != w {
		t.Errorf("\n got %s\nwant %s", got, w)
	}
}
