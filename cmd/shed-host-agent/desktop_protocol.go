package main

import (
	"time"

	"github.com/google/uuid"
)

// desktop_protocol.go — the UDS wire protocol between shed-host-agent and the
// shed-desktop app. Newline-delimited JSON, one typed envelope per line.
//
//   app → agent:  hello, approval_response, pong, token.get, credential.get
//   agent → app:  hello_ack, approval_request, event, ping, token.response,
//                 credential.response

const desktopProtocolVersion = 2

// Agent capabilities advertised in the hello_ack.
//
// shed-desktop and shed-host-agent are SEPARATELY RELEASED components, so every
// combination of versions runs in the field. The version counter alone cannot
// express that: it is stamped on every frame and never checked on receive, and
// bumping it would break the old pairing it is meant to describe. A capability
// list can — the agent says what it can answer, and an app that needs something
// absent from that list fails with a sentence naming what to upgrade instead of
// waiting out a request timeout for a message the agent silently dropped.
//
// The skew is handled in BOTH directions:
//
//   - new app, old agent: the ack carries no agent_capabilities at all, so
//     capCredentialGet is absent and the app reports "upgrade shed-host-agent"
//     rather than sending a credential.get into a switch with no case for it.
//   - old app, new agent: the app never learns the capability exists and keeps
//     sending token.get. Against a token-mode server that keeps working exactly
//     as before; against an mtls server the agent answers with an explicit
//     "upgrade the app" error, because a certificate cannot be delivered through
//     a token.response and pretending otherwise would fail somewhere less
//     legible.
const (
	// capCredentialGet: the agent answers credential.get — a mode-agnostic
	// control credential, including relaying a CSR to an mtls-mode server and
	// returning the issued certificate.
	capCredentialGet = "credential.get"
)

// agentCapabilities is what this build advertises. Ordered, not sorted at send
// time: it is a wire value pinned by golden fixtures in both implementations.
func agentCapabilities() []string { return []string{capCredentialGet} }

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
	V              int       `json:"v"`
	Type           string    `json:"type"`
	ID             string    `json:"id"`
	Ts             string    `json:"ts"`
	Agent          agentInfo `json:"agent"`
	Namespaces     []string  `json:"namespaces"`
	GateNamespaces []string  `json:"gate_namespaces"`
	// AgentCapabilities names the optional messages this agent answers (see
	// agentCapabilities). An older agent omits the field entirely, which is the
	// signal a new app keys "upgrade shed-host-agent" on.
	AgentCapabilities []string `json:"agent_capabilities,omitempty"`
	RequestTimeoutMS  int      `json:"request_timeout_ms"`
	Accepted          bool     `json:"accepted"`
	Reason            string   `json:"reason,omitempty"`
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

// credential.get is the mode-agnostic successor to token.get, and a SEPARATE
// message rather than an extension of it.
//
// Silently widening token.get would have been cheaper to write and worse in
// every direction that matters. An old agent would ignore the new `csr` field
// and answer with a token — leaving a new app to guess whether the server is in
// token mode or the agent is simply old. A new agent answering an old app's
// token.get with certificate fields would produce a `token.response` carrying no
// token, which decodes fine and fails later. A distinct message plus a
// capability advertisement makes both mismatches nameable at the handshake, and
// leaves token.get frozen with exactly the semantics every shipped app expects.

// credentialGetMsg is the app's request (inbound).
//
// CSR is a standard-base64 PKCS#10 CertificationRequest DER, generated BY THE
// APP. Only the request crosses this socket; the private key that will match the
// issued certificate stays in the app process, which is the whole reason the
// exchange is a relay rather than the agent minting on the app's behalf. It is
// optional — an app that has no reason to expect certificates may omit it — and
// an mtls-mode server then answers with its own explicit upgrade error.
type credentialGetMsg struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Server string `json:"server"`
	CSR    string `json:"csr,omitempty"`
}

// credentialResponseMsg is the agent's reply (outbound).
//
// AuthMode names which of Token / ClientCert is populated, so the app never has
// to infer the server's mode from which field happens to be non-empty. On
// failure Error is set and every credential field is empty — fail closed, never
// a partial credential.
type credentialResponseMsg struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	ID         string `json:"id"`
	Ts         string `json:"ts"`
	InReplyTo  string `json:"in_reply_to"`
	Server     string `json:"server"`
	AuthMode   string `json:"auth_mode,omitempty"`
	Token      string `json:"token,omitempty"`
	ClientCert string `json:"client_cert,omitempty"`
	CertSerial string `json:"cert_serial,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

// envelopeType peeks at a frame's discriminator without fully decoding it.
type envelopeType struct {
	Type string `json:"type"`
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
