package tunnels

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/servertls"
)

// refusal is a clienttoken.Refusal, as config.EntryRefusedError is.
type refusal struct{}

func (refusal) Error() string { return `entry "srv" cannot be used` }
func (refusal) EntryRefusal() {}

type acceptCounting struct {
	net.Listener
	n *atomic.Int32
}

func (l acceptCounting) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// TestRefusalBeforeTheAttemptCheckStopsTheAttempt covers the two windows a
// source shared by several calls (a tunnel daemon's connections) leaves open
// after a call has captured its credential:
//
//   - another call records a refusal after this one's capture and before its
//     first attempt's check;
//   - this call's reactive re-mint succeeds (generation 0 to 1), and another
//     call's re-mint of generation 1 is refused before this call's retry's
//     check.
//
// Either way the attempt makes no connection and the call returns the
// recorded refusal itself, for Dial and for Probe. The refusal is recorded
// BEFORE the attempt's check: the contract is per attempt, and an attempt
// that already passed its check when the refusal lands may complete; this
// test claims nothing about that ordering.
func TestRefusalBeforeTheAttemptCheckStopsTheAttempt(t *testing.T) {
	type call struct {
		name string
		run  func(ctx context.Context, c *ConnectClient) error
	}
	calls := []call{
		{"Dial", func(ctx context.Context, c *ConnectClient) error {
			conn, err := c.Dial(ctx, "myshed", 8080)
			if conn != nil {
				conn.Close()
			}
			return err
		}},
		{"Probe", func(ctx context.Context, c *ConnectClient) error { return c.Probe(ctx, "myshed") }},
	}
	windows := []struct {
		name string
		// refuseOn is the attempt (1: the first, 2: the retry) before whose
		// check another call records the refusal.
		refuseOn int32
		// wantAccepted is the connections the server may have accepted: none
		// before a refused first attempt, the first attempt's before a refused
		// retry.
		wantAccepted int32
	}{
		{"refused after capture, before the first attempt", 1, 0},
		{"refused after this call's re-mint, before its retry", 2, 1},
	}
	for _, w := range windows {
		for _, cl := range calls {
			t.Run(w.name+"/"+cl.name, func(t *testing.T) {
				var accepted atomic.Int32
				// Every request is refused with a 401, so a first attempt that
				// reaches the server leads to a re-mint and a retry.
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
					rw.WriteHeader(http.StatusUnauthorized)
				}))
				srv.Listener = acceptCounting{Listener: srv.Listener, n: &accepted}
				srv.Config.ErrorLog = log.New(io.Discard, "", 0)
				srv.StartTLS()
				t.Cleanup(srv.Close)
				target := ConnectTarget{Addr: srv.Listener.Addr().String(), TLSPin: servertls.Fingerprint(srv.Certificate().Raw)}

				// The first re-mint succeeds when the refusal is to land before
				// the retry (this call's own re-mint); every other re-mint is
				// another call's, refused.
				var mints atomic.Int32
				src := clienttoken.New(clienttoken.TokenCredential("tok-0", time.Now().Add(24*time.Hour)),
					func() (clienttoken.Credential, error) {
						if mints.Add(1) == 1 && w.refuseOn == 2 {
							return clienttoken.TokenCredential("tok-1", time.Now().Add(24*time.Hour)), nil
						}
						return clienttoken.Credential{}, refusal{}
					})

				var attempts atomic.Int32
				orig := beforeAttempt
				t.Cleanup(func() { beforeAttempt = orig })
				beforeAttempt = func() {
					if attempts.Add(1) == w.refuseOn {
						// Another call sharing the source re-mints the
						// generation it holds and is refused.
						_, gen := src.Current()
						if _, err := src.Refresh(gen); err != (refusal{}) {
							t.Errorf("the other call's re-mint = %v, want the refusal", err)
						}
					}
				}

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := cl.run(ctx, NewConnectClient(target, src)); err != (refusal{}) {
					t.Errorf("%s = %v, want the recorded refusal itself", cl.name, err)
				}
				if got := attempts.Load(); got != w.refuseOn {
					t.Errorf("attempts = %d, want %d", got, w.refuseOn)
				}
				if got := accepted.Load(); got != w.wantAccepted {
					t.Errorf("the server accepted %d connection(s), want %d", got, w.wantAccepted)
				}
			})
		}
	}
}

// TestRefusedSourceNeverDials: a tunnel whose credential source is refused (the
// entry may not be used) fails its Connect probe and every dial with the
// refusal, unwrapped, and the server accepts no connection at all. That holds
// for a source refused from the start and for one whose proactive re-mint is
// refused while it holds nothing it may present.
func TestRefusedSourceNeverDials(t *testing.T) {
	sources := map[string]func() *clienttoken.Source{
		"refused from the start": func() *clienttoken.Source { return clienttoken.Refused(refusal{}) },
		"refused by its re-mint": func() *clienttoken.Source {
			return clienttoken.New(clienttoken.Credential{Mode: clienttoken.ModeMTLS},
				func() (clienttoken.Credential, error) { return clienttoken.Credential{}, refusal{} })
		},
	}
	for name, newSource := range sources {
		t.Run(name, func(t *testing.T) {
			var accepted atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			srv.Listener = acceptCounting{Listener: srv.Listener, n: &accepted}
			srv.Config.ErrorLog = log.New(io.Discard, "", 0)
			srv.StartTLS()
			t.Cleanup(srv.Close)
			target := ConnectTarget{Addr: srv.Listener.Addr().String(), TLSPin: servertls.Fingerprint(srv.Certificate().Raw)}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := NewConnectClient(target, newSource())
			if err := client.Probe(ctx, "myshed"); err != (refusal{}) {
				t.Errorf("Probe = %v, want the refusal itself", err)
			}
			if _, err := client.Dial(ctx, "myshed", 8080); err != (refusal{}) {
				t.Errorf("Dial = %v, want the refusal itself", err)
			}
			if got := accepted.Load(); got != 0 {
				t.Errorf("the server accepted %d connection(s), want 0", got)
			}
		})
	}
}
