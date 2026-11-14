package remote

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// seedCreating adds an instance labelled creating, made age ago, with no
// local record: what a create leaves when its record is lost.
func seedCreating(e *env, name, id, envName string, schema string, age time.Duration) {
	ident := provider.Identity{Env: envName, ID: id, Name: name}
	labels := ident.Labels(provider.ShedStateCreating)
	if schema != "" {
		labels[provider.LabelSchema] = schema
	}
	e.prov.Seed(provider.Instance{
		Name: ident.InstanceName(), State: provider.StateRunning, Labels: labels,
		CreatedAt: time.Now().Add(-age),
	})
}

func namesLeft(t *testing.T, e *env) map[string]bool {
	t.Helper()
	insts, err := e.prov.List(context.Background(), provider.Selector{})
	if err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, inst := range insts {
		left[inst.Labels[provider.LabelName]] = true
	}
	return left
}

// TestAbandonedCreateIsDestroyed: an instance still labelled creating long
// after the cloud made it, with no local record, is a create nobody will
// finish. Left alone it runs to its limit and its disk is billed for ever,
// on no list and under no limit. The start-up pass and the periodic pass
// each destroy it, and nothing else.
func TestAbandonedCreateIsDestroyed(t *testing.T) {
	seedAll := func(e *env) {
		seedCreating(e, "abandoned", "aaaaaaaaaaaa", testEnv, "", 2*StaleCreateAge)
		seedCreating(e, "young", "bbbbbbbbbbbb", testEnv, "", StaleCreateAge/2)
		seedCreating(e, "theirs", "cccccccccccc", "other", "", 2*StaleCreateAge)
		seedCreating(e, "future", "dddddddddddd", testEnv, "99", 2*StaleCreateAge)
	}
	check := func(t *testing.T, e *env) {
		t.Helper()
		left := namesLeft(t, e)
		if left["abandoned"] {
			t.Errorf("the abandoned create was left: %v", left)
		}
		for _, name := range []string{"young", "theirs", "future"} {
			if !left[name] {
				t.Errorf("instance %s was destroyed; only the abandoned create may be", name)
			}
		}
		if n := e.prov.Count(fake.OpDestroy); n != 1 {
			t.Errorf("%d destroy calls, want 1", n)
		}
		if ms, _ := e.b.store.Markers(); len(ms) != 0 {
			t.Errorf("%d delete markers left after the destroy, want none", len(ms))
		}
		if logs := strings.Join(e.logLines(), "\n"); !strings.Contains(logs, "abandoned") || !strings.Contains(logs, "destroying it") {
			t.Errorf("the destroy was not logged: %s", logs)
		}
	}

	t.Run("at start-up", func(t *testing.T) {
		e := newEnv(t)
		if err := e.b.Close(); err != nil {
			t.Fatal(err)
		}
		seedAll(e)
		e.b = e.open()
		e.idle()
		check(t, e)
	})

	t.Run("in the periodic pass", func(t *testing.T) {
		e := newEnv(t)
		seedAll(e)
		if err := e.b.Sweep(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		e.idle()
		check(t, e)
		// A second pass finds nothing more to do.
		if err := e.b.Sweep(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		e.idle()
		check(t, e)
	})
}

// TestSlowCreateWithARecordIsNotAbandoned: a create this server is working
// on has a record, and is never taken for an abandoned one however long the
// cloud takes.
func TestSlowCreateWithARecordIsNotAbandoned(t *testing.T) {
	var late atomic.Bool
	e := newEnv(t, manual())
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.tune = func(o *Options) {
		o.Now = func() time.Time {
			if late.Load() {
				return time.Now().Add(2 * StaleCreateAge)
			}
			return time.Now()
		}
	}
	e.b = e.open()
	if _, err := e.create("slow"); err != nil {
		t.Fatal(err)
	}
	if e.ref("slow") == "" {
		t.Fatal("the create made no instance")
	}
	late.Store(true)
	if err := e.b.Sweep(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if !namesLeft(t, e)["slow"] || e.prov.Count(fake.OpDestroy) != 0 {
		t.Fatal("the periodic pass destroyed the instance of a create in flight")
	}
	if got := e.status("slow"); got != config.StatusStarting {
		t.Fatalf("shed is %s, want starting", got)
	}
}
