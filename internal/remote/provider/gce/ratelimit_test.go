package gce

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

func TestRateLimitOnAPollIsNotARefusal(t *testing.T) {
	const limited = "Quota exceeded for quota metric 'Read requests' and limit 'Read requests per minute'"
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"operation wait", "POST", `/operations/[^/]+/wait$`},
		{"instance read", "GET", `/instances/[^/]+$`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompute(t)
			c := testClient(t, f)
			ctx := context.Background()
			pend, err := c.Create(ctx, goldenSpec(t, "", time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			// One first try plus three retries.
			f.failNext(tc.method, tc.path, 403, "rateLimitExceeded", limited, 4)
			_, err = c.Wait(ctx, pend)
			if err == nil {
				t.Fatal("Wait = nil, want the rate limit")
			}
			if provider.IsRefusal(err) {
				t.Errorf("Wait = %v: a rate-limited poll is reported as a refusal", err)
			}
			if !strings.Contains(err.Error(), "rateLimitExceeded") {
				t.Errorf("Wait = %v, want the cloud's reason in the error", err)
			}
			// The same Pending still resolves once the limit clears.
			inst, err := c.Wait(ctx, pend)
			if err != nil || inst.State != provider.StateRunning {
				t.Errorf("second Wait = %s, %v; want running", inst.State, err)
			}
		})
	}
	t.Run("list", func(t *testing.T) {
		f := newFakeCompute(t)
		c := testClient(t, f)
		// Worded like a resource quota, but the reason says rate limit.
		f.failNext("GET", `/instances$`, 429, "rateLimitExceeded", "Quota 'READ_REQUESTS' exceeded.  Limit: 1500.0", 4)
		_, err := c.List(context.Background(), provider.Selector{Env: testEnv})
		if err == nil || provider.IsRefusal(err) {
			t.Errorf("List = %v, want an error that is not a refusal", err)
		}
	})
	t.Run("start is still a refusal", func(t *testing.T) {
		f := newFakeCompute(t)
		c := testClient(t, f)
		ref := seedMine(f, "limited", "TERMINATED")
		f.failNext("POST", `/start$`, 403, "rateLimitExceeded", limited, 4)
		if _, err := c.Start(context.Background(), ref, provider.StartOptions{}); !errors.Is(err, provider.ErrQuota) {
			t.Errorf("Start = %v, want ErrQuota", err)
		}
	})
	t.Run("set labels is not", func(t *testing.T) {
		f := newFakeCompute(t)
		c := testClient(t, f)
		ref := seedMine(f, "limited", "RUNNING")
		f.failNext("POST", `/setLabels$`, 403, "rateLimitExceeded", limited, 4)
		err := c.SetLabels(context.Background(), ref, map[string]string{provider.LabelState: "deleting"})
		if err == nil || provider.IsRefusal(err) {
			t.Errorf("SetLabels = %v, want an error that is not a refusal", err)
		}
	})
}
