// Package authtoken issues and validates short-lived, scoped HTTP bearer tokens
// for shed-server.
//
// A token is an opaque capability of the form shed_<scope>_<random>. The
// plaintext is returned to a client exactly once, at mint time, over the
// SSH-authenticated bootstrap channel; it is never persisted. The store keys
// records by SHA-256(plaintext), so a dump of the store yields hashes, not
// usable tokens.
//
// Each record binds the token to the SSH key fingerprint that authorized the
// mint (its subject), a scope, and an expiry. Revocation is authoritative and
// immediate: RevokeBySubject drops every token minted for a key the instant
// that key leaves the allowlist. The store is in-memory by design — a server
// restart drops all tokens and clients transparently re-mint over SSH.
package authtoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Token scopes. The scope is also encoded in the plaintext (shed_<scope>_...)
// for fast rejection and readable logs, but authorization is always by the
// stored record, never by the token string alone. The legacy "admin" scope is
// intentionally absent: minting and revocation are gated by SSH access, so there
// is no separate HTTP admin capability.
const (
	ScopeControl     = "control"     // shed lifecycle / control plane (CLI, desktop)
	ScopeCredentials = "credentials" // credential bus + Connect tunnel + egress stream
)

// Client kinds are advisory metadata, recorded for audit (token ls) only.
const (
	ClientCLI       = "cli"
	ClientHostAgent = "host-agent"
	ClientDesktop   = "desktop"
)

// record is the server-side record for one minted token. The plaintext is never
// stored; hash is SHA-256(plaintext) and is the store key.
type record struct {
	id         string
	hash       [32]byte
	subject    string
	scope      string
	clientKind string
	issuedAt   time.Time
	expiresAt  time.Time
}

// PublicRecord is the non-secret view of a token, returned by List. It carries
// no plaintext and no hash, only the stable id and metadata.
type PublicRecord struct {
	ID         string
	Subject    string
	Scope      string
	ClientKind string
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

func (r *record) public() PublicRecord {
	return PublicRecord{
		ID:         r.id,
		Subject:    r.subject,
		Scope:      r.scope,
		ClientKind: r.clientKind,
		IssuedAt:   r.issuedAt,
		ExpiresAt:  r.expiresAt,
	}
}

// Store is an in-memory set of live token records, safe for concurrent use.
type Store struct {
	mu     sync.RWMutex
	byHash map[[32]byte]*record
	now    func() time.Time
}

// NewStore returns an empty token store.
func NewStore() *Store {
	return &Store{byHash: make(map[[32]byte]*record), now: time.Now}
}

// ValidScope reports whether s is a mintable scope.
func ValidScope(s string) bool {
	return s == ScopeControl || s == ScopeCredentials
}

// Mint issues a token for the given subject (the SSH key fingerprint that
// authorized it), scope, and TTL. It returns the plaintext token (shown to the
// client exactly once) and the non-secret record. clientKind is advisory audit
// metadata and may be empty.
func (s *Store) Mint(subject, scope, clientKind string, ttl time.Duration) (string, PublicRecord, error) {
	if subject == "" {
		return "", PublicRecord{}, errors.New("authtoken: empty subject")
	}
	if !ValidScope(scope) {
		return "", PublicRecord{}, fmt.Errorf("authtoken: invalid scope %q", scope)
	}
	if ttl <= 0 {
		return "", PublicRecord{}, fmt.Errorf("authtoken: non-positive ttl %s", ttl)
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// Generate a token whose hash is not already present. A collision is
	// astronomically unlikely; retry a few times rather than overwrite.
	for attempt := 0; attempt < 8; attempt++ {
		plaintext, err := generatePlaintext(scope)
		if err != nil {
			return "", PublicRecord{}, err
		}
		hash := sha256.Sum256([]byte(plaintext))
		if _, exists := s.byHash[hash]; exists {
			continue
		}
		id, err := generateID()
		if err != nil {
			return "", PublicRecord{}, err
		}
		rec := &record{
			id:         id,
			hash:       hash,
			subject:    subject,
			scope:      scope,
			clientKind: clientKind,
			issuedAt:   now,
			expiresAt:  now.Add(ttl),
		}
		s.byHash[hash] = rec
		return plaintext, rec.public(), nil
	}
	return "", PublicRecord{}, errors.New("authtoken: repeated token hash collision")
}

// Validate looks up a plaintext token and returns its record if it exists and
// has not expired. An expired record is deleted opportunistically.
func (s *Store) Validate(plaintext string) (PublicRecord, bool) {
	if plaintext == "" {
		return PublicRecord{}, false
	}
	hash := sha256.Sum256([]byte(plaintext))
	now := s.now()

	s.mu.RLock()
	rec, ok := s.byHash[hash]
	s.mu.RUnlock()
	if !ok {
		return PublicRecord{}, false
	}
	if !now.Before(rec.expiresAt) {
		s.mu.Lock()
		if cur, ok := s.byHash[hash]; ok && cur == rec {
			delete(s.byHash, hash)
		}
		s.mu.Unlock()
		return PublicRecord{}, false
	}
	return rec.public(), true
}

// deleteWhere removes every record matching pred and returns the count removed.
// It holds the write lock for the scan.
func (s *Store) deleteWhere(pred func(*record) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for hash, rec := range s.byHash {
		if pred(rec) {
			delete(s.byHash, hash)
			n++
		}
	}
	return n
}

// RevokeBySubject deletes every token minted for the given subject and returns
// the number removed. Called when a key leaves the allowlist.
func (s *Store) RevokeBySubject(subject string) int {
	if subject == "" {
		return 0
	}
	return s.deleteWhere(func(r *record) bool { return r.subject == subject })
}

// RevokeByID deletes the token with the given non-secret id and reports whether
// one was found.
func (s *Store) RevokeByID(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, rec := range s.byHash {
		if rec.id == id {
			delete(s.byHash, hash)
			return true
		}
	}
	return false
}

// List returns the non-secret records of all live (unexpired) tokens, oldest
// first.
func (s *Store) List() []PublicRecord {
	now := s.now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PublicRecord, 0, len(s.byHash))
	for _, rec := range s.byHash {
		if now.Before(rec.expiresAt) {
			out = append(out, rec.public())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IssuedAt.Equal(out[j].IssuedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].IssuedAt.Before(out[j].IssuedAt)
	})
	return out
}

// Sweep deletes expired records and returns the number removed.
func (s *Store) Sweep() int {
	now := s.now()
	return s.deleteWhere(func(r *record) bool { return !now.Before(r.expiresAt) })
}

// Len returns the number of records currently held, including expired records
// not yet swept. Intended for tests and metrics.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byHash)
}

// StartSweeper runs Sweep on the given interval until ctx is cancelled.
func (s *Store) StartSweeper(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Sweep()
			}
		}
	}()
}

func generatePlaintext(scope string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "shed_" + scope + "_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
