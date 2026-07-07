package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/charliek/shed/internal/ext/protocol"
)

// statusSchemaVersion is the version of the LiveStatus JSON contract. It is
// bumped only on a breaking change; additive fields do not bump it. See
// docs/reference/host-agent-ipc.md.
const statusSchemaVersion = 1

// LiveStatus is the daemon's authoritative self-report, served over the
// read-only status socket and rendered by `shed-host-agent status`. It is the
// single source of truth for "what is the running agent actually doing": which
// config file it loaded, the effective approval policy per provider, whether an
// approval-channel consumer (e.g. the shed-desktop app) is connected, and each
// watched server's per-namespace subscription state.
type LiveStatus struct {
	Schema          int                   `json:"schema"`
	Version         string                `json:"version"`
	Pid             int                   `json:"pid"`
	StartedAt       string                `json:"started_at"` // RFC3339, daemon start
	WrittenAt       string                `json:"written_at"` // RFC3339, snapshot time
	ConfigPath      string                `json:"config_path"`
	Policies        map[string]string     `json:"policies"` // namespace -> effective policy
	GateNamespaces  []string              `json:"gate_namespaces,omitempty"`
	ApprovalChannel ApprovalChannelStatus `json:"approval_channel"`
	Servers         []ServerHealth        `json:"servers"`
}

// ApprovalChannelStatus describes the approval-channel socket and its current
// consumer (the connected client that answers shed-desktop-policy approvals).
type ApprovalChannelStatus struct {
	SocketPath        string `json:"socket_path"`
	ConsumerConnected bool   `json:"consumer_connected"`
	ClientName        string `json:"client_name,omitempty"`
	ClientVersion     string `json:"client_version,omitempty"`
}

// ServerHealth is one watched server's connection state.
type ServerHealth struct {
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Namespaces []NamespaceHealth `json:"namespaces"`
}

// NamespaceHealth is one namespace subscription's state on a server.
type NamespaceHealth struct {
	Namespace string `json:"namespace"`
	State     string `json:"state"` // connected | reconnecting | stopped
	LastError string `json:"last_error,omitempty"`
	Since     string `json:"since"` // RFC3339 — when the state began
}

// buildLiveStatus snapshots the daemon's live self-report for the status socket.
// configPath is the resolved config the daemon loaded; startedAt is its start
// time. The desktop server may be nil only in tests.
func buildLiveStatus(sup *Supervisor, desktop *DesktopServer, cfg Config, configPath, startedAt, version string) LiveStatus {
	ac := ApprovalChannelStatus{SocketPath: desktopSocketPath()}
	if desktop != nil {
		if connected, client := desktop.ConsumerInfo(); connected {
			ac.ConsumerConnected = true
			ac.ClientName = client.Name
			ac.ClientVersion = client.Version
		}
	}
	return LiveStatus{
		Schema:     statusSchemaVersion,
		Version:    version,
		Pid:        os.Getpid(),
		StartedAt:  startedAt,
		WrittenAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath: configPath,
		Policies: map[string]string{
			protocol.NamespaceSSHAgent:          cfg.SSH.Approval.EffectivePolicy(),
			protocol.NamespaceAWSCredentials:    cfg.AWS.Approval.EffectivePolicy(),
			protocol.NamespaceDockerCredentials: cfg.Docker.Approval.EffectivePolicy(),
		},
		GateNamespaces:  desktopGateNamespaces(cfg),
		ApprovalChannel: ac,
		Servers:         sup.Health(),
	}
}

// serveStatusSocket serves a read-only UDS that, on each connection, writes the
// current LiveStatus JSON and closes. It's separate from the desktop approval
// socket (so a query never promotes itself as the approval consumer) and always
// available, so `shed-host-agent status` can query the running daemon. `health`
// is called per connection to snapshot live state. Serves until ctx is cancelled.
func serveStatusSocket(ctx context.Context, path string, health func() LiveStatus, logger *slog.Logger) {
	ln := bindUnixSocket("status socket", path, logger)
	if ln == nil {
		return
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (shutdown) or fatal accept error
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = json.NewEncoder(c).Encode(health())
		}(conn)
	}
}
