// Package sourcetest is the behaviour table every source.Bindings must pass.
// It runs against source.FileBindings (internal/source) and fake.Bindings
// (internal/source/fake), so the test double cannot drift from the store.
package sourcetest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/source"
)

// Instance is one store under test with the controls the table needs.
type Instance struct {
	Bindings source.Bindings
	// FailWrites makes every save fail with err from now on; nil restores.
	FailWrites func(err error)
	// Writes is how many saves have succeeded.
	Writes func() int
	// BeforeWrite sets a function called at the start of every save, inside
	// the store's write lock.
	BeforeWrite func(fn func())
	// Reopen returns the store as a restart would find it. The old instance
	// is not used again.
	Reopen func(t *testing.T) Instance
}

// Harness makes empty stores.
type Harness struct {
	New func(t *testing.T, opts source.Options) Instance
}

// Clock is a settable clock for Options.Now.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock at a fixed instant.
func NewClock() *Clock {
	return &Clock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
}

// Now reads the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock on.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ID is the n-th test machine id.
func ID(n int) string { return fmt.Sprintf("%032x", n) }

// M is the n-th test machine.
func M(n int) source.Machine {
	return source.Machine{ID: ID(n), Name: fmt.Sprintf("machine-%d", n), KeyFingerprint: fmt.Sprintf("SHA256:key%d", n)}
}

var errInjected = errors.New("injected write failure")

