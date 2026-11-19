// Package audit is shed-server's audit log: one JSON object per line in
// <state>/audit.jsonl, rotated by size. It records what service clients do
// (one "api" line per request), every HTTPS exec ("exec.end"), the max_age
// reaper's deletions ("reap"), each time a person enters a service client's
// shed over SSH ("ssh"), and each tunnel a person opens into one through the
// HTTPS Connect API ("connect"). It is on when auth.clients is set or
// audit.enabled is true; otherwise no file is created.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// Defaults for the server's audit log: rotate at 50 MiB, keep 5 old files
// (audit.jsonl.1 is the newest, audit.jsonl.5 the oldest).
const (
	DefaultMaxBytes int64 = 50 << 20
	DefaultKeep           = 5
	// FileName is the log's name in the backend's state directory.
	FileName = "audit.jsonl"
)

// Event names.
const (
	EventAPI     = "api"
	EventExecEnd = "exec.end"
	EventReap    = "reap"
	EventSSH     = "ssh"
	EventConnect = "connect"
)

// The channels of an ssh line (SSHEvent.Channel): what the person opened in
// the shed.
const (
	// SSHChannelShell is an interactive login: a session with no command.
	SSHChannelShell = "shell"
	// SSHChannelExec is a session with a command (`shed exec`, `ssh host
	// cmd`, `shed attach`).
	SSHChannelExec = "exec"
	// SSHChannelSFTP is the sftp subsystem (sftp, scp).
	SSHChannelSFTP = "sftp"
	// SSHChannelTunnel is one connection forwarded over SSH to a port in
	// the shed (`ssh -L`). `shed tunnels` does not use SSH: it opens its
	// tunnels through the HTTPS Connect API, which writes a connect line
	// (ConnectEvent).
	SSHChannelTunnel = "tunnel"
)

// Identity kinds.
const (
	// KindClient is a service client (auth.clients), by name and serial.
	KindClient = "client"
	// KindUser is a person: a user of the server by name, or the implicit
	// user of a server with no user list.
	KindUser = "user"
	// KindSystem is the server acting for itself (the max_age reaper).
	KindSystem = "system"
	// KindUnknown is a caller the server could not name. It is never
	// expected (the access guard refuses such a caller before the backend);
	// it is recorded as it is rather than passed off as a person.
	KindUnknown = "unknown"
)

// Identity is who did something. Its shape depends on Kind:
//
//   - client: {kind, name, serial}, nothing else.
//   - user: {kind, user, admin, script, subject, serial}: the user's name, the
//     two flags (always present, true or false), the key's fingerprint as
//     subject, and the certificate's serial when there is one. The implicit
//     user carries implicit: true and no user.
//   - system and unknown: {kind} alone.
//
// Build one with ClientIdentity, UserIdentity, SystemIdentity or
// UnknownIdentity.
type Identity struct {
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	User     string `json:"user,omitempty"`
	Implicit bool   `json:"implicit,omitempty"`
	// Admin and Script are pointers so a person's line always carries both
	// and a client's or the system's carries neither.
	Admin   *bool  `json:"admin,omitempty"`
	Script  *bool  `json:"script,omitempty"`
	Subject string `json:"subject,omitempty"`
	Serial  string `json:"serial,omitempty"`
}

// ClientIdentity is the service client name, by its certificate serial.
func ClientIdentity(name, serial string) Identity {
	return Identity{Kind: KindClient, Name: name, Serial: serial}
}

// UserIdentity is a person: the user named user, or, when implicit is set,
// the one user of a server with no user list, which carries no name (user is
// then ignored). admin is whether the person may use what is shared on the
// host (the implicit user always may); script whether they acted with a
// script key. subject is the key's fingerprint and serial the certificate's
// serial; either may be empty (open mode presents no certificate, and an SSH
// login no serial).
func UserIdentity(user string, implicit, admin, script bool, subject, serial string) Identity {
	id := Identity{Kind: KindUser, Admin: &admin, Script: &script, Subject: subject, Serial: serial}
	if implicit {
		id.Implicit = true
	} else {
		id.User = user
	}
	return id
}

// SystemIdentity is the server acting for itself.
func SystemIdentity() Identity { return Identity{Kind: KindSystem} }

// UnknownIdentity is a caller the server could not name.
func UnknownIdentity() Identity { return Identity{Kind: KindUnknown} }

