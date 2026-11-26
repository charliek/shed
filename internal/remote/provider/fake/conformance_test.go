package fake

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/providertest"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample shed-server"

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Harness{
		New: func(t *testing.T) *providertest.Target {
			clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
			p := New(Options{Manual: true, Now: clk.Now, OwnerRequired: true})
			return &providertest.Target{
				Provider: p,
				// The fake keeps its machines in memory; another process
				// is the same memory.
				Fresh:  func() provider.Provider { return p },
				Settle: func(ref provider.Ref) { p.Complete(ref) },
				LoseNextReply: func(call string) {
					op := map[string]Op{"create": OpCreate, "start": OpStart, "stop": OpStop, "delete": OpDestroy, "labels": OpSetLabels, "extend": OpExtend}[call]
					p.LoseNextReply(op)
				},
				ExpireOperations: p.ForgetOperations,
				DeleteOutside:    p.DeleteAs,
				Advance: func(d time.Duration) {
					clk.advance(d)
					p.EndLeases()
				},
				FailNextCreate: func() {
					p.FailOperation(OpCreate, &provider.Error{Kind: provider.ErrQuota, Op: "wait", Code: "CLOUD_QUOTA"})
				},
				User:   "ada",
				Client: "nocturn",
				Key:    testKey,
			}
		},
	})
}
