// Package audit is shed-server's audit log: one JSON object per line in
// <state>/audit.jsonl, rotated by size. It records what service clients do
// (one "api" line per request), every HTTPS exec ("exec.end"), and the max_age
// reaper's deletions ("reap"). It is on when auth.clients is set or
// audit.enabled is true; otherwise no file is created.
package audit

import (
	"bytes"
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
)

// Identity kinds.
const (
	KindClient = "client"
	KindAdmin  = "admin"
)

// Identity is who made a request. A service client is {kind: client, name,
// serial}; an admin is {kind: admin, subject} (the token's subject or the user
// certificate's fingerprint; no subject in open mode).
type Identity struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Subject string `json:"subject,omitempty"`
	Serial  string `json:"serial,omitempty"`
}

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

// ReapEvent is one owned shed the max_age reaper deleted (or failed to).
type ReapEvent struct {
	Event      string `json:"event"`
	Shed       string `json:"shed"`
	Owner      string `json:"owner"`
	AgeSeconds int64  `json:"age_seconds"`
	Error      string `json:"error,omitempty"`
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