// APIEvent is one service client API request, allowed or refused. Route is
// the matched route pattern ("" when the path matched no route); Path is the
// request path as sent.
type APIEvent struct {
	Event      string   `json:"event"`
	RequestID  string   `json:"request_id,omitempty"`
	Identity   Identity `json:"identity"`
	Remote     string   `json:"remote,omitempty"`
	Method     string   `json:"method"`
	Route      string   `json:"route"`
	Path       string   `json:"path"`
	Shed       string   `json:"shed,omitempty"`
	Status     int      `json:"status"`
	DurationMS int64    `json:"duration_ms"`
}

// ExecEndEvent is one HTTPS exec that reached the backend, however it ended.
// By default only the program (Argv0) and the argument count (Argc) are
// recorded, because arguments may carry prompts or secrets; with
// audit.exec_argv: full, Argv holds the whole argv instead. ExitCode is -1
// when there was no exit status (lost result, disconnect, a failure before
// the command started).
type ExecEndEvent struct {
	Event        string   `json:"event"`
	RequestID    string   `json:"request_id,omitempty"`
	Identity     Identity `json:"identity"`
	Shed         string   `json:"shed"`
	Argv0        string   `json:"argv0,omitempty"`
	Argc         int      `json:"argc,omitempty"`
	Argv         []string `json:"argv,omitempty"`
	ExitCode     int      `json:"exit_code"`
	TimedOut     bool     `json:"timed_out"`
	Truncated    bool     `json:"truncated"`
	Disconnected bool     `json:"disconnected"`
	DurationMS   int64    `json:"duration_ms"`
}

// ReapEvent is one owned shed the max_age reaper deleted (or failed to). Its
// Identity is always SystemIdentity(): the server deleted it, not a person.
type ReapEvent struct {
	Event      string   `json:"event"`
	Identity   Identity `json:"identity"`
	Shed       string   `json:"shed"`
	Owner      string   `json:"owner"`
	AgeSeconds int64    `json:"age_seconds"`
	Error      string   `json:"error,omitempty"`
}

// SSHEvent is one time a person entered a service client's shed over SSH: a
// session (Channel shell or exec), the sftp subsystem, or one forwarded
// connection (tunnel, with the shed's Port). It is written only once the entry
// has been admitted: a session's or sftp's command was handed to the shed's
// agent, or a tunnel's connection was dialled and its channel accepted. Its
// Identity is always a person (UserIdentity, with the key's fingerprint as
// subject and no serial). Remote is the SSH peer's address. The command is
// never recorded: an SSH command is one shell string, not an argv, and can
// carry secrets.
type SSHEvent struct {
	Event    string   `json:"event"`
	Identity Identity `json:"identity"`
	Remote   string   `json:"remote,omitempty"`
	Shed     string   `json:"shed"`
	Owner    string   `json:"owner"`
	Channel  string   `json:"channel"`
	Port     int      `json:"port,omitempty"`
}

// ConnectEvent is one tunnel a person opened into a service client's shed
// through the HTTPS Connect API (GET /api/sheds/{name}/connect/{port}, what
// `shed tunnels` uses). It is written only once the tunnel is established:
// the guarded dial into the shed connected and the HTTP connection was
// switched to the tunnel. A refused or failed Connect, a person's Connect to
// their own shed and a service client's own Connect (its api line records
// that) write none. Its Identity is the person as an HTTPS caller is
// recorded (UserIdentity, with the certificate's subject and serial when one
// was presented). Remote is the HTTPS peer's address; Port the shed's port.
// It carries no request id: the server's request id accepts a caller's
// X-Request-Id header, and no text a caller supplies belongs in this line
// (the ssh line has none either).
type ConnectEvent struct {
	Event    string   `json:"event"`
	Identity Identity `json:"identity"`
	Remote   string   `json:"remote,omitempty"`
	Shed     string   `json:"shed"`
	Owner    string   `json:"owner"`
	Port     int      `json:"port"`
}

// Pending counts the entries into service clients' sheds a server has begun
// to admit and whose audit line is not written yet, so the server's shutdown
// can wait for those lines before the log is closed (Drain). Once the gate is
// closed (Close, or Drain, which closes it first), no new entry may begin: an
// entry admitted after it could write its line after the log is closed. The
// zero value is ready to use.
type Pending struct {
	mu     sync.Mutex
	closed bool
	n      int
	idle   chan struct{} // closed when n falls to 0 after Drain began
	// waiting is closed the first time a Drain waits on entries still
	// begun (Waiting); waited records that it was.
	waiting chan struct{}
	waited  bool
}

// Close closes the gate: every later Begin is refused. It does not wait
// (Drain does). Calling it again, or Drain after it, is safe. A server's
// shutdown closes every gate first, before it waits on anything, so that no
// wait elsewhere keeps a gate open.
func (p *Pending) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}

