package vmimage

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

// TestPullToOCILayout_RedirectLoopIsBounded: a registry that redirects a
// manifest request back to itself fails the pull after ten redirects, as
// Go's default client would, rather than being followed until the context
// ends (go-containerregistry's SSRF CheckRedirect replaced that bound).
func TestPullToOCILayout_RedirectLoopIsBounded(t *testing.T) {
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var redirects atomic.Int64
	host := startTestRegistryHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			n := redirects.Add(1)
			http.Redirect(w, r, fmt.Sprintf("%s?hop=%d", r.URL.Path, n), http.StatusFound)
			return
		}
		inner.ServeHTTP(w, r)
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := PullToOCILayout(ctx, PullOptions{
		Ref:       host + "/test/loop:v1",
		ImagesDir: t.TempDir(),
		Insecure:  true,
	})
	if err == nil {
		t.Fatal("pull through a redirect loop succeeded")
	}
	if ctx.Err() != nil {
		t.Fatalf("pull ran until the deadline (%d redirects): %v", redirects.Load(), err)
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("want the redirect bound's error, got: %v", err)
	}
	// Ten redirects per attempt; allow a library-level retry or two.
	if got := redirects.Load(); got > 3*maxRegistryRedirects {
		t.Fatalf("registry served %d redirects, want at most %d", got, 3*maxRegistryRedirects)
	}
}

// TestRedirectLimitTransport_AllowsShortChains: a registry's ordinary
// redirect (a blob served from another path) still works.
func TestRedirectLimitTransport_AllowsShortChains(t *testing.T) {
	host := startTestRegistryHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/end", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	c := &http.Client{
		Transport:     redirectLimitTransport{inner: http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
	}
	resp, err := c.Get("http://" + host + "/start")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body %q, want ok", body)
	}
}
