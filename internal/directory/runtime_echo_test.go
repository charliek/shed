package directory

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/users"
)

// TestRuntimeRefusesAnAnswerEchoingTheBearer: a directory 200 that holds the
// access token the request carried is not a good pull, wherever the token
// is: in a member a reader ignores, as an entry's name (an entry that would
// be skipped, and its name logged), or written with a \u escape. It is a
// failed refresh of its own kind: nothing is written to the kept copy,
// nothing is published, and neither the log nor the row nor status holds
// the token.
func TestRuntimeRefusesAnAnswerEchoingTheBearer(t *testing.T) {
	for _, mode := range []faultMode{faultEchoIgnored, faultEchoName, faultEchoEscaped} {
		t.Run(string(mode), func(t *testing.T) {
			clock, svc, dir, opsLine, ada := rtSetup(t)
			srv := startRT(t, svc, dir, opsLine, nil)
			first := srv.refresh(t)
			if first.Status != users.DirectoryRefreshGood {
				t.Fatalf("set-up pull: %+v", first)
			}
			kept := copyBytes(t, dir)

			clock.advance(time.Minute)
			bob := newPerson(t, "bob", false)
			svc.dir.setUsers(ada.served(), bob.served())
			line, _ := newTestKey(t)
			svc.mu.Lock()
			svc.echoKey = line
			svc.mu.Unlock()
			svc.setMode(mode, "")
			row := srv.refresh(t)
			svc.mu.Lock()
			echoed := svc.echoed
			svc.mu.Unlock()
			if echoed == 0 {
				t.Fatal("the service echoed no token: the test checks nothing")
			}
			if row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, string(KindCredentialEchoed)) ||
				!strings.Contains(row.Error, "holds the access token this server sent") || row.Version != first.Version {
				t.Fatalf("the echoing answer's row: %+v", row)
			}
			if !bytes.Equal(copyBytes(t, dir), kept) {
				t.Fatal("the kept copy changed")
			}
			if srv.owner(bob.fp) != "" || srv.owner(ada.fp) != "ada" {
				t.Fatal("the echoing answer's list was published")
			}
			secrets := svc.secrets()
			if len(secrets) == 0 {
				t.Fatal("no token was recorded")
			}
			st := srv.rt.Status()
			body, err := base64.StdEncoding.DecodeString(readCopy(t, dir).Body)
			if err != nil {
				t.Fatal(err)
			}
			for what, text := range map[string]string{
				"the log":         srv.log.String(),
				"the row":         fmt.Sprintf("%+v", row),
				"status":          fmt.Sprintf("%+v", st),
				"the kept copy":   string(copyBytes(t, dir)),
				"the copy's body": string(body),
			} {
				noSecretsIn(t, what, text, secrets)
			}
			if !strings.Contains(srv.log.String(), string(KindCredentialEchoed)) {
				t.Errorf("the failure was not logged:\n%s", srv.log)
			}

			// The same service, answering plainly again: a good pull.
			svc.setMode(faultNone, "")
			if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || srv.owner(bob.fp) != "bob" {
				t.Fatalf("the plain answer after: %+v", row)
			}
		})
	}
}

// TestPullRefusesAnAnswerEchoingTheRefusedToken: the first access token is
// refused (401 invalid_token), the client logs in again, and the 200 to its
// second try holds the FIRST token: that answer is refused too, its failure
// text naming neither token. The same sequence with a 200 that holds no
// token is a good pull.
func TestPullRefusesAnAnswerEchoingTheRefusedToken(t *testing.T) {
	const first, second = "opaque-token", "another-bearer-value"
	secondToken := goodToken
	secondToken.body = []byte(`{"access_token":"` + second + `","token_type":"bearer","expires_in":600}`)
	notValid := served{status: 401, header: http.Header{"Www-Authenticate": {`Bearer error="invalid_token"`}},
		body: []byte(`{"error":{"code":"invalid_token","message":"m"}}`)}
	body := worldListBody(t)
	for _, tc := range []struct {
		name string
		body []byte
		good bool
	}{
		{"the first token echoed", append([]byte(`{"note":"`+first+`",`), body[1:]...), false},
		{"no token echoed", body, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeService(t)
			fs.tokens = []served{goodToken, secondToken}
			fs.directory = []served{notValid, {status: 200, body: tc.body}}
			_, err := fs.client(t, worldOrg).Pull(context.Background())
			if tc.good {
				if err != nil {
					t.Fatalf("Pull: %v", err)
				}
				return
			}
			if f := asFailure(t, err); f.Kind != KindCredentialEchoed {
				t.Fatalf("Pull: %v, want %s", err, KindCredentialEchoed)
			}
			if strings.Contains(err.Error(), first) || strings.Contains(err.Error(), second) {
				t.Fatalf("the failure names a token: %v", err)
			}
		})
	}
}

// TestEchoesFindsATokenHoweverItIsWritten: echoes finds a token sent as its
// bytes stand, inside a member's name, and written with escapes; a body
// without it, and an empty token, find nothing.
func TestEchoesFindsATokenHoweverItIsWritten(t *testing.T) {
	const tok = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"
	// esc is a JSON \u escape of the code unit hex (the backslash made from
	// its byte, so no tool reading this file can turn it into the
	// character).
	esc := func(hex string) string { return string([]byte{92, 'u'}) + hex }
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"absent", `{"users":[],"note":"nothing"}`, false},
		{"a value", `{"users":[],"note":"x ` + tok + ` y"}`, true},
		{"a member's name", `{"users":[],"` + tok + `":1}`, true},
		{"escaped", `{"users":[],"note":"` + esc("0065") + tok[1:] + `"}`, true},
		{"its dots escaped", `{"users":[],"note":"` + strings.ReplaceAll(tok, ".", esc("002e")) + `"}`, true},
		{"escaped, after a number no float holds", `{"n":1e400,"note":"` + esc("0065") + tok[1:] + `"}`, true},
		{"absent, after a number no float holds", `{"n":1e400,"note":"x"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := echoes([]byte(tc.body), []string{"", tok})
			if err != nil || got != tc.want {
				t.Fatalf("echoes = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if got, err := echoes([]byte(`{"note":""}`), []string{""}); got || err != nil {
		t.Fatalf("an empty token: %v, %v", got, err)
	}
}
