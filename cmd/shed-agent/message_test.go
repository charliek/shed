//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/guest"
)

func healthRequest(t *testing.T, payload *plugin.HealthRequestPayload) *plugin.Envelope {
	t.Helper()
	var raw json.RawMessage
	if payload != nil {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			t.Fatal(err)
		}
	}
	return plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeRequest, raw)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseProbe(t *testing.T) {
	probe := healthRequest(t, &plugin.HealthRequestPayload{Probe: true})
	tests := []struct {
		name    string
		msgType byte
		data    []byte
		want    bool
	}{
		{name: "probe", msgType: MsgTypePluginMessage, data: mustMarshal(t, probe), want: true},
		{name: "probe with extensions is still a probe", msgType: MsgTypePluginMessage,
			data: mustMarshal(t, healthRequest(t, &plugin.HealthRequestPayload{Probe: true, Extensions: []string{"ssh-agent"}})), want: true},
		{name: "old server health check (no payload)", msgType: MsgTypePluginMessage, data: mustMarshal(t, healthRequest(t, nil))},
		{name: "handshake with extensions", msgType: MsgTypePluginMessage,
			data: mustMarshal(t, healthRequest(t, &plugin.HealthRequestPayload{Extensions: []string{"ssh-agent"}}))},
		{name: "probe false", msgType: MsgTypePluginMessage, data: mustMarshal(t, healthRequest(t, &plugin.HealthRequestPayload{}))},
		{name: "event not request", msgType: MsgTypePluginMessage,
			data: mustMarshal(t, plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, []byte(`{"probe":true}`)))},
		{name: "other namespace", msgType: MsgTypePluginMessage,
			data: mustMarshal(t, plugin.NewEnvelope("system:other", plugin.MessageTypeRequest, []byte(`{"probe":true}`)))},
		{name: "not a plugin frame", msgType: MsgTypeData, data: mustMarshal(t, probe)},
		{name: "bad envelope", msgType: MsgTypePluginMessage, data: []byte("not json")},
		{name: "bad payload", msgType: MsgTypePluginMessage,
			data: mustMarshal(t, plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeRequest, []byte(`"x"`)))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := parseProbe(tt.msgType, tt.data); got != tt.want {
				t.Errorf("parseProbe() = %v, want %v", got, tt.want)
			}
		})
	}
}

// newMessageTestServer builds a Server for driving handleNotifyConnection
// directly, with a short heartbeat.
func newMessageTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(0, 0, 0, 0, "test")
	s.heartbeatInterval = 20 * time.Millisecond
	s.info = guest.AgentInfo{Version: "v-test", Protocol: guest.AgentProtocol, Layout: 1}
	t.Cleanup(s.cancel)
	return s
}

// serveNotify runs handleNotifyConnection on the agent end of a unix socket
// pair and returns the host end. done closes when the handler returns.
func serveNotify(t *testing.T, s *Server) (host net.Conn, done <-chan struct{}) {
	t.Helper()
	hostConn, agentConn := unixPair(t)
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		s.handleNotifyConnection(agentConn)
	}()
	return hostConn, ch
}

// unixPair returns two connected unix stream conns (they support CloseWrite
// and deadlines, like the agent's real conns).
func unixPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	a, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	b := <-accepted
	if b == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// shortSocketPath keeps socket paths under the 108-byte sun_path limit.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir + "/s.sock"
}

func sendEnvelope(t *testing.T, conn net.Conn, env *plugin.Envelope) {
	t.Helper()
	if err := agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, mustMarshal(t, env)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readEnvelope(t *testing.T, conn net.Conn, timeout time.Duration) (*plugin.Envelope, error) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	msgType, data, err := agentproto.ReadMessage(conn)
	if err != nil {
		return nil, err
	}
	if msgType != agentproto.MsgTypePluginMessage {
		t.Fatalf("frame type 0x%02x, want plugin message", msgType)
	}
	var env plugin.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &env, nil
}

func (s *Server) currentMsgConn() net.Conn {
	s.msgMu.Lock()
	defer s.msgMu.Unlock()
	return s.msgConn
}

// startChannel opens a message channel the way every server does (a health
// request with no probe) and returns a stream of what the agent sends on it.
// This is also the old-server path: a server older than the probe field
// sends its health checks exactly like this.
func startChannel(t *testing.T, s *Server, payload *plugin.HealthRequestPayload) (net.Conn, <-chan *plugin.Envelope, *plugin.Envelope) {
	t.Helper()
	host, _ := serveNotify(t, s)
	req := healthRequest(t, payload)
	sendEnvelope(t, host, req)
	msgs := make(chan *plugin.Envelope, 64)
	go func() {
		defer close(msgs)
		for {
			msgType, data, err := agentproto.ReadMessage(host)
			if err != nil || msgType != agentproto.MsgTypePluginMessage {
				return
			}
			var env plugin.Envelope
			if json.Unmarshal(data, &env) == nil {
				msgs <- &env
			}
		}
	}()
	return host, msgs, req
}

