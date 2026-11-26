package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/providertest"
	"github.com/charliek/shed/internal/remote/provider/service/servicetest"
)

// TestConformance runs the provider suite against the compute service's
// client and the reference compute service, in process over loopback: every
// call through the status matrix and the validation, with a lost reply and
// an expired operation for each mutation, and a fresh server process (its
// own runtime and token) waiting on a stored Pending.
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Harness{
		New: func(t *testing.T) *providertest.Target {
			w := servicetest.Start(t, servicetest.Options{Clients: []string{"nocturn"}})
			// Leases long enough that a day's clock step (an expired
			// operation) ends none.
			w.Step("policy.set", map[string]any{"initial_lease_s": 4 * 86400, "max_age_s": 5 * 86400, "person_max": 100, "org_max": 100})
			return &providertest.Target{
				Provider: w.Client(),
				Fresh:    func() provider.Provider { return w.Client() },
				Settle:   func(ref provider.Ref) { w.Settle(string(ref)) },
				LoseNextReply: func(call string) {
					w.Step("cloud.fail", []string{call, "lost_reply"})
				},
				ExpireOperations: func() { w.Step("clock.advance", 24*3600+1) },
				DeleteOutside: func(ref provider.Ref, cause string) {
					w.Step("machine.delete", []string{w.Server, string(ref), cause})
				},
				Advance:        func(d time.Duration) { w.Step("clock.advance", int64(d/time.Second)) },
				FailNextCreate: func() { w.Step("cloud.fail", []string{"create", "quota"}) },
				User:           "ada",
				Client:         "nocturn",
				Key:            servicetest.NewKey(t),
			}
		},
	})
}

// A server without the compute grant is refused at the token
// (invalid_scope): the client answers COMPUTE_DENIED and sends no call. A
// bearer issued before the grant was removed gets 403 INSUFFICIENT_SCOPE,
// which is denied too (S0 ruling 7).
func TestComputeDenied(t *testing.T) {
	w := servicetest.Start(t, servicetest.Options{NoGrant: true})
	_, err := w.Client().Catalog(context.Background())
	if !errors.Is(err, provider.ErrDenied) || provider.CodeOf(err) != "COMPUTE_DENIED" {
		t.Fatalf("a catalog read without the grant = %v, want denied", err)
	}

	w = servicetest.Start(t, servicetest.Options{})
	c := w.Client()
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatalf("with the grant: %v", err)
	}
	w.Step("compute.deny", w.Server)
	_, err = c.Catalog(context.Background())
	if !errors.Is(err, provider.ErrDenied) || provider.CodeOf(err) != "INSUFFICIENT_SCOPE" {
		t.Fatalf("a held bearer after the grant was removed = %v, want denied (INSUFFICIENT_SCOPE)", err)
	}
}

// The compute token is reused while it lasts: two calls, one token.
func TestComputeTokenReused(t *testing.T) {
	w := servicetest.Start(t, servicetest.Options{})
	rt := w.Runtime()
	a, err := rt.ComputeAccess(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := rt.ComputeAccess(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Bearer == "" || a.Bearer != b.Bearer || a.Assertion != b.Assertion || a.Server != w.Server {
		t.Fatalf("two accesses: %q/%q, server %q", a.Bearer, b.Bearer, a.Server)
	}
	rt.ForgetComputeToken(a.Bearer)
	c, err := rt.ComputeAccess(context.Background())
	if err != nil || c.Bearer == a.Bearer {
		t.Fatalf("after a forget: %v, the same bearer %v", err, c.Bearer == a.Bearer)
	}
}

// A request that never reaches the service (nothing listens) is
// ErrNotDelivered; one the service read before the connection dropped is
// unavailable with its outcome unknown, never ErrNotDelivered.
func TestNotDeliveredIsToldFromALostReply(t *testing.T) {
	w := servicetest.Start(t, servicetest.Options{})
	c := w.Client()
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	rid := provider.NewRequestID()

	w.Down(true)
	_, err := c.Start(context.Background(), provider.Ref("q6c3g4ymb5m2dkxwe7kyz5hhzu"), rid)
	if !errors.Is(err, provider.ErrUnavailable) || errors.Is(err, provider.ErrNotDelivered) {
		t.Fatalf("a request read and then dropped = %v, want unavailable with an unknown outcome", err)
	}
	w.Down(false)

	w.Stop()
	_, err = c.Start(context.Background(), provider.Ref("q6c3g4ymb5m2dkxwe7kyz5hhzu"), rid)
	if !errors.Is(err, provider.ErrUnavailable) || !errors.Is(err, provider.ErrNotDelivered) {
		t.Fatalf("a request to a stopped service = %v, want never delivered", err)
	}
	// A new process has no token: the mint fails before any call.
	_, err = w.Client().Catalog(context.Background())
	if !errors.Is(err, provider.ErrNotDelivered) {
		t.Fatalf("a call with no token to be had = %v, want never delivered", err)
	}
}

// A 403 on the read that follows accepted work (the grant was removed after
// the create) says nothing about the work: Wait answers an unknown outcome,
// never denied and never an operation that failed.
func TestWaitTreatsARefusedReadAsUnknown(t *testing.T) {
	w := servicetest.Start(t, servicetest.Options{})
	c := w.Client()
	spec, err := provider.NewSpec("q6c3g4ymb5m2dkxwe7kyz5hhzu", "box", provider.Owner{User: "ada"},
		provider.ShedSpec{CreatedAt: time.Now(), Network: "open"}, []string{servicetest.NewKey(t)}, "standard", 0, provider.ImageDefault)
	if err != nil {
		t.Fatal(err)
	}
	pend, _, err := c.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	w.Step("compute.deny", w.Server)
	_, err = c.Wait(context.Background(), pend)
	if !errors.Is(err, provider.ErrUnavailable) || errors.Is(err, provider.ErrDenied) || errors.Is(err, provider.ErrNotDelivered) {
		t.Fatalf("Wait after the grant was removed = %v, want an unknown outcome", err)
	}
}