// Run runs the behaviour table.
func Run(t *testing.T, h Harness) {
	newInst := func(t *testing.T) (Instance, *Clock) {
		clk := NewClock()
		return h.New(t, source.Options{Now: clk.Now}), clk
	}
	admit := func(t *testing.T, b source.Bindings, user string, m source.Machine) source.Decision {
		t.Helper()
		d, err := b.Admit(user, m)
		if err != nil {
			t.Fatalf("Admit(%q, %s): %v", user, m.ID, err)
		}
		return d
	}

	t.Run("first admit binds and persists", func(t *testing.T) {
		in, clk := newInst(t)
		d := admit(t, in.Bindings, "alice", M(1))
		if !d.Admitted || !d.NewlyBound || !d.NewMachine {
			t.Fatalf("decision = %+v; want admitted, newly bound, new machine", d)
		}
		if d.Binding.Generation == 0 {
			t.Fatalf("generation = 0; want > 0")
		}
		want := []source.Ref{{Kind: source.KindMachine, ID: ID(1)}}
		if !refsEqual(d.Binding.Delivery, want) || !refsEqual(d.Binding.Signing, want) {
			t.Fatalf("slots = %+v / %+v; want both %+v", d.Binding.Delivery, d.Binding.Signing, want)
		}
		if d.Binding.BoundBy != source.BoundByFirstConnect || !d.Binding.BoundAt.Equal(clk.Now()) || d.Binding.User != "alice" {
			t.Fatalf("binding = %+v", d.Binding)
		}
		if d.Bound.ID != ID(1) || d.Bound.Name != "machine-1" || d.Bound.KeyFingerprint != "SHA256:key1" {
			t.Fatalf("bound = %+v", d.Bound)
		}
		if in.Writes() != 1 {
			t.Fatalf("writes = %d; want 1", in.Writes())
		}
		re := in.Reopen(t)
		b, ok := re.Bindings.Get("alice")
		if !ok || b.MachineID() != ID(1) || b.Generation != d.Binding.Generation || !b.BoundAt.Equal(d.Binding.BoundAt) || b.BoundBy != source.BoundByFirstConnect {
			t.Fatalf("after reopen Get = %+v, %v; want %+v", b, ok, d.Binding)
		}
		ms := re.Bindings.Machines("alice")
		if len(ms) != 1 || ms[0].ID != ID(1) || ms[0].Name != "machine-1" || ms[0].KeyFingerprint != "SHA256:key1" || !ms[0].FirstSeen.Equal(clk.Now()) {
			t.Fatalf("after reopen Machines = %+v", ms)
		}
		if !ms[0].LastSeen.IsZero() {
			t.Fatalf("last seen survived a reopen: %v", ms[0].LastSeen)
		}
	})

	t.Run("another machine is refused, named, and remembered", func(t *testing.T) {
		in, _ := newInst(t)
		first := admit(t, in.Bindings, "alice", M(1))
		d := admit(t, in.Bindings, "alice", M(2))
		if d.Admitted || d.NewlyBound || !d.NewMachine {
			t.Fatalf("decision = %+v; want refused new machine", d)
		}
		if d.Bound.ID != ID(1) || d.Bound.Name != "machine-1" {
			t.Fatalf("refusal names %+v; want machine-1", d.Bound)
		}
		if d.Binding.Generation != first.Binding.Generation {
			t.Fatalf("a refusal changed the generation: %d -> %d", first.Binding.Generation, d.Binding.Generation)
		}
		if got := ids(in.Bindings.Machines("alice")); got != ID(1)+","+ID(2) {
			t.Fatalf("machines = %s", got)
		}
		re := in.Reopen(t)
		if got := ids(re.Bindings.Machines("alice")); got != ID(1)+","+ID(2) {
			t.Fatalf("after reopen machines = %s", got)
		}
		if d := admit(t, re.Bindings, "alice", M(2)); d.Admitted {
			t.Fatalf("after reopen machine 2 admitted")
		}
		if d := admit(t, re.Bindings, "alice", M(1)); !d.Admitted {
			t.Fatalf("after reopen machine 1 refused")
		}
	})

	t.Run("repeat admits are one machine and write nothing", func(t *testing.T) {
		in, clk := newInst(t)
		admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "alice", M(2))
		w := in.Writes()
		for i := 0; i < 3; i++ {
			clk.Advance(time.Second)
			if d := admit(t, in.Bindings, "alice", M(1)); !d.Admitted || d.NewMachine || d.NewlyBound || d.NameChanged || d.KeyFingerprintChanged {
				t.Fatalf("repeat admit = %+v", d)
			}
			if d := admit(t, in.Bindings, "alice", M(2)); d.Admitted || d.NewMachine {
				t.Fatalf("repeat refused admit = %+v", d)
			}
		}
		if in.Writes() != w {
			t.Fatalf("repeat admits wrote %d times; want 0", in.Writes()-w)
		}
		ms := in.Bindings.Machines("alice")
		if len(ms) != 2 {
			t.Fatalf("machines = %d; want 2", len(ms))
		}
		for _, m := range ms {
			if !m.LastSeen.Equal(clk.Now()) {
				t.Fatalf("machine %s last seen %v; want %v (kept in memory)", m.ID, m.LastSeen, clk.Now())
			}
		}
	})

	t.Run("a new name or fingerprint for a known id is recorded and reported", func(t *testing.T) {
		in, _ := newInst(t)
		first := admit(t, in.Bindings, "alice", M(1))
		w := in.Writes()
		m := M(1)
		m.Name = "renamed ’laptop"
		d := admit(t, in.Bindings, "alice", m)
		if !d.Admitted || !d.NameChanged || d.PreviousName != "machine-1" || d.KeyFingerprintChanged {
			t.Fatalf("decision = %+v", d)
		}
		if d.Bound.Name != m.Name || d.Machine.Name != m.Name {
			t.Fatalf("bound = %+v machine = %+v; want the new name", d.Bound, d.Machine)
		}
		m.KeyFingerprint = "SHA256:rotated"
		d = admit(t, in.Bindings, "alice", m)
		if !d.Admitted || d.NameChanged || !d.KeyFingerprintChanged || d.PreviousKeyFingerprint != "SHA256:key1" {
			t.Fatalf("decision = %+v", d)
		}
		m.KeyFingerprint = "" // open mode: no fingerprint
		d = admit(t, in.Bindings, "alice", m)
		if !d.KeyFingerprintChanged || d.PreviousKeyFingerprint != "SHA256:rotated" || d.Machine.KeyFingerprint != "" {
			t.Fatalf("decision = %+v", d)
		}
		if in.Writes() != w+3 {
			t.Fatalf("writes = %d; want %d", in.Writes(), w+3)
		}
		if d.Binding.Generation != first.Binding.Generation {
			t.Fatalf("a rename changed the generation")
		}
		re := in.Reopen(t)
		ms := re.Bindings.Machines("alice")
		if len(ms) != 1 || ms[0].Name != m.Name || ms[0].KeyFingerprint != "" {
			t.Fatalf("after reopen machines = %+v", ms)
		}
	})

	t.Run("bound reads the binding and its machine together", func(t *testing.T) {
		in, _ := newInst(t)
		if _, _, ok := in.Bindings.Bound("alice"); ok {
			t.Fatal("Bound with no binding reports one")
		}
		first := admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "alice", M(2))
		bd, m, ok := in.Bindings.Bound("alice")
		if !ok || bd.Generation != first.Binding.Generation || m.ID != ID(1) || m.Name != "machine-1" || m.KeyFingerprint != "SHA256:key1" {
			t.Fatalf("Bound = %+v, %+v, %v", bd, m, ok)
		}
		if _, err := in.Bindings.Use("alice", ID(2), "me"); err != nil {
			t.Fatal(err)
		}
		if bd, m, ok := in.Bindings.Bound("alice"); !ok || bd.MachineID() != ID(2) || m.ID != ID(2) || m.Name != "machine-2" {
			t.Fatalf("Bound after a move = %+v, %+v, %v", bd, m, ok)
		}
		if _, err := in.Bindings.Drop("alice"); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := in.Bindings.Bound("alice"); ok {
			t.Fatal("Bound after Drop reports a binding")
		}
	})

	t.Run("use moves both slots", func(t *testing.T) {
		in, clk := newInst(t)
		first := admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "alice", M(2))
		clk.Advance(time.Minute)
		mv, err := in.Bindings.Use("alice", ID(2), "SHA256:mover")
		if err != nil {
			t.Fatal(err)
		}
		want := []source.Ref{{Kind: source.KindMachine, ID: ID(2)}}
		if !mv.Moved || mv.From.ID != ID(1) || mv.To.ID != ID(2) ||
			!refsEqual(mv.Binding.Delivery, want) || !refsEqual(mv.Binding.Signing, want) ||
			mv.Binding.BoundBy != "SHA256:mover" || !mv.Binding.BoundAt.Equal(clk.Now()) {
			t.Fatalf("move = %+v", mv)
		}
		if mv.Binding.Generation <= first.Binding.Generation {
			t.Fatalf("generation %d after move; want > %d", mv.Binding.Generation, first.Binding.Generation)
		}
		if d := admit(t, in.Bindings, "alice", M(1)); d.Admitted || d.Bound.ID != ID(2) {
			t.Fatalf("old machine after move: %+v", d)
		}
		if d := admit(t, in.Bindings, "alice", M(2)); !d.Admitted {
			t.Fatalf("new machine refused after move")
		}
		re := in.Reopen(t)
		b, _ := re.Bindings.Get("alice")
		if b.MachineID() != ID(2) || b.Generation != mv.Binding.Generation || b.BoundBy != "SHA256:mover" {
			t.Fatalf("after reopen = %+v", b)
		}
	})

	t.Run("use refusals", func(t *testing.T) {
		in, _ := newInst(t)
		admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "bob", M(2))
		w := in.Writes()
		cases := []struct {
			name, user, id, by string
			want               error
		}{
			{"unknown id", "alice", ID(9), "me", source.ErrUnknownMachine},
			{"another user's machine", "alice", ID(2), "me", source.ErrUnknownMachine},
			{"user with no records", "carol", ID(1), "me", source.ErrUnknownMachine},
			{"empty id", "alice", "", "me", source.ErrInvalidMachine},
			{"uppercase id", "alice", strings.ToUpper("abcdef" + ID(1)[6:]), "me", source.ErrInvalidMachine},
			{"empty mover", "alice", ID(1), "", source.ErrInvalidMover},
			{"control character in mover", "alice", ID(1), "a\nb", source.ErrInvalidMover},
			{"invalid user", "Alice", ID(1), "me", source.ErrInvalidUser},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if _, err := in.Bindings.Use(c.user, c.id, c.by); !errors.Is(err, c.want) {
					t.Fatalf("Use = %v; want %v", err, c.want)
				}
			})
		}
		if in.Writes() != w {
			t.Fatalf("refused moves wrote")
		}
		if b, _ := in.Bindings.Get("alice"); b.MachineID() != ID(1) {
			t.Fatalf("binding changed: %+v", b)
		}
	})

	t.Run("use of the bound machine changes nothing", func(t *testing.T) {
		in, _ := newInst(t)
		first := admit(t, in.Bindings, "alice", M(1))
		w := in.Writes()
		mv, err := in.Bindings.Use("alice", ID(1), "someone")
		if err != nil {
			t.Fatal(err)
		}
		if mv.Moved || mv.Binding.Generation != first.Binding.Generation || mv.Binding.BoundBy != source.BoundByFirstConnect || mv.To.ID != ID(1) {
			t.Fatalf("move = %+v; want a no-op", mv)
		}
		if in.Writes() != w {
			t.Fatalf("a no-op move wrote")
		}
		if !in.Bindings.Current("alice", ID(1), first.Binding.Generation) {
			t.Fatalf("no-op move made the binding stale")
		}
	})

	t.Run("admit refuses invalid input", func(t *testing.T) {
		in, _ := newInst(t)
		long := strings.Repeat("é", 32) // 64 bytes
		ok63 := strings.Repeat("a", 61) + "é"
		cases := []struct {
			name string
			user string
			m    source.Machine
			want error
		}{
			{"empty id", "alice", source.Machine{Name: "x"}, source.ErrInvalidMachine},
			{"short id", "alice", source.Machine{ID: "abc", Name: "x"}, source.ErrInvalidMachine},
			{"hyphenated id", "alice", source.Machine{ID: "00000000-0000-0000-0000-000000000001", Name: "x"}, source.ErrInvalidMachine},
			{"uppercase id", "alice", source.Machine{ID: strings.Repeat("A", 32), Name: "x"}, source.ErrInvalidMachine},
			{"empty name", "alice", source.Machine{ID: ID(1)}, source.ErrInvalidMachine},
			{"name over 63 bytes", "alice", source.Machine{ID: ID(1), Name: long}, source.ErrInvalidMachine},
			{"control character in name", "alice", source.Machine{ID: ID(1), Name: "a\x00b"}, source.ErrInvalidMachine},
			{"C1 control in name", "alice", source.Machine{ID: ID(1), Name: "a\u0085b"}, source.ErrInvalidMachine},
			{"DEL in name", "alice", source.Machine{ID: ID(1), Name: "a\x7fb"}, source.ErrInvalidMachine},
			{"invalid UTF-8 name", "alice", source.Machine{ID: ID(1), Name: "a\xffb"}, source.ErrInvalidMachine},
			{"control character in fingerprint", "alice", source.Machine{ID: ID(1), Name: "x", KeyFingerprint: "a\tb"}, source.ErrInvalidMachine},
			{"fingerprint too long", "alice", source.Machine{ID: ID(1), Name: "x", KeyFingerprint: strings.Repeat("f", source.MaxTextBytes+1)}, source.ErrInvalidMachine},
			{"invalid user", "Alice", M(1), source.ErrInvalidUser},
			{"user with a space", "a b", M(1), source.ErrInvalidUser},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if _, err := in.Bindings.Admit(c.user, c.m); !errors.Is(err, c.want) {
					t.Fatalf("Admit = %v; want %v", err, c.want)
				}
			})
		}
		if in.Writes() != 0 {
			t.Fatalf("refused admits wrote")
		}
		if _, ok := in.Bindings.Get("alice"); ok {
			t.Fatalf("a refused admit bound something")
		}
		if d := admit(t, in.Bindings, "alice", source.Machine{ID: ID(1), Name: ok63}); !d.Admitted || d.Machine.Name != ok63 {
			t.Fatalf("63-byte name refused: %+v", d)
		}
	})

	t.Run("users are independent and the implicit user is one of them", func(t *testing.T) {
		in, _ := newInst(t)
		admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "bob", M(2))
		admit(t, in.Bindings, source.ImplicitUser, M(3))
		for _, c := range []struct {
			user string
			want string
		}{{"alice", ID(1)}, {"bob", ID(2)}, {source.ImplicitUser, ID(3)}} {
			if b, ok := in.Bindings.Get(c.user); !ok || b.MachineID() != c.want || b.User != c.user {
				t.Fatalf("Get(%q) = %+v, %v", c.user, b, ok)
			}
		}
		// The same machine id presented by another user is a separate record
		// for that user, and does not take over alice's binding.
		if d := admit(t, in.Bindings, "bob", M(1)); d.Admitted || d.Bound.ID != ID(2) {
			t.Fatalf("alice's machine under bob: %+v", d)
		}
		if _, err := in.Bindings.Use("alice", ID(2), "me"); !errors.Is(err, source.ErrUnknownMachine) {
			t.Fatalf("alice moved to bob's machine: %v", err)
		}
		if got := ids(in.Bindings.Machines("alice")); got != ID(1) {
			t.Fatalf("alice machines = %s", got)
		}
	})

	t.Run("generation increases on every bind and move and never repeats", func(t *testing.T) {
		in, _ := newInst(t)
		seen := map[uint64]bool{}
		var last uint64
		step := func(what string, g uint64) {
			t.Helper()
			if g <= last || seen[g] {
				t.Fatalf("%s: generation %d after %d (seen %v)", what, g, last, seen)
			}
			seen[g] = true
			last = g
		}
		step("bind", admit(t, in.Bindings, "alice", M(1)).Binding.Generation)
		admit(t, in.Bindings, "alice", M(2))
		mv, err := in.Bindings.Use("alice", ID(2), "me")
		if err != nil {
			t.Fatal(err)
		}
		step("move", mv.Binding.Generation)
		// Another user's bind takes from the same counter.
		admit(t, in.Bindings, "bob", M(5))
		in = in.Reopen(t)
		mv, err = in.Bindings.Use("alice", ID(1), "me")
		if err != nil {
			t.Fatal(err)
		}
		step("move after reopen", mv.Binding.Generation)
		if ok, err := in.Bindings.Drop("alice"); err != nil || !ok {
			t.Fatalf("Drop = %v, %v", ok, err)
		}
		in = in.Reopen(t)
		step("bind after drop and reopen", admit(t, in.Bindings, "alice", M(1)).Binding.Generation)
		if ok, err := in.Bindings.Drop("alice"); err != nil || !ok {
			t.Fatalf("Drop = %v, %v", ok, err)
		}
		step("bind after drop", admit(t, in.Bindings, "alice", M(2)).Binding.Generation)
	})

	t.Run("current", func(t *testing.T) {
		in, _ := newInst(t)
		d := admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "alice", M(2))
		g := d.Binding.Generation
		if !in.Bindings.Current("alice", ID(1), g) {
			t.Fatalf("current binding not current")
		}
		for _, c := range []struct {
			user, id string
			gen      uint64
		}{{"alice", ID(2), g}, {"alice", ID(1), g + 1}, {"alice", "", g}, {"bob", ID(1), g}, {source.ImplicitUser, ID(1), g}} {
			if in.Bindings.Current(c.user, c.id, c.gen) {
				t.Fatalf("Current(%q, %q, %d) = true", c.user, c.id, c.gen)
			}
		}
		mv, err := in.Bindings.Use("alice", ID(2), "me")
		if err != nil {
			t.Fatal(err)
		}
		if in.Bindings.Current("alice", ID(1), g) {
			t.Fatalf("stale generation still current after a move")
		}
		if !in.Bindings.Current("alice", ID(2), mv.Binding.Generation) {
			t.Fatalf("new binding not current")
		}
		// Moving back does not revive the old generation.
		if _, err := in.Bindings.Use("alice", ID(1), "me"); err != nil {
			t.Fatal(err)
		}
		if in.Bindings.Current("alice", ID(1), g) {
			t.Fatalf("moving back revived generation %d", g)
		}
	})

	t.Run("a failed write leaves the state unchanged", func(t *testing.T) {
		type op struct {
			name  string
			setup func(t *testing.T, b source.Bindings)
			do    func(b source.Bindings) error
		}
		ops := []op{
			{"first bind", nil, func(b source.Bindings) error { _, err := b.Admit("alice", M(1)); return err }},
			{"new refused machine", func(t *testing.T, b source.Bindings) { admit(t, b, "alice", M(1)) },
				func(b source.Bindings) error { _, err := b.Admit("alice", M(2)); return err }},
			{"rename", func(t *testing.T, b source.Bindings) { admit(t, b, "alice", M(1)) },
				func(b source.Bindings) error { m := M(1); m.Name = "other"; _, err := b.Admit("alice", m); return err }},
			{"move", func(t *testing.T, b source.Bindings) { admit(t, b, "alice", M(1)); admit(t, b, "alice", M(2)) },
				func(b source.Bindings) error { _, err := b.Use("alice", ID(2), "me"); return err }},
			{"drop", func(t *testing.T, b source.Bindings) { admit(t, b, "alice", M(1)) },
				func(b source.Bindings) error { _, err := b.Drop("alice"); return err }},
			{"retain", func(t *testing.T, b source.Bindings) { admit(t, b, "alice", M(1)) },
				func(b source.Bindings) error { _, err := b.RetainUsers([]string{"bob"}); return err }},
			{"adopt", func(t *testing.T, b source.Bindings) { admit(t, b, source.ImplicitUser, M(1)) },
				func(b source.Bindings) error { _, err := b.AdoptImplicit("alice"); return err }},
		}
		for _, o := range ops {
			t.Run(o.name, func(t *testing.T) {
				in, clk := newInst(t)
				if o.setup != nil {
					o.setup(t, in.Bindings)
				}
				before := snapshot(in.Bindings)
				w := in.Writes()
				clk.Advance(time.Minute)
				in.FailWrites(errInjected)
				if err := o.do(in.Bindings); !errors.Is(err, errInjected) {
					t.Fatalf("err = %v; want the injected failure", err)
				}
				if after := snapshot(in.Bindings); after != before {
					t.Fatalf("state changed by a failed write:\nbefore %s\nafter  %s", before, after)
				}
				if in.Writes() != w {
					t.Fatalf("a failed write counted as written")
				}
				in.FailWrites(nil)
				if err := o.do(in.Bindings); err != nil {
					t.Fatalf("retry after the failure cleared: %v", err)
				}
				if after := snapshot(in.Bindings); after == before {
					t.Fatalf("retry changed nothing")
				}
				mem := snapshot(in.Bindings)
				if re := snapshot(in.Reopen(t).Bindings); re != stripLastSeen(mem) {
					t.Fatalf("reopen differs:\nmemory %s\nreopen %s", mem, re)
				}
			})
		}
	})

	t.Run("a read during a save that then fails sees the old state", func(t *testing.T) {
		in, clk := newInst(t)
		first := admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "alice", M(2))
		before := snapshot(in.Bindings)
		clk.Advance(time.Minute)
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		// A failed check must not leave the save held (closing the store at
		// clean-up would then wait for it forever).
		t.Cleanup(unblock)
		in.BeforeWrite(func() {
			close(entered)
			<-release
		})
		in.FailWrites(errInjected)
		done := make(chan error, 1)
		go func() {
			_, err := in.Bindings.Use("alice", ID(2), "me")
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the move never reached its save")
		}
		// The save is held open: reads must not wait for it, and must not see
		// the state it is saving.
		check := func(when string) {
			t.Helper()
			if got := snapshot(in.Bindings); got != before {
				t.Fatalf("%s: state = %s; want %s", when, got, before)
			}
			if !in.Bindings.Current("alice", ID(1), first.Binding.Generation) {
				t.Fatalf("%s: the old binding is not current", when)
			}
			if b, _ := in.Bindings.Get("alice"); b.MachineID() != ID(1) {
				t.Fatalf("%s: Get names %s", when, b.MachineID())
			}
		}
		check("during the save")
		unblock()
		select {
		case err := <-done:
			if !errors.Is(err, errInjected) {
				t.Fatalf("Use = %v; want the injected failure", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the move did not finish")
		}
		check("after the failed save")
	})

	t.Run("returned bindings are copies", func(t *testing.T) {
		in, _ := newInst(t)
		scribble := func(b *source.Binding) {
			b.Delivery[0].ID = ID(99)
			b.Signing[0].ID = ID(99)
			b.Delivery = append(b.Delivery, source.Ref{Kind: source.KindMachine, ID: ID(98)})
		}
		intact := func(when, user, id string) {
			t.Helper()
			b, ok := in.Bindings.Get(user)
			want := []source.Ref{{Kind: source.KindMachine, ID: id}}
			if !ok || !refsEqual(b.Delivery, want) || !refsEqual(b.Signing, want) {
				t.Fatalf("after scribbling on %s: Get(%q) = %+v", when, user, b)
			}
		}
		d := admit(t, in.Bindings, "alice", M(1))
		scribble(&d.Binding)
		intact("the Admit decision", "alice", ID(1))
		g, _ := in.Bindings.Get("alice")
		scribble(&g)
		intact("a Get result", "alice", ID(1))
		admit(t, in.Bindings, "alice", M(2))
		mv, err := in.Bindings.Use("alice", ID(2), "me")
		if err != nil {
			t.Fatal(err)
		}
		scribble(&mv.Binding)
		intact("the Move", "alice", ID(2))
		noop, err := in.Bindings.Use("alice", ID(2), "me")
		if err != nil {
			t.Fatal(err)
		}
		scribble(&noop.Binding)
		intact("a no-op Move", "alice", ID(2))
		admit(t, in.Bindings, source.ImplicitUser, M(3))
		ad, err := in.Bindings.AdoptImplicit("bob")
		if err != nil || ad.Binding == nil {
			t.Fatalf("AdoptImplicit = %+v, %v", ad, err)
		}
		scribble(ad.Binding)
		intact("the Adoption", "bob", ID(3))
		if !in.Bindings.Current("alice", ID(2), mv.Binding.Generation) {
			t.Fatal("binding not current after scribbling on copies")
		}
		// What was saved is untouched too.
		in = in.Reopen(t)
		intact("copies, after a reopen", "alice", ID(2))
		intact("copies, after a reopen", "bob", ID(3))
	})

	t.Run("metadata churn on a refused machine is bounded in storage", func(t *testing.T) {
		in, clk := newInst(t)
		admit(t, in.Bindings, "alice", M(1))
		w := in.Writes()
		const rounds = 50
		var last source.Machine
		for i := 0; i < rounds; i++ {
			clk.Advance(time.Second)
			m := M(2)
			m.Name = []string{"name-a", "name-b"}[i%2]
			m.KeyFingerprint = []string{"SHA256:x", "SHA256:y"}[(i/2)%2]
			d := admit(t, in.Bindings, "alice", m)
			if d.Admitted || d.Bound.ID != ID(1) {
				t.Fatalf("round %d: refused machine admitted: %+v", i, d)
			}
			last = m
		}
		// Every change of name or fingerprint is a write (the caller
		// rate-limits them); what is kept stays one record.
		if in.Writes() == w {
			t.Fatal("churn wrote nothing; the changes were not recorded")
		}
		check := func(when string, b source.Bindings) {
			t.Helper()
			ms := b.Machines("alice")
			if len(ms) != 2 {
				t.Fatalf("%s: machines = %d; want 2", when, len(ms))
			}
			got := ms[1]
			if got.ID != ID(2) || got.Name != last.Name || got.KeyFingerprint != last.KeyFingerprint {
				t.Fatalf("%s: machine = %+v; want the last name and fingerprint %+v", when, got, last)
			}
			if bd, _ := b.Get("alice"); bd.MachineID() != ID(1) {
				t.Fatalf("%s: binding moved: %+v", when, bd)
			}
		}
		check("in memory", in.Bindings)
		check("after a reopen", in.Reopen(t).Bindings)
	})

	t.Run("retain refuses an empty list", func(t *testing.T) {
		in, _ := newInst(t)
		admit(t, in.Bindings, source.ImplicitUser, M(1))
		admit(t, in.Bindings, "alice", M(2))
		before := snapshot(in.Bindings)
		w := in.Writes()
		for _, keep := range [][]string{nil, {}} {
			dropped, err := in.Bindings.RetainUsers(keep)
			if !errors.Is(err, source.ErrNoUsersToKeep) || dropped != nil {
				t.Fatalf("RetainUsers(%#v) = %v, %v; want ErrNoUsersToKeep", keep, dropped, err)
			}
		}
		if in.Writes() != w || snapshot(in.Bindings) != before {
			t.Fatal("a refused RetainUsers changed the store")
		}
		// The implicit user alone is the list for a server with no users.
		dropped, err := in.Bindings.RetainUsers([]string{source.ImplicitUser})
		if err != nil || strings.Join(dropped, ",") != "alice" {
			t.Fatalf("RetainUsers(implicit) = %v, %v", dropped, err)
		}
	})

	t.Run("machines per user are bounded; the least recently seen unbound one goes", func(t *testing.T) {
		in, clk := newInst(t)
		// Machine 1 is bound and the oldest; never seen again.
		admit(t, in.Bindings, "alice", M(1))
		for n := 2; n <= source.MaxMachinesPerUser; n++ {
			clk.Advance(time.Second)
			admit(t, in.Bindings, "alice", M(n))
		}
		// Machine 2 is next oldest by first seen but seen again now; machine 3
		// is then the least recently seen unbound one.
		clk.Advance(time.Second)
		admit(t, in.Bindings, "alice", M(2))
		admit(t, in.Bindings, "bob", M(1000)) // another user's machines do not count
		clk.Advance(time.Second)
		d := admit(t, in.Bindings, "alice", M(500))
		if len(d.Evicted) != 1 || d.Evicted[0].ID != ID(3) {
			t.Fatalf("evicted = %+v; want machine 3", d.Evicted)
		}
		ms := in.Bindings.Machines("alice")
		if len(ms) != source.MaxMachinesPerUser {
			t.Fatalf("machines = %d; want %d", len(ms), source.MaxMachinesPerUser)
		}
		has := map[string]bool{}
		for _, m := range ms {
			has[m.ID] = true
		}
		if !has[ID(1)] || !has[ID(2)] || has[ID(3)] || !has[ID(500)] {
			t.Fatalf("machines = %s", ids(ms))
		}
		if b, _ := in.Bindings.Get("alice"); b.MachineID() != ID(1) {
			t.Fatalf("binding moved by eviction: %+v", b)
		}
		if _, err := in.Bindings.Use("alice", ID(3), "me"); !errors.Is(err, source.ErrUnknownMachine) {
			t.Fatalf("Use of an evicted machine = %v", err)
		}
		// After a reopen last-seen is gone, so eviction falls back to first
		// seen: machine 2 (oldest unbound) goes, the bound machine 1 stays.
		in = in.Reopen(t)
		clk.Advance(time.Second)
		d = admit(t, in.Bindings, "alice", M(501))
		if len(d.Evicted) != 1 || d.Evicted[0].ID != ID(2) {
			t.Fatalf("evicted after reopen = %+v; want machine 2", d.Evicted)
		}
		if n := len(in.Bindings.Machines("alice")); n != source.MaxMachinesPerUser {
			t.Fatalf("machines after reopen = %d", n)
		}
		if b, _ := in.Bindings.Get("alice"); b.MachineID() != ID(1) {
			t.Fatalf("binding moved by eviction: %+v", b)
		}
	})

	t.Run("seen keeps a known machine fresh in memory and records nothing", func(t *testing.T) {
		in, clk := newInst(t)
		admit(t, in.Bindings, "alice", M(1))
		admit(t, in.Bindings, "alice", M(2))
		w := in.Writes()
		before := snapshot(in.Bindings)
		clk.Advance(time.Minute)
		if !in.Bindings.Seen("alice", ID(2)) {
			t.Fatal("Seen of a remembered machine = false")
		}
		if in.Writes() != w {
			t.Fatalf("Seen wrote %d times; want 0", in.Writes()-w)
		}
		// A failing save would fail any write: Seen never saves.
		in.FailWrites(errInjected)
		clk.Advance(time.Minute)
		if !in.Bindings.Seen("alice", ID(2)) {
			t.Fatal("Seen with failing saves = false")
		}
		var got source.Machine
		for _, m := range in.Bindings.Machines("alice") {
			if m.ID == ID(2) {
				got = m
			}
		}
		if !got.LastSeen.Equal(clk.Now()) || got.Name != "machine-2" || got.KeyFingerprint != "SHA256:key2" {
			t.Fatalf("after Seen machine 2 = %+v; want last seen %v and its name and key unchanged", got, clk.Now())
		}
		// Unknown machines and users: false, nothing changes.
		after := snapshot(in.Bindings)
		if in.Bindings.Seen("alice", ID(3)) || in.Bindings.Seen("bob", ID(1)) {
			t.Fatal("Seen of a machine the store does not remember = true")
		}
		if snapshot(in.Bindings) != after {
			t.Fatal("Seen of an unknown machine changed the store")
		}
		if in.Writes() != w {
			t.Fatalf("Seen wrote %d times; want 0", in.Writes()-w)
		}
		in.FailWrites(nil)
		// Nothing of it is kept across a restart.
		if re := snapshot(in.Reopen(t).Bindings); re != stripLastSeen(before) {
			t.Fatalf("after a reopen:\n%s\nwant:\n%s", re, stripLastSeen(before))
		}
	})

	t.Run("a machine kept fresh by seen is not the one evicted", func(t *testing.T) {
		in, clk := newInst(t)
		admit(t, in.Bindings, "alice", M(1))
		for n := 2; n <= source.MaxMachinesPerUser; n++ {
			clk.Advance(time.Second)
			admit(t, in.Bindings, "alice", M(n))
		}
		// Machine 2 is the oldest unbound one, but it is seen (not admitted)
		// after every other; machine 3 is then the least recently seen.
		clk.Advance(time.Second)
		if !in.Bindings.Seen("alice", ID(2)) {
			t.Fatal("Seen = false")
		}
		clk.Advance(time.Second)
		d := admit(t, in.Bindings, "alice", M(500))
		if len(d.Evicted) != 1 || d.Evicted[0].ID != ID(3) {
			t.Fatalf("evicted = %+v; want machine 3", d.Evicted)
		}
	})

	t.Run("adopt the implicit user's records", func(t *testing.T) {
		in, _ := newInst(t)
		d := admit(t, in.Bindings, source.ImplicitUser, M(1))
		admit(t, in.Bindings, source.ImplicitUser, M(2))
		ad, err := in.Bindings.AdoptImplicit("alice")
		if err != nil {
			t.Fatal(err)
		}
		if !ad.Adopted || ad.Dropped || ad.Machines != 2 || ad.Binding == nil || ad.Binding.User != "alice" || ad.Binding.MachineID() != ID(1) {
			t.Fatalf("adoption = %+v", ad)
		}
		if ad.Binding.Generation <= d.Binding.Generation {
			t.Fatalf("adopted generation %d; want > %d", ad.Binding.Generation, d.Binding.Generation)
		}
		if ad.Binding.BoundBy != source.BoundByFirstConnect || !ad.Binding.BoundAt.Equal(d.Binding.BoundAt) {
			t.Fatalf("adoption changed when and by whom: %+v", ad.Binding)
		}
		if _, ok := in.Bindings.Get(source.ImplicitUser); ok || len(in.Bindings.Machines(source.ImplicitUser)) != 0 {
			t.Fatalf("implicit user still has records")
		}
		if in.Bindings.Current(source.ImplicitUser, ID(1), d.Binding.Generation) {
			t.Fatalf("implicit binding still current")
		}
		re := in.Reopen(t)
		b, ok := re.Bindings.Get("alice")
		if !ok || b.User != "alice" || b.MachineID() != ID(1) || b.Generation != ad.Binding.Generation {
			t.Fatalf("after reopen = %+v, %v", b, ok)
		}
		if got := ids(re.Bindings.Machines("alice")); got != ID(1)+","+ID(2) {
			t.Fatalf("after reopen machines = %s", got)
		}
		if d := admit(t, re.Bindings, "alice", M(1)); !d.Admitted {
			t.Fatalf("adopted machine refused")
		}
	})

	t.Run("adopt drops the implicit user's records when the user has their own", func(t *testing.T) {
		in, _ := newInst(t)
		admit(t, in.Bindings, source.ImplicitUser, M(1))
		own := admit(t, in.Bindings, "alice", M(2))
		ad, err := in.Bindings.AdoptImplicit("alice")
		if err != nil {
			t.Fatal(err)
		}
		if ad.Adopted || !ad.Dropped || ad.Machines != 1 {
			t.Fatalf("adoption = %+v", ad)
		}
		if b, _ := in.Bindings.Get("alice"); b.MachineID() != ID(2) || b.Generation != own.Binding.Generation {
			t.Fatalf("alice's own binding changed: %+v", b)
		}
		if got := ids(in.Bindings.Machines("alice")); got != ID(2) {
			t.Fatalf("alice machines = %s", got)
		}
		if _, ok := in.Bindings.Get(source.ImplicitUser); ok {
			t.Fatalf("implicit binding kept")
		}
	})

	t.Run("adopt with nothing to adopt writes nothing", func(t *testing.T) {
		in, _ := newInst(t)
		admit(t, in.Bindings, "alice", M(1))
		w := in.Writes()
		ad, err := in.Bindings.AdoptImplicit("bob")
		if err != nil || ad != (source.Adoption{}) {
			t.Fatalf("AdoptImplicit = %+v, %v", ad, err)
		}
		if in.Writes() != w {
			t.Fatalf("an empty adoption wrote")
		}
		for _, bad := range []string{source.ImplicitUser, "Bob"} {
			if _, err := in.Bindings.AdoptImplicit(bad); !errors.Is(err, source.ErrInvalidUser) {
				t.Fatalf("AdoptImplicit(%q) = %v", bad, err)
			}
		}
	})

	t.Run("drop and retain", func(t *testing.T) {
		in, _ := newInst(t)
		admit(t, in.Bindings, source.ImplicitUser, M(1))
		admit(t, in.Bindings, "alice", M(2))
		admit(t, in.Bindings, "bob", M(3))
		admit(t, in.Bindings, "carol", M(4))
		ok, err := in.Bindings.Drop(source.ImplicitUser)
		if err != nil || !ok {
			t.Fatalf("Drop implicit = %v, %v", ok, err)
		}
		w := in.Writes()
		if ok, err := in.Bindings.Drop(source.ImplicitUser); err != nil || ok {
			t.Fatalf("second Drop = %v, %v", ok, err)
		}
		if _, err := in.Bindings.Drop("Bad"); !errors.Is(err, source.ErrInvalidUser) {
			t.Fatalf("Drop(bad) = %v", err)
		}
		if dropped, err := in.Bindings.RetainUsers([]string{"alice", "bob", "carol", "dave"}); err != nil || dropped != nil {
			t.Fatalf("RetainUsers(all) = %v, %v", dropped, err)
		}
		if in.Writes() != w {
			t.Fatalf("no-op drop or retain wrote")
		}
		if _, err := in.Bindings.RetainUsers([]string{"alice", "Bad"}); !errors.Is(err, source.ErrInvalidUser) {
			t.Fatalf("RetainUsers(bad) = %v", err)
		}
		dropped, err := in.Bindings.RetainUsers([]string{"alice"})
		if err != nil || strings.Join(dropped, ",") != "bob,carol" {
			t.Fatalf("RetainUsers = %v, %v", dropped, err)
		}
		re := in.Reopen(t)
		for _, u := range []string{source.ImplicitUser, "bob", "carol"} {
			if _, ok := re.Bindings.Get(u); ok || len(re.Bindings.Machines(u)) != 0 {
				t.Fatalf("user %q kept records", u)
			}
		}
		if b, ok := re.Bindings.Get("alice"); !ok || b.MachineID() != ID(2) {
			t.Fatalf("alice = %+v, %v", b, ok)
		}
		// A dropped name that comes back starts clean: its first machine binds.
		if d := admit(t, re.Bindings, "bob", M(7)); !d.Admitted || !d.NewlyBound {
			t.Fatalf("reused name: %+v", d)
		}
	})

	t.Run("two first admissions held across a save bind exactly one", func(t *testing.T) {
		in, _ := newInst(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		second := make(chan struct{}, 8)
		var calls int
		var mu sync.Mutex
		in.BeforeWrite(func() {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				close(entered)
				<-release
				return
			}
			second <- struct{}{}
		})
		res := make(chan source.Decision, 2)
		errs := make(chan error, 2)
		run := func(m source.Machine) {
			d, err := in.Bindings.Admit("alice", m)
			if err != nil {
				errs <- err
				return
			}
			res <- d
		}
		go run(M(1))
		<-entered // machine 1 has decided to bind and is saving
		go run(M(2))
		// A store that let machine 2 decide before machine 1's save finished
		// would reach its own save now; a correct one holds it back. Either
		// way machine 1 is then released.
		select {
		case <-second:
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		var ds []source.Decision
		for i := 0; i < 2; i++ {
			select {
			case d := <-res:
				ds = append(ds, d)
			case err := <-errs:
				t.Fatal(err)
			case <-time.After(10 * time.Second):
				t.Fatal("admissions did not finish")
			}
		}
		admitted := 0
		for _, d := range ds {
			if d.Admitted {
				admitted++
				if d.Machine.ID != ID(1) {
					t.Fatalf("machine %s admitted; machine 1 saved first", d.Machine.ID)
				}
			} else if d.Bound.ID != ID(1) {
				t.Fatalf("refusal names %s; want machine 1", d.Bound.ID)
			}
		}
		if admitted != 1 {
			t.Fatalf("%d machines admitted; want exactly 1: %+v", admitted, ds)
		}
		if b, _ := in.Bindings.Get("alice"); b.MachineID() != ID(1) {
			t.Fatalf("bound = %s", b.MachineID())
		}
		if n := len(in.Bindings.Machines("alice")); n != 2 {
			t.Fatalf("machines = %d; want 2", n)
		}
	})

	t.Run("many concurrent first admissions bind exactly one", func(t *testing.T) {
		in, _ := newInst(t)
		const n = 16
		start := make(chan struct{})
		var wg sync.WaitGroup
		res := make([]source.Decision, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				res[i], errs[i] = in.Bindings.Admit("alice", M(i+1))
				_ = in.Bindings.Current("alice", ID(i+1), res[i].Binding.Generation)
				_, _ = in.Bindings.Get("alice")
				_ = in.Bindings.Machines("alice")
			}(i)
		}
		close(start)
		wg.Wait()
		admitted := ""
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if res[i].Admitted {
				if admitted != "" {
					t.Fatalf("both %s and %s admitted", admitted, res[i].Machine.ID)
				}
				admitted = res[i].Machine.ID
			}
		}
		b, _ := in.Bindings.Get("alice")
		if admitted == "" || b.MachineID() != admitted {
			t.Fatalf("admitted %q, bound %q", admitted, b.MachineID())
		}
		for i := 0; i < n; i++ {
			if res[i].Bound.ID != admitted {
				t.Fatalf("decision %d names %s; bound is %s", i, res[i].Bound.ID, admitted)
			}
		}
		if got := len(in.Bindings.Machines("alice")); got != n {
			t.Fatalf("machines = %d; want %d", got, n)
		}
	})
}

