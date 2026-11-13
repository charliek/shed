package gce

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenConcurrency(t *testing.T) {
	t.Run("many callers, one exchange", func(t *testing.T) {
		f := newFakeCompute(t)
		creds := testCredentials(t, f)
		const n = 50
		toks := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				toks[i], errs[i] = creds.Token(context.Background())
			}()
		}
		wg.Wait()
		for i := range n {
			if errs[i] != nil || toks[i] != toks[0] {
				t.Fatalf("caller %d: %q, %v; caller 0 got %q", i, toks[i], errs[i], toks[0])
			}
		}
		f.mu.Lock()
		hits := f.tokenHits
		f.mu.Unlock()
		if hits != 1 {
			t.Errorf("token endpoint hit %d times, want 1", hits)
		}
	})

	t.Run("a waiter leaves when its context ends", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 1 {
				close(entered)
			}
			<-release
			_, _ = fmt.Fprint(w, `{"access_token":"tok-slow","expires_in":3600}`)
		}))
		defer srv.Close()
		creds, err := ParseCredentials(keyJSON(t, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		first := make(chan string, 1)
		go func() {
			tok, _ := creds.Token(context.Background())
			first <- tok
		}()
		<-entered

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err = creds.Token(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waiter = %v, want DeadlineExceeded", err)
		}
		if waited := time.Since(started); waited > 5*time.Second {
			t.Errorf("waiter was held %v by another caller's exchange", waited)
		}

		close(release)
		if tok := <-first; tok != "tok-slow" {
			t.Errorf("first caller got %q", tok)
		}
		if tok, err := creds.Token(context.Background()); err != nil || tok != "tok-slow" || hits.Load() != 1 {
			t.Errorf("cached token = %q, %v after %d exchanges", tok, err, hits.Load())
		}
	})

	t.Run("a failed exchange does not stick", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 1 {
				http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprint(w, `{"access_token":"tok-ok","expires_in":3600}`)
		}))
		defer srv.Close()
		creds, err := ParseCredentials(keyJSON(t, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := creds.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "temporarily_unavailable") {
			t.Errorf("first Token = %v, want the endpoint's error", err)
		}
		if tok, err := creds.Token(context.Background()); err != nil || tok != "tok-ok" {
			t.Errorf("second Token = %q, %v", tok, err)
		}
	})
}

func TestCredentialsDoNotPrint(t *testing.T) {
	f := newFakeCompute(t)
	creds := testCredentials(t, f)
	tok, err := creds.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Project: "p", Token: creds}
	var asSource TokenSource = creds
	for _, out := range []string{
		fmt.Sprint(creds), fmt.Sprintf("%v", creds), fmt.Sprintf("%+v", creds), fmt.Sprintf("%#v", creds), creds.String(),
		fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%+v", asSource),
	} {
		if strings.Contains(out, tok) {
			t.Errorf("output shows the access token: %s", out)
		}
		if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "rsa.PrivateKey") {
			t.Errorf("output shows key material: %s", out)
		}
	}
	if got := fmt.Sprint(creds); !strings.Contains(got, creds.Email()) {
		t.Errorf("String() = %q, want the account's address", got)
	}
}
