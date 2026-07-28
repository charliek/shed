package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charliek/shed/internal/ext/protocol"
)

const maxFrameBytes = 1 << 20 // 1 MiB per-line cap on inbound frames

// socketProbeTimeout bounds the "is this socket live?" dial. A live local agent
// accepts in well under this; a stale leftover file fails fast (ECONNREFUSED).
const socketProbeTimeout = 500 * time.Millisecond

// socketIsLive reports whether a Unix socket at path currently has a process
// accepting connections (vs. a stale leftover file from an unclean exit).
func socketIsLive(path string) bool {
	conn, err := net.DialTimeout("unix", path, socketProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// prepareSocketPath makes path bindable for a fresh listener. It errors when the
// path is a non-socket file (a misconfigured path must never delete an unrelated
// file) or when another process is still accepting on it — clobbering a live
// socket would silently break the running agent: its listener keeps the
// now-deleted inode and never recreates the file, so a connected shed-desktop
// can't reconnect. A truly stale socket (nothing accepting) is removed so a
// fresh agent can bind.
func prepareSocketPath(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("path exists but is not a socket: %s", path)
	}
	if socketIsLive(path) {
		return fmt.Errorf("socket already in use by another process: %s", path)
	}
	return os.Remove(path)
}

// bindUnixSocket binds a fresh listener for one of the agent's sockets, applying
// the shared safety ceremony: an owner-only parent dir, a refuse-to-clobber-live
// prepare (prepareSocketPath), and a 0600 socket. Dir/perm failures are
// best-effort (logged, non-fatal); a refused or failed bind is fatal and returns
// nil. name prefixes the log lines (e.g. "desktop", "status socket").
func bindUnixSocket(name, path string, logger *slog.Logger) net.Listener {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		logger.Warn(name+": could not create socket dir", "dir", dir, "error", err)
	}
	// Owner-only parent dir is the real protection (it covers the brief window
	// before the socket Chmod); enforce it even if the dir already existed.
	if err := os.Chmod(dir, 0700); err != nil {
		logger.Warn(name+": could not restrict socket dir perms", "dir", dir, "error", err)
	}
	if err := prepareSocketPath(path); err != nil {
		logger.Error(name+": refusing to bind", "path", path, "error", err)
		return nil
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		logger.Error(name+": failed to listen", "path", path, "error", err)
		return nil
	}
	if err := os.Chmod(path, 0600); err != nil {
		logger.Warn(name+": could not set socket perms to 0600", "path", path, "error", err)
	}
	logger.Info(name+": socket listening", "path", path)
	return ln
}

var (
	errNoConsumer = errors.New("no shed-desktop connected")
	errTimeout    = errors.New("approval timed out")
)

// consumerConn is the single active shed-desktop connection.
type consumerConn struct {
	conn    net.Conn
	client  clientInfo // self-reported in the hello (name/version), for status
	writeMu sync.Mutex
}

// consumerWriteTimeout bounds every frame write so a connected-but-not-reading
// app can't block a send forever (which would hang an approval past its budget
// or strand the ping/forward goroutines). A write past this deadline errors;
// the caller then denies / the connection is dropped — fail closed.
const consumerWriteTimeout = 5 * time.Second

func (c *consumerConn) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(consumerWriteTimeout))
	_, err = c.conn.Write(data)
	return err
}

// desktopDecision is the app's reply to an approval request, including how it
// decided (for the durable audit log).
type desktopDecision struct {
	approved  bool
	decidedBy string
	scope     string
	ttl       string
}

// DesktopServer exposes the approval channel UDS: the all-namespace
// audit/event stream plus the approval request/response channel. Single
// active consumer (normally the shed-desktop app), last-writer-wins;
// fail-closed (deny) when none connected.
type DesktopServer struct {
	socketPath     string
	audit          *AuditLogger
	logger         *slog.Logger
	timeout        time.Duration
	agentVersion   string
	gateNamespaces []string              // namespaces whose policy is shed-desktop
	controlTokens  *controlTokenProvider // answers token.get; nil makes it fail closed

	mu       sync.Mutex
	consumer *consumerConn
	pending  map[string]pendingReq
	ring     []eventMsg
	ringMax  int
}

// pendingReq binds an in-flight approval to the consumer that owns it, so a
// superseded/old connection can't resolve a request it merely observed.
type pendingReq struct {
	ch    chan desktopDecision
	owner *consumerConn
}

// NewDesktopServer builds a server bound to socketPath. approvalTimeout bounds
// each delegated approval before fail-closed deny (a non-positive value falls
// back to 25s). gateNamespaces are the credential namespaces whose approval
// policy is shed-desktop (advertised to the app so it shows the matching UI).
func NewDesktopServer(socketPath string, approvalTimeout time.Duration, audit *AuditLogger, agentVersion string, gateNamespaces []string, logger *slog.Logger) *DesktopServer {
	if approvalTimeout <= 0 {
		approvalTimeout = 25 * time.Second
	}
	return &DesktopServer{
		socketPath: socketPath, audit: audit, logger: logger, timeout: approvalTimeout, agentVersion: agentVersion,
		gateNamespaces: gateNamespaces,
		pending:        make(map[string]pendingReq),
		ringMax:        100,
	}
}

