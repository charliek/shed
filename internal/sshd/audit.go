package sshd

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// SetAuditLog attaches the audit log (<state>/audit.jsonl) the API server
// writes too. With it, each time a person enters a service client's shed over
// SSH (a shell, a command, sftp, a forwarded connection) writes one ssh line
// (auditEntry). nil, the default, records nothing. Call it before Start.
func (s *Server) SetAuditLog(l *audit.Log) { s.audit = l }

// pinEntry returns ctx bound to the shed the channel's first check returned
// (ensureRunning's, before any wake, or the forward's own): its incarnation
// (creation time) and the owner and user it was checked against. Every
// guarded call after it (the wake's start, polls and waits; then Guard.Exec
// or Guard.DialService, which enters the shed) loads the shed again and
// refuses one that is no longer that shed, as one that is not there; so the
// entry it admits is into exactly the shed, with exactly the owner, that was
// first checked and that its audit line names.
func pinEntry(ctx context.Context, shed *config.Shed) context.Context {
	return backend.PinOwnership(backend.PinCreatedAt(ctx, shed.CreatedAt), shed.Owner, shed.User)
}

// errShuttingDown refuses an entry into a service client's shed that would
// begin after the server's shutdown has begun (beginEntry).
var errShuttingDown = errors.New("the server is shutting down; try again once it is back")

// audited reports whether an entry into shed writes an ssh line: an audit
// log is attached and the shed belongs to a service client.
func (s *Server) audited(shed *config.Shed) bool {
	return s.audit != nil && shed != nil && shed.Owner != ""
}

// beginEntry readies a channel's entry into shed, just before the guarded
// exec or dial that enters it. For an entry that writes an ssh line
// (audited), it registers the entry with the server's pending entries, so
// Shutdown waits for its line before the audit log is closed, and ok is
// false once the entry gate is closed (CloseEntries, which shed-server calls
// first thing in its shutdown, or Shutdown): the channel must then be
// refused, since an entry admitted then could lose its line. onStarted is the exec's
// ExecOptions.OnStarted hook (onEntered, then end); end must be called once
// the channel is done with the entry (a tunnel calls it after its line); it
// is safe to call more than once. An entry that writes no line is never
// refused and registers nothing.
func (s *Server) beginEntry(p backend.Principal, fingerprint string, remote net.Addr, shed *config.Shed, channel string) (onStarted, end func(), ok bool) {
	if !s.audited(shed) {
		return nil, func() {}, true
	}
	end, ok = s.entries.Begin()
	if !ok {
		return nil, end, false
	}
	entered := s.onEntered(p, fingerprint, remote, shed, channel)
	return func() { entered(); end() }, end, true
}

// auditEntry writes the ssh line for an entry the guarded call admitted: the
// person p, by the fingerprint of the key the connection authenticated with,
// from remote, entered shed through channel (port, for a tunnel). It writes
// nothing when no audit log is attached, or when the shed belongs to no
// service client: a person's own shed is not recorded. A write error is
// logged and never fails the channel. The command is never recorded.
//
// The line is written inline, as the API's audit lines are: the channel
// waits for the write (and for any other line being written, which holds the
// log's lock) before its session or tunnel goes on. A stalled audit file
// stalls entries into a service client's sheds, which is the fail-closed
// choice for an audited entry.
func (s *Server) auditEntry(p backend.Principal, fingerprint string, remote net.Addr, shed *config.Shed, channel string, port int) {
	if !s.audited(shed) {
		return
	}
	ev := audit.SSHEvent{
		Event:    audit.EventSSH,
		Identity: sshIdentity(p, fingerprint),
		Shed:     shed.Name,
		Owner:    shed.Owner,
		Channel:  channel,
		Port:     port,
	}
	if remote != nil {
		ev.Remote = remote.String()
	}
	if err := s.audit.Record(ev); err != nil {
		log.Printf("audit: %v", err)
	}
}

// onEntered is auditEntry as an ExecOptions.OnStarted hook: the backend calls
// it once the guarded exec has passed its own check and the command was
// handed to the shed's agent, never for an exec refused or failed before
// that. It records at most once.
func (s *Server) onEntered(p backend.Principal, fingerprint string, remote net.Addr, shed *config.Shed, channel string) func() {
	var once sync.Once
	return func() {
		once.Do(func() { s.auditEntry(p, fingerprint, remote, shed, channel, 0) })
	}
}

// sshIdentity is the audit identity of the person a channel was admitted
// for: the principal the guard decided on (its user is the one the connection
// is bound to, principalFor), with the key's fingerprint as subject and no
// serial (an SSH login has no certificate). A principal that is not a person
// (or a user that is neither named nor implicit) never reaches a shed over
// SSH; it would be recorded as unknown, never as a person: the rule the API's
// auditIdentity applies too (audit.PersonIdentity).
func sshIdentity(p backend.Principal, fingerprint string) audit.Identity {
	return audit.PersonIdentity(p.Kind == backend.PrincipalUser, p.User, p.Implicit, p.IsAdmin(), p.Script, fingerprint, "")
}
