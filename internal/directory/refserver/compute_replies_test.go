package refserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider/service"
)

// The compute contract's replies, sent by the reference service: for every
// reply of fixtures/replies, the service is told (as compute.json's
// replies tell it) to send it, a real HTTP call is made with a compute
// token, what arrives is held to the fixture (its status, the headers it
// names, its body), and shed's classifier (internal/remote/provider/service)
// reads it as the fixture expects. This test may import shed packages; the
// service's own code imports none.

type computeReplyFixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Call        string `json:"call"`
	Replies     []struct {
		ComputeReply
		Unreachable bool `json:"unreachable"`
	} `json:"replies"`
	Expect struct {
		Result      string `json:"result"`
		Code        string `json:"code"`
		Kind        string `json:"kind"`
		RetryAfterS *int   `json:"retry_after_s"`
		Operation   string `json:"operation"`
	} `json:"expect"`
}

// replyRequest is a request for a call: its method, path and body.
func replyRequest(call string) (string, string, any) {
	id := "q6c3g4ymb5m2dkxwe7kyz5hhzu"
	m := pathComputePrefix + "machines/" + id
	rq := map[string]any{"request_id": "00000000-0000-4000-8000-000000000001"}
	switch call {
	case "catalog":
		return http.MethodGet, pathComputePrefix + "catalog", nil
	case "create":
		return http.MethodPut, m, map[string]any{"request_id": rq["request_id"], "name": "widgets", "user": "ada",
			"size": "standard", "image": "default", "labels": map[string]string{}, "authorized_keys": []string{"ssh-ed25519 AAAA"},
			"spec": "", "access_kinds": []string{"ssh"}}
	case "start", "stop":
		return http.MethodPost, m + "/" + call, rq
	case "delete":
		return http.MethodDelete, m, rq
	case "operation":
		return http.MethodGet, pathComputePrefix + "operations/op-1", nil
	case "get":
		return http.MethodGet, m, nil
	case "list":
		return http.MethodGet, pathComputePrefix + "machines", nil
	case "labels":
		return http.MethodPost, m + "/labels", map[string]any{"request_id": rq["request_id"], "set": map[string]string{}}
	case "extend":
		return http.MethodPost, m + "/extend", map[string]any{"request_id": rq["request_id"], "by_s": 3600}
	case "usage":
		return http.MethodGet, pathComputePrefix + "usage", nil
	case "ingress_get":
		return http.MethodGet, pathComputePrefix + "ingress", nil
	case "ingress_put":
		return http.MethodPut, pathComputePrefix + "ingress", map[string]any{"request_id": rq["request_id"], "sources": []string{}}
	}
	return "", "", nil
}

// testComputeAssertions are the made-up client assertions each attempt's
// token was obtained with: a fixture's {{assertion}} stands for its
// attempt's.
var testComputeAssertions = [2]string{
	"eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJzcnZfdGVzdCJ9.YXNzZXJ0aW9u",
	"eyJhbGciOiJFZERTQSJ9.eyJqdGkiOiJyZXRyeSJ9.YmlzYmlz",
}