// SetControlTokens wires the provider that answers token.get requests. Call it
// before Listen; a nil provider makes token.get fail closed.
func (s *DesktopServer) SetControlTokens(p *controlTokenProvider) {
	s.controlTokens = p
}

// Listen binds the socket and serves until ctx is cancelled.
func (s *DesktopServer) Listen(ctx context.Context) {
	ln := bindUnixSocket("desktop", s.socketPath, s.logger)
	if ln == nil {
		return
	}

	auditCh, unsub := s.audit.Subscribe(256)
	defer unsub()
	go s.forwardAudit(ctx, auditCh)

	go func() {
		<-ctx.Done()
		// Closing the UnixListener unlinks our own socket file. Don't probe and
		// remove by path here — that's bind-time's job, and on shutdown it could
		// race a replacement agent that already rebound the path.
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (shutdown) or fatal accept error
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *DesktopServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Scanner with a hard max token size so a client can't force unbounded
	// memory growth with a giant line that never ends in '\n'.
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)

	// Require a hello within a short grace period.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if !sc.Scan() {
		return
	}
	line := sc.Bytes()
	var probe envelopeType
	if json.Unmarshal(line, &probe) != nil || probe.Type != "hello" {
		return
	}
	var hello helloMsg
	_ = json.Unmarshal(line, &hello)
	_ = conn.SetReadDeadline(time.Time{})

	c := &consumerConn{conn: conn, client: hello.Client}
	// Send hello_ack BEFORE promoting, so RequestApproval can't route an
	// approval_request to this connection before the handshake completes.
	if err := c.send(helloAckMsg{
		V: desktopProtocolVersion, Type: "hello_ack", ID: newID(), Ts: nowRFC3339(),
		Agent:             agentInfo{Version: s.agentVersion, ApprovalMethod: "shed-desktop"},
		Namespaces:        []string{protocol.NamespaceSSHAgent, protocol.NamespaceAWSCredentials, protocol.NamespaceDockerCredentials, namespaceEgress},
		GateNamespaces:    s.gateNamespaces,
		AgentCapabilities: agentCapabilities(),
		RequestTimeoutMS:  int(s.timeout / time.Millisecond),
		Accepted:          true,
	}); err != nil {
		return
	}
	s.promote(c)
	defer s.demote(c)
	s.replay(c, hello.ReplayEvents)

	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = c.send(pingMsg{V: desktopProtocolVersion, Type: "ping", ID: newID(), Ts: nowRFC3339()})
			}
		}
	}()

	for sc.Scan() {
		line := sc.Bytes()
		var et envelopeType
		if json.Unmarshal(line, &et) != nil {
			continue
		}
		switch et.Type {
		case "approval_response":
			var resp approvalResponseMsg
			if json.Unmarshal(line, &resp) == nil {
				s.resolve(resp.RequestID, desktopDecision{
					approved:  resp.Decision == "approve",
					decidedBy: resp.DecidedBy,
					scope:     resp.Scope,
					ttl:       resp.TTL,
				}, c)
			}
		case "token.get":
			var req tokenGetMsg
			if json.Unmarshal(line, &req) == nil {
				// Mint in its own goroutine: a bootstrap is a bounded SSH round-trip
				// and must not stall this connection's read loop (and thus approvals).
				go s.handleTokenGet(c, req)
			}
		case "credential.get":
			var req credentialGetMsg
			if json.Unmarshal(line, &req) == nil {
				go s.handleCredentialGet(c, req)
			}
		case "pong":
			// liveness only
		}
	}
}

// handleTokenGet answers a token.get: mint a control-scoped token for the
// requested server and reply with token.response. On any failure Error is set
// and Token/ExpiresAt stay empty — fail closed, never a partial token.
func (s *DesktopServer) handleTokenGet(c *consumerConn, req tokenGetMsg) {
	resp := tokenResponseMsg{
		V: desktopProtocolVersion, Type: "token.response", ID: newID(), Ts: nowRFC3339(),
		InReplyTo: req.ID, Server: req.Server,
	}
	if s.controlTokens == nil {
		resp.Error = "control-token minting is not available"
	} else if tok, exp, err := s.controlTokens.Token(req.Server); err != nil {
		resp.Error = err.Error()
	} else {
		resp.Token = tok
		if !exp.IsZero() {
			resp.ExpiresAt = exp.UTC().Format(time.RFC3339)
		}
	}
	_ = c.send(resp)
}

