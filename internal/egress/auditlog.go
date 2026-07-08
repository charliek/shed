package egress

import (
	"encoding/json"
	"os"
	"sync"
)

// defaultAuditRing is the in-memory record count retained for `shed egress show`
// when the caller doesn't specify one.
const defaultAuditRing = 1000

// AuditLog is shed-server's durable sink for egress decisions. Every record is
// appended as one JSON line to a file AND kept in a bounded in-memory ring for
// `shed egress show`. Safe for concurrent Record calls. A write error never
// breaks the data path (audit is best-effort on disk; the ring still updates).
type AuditLog struct {
	mu     sync.Mutex
	f      *os.File
	enc    *json.Encoder
	ring   []AuditRecord
	pos    int  // next write index into ring
	full   bool // ring has wrapped at least once
	max    int
	subs   map[int]func(AuditRecord) // live subscribers (e.g. the SSE stream)
	nextID int
}

// OpenAuditLog opens (creating + appending) the JSONL file and sizes the ring.
func OpenAuditLog(path string, ringSize int) (*AuditLog, error) {
	if ringSize <= 0 {
		ringSize = defaultAuditRing
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &AuditLog{f: f, enc: json.NewEncoder(f), ring: make([]AuditRecord, ringSize), max: ringSize}, nil
}

// Record appends one decision to the JSONL file and the in-memory ring, then
// fans it out to live subscribers. This is the onAudit callback handed to
// StartManager.
func (a *AuditLog) Record(rec AuditRecord) {
	a.mu.Lock()
	if a.enc != nil {
		_ = a.enc.Encode(rec) // best-effort; never fail the data path
	}
	a.ring[a.pos] = rec
	a.pos++
	if a.pos == a.max {
		a.pos = 0
		a.full = true
	}
	// Snapshot subscribers and notify after releasing the lock, so a slow
	// subscriber can't stall the file write or another Record.
	var fns []func(AuditRecord)
	for _, fn := range a.subs {
		fns = append(fns, fn)
	}
	a.mu.Unlock()
	for _, fn := range fns {
		fn(rec)
	}
}

// Subscribe registers fn to receive every subsequent record; the returned
// function unsubscribes. fn must not block — the SSE handler pushes to a
// buffered channel and drops on overflow.
func (a *AuditLog) Subscribe(fn func(AuditRecord)) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subs == nil {
		a.subs = map[int]func(AuditRecord){}
	}
	id := a.nextID
	a.nextID++
	a.subs[id] = fn
	return func() {
		a.mu.Lock()
		delete(a.subs, id)
		a.mu.Unlock()
	}
}

// Recent returns up to limit of the most recent records (oldest first, newest
// last), filtered by shed when shed != "". limit <= 0 returns all retained.
func (a *AuditLog) Recent(shed string, limit int) []AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, start := a.pos, 0
	if a.full {
		n, start = a.max, a.pos
	}
	out := make([]AuditRecord, 0, n)
	for i := 0; i < n; i++ {
		if rec := a.ring[(start+i)%a.max]; shed == "" || rec.Shed == shed {
			out = append(out, rec)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Close closes the underlying file. The ring remains readable after close.
func (a *AuditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		err := a.f.Close()
		a.f, a.enc = nil, nil
		return err
	}
	return nil
}