func TestComputeRepliesTheServiceSends(t *testing.T) {
	h := newHarness(t)
	n := 0
	for _, rel := range computeFiles(t, "fixtures/replies") {
		var f computeReplyFixture
		decodeClosed(t, rel, readCompute(t, rel), &f)
		n++
		t.Run(f.Name, func(t *testing.T) {
			s := h.newScenario(t, &exchange{Name: f.Name})
			if err := s.srv.applyComputeStep("compute.allow", json.RawMessage(`"srv-a"`), nil); err != nil {
				t.Fatal(err)
			}
			hs := httptest.NewServer(s.srv)
			defer hs.Close()
			mint := func() string {
				return s.accessToken(json.RawMessage(`{"client_id":"` + srvA + `","scope":["compute:manage"]}`))
			}
			method, path, body := replyRequest(f.Call)
			if method == "" {
				t.Fatalf("call %q", f.Call)
			}
			var replies []service.Reply
			for i, fr := range f.Replies {
				// Each attempt presents its own credentials: a token minted
				// for it, and a made-up assertion it was obtained with (the
				// retry after a 401 has a new pair). {{bearer}} and
				// {{assertion}} stand for this attempt's.
				tok := mint()
				assertion := testComputeAssertions[i]
				want := fr.ComputeReply
				esc := func(c string) string { return fmt.Sprintf(`\u%04x%s`, c[0], c[1:]) }
				fill := func(b string) string {
					return strings.NewReplacer("{{bearer}}", tok, "{{assertion}}", assertion,
						"{{bearer_escaped}}", esc(tok), "{{assertion_escaped}}", esc(assertion)).Replace(b)
				}
				if want.Body != nil {
					want.Body = json.RawMessage(fill(string(want.Body)))
				}
				if want.Text != nil {
					t2 := fill(*want.Text)
					want.Text = &t2
				}
				base := hs.URL
				if fr.Unreachable {
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					base = "http://" + ln.Addr().String()
					ln.Close()
				} else {
					s.srv.compute.mu.Lock()
					s.srv.compute.replies = map[string]ComputeReply{f.Call: want}
					s.srv.compute.mu.Unlock()
				}
				var rd io.Reader
				if body != nil {
					b, _ := json.Marshal(body)
					rd = bytes.NewReader(b)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if fr.Silent {
					ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
				}
				req, _ := http.NewRequestWithContext(ctx, method, base+path, rd)
				req.Header.Set("Authorization", "Bearer "+tok)
				if body != nil {
					req.Header.Set("Content-Type", "application/json")
				}
				client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				res, err := client.Do(req)
				rep := service.Reply{Bearer: tok, Assertion: assertion}
				if err != nil {
					rep.Err = err
					cancel()
					if !fr.Silent && !fr.Unreachable {
						t.Fatalf("reply %d: %v", i, err)
					}
					replies = append(replies, rep)
					continue
				}
				rep.Status, rep.Header = res.StatusCode, res.Header
				b, rerr := service.ReadBody(res.Body, service.BodyLimit(service.Call(f.Call), res.StatusCode))
				res.Body.Close()
				cancel()
				switch {
				case errors.Is(rerr, service.ErrTooLarge):
					rep.Body, rep.TooLarge = b, true
				case rerr != nil:
					rep.Err = rerr
				default:
					rep.Body = b
				}
				if fr.Silent || fr.Unreachable {
					t.Fatalf("reply %d: a reply arrived (%d)", i, res.StatusCode)
				}
				checkArrived(t, i, want, rep, tok)
				replies = append(replies, rep)
			}
			c := service.Context{ShedID: "q6c3g4ymb5m2dkxwe7kyz5hhzu", Server: "srv-a"}
			if f.Call == "list" {
				c.ShedID = ""
			}
			got := service.Resolve(service.Call(f.Call), replies, c)
			if string(got.Result) != f.Expect.Result || got.Code != f.Expect.Code {
				t.Fatalf("classified %s %q (%v), want %s %q", got.Result, got.Code, got.Kind, f.Expect.Result, f.Expect.Code)
			}
			if f.Expect.RetryAfterS != nil && got.RetryAfter != time.Duration(*f.Expect.RetryAfterS)*time.Second {
				t.Errorf("retry after %v, want %d s", got.RetryAfter, *f.Expect.RetryAfterS)
			}
		})
	}
	if n < 40 {
		t.Fatalf("only %d replies", n)
	}
}

// checkArrived holds what arrived to the reply the service was told to
// send.
func checkArrived(t *testing.T, i int, want ComputeReply, got service.Reply, bearer string) {
	t.Helper()
	switch want.Generate {
	case "":
	case GenTruncated:
		if got.Err == nil {
			t.Errorf("reply %d: a truncated body read whole", i)
		}
		return
	case GenOversize, GenOversizeList, GenOversizeError, GenOversizeErrorEchoBearer:
		if !got.TooLarge {
			t.Errorf("reply %d: %s was not over its limit", i, want.Generate)
		}
		return
	case GenEchoBearer:
		if !bytes.Contains(got.Body, []byte(bearer)) {
			t.Errorf("reply %d: the echo does not hold the bearer", i)
		}
		return
	case GenEchoBearerEscaped:
		var v struct {
			Machine struct {
				Spec string `json:"spec"`
			} `json:"machine"`
		}
		if bytes.Contains(got.Body, []byte(bearer)) || json.Unmarshal(got.Body, &v) != nil || v.Machine.Spec != bearer {
			t.Errorf("reply %d: the escaped echo is not the bearer, escaped", i)
		}
		return
	case GenList501:
		var l struct {
			Machines []json.RawMessage `json:"machines"`
		}
		if json.Unmarshal(got.Body, &l) != nil || len(l.Machines) != maxComputeListMachines+1 {
			t.Errorf("reply %d: not a list of 501 machines", i)
		}
		return
	}
	if got.Status != want.Status {
		t.Fatalf("reply %d: status %d, want %d", i, got.Status, want.Status)
	}
	for k, v := range want.Headers {
		if g := got.Header.Get(k); g != v {
			t.Errorf("reply %d: %s %q, want %q", i, k, g, v)
		}
	}
	switch {
	case want.Text != nil:
		if string(got.Body) != *want.Text {
			t.Errorf("reply %d: body %q, want %q", i, got.Body, *want.Text)
		}
	case len(want.Body) > 0:
		var w, g any
		if json.Unmarshal(want.Body, &w) != nil || json.Unmarshal(got.Body, &g) != nil || !reflect.DeepEqual(w, g) {
			t.Errorf("reply %d: body %s, want %s", i, got.Body, strings.TrimSpace(string(want.Body)))
		}
	default:
		if len(got.Body) != 0 {
			t.Errorf("reply %d: body %q, want none", i, got.Body)
		}
	}
}
