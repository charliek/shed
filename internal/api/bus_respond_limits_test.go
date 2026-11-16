package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

// answerEnvelope is the final answer to request id of shed, as the respond
// route decodes it.
func answerEnvelope(shed, id string) *plugin.Envelope {
	return &plugin.Envelope{
		Namespace: "ssh-agent",
		Type:      plugin.MessageTypeResponse,
		InReplyTo: id,
		Final:     true,
		Shed:      &plugin.ShedInfo{Name: shed},
	}
}

// TestBusRespondRefusesBeyondTheShedsResponseBound: while a shed has as many
// responses being delivered as it may, another response to it is refused
// with a retryable 429 (Retry-After) and nothing of it is consumed: once a
// delivery ends the same answer goes through.
func TestBusRespondRefusesBeyondTheShedsResponseBound(t *testing.T) {
	s := busUsersServer(t)
	box := &sendCounter{}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: box.send})
	if _, err := s.plugins.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"}); err != nil {
		t.Fatal(err)
	}
	id := publishFromShed(t, s, "box", gen)

	// Every response slot of box is taken by a delivery in progress.
	var inFlight []*plugin.Response
	for {
		resp, err := s.bridge.BeginResponse("alice", answerEnvelope("box", id))
		if errors.Is(err, plugin.ErrTooManyResponses) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		inFlight = append(inFlight, resp)
		if len(inFlight) > 1000 {
			t.Fatal("test set-up: the shed's responses in flight are not bounded")
		}
	}

	const path = "/api/plugins/listeners/ssh-agent/respond"
	r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBody("box", id))
	if r.code != http.StatusTooManyRequests || r.errCode(t) != "TOO_MANY_RESPONSES" || r.header.Get("Retry-After") == "" {
		t.Fatalf("respond with every slot taken = %d %s (Retry-After %q), want 429 TOO_MANY_RESPONSES with Retry-After", r.code, r.body, r.header.Get("Retry-After"))
	}
	if n := box.count(); n != 0 {
		t.Fatalf("box was sent %d responses past its bound", n)
	}

	inFlight[0].End()
	// Nothing was consumed by the refusal: the same answer goes through.
	if r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBody("box", id)); r.code != http.StatusNoContent {
		t.Fatalf("the same answer once a slot freed = %d %s, want 204", r.code, r.body)
	}
	if n := box.count(); n != 1 {
		t.Fatalf("box was sent %d responses, want 1", n)
	}
	for _, resp := range inFlight[1:] {
		resp.End()
	}
}

// TestBusRespondNotWrittenInTimeCanBeRetried: a final response that could
// not begin its write before its deadline (the shed's transport was busy)
// is answered 503 with Retry-After, and its request is still pending: the
// same response, retried, is delivered. (Consumed before delivery, the
// retry got 403 while the guest still waited.) A response whose write
// failed is 404.
func TestBusRespondNotWrittenInTimeCanBeRetried(t *testing.T) {
	s := busUsersServer(t)
	busy := true
	var written []*plugin.Envelope
	writeErr := error(nil)
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1,
		SendWithin: func(env *plugin.Envelope, _ time.Time, admit func() error, wrote func()) error {
			if busy {
				return plugin.ErrResponseTimeout // the write lock was not had by the deadline
			}
			if err := admit(); err != nil {
				return err
			}
			if writeErr != nil {
				return writeErr
			}
			written = append(written, env)
			wrote()
			return nil
		}})
	if _, err := s.plugins.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"}); err != nil {
		t.Fatal(err)
	}
	id := publishFromShed(t, s, "box", gen)
	const path = "/api/plugins/listeners/ssh-agent/respond"

	r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBody("box", id))
	if r.code != http.StatusServiceUnavailable || r.errCode(t) != "RESPONSE_NOT_DELIVERED" || r.header.Get("Retry-After") == "" {
		t.Fatalf("a response not written in time = %d %s (Retry-After %q), want 503 RESPONSE_NOT_DELIVERED with Retry-After", r.code, r.body, r.header.Get("Retry-After"))
	}

	busy, writeErr = false, errors.New("broken pipe")
	if r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBody("box", id)); r.code != http.StatusNotFound || r.errCode(t) != "SHED_NOT_CONNECTED" {
		t.Fatalf("a response whose write failed = %d %s, want 404 SHED_NOT_CONNECTED", r.code, r.body)
	}

	writeErr = nil
	if r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBody("box", id)); r.code != http.StatusNoContent {
		t.Fatalf("the same response retried = %d %s, want 204", r.code, r.body)
	}
	if len(written) != 1 {
		t.Fatalf("box was written %d responses, want the retried one", len(written))
	}
	if r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBody("box", id)); r.code != http.StatusForbidden {
		t.Fatalf("a replay after the response was written = %d %s, want 403", r.code, r.body)
	}
}

// respondBodyPadded is a final response to request id of shed whose payload
// makes the body exactly size bytes.
func respondBodyPadded(t *testing.T, shed, id string, size int) string {
	t.Helper()
	head := fmt.Sprintf(`{"namespace":"ssh-agent","type":"response","in_reply_to":%q,"final":true,"shed":{"name":%q},"payload":{"pad":"`, id, shed)
	const tail = `"}}`
	pad := size - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("test set-up: a body of %d bytes is too small", size)
	}
	return head + strings.Repeat("x", pad) + tail
}

// TestBusRespondBodyIsCapped: the respond route reads a body only up to the
// largest frame a guest takes. A larger one is refused with 413 before it is
// decoded or matched, and consumes nothing: a body within the cap answering
// the same request is then delivered.
func TestBusRespondBodyIsCapped(t *testing.T) {
	s := busUsersServer(t)
	box := &sendCounter{}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: box.send})
	if _, err := s.plugins.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"}); err != nil {
		t.Fatal(err)
	}
	id := publishFromShed(t, s, "box", gen)
	const path = "/api/plugins/listeners/ssh-agent/respond"

	r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBodyPadded(t, "box", id, agentproto.MaxMessageSize+1))
	if r.code != http.StatusRequestEntityTooLarge || r.errCode(t) != config.ErrRequestTooLarge {
		t.Fatalf("a response body over the cap = %d %.200s, want 413 %s", r.code, r.body, config.ErrRequestTooLarge)
	}
	if n := box.count(); n != 0 {
		t.Fatalf("box was sent %d responses from a body over the cap", n)
	}

	if r := do(s.Router(), credsAs(aliceFP), http.MethodPost, path, respondBodyPadded(t, "box", id, 1<<20)); r.code != http.StatusNoContent {
		t.Fatalf("a response body within the cap = %d %.200s, want 204", r.code, r.body)
	}
	if n := box.count(); n != 1 {
		t.Fatalf("box was sent %d responses, want 1", n)
	}
}