// Waiting returns a channel that is closed once a Drain is waiting on
// entries still begun. A test releases an entry only after that, so that it
// proves the drain waited for it.
func (p *Pending) Waiting() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitingLocked()
}

func (p *Pending) waitingLocked() chan struct{} {
	if p.waiting == nil {
		p.waiting = make(chan struct{})
	}
	return p.waiting
}

// Begin registers one entry about to be admitted. ok is false once the gate
// is closed (Close or Drain): the caller must then refuse the entry.
// Otherwise end must be called once the entry's line is written or the entry
// was not admitted after all; calling it more than once is safe.
func (p *Pending) Begin() (end func(), ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return func() {}, false
	}
	p.n++
	var once sync.Once
	return func() { once.Do(p.end) }, true
}

func (p *Pending) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n--
	if p.n == 0 && p.idle != nil {
		close(p.idle)
		p.idle = nil
	}
}

// Drain closes the gate (Close) and waits until every entry begun has
// ended, or ctx ends; it then returns an error that says how many had not.
// It returns nil whenever none is left, even with ctx already ended.
func (p *Pending) Drain(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	if p.n == 0 {
		p.mu.Unlock()
		return nil
	}
	if p.idle == nil {
		p.idle = make(chan struct{})
	}
	idle := p.idle
	if !p.waited {
		p.waited = true
		close(p.waitingLocked())
	}
	p.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		n := p.n
		p.mu.Unlock()
		if n == 0 {
			return nil
		}
		return fmt.Errorf("audit: %d entries into service clients' sheds were still being admitted; their lines may be lost: %w", n, ctx.Err())
	}
}

// Log is an append-only JSONL file with size rotation. Safe for concurrent
// use. A nil *Log records nothing, so callers need no "is audit on" checks.
type Log struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
	now      func() time.Time
}

// Open opens (creating, mode 0600) or appends to the log at path. When a line
// would take the file past maxBytes it is rotated first: path becomes
// path.1, path.1 becomes path.2, and so on, keeping keep old files. maxBytes
// <= 0 and keep <= 0 take the defaults.
func Open(path string, maxBytes int64, keep int) (*Log, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	l := &Log{path: path, maxBytes: maxBytes, keep: keep, now: time.Now}
	if err := l.openLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) openLocked() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f = f
	l.size = st.Size()
	return nil
}

// Path is the log's file path.
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Record appends v, which must marshal to a JSON object, as one line with a
// "ts" field (RFC 3339, UTC, nanoseconds) first.
func (l *Log) Record(v any) error {
	if l == nil {
		return nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	body = bytes.TrimSpace(body)
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return fmt.Errorf("audit: record is not a JSON object")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return fmt.Errorf("audit: log is closed")
	}
	ts := strconv.Quote(l.now().UTC().Format(time.RFC3339Nano))
	line := make([]byte, 0, len(body)+len(ts)+8)
	line = append(line, `{"ts":`...)
	line = append(line, ts...)
	if inner := body[1 : len(body)-1]; len(bytes.TrimSpace(inner)) > 0 {
		line = append(line, ',')
		line = append(line, inner...)
	}
	line = append(line, '}', '\n')

	if l.size > 0 && l.size+int64(len(line)) > l.maxBytes {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	if err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}

// rotateLocked shifts path.N to path.N+1 (dropping the oldest), moves path to
// path.1 and opens a fresh path.
func (l *Log) rotateLocked() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("audit: close for rotation: %w", err)
	}
	l.f = nil
	_ = os.Remove(l.rotated(l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		if err := os.Rename(l.rotated(i), l.rotated(i+1)); err != nil && !os.IsNotExist(err) {
			return l.reopenAfter(fmt.Errorf("audit: rotate %s: %w", l.rotated(i), err))
		}
	}
	if err := os.Rename(l.path, l.rotated(1)); err != nil && !os.IsNotExist(err) {
		return l.reopenAfter(fmt.Errorf("audit: rotate %s: %w", l.path, err))
	}
	return l.openLocked()
}

// reopenAfter reopens the current file after a failed rotation, so a later
// record can still be written, and returns cause.
func (l *Log) reopenAfter(cause error) error {
	if err := l.openLocked(); err != nil {
		return fmt.Errorf("%w (and reopen failed: %v)", cause, err)
	}
	return cause
}

func (l *Log) rotated(i int) string { return l.path + "." + strconv.Itoa(i) }

// Close closes the file. Later records fail.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