// handleCredentialGet answers a credential.get: obtain a control-scoped
// credential for the requested server — relaying the app's CSR so an mtls-mode
// server can issue a certificate against a key that never leaves the app — and
// reply with credential.response.
//
// On any failure Error is set and every credential field stays empty: fail
// closed, never a partial credential, and never a response that looks like a
// success with nothing in it.
func (s *DesktopServer) handleCredentialGet(c *consumerConn, req credentialGetMsg) {
	resp := credentialResponseMsg{
		V: desktopProtocolVersion, Type: "credential.response", ID: newID(), Ts: nowRFC3339(),
		InReplyTo: req.ID, Server: req.Server,
	}
	if s.controlTokens == nil {
		resp.Error = "control-credential minting is not available"
	} else if cred, err := s.controlTokens.Credential(req.Server, req.CSR); err != nil {
		resp.Error = err.Error()
	} else {
		resp.AuthMode = cred.AuthMode
		resp.Token = cred.Token
		resp.ClientCert = cred.ClientCert
		resp.CertSerial = cred.CertSerial
		if !cred.ExpiresAt.IsZero() {
			resp.ExpiresAt = cred.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	_ = c.send(resp)
}

// RequestApproval sends an approval request to the connected app and blocks on
// the reply within the timeout. Fail-closed: returns an error (→ deny) when no
// app is connected, on timeout, or on a transport error. On approval it returns
// how the app decided (decided_by/scope/ttl) for the durable audit log.
func (s *DesktopServer) RequestApproval(ctx context.Context, namespace, op, server, shed, detail string) (desktopDecision, error) {
	s.mu.Lock()
	consumer := s.consumer
	if consumer == nil {
		s.mu.Unlock()
		return desktopDecision{}, errNoConsumer
	}
	id := newID()
	ch := make(chan desktopDecision, 1)
	s.pending[id] = pendingReq{ch: ch, owner: consumer}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()

	req := approvalRequestMsg{
		V: desktopProtocolVersion, Type: "approval_request", ID: id, Ts: nowRFC3339(),
		Namespace: namespace, Op: op, Server: server, Shed: shed, Detail: detail,
		ExpiresAt: time.Now().Add(s.timeout).UTC().Format(time.RFC3339),
	}
	if err := consumer.send(req); err != nil {
		return desktopDecision{}, err
	}
	select {
	case dec := <-ch:
		return dec, nil
	case <-time.After(s.timeout):
		return desktopDecision{}, errTimeout
	case <-ctx.Done():
		return desktopDecision{}, ctx.Err()
	}
}

func (s *DesktopServer) promote(c *consumerConn) {
	s.mu.Lock()
	old := s.consumer
	s.consumer = c
	s.mu.Unlock()
	if old != nil && old != c {
		_ = old.send(helloAckMsg{
			V: desktopProtocolVersion, Type: "hello_ack", ID: newID(), Ts: nowRFC3339(),
			Accepted: false, Reason: "superseded",
		})
		_ = old.conn.Close()
	}
}

func (s *DesktopServer) demote(c *consumerConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumer == c {
		s.consumer = nil
	}
	// Fail any in-flight approvals owned by this (disconnecting) consumer,
	// whether it's still the active one or was already superseded — fail-closed.
	for id, p := range s.pending {
		if p.owner == c {
			select {
			case p.ch <- desktopDecision{approved: false}:
			default:
			}
			delete(s.pending, id)
		}
	}
}

// hasConsumer reports whether an app is currently the active consumer. Used
// by tests to wait for promotion before driving an approval (promotion now
// happens after the hello_ack write).
func (s *DesktopServer) hasConsumer() bool {
	connected, _ := s.ConsumerInfo()
	return connected
}

// ConsumerInfo reports whether an app is currently connected and, if so, its
// self-reported client identity (from the hello). Used by `status` to show who
// owns the approval channel.
func (s *DesktopServer) ConsumerInfo() (bool, clientInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumer == nil {
		return false, clientInfo{}
	}
	return true, s.consumer.client
}

func (s *DesktopServer) resolve(requestID string, dec desktopDecision, from *consumerConn) {
	s.mu.Lock()
	p, ok := s.pending[requestID]
	// Only the consumer the request was sent to may resolve it; a superseded
	// connection that merely observed the id cannot.
	if ok && p.owner == from {
		delete(s.pending, requestID)
	} else {
		ok = false
	}
	s.mu.Unlock()
	if ok {
		select {
		case p.ch <- dec:
		default:
		}
	}
}

func (s *DesktopServer) forwardAudit(ctx context.Context, ch <-chan AuditEntry) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			ev := eventMsg{
				V: desktopProtocolVersion, Type: "event", ID: newID(), Ts: entry.Timestamp,
				Kind: "audit", Server: entry.Server, Shed: entry.Shed, Ns: entry.Namespace, Op: entry.Operation,
				Result: entry.Result, Detail: entry.Detail, Code: entry.Code, Reason: entry.Reason, Approval: entry.Approval,
				DecidedBy: entry.DecidedBy, Scope: entry.Scope, TTL: entry.TTL,
			}
			s.mu.Lock()
			s.ring = append(s.ring, ev)
			if len(s.ring) > s.ringMax {
				s.ring = s.ring[len(s.ring)-s.ringMax:]
			}
			consumer := s.consumer
			s.mu.Unlock()
			if consumer != nil {
				_ = consumer.send(ev)
			}
		}
	}
}

func (s *DesktopServer) replay(c *consumerConn, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	start := 0
	if len(s.ring) > n {
		start = len(s.ring) - n
	}
	evs := append([]eventMsg(nil), s.ring[start:]...)
	s.mu.Unlock()
	for _, ev := range evs {
		_ = c.send(ev)
	}
}