func waitFor(t *testing.T, msgs <-chan *plugin.Envelope, what string, match func(*plugin.Envelope) bool) *plugin.Envelope {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env, ok := <-msgs:
			if !ok {
				t.Fatalf("channel closed waiting for %s", what)
			}
			if match(env) {
				return env
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func isHeartbeat(env *plugin.Envelope) bool {
	return env.Namespace == plugin.NamespaceHealth && env.Type == plugin.MessageTypeEvent
}

// Old server, new agent: a health request without probe gets today's reply
// (no payload) and the connection becomes the message channel.
func TestNotifyHealthWithoutProbeIsPromoted(t *testing.T) {
	s := newMessageTestServer(t)
	host, msgs, req := startChannel(t, s, nil)

	resp := waitFor(t, msgs, "health response", func(e *plugin.Envelope) bool { return e.Type == plugin.MessageTypeResponse })
	if resp.InReplyTo != req.ID {
		t.Errorf("in_reply_to = %q, want %q", resp.InReplyTo, req.ID)
	}
	// Today's reply: no payload (it encodes as null), which a new server
	// reads as protocol 0.
	if info, err := guest.ParseAgentInfo(resp.Payload); err != nil || info != (guest.AgentInfo{}) {
		t.Errorf("payload = %s, want none for a non-probe request", resp.Payload)
	}
	waitFor(t, msgs, "heartbeat", isHeartbeat)
	if s.currentMsgConn() == nil {
		t.Fatal("connection was not promoted")
	}
	host.Close()
}

// A probe is answered with version, protocol and layout, then closed. It is
// never promoted and never sees a heartbeat.
func TestNotifyProbeAnsweredNotPromoted(t *testing.T) {
	s := newMessageTestServer(t)
	host, done := serveNotify(t, s)

	req := healthRequest(t, &plugin.HealthRequestPayload{Probe: true})
	sendEnvelope(t, host, req)
	resp, err := readEnvelope(t, host, 2*time.Second)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if resp.Type != plugin.MessageTypeResponse || resp.InReplyTo != req.ID {
		t.Fatalf("reply = %+v", resp)
	}
	info, err := guest.ParseAgentInfo(resp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if info != s.info {
		t.Errorf("reply info = %+v, want %+v", info, s.info)
	}
	if s.currentMsgConn() != nil {
		t.Error("probe connection was promoted to the message channel")
	}

	// Several heartbeat intervals pass with nothing more sent.
	if env, err := readEnvelope(t, host, 5*s.heartbeatInterval); err == nil {
		t.Fatalf("probe connection received %+v after the reply", env)
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("read after reply: %v, want a timeout", err)
	}

	host.Close()
	select {
	case <-done:
	case <-time.After(probeLinger + time.Second):
		t.Fatal("handler did not return after the host closed")
	}
}

// The agent closes a probe by itself even when the host never hangs up.
func TestNotifyProbeClosesWithoutHost(t *testing.T) {
	s := newMessageTestServer(t)
	host, done := serveNotify(t, s)
	sendEnvelope(t, host, healthRequest(t, &plugin.HealthRequestPayload{Probe: true}))
	if _, err := readEnvelope(t, host, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(probeLinger + time.Second):
		t.Fatal("agent held the probe connection past probeLinger")
	}
	if _, err := readEnvelope(t, host, time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("read after agent close: %v, want EOF", err)
	}
}

// The promotion bug: a probe arriving while the message channel is up must
// leave the channel in place and its heartbeats flowing.
func TestNotifyProbeDuringLiveChannelKeepsHeartbeats(t *testing.T) {
	s := newMessageTestServer(t)
	_, msgs, _ := startChannel(t, s, nil)
	waitFor(t, msgs, "first heartbeat", isHeartbeat)
	live := s.currentMsgConn()
	if live == nil {
		t.Fatal("channel not promoted")
	}

	for range 3 {
		host, done := serveNotify(t, s)
		sendEnvelope(t, host, healthRequest(t, &plugin.HealthRequestPayload{Probe: true}))
		if _, err := readEnvelope(t, host, 2*time.Second); err != nil {
			t.Fatalf("probe reply: %v", err)
		}
		host.Close()
		<-done
	}

	if got := s.currentMsgConn(); got != live {
		t.Fatal("a probe replaced the live message channel")
	}
	// Drain anything queued, then require a fresh heartbeat.
	for len(msgs) > 0 {
		<-msgs
	}
	waitFor(t, msgs, "heartbeat after probes", isHeartbeat)
}