// refsEqual compares two ref lists.
func refsEqual(a, b []source.Ref) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ids lists machine ids, comma-separated.
func ids(ms []source.Machine) string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return strings.Join(out, ",")
}

// snapshot renders what the store holds for the table's users, in a stable
// form, for comparison.
func snapshot(b source.Bindings) string {
	var sb strings.Builder
	for _, u := range []string{source.ImplicitUser, "alice", "bob", "carol"} {
		if bd, ok := b.Get(u); ok {
			fmt.Fprintf(&sb, "[%q bound %s gen %d at %s by %s]", u, bd.MachineID(), bd.Generation, bd.BoundAt.Format(time.RFC3339Nano), bd.BoundBy)
		}
		for _, m := range b.Machines(u) {
			fmt.Fprintf(&sb, "[%q machine %s %q %q first %s last %s]", u, m.ID, m.Name, m.KeyFingerprint,
				m.FirstSeen.Format(time.RFC3339Nano), m.LastSeen.Format(time.RFC3339Nano))
		}
	}
	return sb.String()
}

// stripLastSeen blanks the last-seen times in a snapshot (a reopen does not
// keep them).
func stripLastSeen(s string) string {
	var out strings.Builder
	for _, part := range strings.SplitAfter(s, "]") {
		if i := strings.Index(part, " last "); i >= 0 {
			part = part[:i] + " last " + time.Time{}.Format(time.RFC3339Nano) + "]"
		}
		out.WriteString(part)
	}
	return out.String()
}
