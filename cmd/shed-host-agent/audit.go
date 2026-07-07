package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry is a single audit log record.
type AuditEntry struct {
	Timestamp string `json:"ts"`
	// Server is the shed server the request came from. Empty (and omitted) in
	// single-server mode so existing log consumers and historical logs are
	// unaffected.
	Server    string `json:"server,omitempty"`
	Shed      string `json:"shed"`
	Namespace string `json:"ns"`
	Operation string `json:"op"`
	Result    string `json:"result"`
	Detail    string `json:"detail,omitempty"`
	// Code is a machine-readable cause for a non-ok result — the protocol error
	// code (e.g. REGISTRY_NOT_ALLOWED, CREDENTIALS_NOT_FOUND) or an audit-only
	// code such as APPROVAL_DENIED. Empty/omitted for successful operations. It
	// lets the durable log and the shed-desktop feed show (and filter on) *why*
	// an operation failed, not merely that it did.
	Code string `json:"code,omitempty"`
	// Reason is a short host-side human explanation for a non-ok result (e.g.
	// `registry "index.docker.io" not in allowlist`). Safe for host/admin
	// surfaces (the durable log, shed-desktop): it is derived from the broker's
	// own error and never carries raw credential-helper stderr, which can hold
	// secrets and stays only in the host's debug log. Empty/omitted on success.
	Reason   string `json:"reason,omitempty"`
	Approval string `json:"approval"`
	// Decision detail for gated operations (who decided + the scope/TTL applied),
	// so the durable file records the full approval outcome even when the decision
	// was made in shed-desktop. Empty/omitted for non-gated operations.
	DecidedBy string `json:"decided_by,omitempty"`
	Scope     string `json:"scope,omitempty"`
	TTL       string `json:"ttl,omitempty"`
}

// AuditLogger writes JSON lines to the audit log file and fans every entry
// out to in-process subscribers (the shed-desktop UDS server consumes this
// to build the app's all-namespace activity feed).
type AuditLogger struct {
	mu      sync.Mutex
	file    *os.File
	encoder *json.Encoder
	logger  *slog.Logger
	subs    map[int]chan AuditEntry
	nextSub int
}

// NewAuditLogger creates an audit logger. If the config disables logging or the
// file cannot be opened, returns a no-op logger that silently discards entries.
func NewAuditLogger(cfg LogConfig, logger *slog.Logger) *AuditLogger {
	if !cfg.Enabled {
		return &AuditLogger{logger: logger}
	}

	// Ensure directory exists
	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		logger.Warn("failed to create audit log directory", "path", dir, "error", err)
		return &AuditLogger{logger: logger}
	}

	f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		logger.Warn("failed to open audit log", "path", cfg.Path, "error", err)
		return &AuditLogger{logger: logger}
	}

	logger.Info("audit logging enabled", "path", cfg.Path)
	return &AuditLogger{
		file:    f,
		encoder: json.NewEncoder(f),
		logger:  logger,
	}
}

// Log writes a basic audit entry (no approval-decision detail). Safe for
// concurrent use.
func (a *AuditLogger) Log(server, shed, namespace, operation, result, detail, approval string) {
	a.LogEntry(AuditEntry{
		Server:    server,
		Shed:      shed,
		Namespace: namespace,
		Operation: operation,
		Result:    result,
		Detail:    detail,
		Approval:  approval,
	})
}

// LogEntry writes a fully-formed audit entry — used by gated operations to
// record the approval outcome (decided_by/scope/ttl). The Timestamp is stamped
// here if unset. The entry is always published to subscribers (so the desktop
// activity feed works even when file logging is disabled); the file write is
// skipped when no file is open — the file remains the durable record.
func (a *AuditLogger) LogEntry(entry AuditEntry) {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.file != nil {
		if err := a.encoder.Encode(entry); err != nil {
			a.logger.Error("failed to write audit log", "error", err)
		}
	}
	a.publish(entry)
}

// Subscribe returns a channel of every future audit entry plus an
// unsubscribe func. Buffered; a slow subscriber drops entries rather than
// stalling a credential operation (the file is the durable record).
func (a *AuditLogger) Subscribe(buf int) (<-chan AuditEntry, func()) {
	ch := make(chan AuditEntry, buf)
	a.mu.Lock()
	if a.subs == nil {
		a.subs = make(map[int]chan AuditEntry)
	}
	id := a.nextSub
	a.nextSub++
	a.subs[id] = ch
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		if c, ok := a.subs[id]; ok {
			delete(a.subs, id)
			close(c)
		}
		a.mu.Unlock()
	}
}

// publish fans an entry out to subscribers. Caller must hold a.mu.
// Non-blocking: a full subscriber channel drops the entry.
func (a *AuditLogger) publish(entry AuditEntry) {
	for _, ch := range a.subs {
		select {
		case ch <- entry:
		default:
		}
	}
}

// Close closes the audit log file.
func (a *AuditLogger) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file != nil {
		a.file.Close()
		a.file = nil
	}
}
