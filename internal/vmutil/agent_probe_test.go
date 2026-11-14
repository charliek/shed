package vmutil

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/guest"
)

// readHealthRequest reads the host's health request and decodes its payload.
func readHealthRequest(t *testing.T, conn net.Conn) (*plugin.Envelope, plugin.HealthRequestPayload) {
	t.Helper()
	msgType, data, err := agentproto.ReadMessage(conn)
	if err != nil || msgType != agentproto.MsgTypePluginMessage {
		t.Errorf("read health request: type 0x%02x err %v", msgType, err)
		return nil, plugin.HealthRequestPayload{}
	}
	var env plugin.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Errorf("decode envelope: %v", err)
		return nil, plugin.HealthRequestPayload{}
	}
	var req plugin.HealthRequestPayload
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			t.Errorf("decode payload: %v", err)
		}
	}
	return &env, req
}

func writeEnvelope(conn net.Conn, env *plugin.Envelope) {
	data, _ := json.Marshal(env)
	_ = agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, data)
}

func TestProbe(t *testing.T) {
	newInfo := guest.AgentInfo{Version: "v0.9.2", Protocol: 1, Layout: 1}
	newPayload, _ := json.Marshal(newInfo)
	heartbeat := func() *plugin.Envelope {
		hb, _ := json.Marshal(plugin.HeartbeatPayload{StartedAt: time.Now()})
		return plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, hb)
	}

	tests := []struct {
		name string
		// reply writes the agent's side after the request was read.
		reply   func(conn net.Conn, req *plugin.Envelope)
		want    guest.AgentInfo
		wantErr string
	}{
		{
			name: "new agent reports version protocol and layout",
			reply: func(conn net.Conn, req *plugin.Envelope) {
				writeEnvelope(conn, plugin.NewResponse(req.ID, plugin.NamespaceHealth, newPayload))
			},
			want: newInfo,
		},
		{
			// New server, old agent: the field is ignored and the reply
			// has no payload.
			name: "old agent without payload is protocol 0",
			reply: func(conn net.Conn, req *plugin.Envelope) {
				writeEnvelope(conn, plugin.NewResponse(req.ID, plugin.NamespaceHealth, nil))
			},
			want: guest.AgentInfo{},
		},
		{
			// An old agent promotes the probe connection and its first
			// heartbeat can win the race with the reply.
			name: "old agent heartbeat before reply is skipped",
			reply: func(conn net.Conn, req *plugin.Envelope) {
				writeEnvelope(conn, heartbeat())
				writeEnvelope(conn, plugin.NewResponse(req.ID, plugin.NamespaceHealth, nil))
			},
			want: guest.AgentInfo{},
		},
		{
			name: "unknown reply fields are ignored",
			reply: func(conn net.Conn, req *plugin.Envelope) {
				p := []byte(`{"version":"v1","protocol":1,"layout":2,"future":true}`)
				writeEnvelope(conn, plugin.NewResponse(req.ID, plugin.NamespaceHealth, p))
			},
			want: guest.AgentInfo{Version: "v1", Protocol: 1, Layout: 2},
		},
		{
			name: "malformed payload",
			reply: func(conn net.Conn, req *plugin.Envelope) {
				writeEnvelope(conn, plugin.NewResponse(req.ID, plugin.NamespaceHealth, []byte(`"nope"`)))
			},
			wantErr: "invalid health response payload",
		},
		{
			name: "only heartbeats",
			reply: func(conn net.Conn, req *plugin.Envelope) {
				for range maxProbeFrames {
					writeEnvelope(conn, heartbeat())
				}
			},
			wantErr: "no health response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sawProbe := make(chan bool, 1)
			dialer := &pipeDialer{handler: func(conn net.Conn) {
				defer conn.Close()
				env, req := readHealthRequest(t, conn)
				sawProbe <- req.Probe && len(req.Extensions) == 0
				if env != nil {
					tt.reply(conn, env)
				}
			}}
			client := NewAgentClient(dialer, 1024, 1026)

			got, err := client.Probe(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Probe() error = %v, want %q", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("Probe() error = %v", err)
				}
				if got != tt.want {
					t.Errorf("Probe() = %+v, want %+v", got, tt.want)
				}
			}
			if !<-sawProbe {
				t.Error("request did not carry probe:true alone")
			}
		})
	}
}

// CheckHealth keeps passing against an agent that predates the probe field.
func TestCheckHealthOldAgentPasses(t *testing.T) {
	client := NewAgentClient(&pipeDialer{handler: healthHandler}, 1024, 1026)
	if err := client.CheckHealth(context.Background()); err != nil {
		t.Fatalf("CheckHealth() error = %v", err)
	}
	info, err := client.WaitForProbe(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForProbe() error = %v", err)
	}
	if info.Protocol != 0 {
		t.Errorf("protocol = %d, want 0", info.Protocol)
	}
}

// A hung agent (accepts, never replies) must not hold Probe past ctx.
func TestProbeCancelUnblocksHungAgent(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	dialer := &pipeDialer{handler: func(conn net.Conn) {
		defer conn.Close()
		_, _, _ = agentproto.ReadMessage(conn)
		<-release
	}}
	client := NewAgentClient(dialer, 1024, 1026)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Probe(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Probe() took %v after the deadline", elapsed)
	}
}
