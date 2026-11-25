package directory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/charliek/shed/internal/delivery"
)

// fixtureRequestLine is requests/create-repo's document, compact: the line
// a server sends for it (id 0123456789abcdef0123456789abcdef).
func fixtureRequestLine(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(providedV2, "fixtures", "requests", "create-repo.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rf struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		t.Fatal(err)
	}
	req, refusal := delivery.ParseRequest(rf.Request)
	if refusal != nil {
		t.Fatal(refusal)
	}
	line, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// echoService grants a delivery token and answers the call with what reply
// makes of the bearer and the assertion presented.
func echoService(t *testing.T, reply func(w http.ResponseWriter, bearer, assertion string)) *Client {
	t.Helper()
	var mu sync.Mutex
	assertion := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			_ = r.ParseForm()
			mu.Lock()
			assertion = r.PostForm.Get("client_assertion")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"tok-0f3c9a7e","token_type":"bearer","expires_in":600,"scope":"deliveries:request"}`)
		case "/v1/deliveries":
			mu.Lock()
			a := assertion
			mu.Unlock()
			reply(w, "tok-0f3c9a7e", a)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, srv.URL), AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestDeliveryEchoInDecodedContent: an answer that is valid in every rule
// but holds the bearer or the assertion the server presented where only the
// answer rules decode it (a file's base64 content), or in an env value, is
// refused as an echo, and its body is not returned.
func TestDeliveryEchoInDecodedContent(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	file := func(content string) string {
		return fmt.Sprintf(`{"name":"note","provider":"files","kind":"file","stage":1,"base":"home","path":".config/note","mode":"0600","content":%q,"fingerprint":"f"}`,
			base64.StdEncoding.EncodeToString([]byte(content)))
	}
	for _, tc := range []struct {
		name string
		item func(bearer, assertion string) string
	}{
		{"the bearer in a file's content", func(b, _ string) string { return file("token: " + b + "\n") }},
		{"the assertion in a file's content", func(_, a string) string { return file(a) }},
		{"the assertion in an env value", func(_, a string) string {
			return fmt.Sprintf(`{"name":"e","provider":"p","kind":"env","variable":"E","value":%q,"fingerprint":"f"}`, a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := echoService(t, func(w http.ResponseWriter, bearer, assertion string) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"version":2,"id":%q,"items":[%s],"problems":[]}`, id, tc.item(bearer, assertion))
			})
			st, body, _ := c.Deliver(context.Background(), fixtureRequestLine(t))
			if st != delivery.HostedEchoed || body != nil {
				t.Fatalf("Deliver = %s (%d bytes), want an echo refused", st, len(body))
			}
		})
	}
	// The same answer with other content is an answer.
	c := echoService(t, func(w http.ResponseWriter, _, _ string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"version":2,"id":%q,"items":[%s],"problems":[]}`, id, file("nothing presented"))
	})
	if st, body, _ := c.Deliver(context.Background(), fixtureRequestLine(t)); st != delivery.HostedAnswer || body == nil {
		t.Fatalf("a clean answer = %s", st)
	}
}

// TestDelivery410ByStatusAlone: a 410 is source_timeout whatever its body:
// none, HTML, JSON that does not parse, or another code.
func TestDelivery410ByStatusAlone(t *testing.T) {
	for _, body := range []string{"", "<html>gone</html>", `{"error":`, `{"error":{"code":"GONE","message":"x"}}`} {
		c := echoService(t, func(w http.ResponseWriter, _, _ string) {
			w.WriteHeader(http.StatusGone)
			fmt.Fprint(w, body)
		})
		if st, _, _ := c.Deliver(context.Background(), fixtureRequestLine(t)); st != delivery.HostedTimeout {
			t.Fatalf("410 with body %q = %s, want timeout", body, st)
		}
	}

}
