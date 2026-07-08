package main

import (
	"time"

	"github.com/google/uuid"
)

// desktop_protocol.go — the UDS wire protocol between shed-host-agent and the
// shed-desktop app. Newline-delimited JSON, one typed envelope per line.
//
//   app → agent:  hello, approval_response, pong, token.get
//   agent → app:  hello_ack, approval_request, event, ping, token.response

const desktopProtocolVersion = 2

type agentInfo struct {
	Version        string `json:"version"`
	ApprovalMethod string `json:"approval_method"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// inbound (app → agent)

type helloMsg struct {
	Type         string     `json:"type"`
	Client       clientInfo `json:"client"`
	Capabilities []string   `json:"capabilities"`
	ReplayEvents int        `json:"replay_events"`
}

type approvalResponseMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by"`
	// Scope/TTL describe how the app decided (e.g. per-session, 4h) — recorded
	// in the durable audit log. Empty for a one-off per-request decision.
	Scope string `json:"scope,omitempty"`
	TTL   string `json:"ttl,omitempty"`
}

// outbound (agent → app)

type helloAckMsg struct {
	V                int       `json:"v"`
	Type             string    `json:"type"`
	ID               string    `json:"id"`
	Ts               string    `json:"ts"`
	Agent            agentInfo `json:"agent"`
	Namespaces       []string  `json:"namespaces"`
	GateNamespaces   []string  `json:"gate_namespaces"`
	RequestTimeoutMS int       `json:"request_timeout_ms"`
	Accepted         bool      `json:"accepted"`
	Reason           string    `json:"reason,omitempty"`
}

type approvalRequestMsg struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	Ts        string `json:"ts"`
	Namespace string `json:"namespace"`
	Op        string `json:"op"`
	Server    string `json:"server,omitempty"`
	Shed      string `json:"shed"`
	Detail    string `json:"detail"`
	ExpiresAt string `json:"expires_at"`
}

type eventMsg struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	Ts        string `json:"ts"`
	Kind      string `json:"kind"`
	Server    string `json:"server,omitempty"`
	Shed      string `json:"shed,omitempty"`
	Ns        string `json:"ns,omitempty"`
	Op        string `json:"op,omitempty"`
	Result    string `json:"result"`
	Detail    string `json:"detail,omitempty"`
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Approval  string `json:"approval,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
	Scope     string `json:"scope,omitempty"`
	TTL       string `json:"ttl,omitempty"`
}

type pingMsg struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	ID   string `json:"id"`
	Ts   string `json:"ts"`
}

// token.get is a request/response pair (app → agent → app), correlated by ID
// (echoed back as in_reply_to). The app asks for a CONTROL-scoped token for a
// named server it wants to reach; the agent mints it over SSH on the app's behalf.

// tokenGetMsg is the app's request (inbound).
type tokenGetMsg struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Server string `json:"server"`
}

// tokenResponseMsg is the agent's reply (outbound). On failure Error is set and
// Token/ExpiresAt are empty — fail closed, never a partial token.
type tokenResponseMsg struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	Ts        string `json:"ts"`
	InReplyTo string `json:"in_reply_to"`
	Server    string `json:"server"`
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// envelopeType peeks at a frame's discriminator without fully decoding it.
type envelopeType struct {
	Type string `json:"type"`
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
