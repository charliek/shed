package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// bindingBackend is the async fake with a creation time and an owner on what
// CreateShed returns, recording the context WaitSettled is given.
type bindingBackend struct {
	*asyncFakeBackend
	created time.Time
	waitCtx context.Context
}

func (b *bindingBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	shed, err := b.asyncFakeBackend.CreateShed(ctx, req)
	if shed != nil {
		shed.CreatedAt = b.created
	}
	return shed, err
}

func (b *bindingBackend) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	b.waitCtx = ctx
	return b.asyncFakeBackend.WaitSettled(ctx, name)
}

// The SSE create waits by name, for minutes. The wait must be bound to the
// shed the request made (its owner and creation time), so a shed deleted and
// created again under the name by another client is not followed and its
// progress and final state are not sent down this caller's stream.
func TestCreateAndWaitFollowsOnlyTheShedItCreated(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	inner := newAsyncFakeBackend()
	inner.final = config.StatusRunning
	close(inner.release)
	b := &bindingBackend{asyncFakeBackend: inner, created: created}
	s := &Server{backend: b}

	req := config.CreateShedRequest{Name: "a1", Owner: "alice"}
	if _, err := s.createAndWait(context.Background(), context.Background(), b, req); err != nil {
		t.Fatalf("createAndWait: %v", err)
	}
	if b.waitCtx == nil {
		t.Fatal("WaitSettled was not called")
	}
	tests := []struct {
		name    string
		check   error
		refused bool
	}{
		{"the shed it created", errors.Join(backend.CheckOwner(b.waitCtx, "a1", "alice"), backend.CheckCreatedAt(b.waitCtx, "a1", created)), false},
		{"another owner's shed of the name", backend.CheckOwner(b.waitCtx, "a1", "bob"), true},
		{"a later shed of the name", backend.CheckCreatedAt(b.waitCtx, "a1", created.Add(time.Minute)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.check, config.ErrShedNotFoundSentinel); got != tt.refused {
				t.Fatalf("check = %v, refused=%v, want refused=%v", tt.check, got, tt.refused)
			}
		})
	}
}
