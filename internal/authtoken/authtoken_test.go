package authtoken

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustMint(t *testing.T, s *Store, subject, scope string, ttl time.Duration) (string, PublicRecord) {
	t.Helper()
	pt, rec, err := s.Mint(subject, scope, ClientCLI, ttl)
	if err != nil {
		t.Fatalf("Mint(%s, %s): %v", subject, scope, err)
	}
	return pt, rec
}

func TestMintValidate(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		scope   string
		ttl     time.Duration
		wantErr bool
	}{
		{"control ok", "SHA256:abc", ScopeControl, time.Hour, false},
		{"credentials ok", "SHA256:abc", ScopeCredentials, time.Hour, false},
		{"admin rejected", "SHA256:abc", "admin", time.Hour, true},
		{"empty scope rejected", "SHA256:abc", "", time.Hour, true},
		{"garbage scope rejected", "SHA256:abc", "root", time.Hour, true},
		{"empty subject rejected", "", ScopeControl, time.Hour, true},
		{"zero ttl rejected", "SHA256:abc", ScopeControl, 0, true},
		{"negative ttl rejected", "SHA256:abc", ScopeControl, -time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore()
			pt, rec, err := s.Mint(tt.subject, tt.scope, ClientCLI, tt.ttl)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got token %q", pt)
				}
				return
			}
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			if !strings.HasPrefix(pt, "shed_"+tt.scope+"_") {
				t.Errorf("plaintext %q missing scope prefix", pt)
			}
			if rec.ID == "" || rec.Subject != tt.subject || rec.Scope != tt.scope {
				t.Errorf("unexpected record %+v", rec)
			}
			got, ok := s.Validate(pt)
			if !ok {
				t.Fatal("Validate: token not found")
			}
			if got.ID != rec.ID || got.Scope != tt.scope {
				t.Errorf("Validate returned %+v, want id=%s scope=%s", got, rec.ID, tt.scope)
			}
		})
	}
}

func TestValidateRejectsUnknownAndEmpty(t *testing.T) {
	s := NewStore()
	if _, ok := s.Validate(""); ok {
		t.Error("empty token validated")
	}
	if _, ok := s.Validate("shed_control_bogus"); ok {
		t.Error("unknown token validated")
	}
}

func TestValidateExpiry(t *testing.T) {
	s := NewStore()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clk.now
	pt, _ := mustMint(t, s, "SHA256:k", ScopeControl, time.Minute)

	clk.advance(time.Minute - time.Nanosecond)
	if _, ok := s.Validate(pt); !ok {
		t.Fatal("token rejected before expiry")
	}
	clk.advance(time.Nanosecond)
	if _, ok := s.Validate(pt); ok {
		t.Fatal("token accepted at expiry")
	}
	if s.Len() != 0 {
		t.Errorf("expired record not opportunistically deleted, Len=%d", s.Len())
	}
}

func TestRevokeBySubject(t *testing.T) {
	s := NewStore()
	a1, _ := mustMint(t, s, "subjA", ScopeControl, time.Hour)
	a2, _ := mustMint(t, s, "subjA", ScopeCredentials, time.Hour)
	b1, _ := mustMint(t, s, "subjB", ScopeControl, time.Hour)

	if n := s.RevokeBySubject("subjA"); n != 2 {
		t.Fatalf("RevokeBySubject=%d, want 2", n)
	}
	if _, ok := s.Validate(a1); ok {
		t.Error("a1 still valid after revoke")
	}
	if _, ok := s.Validate(a2); ok {
		t.Error("a2 still valid after revoke")
	}
	if _, ok := s.Validate(b1); !ok {
		t.Error("b1 wrongly revoked")
	}
	if n := s.RevokeBySubject(""); n != 0 {
		t.Errorf("RevokeBySubject(empty)=%d, want 0", n)
	}
}

func TestRevokeByID(t *testing.T) {
	s := NewStore()
	pt, rec := mustMint(t, s, "subj", ScopeControl, time.Hour)
	if !s.RevokeByID(rec.ID) {
		t.Fatal("RevokeByID returned false for live token")
	}
	if _, ok := s.Validate(pt); ok {
		t.Error("token valid after RevokeByID")
	}
	if s.RevokeByID(rec.ID) {
		t.Error("RevokeByID returned true for already-removed id")
	}
	if s.RevokeByID("nonexistent") {
		t.Error("RevokeByID returned true for unknown id")
	}
}

func TestListExcludesExpiredAndIsSorted(t *testing.T) {
	s := NewStore()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clk.now
	_, r1 := mustMint(t, s, "s1", ScopeControl, time.Hour)
	clk.advance(time.Second)
	mustMint(t, s, "s2", ScopeCredentials, time.Minute)
	clk.advance(time.Second)
	mustMint(t, s, "s3", ScopeControl, time.Second)

	clk.advance(2 * time.Minute) // s3 + s2 expired, s1 alive
	list := s.List()
	if len(list) != 1 || list[0].ID != r1.ID {
		t.Fatalf("List=%+v, want only r1=%s", list, r1.ID)
	}
	for _, pr := range list {
		if strings.HasPrefix(pr.Subject, "shed_") {
			t.Error("subject looks like a token plaintext")
		}
	}
}

func TestSweep(t *testing.T) {
	s := NewStore()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clk.now
	mustMint(t, s, "s1", ScopeControl, time.Minute)
	mustMint(t, s, "s2", ScopeControl, time.Hour)
	clk.advance(2 * time.Minute)
	if n := s.Sweep(); n != 1 {
		t.Fatalf("Sweep=%d, want 1", n)
	}
	if s.Len() != 1 {
		t.Errorf("Len=%d after sweep, want 1", s.Len())
	}
}

func TestHashAtRest(t *testing.T) {
	// The store must never hold the plaintext: the map is keyed by SHA-256 and
	// records carry only the hash.
	s := NewStore()
	pt, _ := mustMint(t, s, "subj", ScopeControl, time.Hour)
	h := sha256.Sum256([]byte(pt))

	s.mu.RLock()
	rec, ok := s.byHash[h]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("record not keyed by sha256(plaintext)")
	}
	if rec.hash != h {
		t.Error("record hash != sha256(plaintext)")
	}
	if rec.subject == pt || rec.id == pt {
		t.Error("plaintext leaked into record fields")
	}
}

func TestConcurrentMintSameSubject(t *testing.T) {
	s := NewStore()
	const n = 200
	var wg sync.WaitGroup
	toks := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pt, _, err := s.Mint("same-subject", ScopeControl, ClientCLI, time.Hour)
			if err != nil {
				t.Errorf("Mint: %v", err)
				return
			}
			toks[i] = pt
		}(i)
	}
	wg.Wait()

	if s.Len() != n {
		t.Fatalf("Len=%d, want %d", s.Len(), n)
	}
	seen := make(map[string]bool, n)
	for _, pt := range toks {
		if pt == "" || seen[pt] {
			t.Fatalf("duplicate or empty token %q", pt)
		}
		seen[pt] = true
		if _, ok := s.Validate(pt); !ok {
			t.Errorf("token %q not valid", pt)
		}
	}
}

func TestStartSweeper(t *testing.T) {
	s := NewStore()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clk.now
	mustMint(t, s, "s", ScopeControl, time.Minute)
	clk.advance(2 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartSweeper(ctx, 5*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for s.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not evict expired token")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeClock is a concurrency-safe, manually-advanced clock for deterministic
// TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
