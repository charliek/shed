package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNeedsRefresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"zero expiry (static token / open server) never refreshes", time.Time{}, false},
		{"far-future expiry does not refresh", now.Add(24 * time.Hour), false},
		{"within the refresh window refreshes", now.Add(time.Hour), true},
		{"exactly at the window edge refreshes", now.Add(tokenRefreshWindow), true},
		{"expired refreshes", now.Add(-time.Minute), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsRefresh(tt.expiresAt, now); got != tt.want {
				t.Errorf("needsRefresh = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDoRequest401RefreshAndRetry(t *testing.T) {
	var hits int
	refreshed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		// The stale token gets a 401; after refresh, the new token gets a 200.
		if r.Header.Get("Authorization") == "Bearer new" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newAPIClient(srv.URL, "stale", "", DefaultTimeout)
	c.refreshFn = func() (string, error) { refreshed = true; return "new", nil }

	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.doRequest("GET", "/x", nil, &out); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if !refreshed {
		t.Error("refreshFn should have been called on the 401")
	}
	if !out.OK {
		t.Error("the retry should have succeeded with the refreshed token")
	}
	if hits != 2 {
		t.Errorf("want 2 requests (401 then 200), got %d", hits)
	}
}

func TestDoRequest401RetryAtMostOnce(t *testing.T) {
	var hits, refreshes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized) // always 401
	}))
	defer srv.Close()

	c := newAPIClient(srv.URL, "stale", "", DefaultTimeout)
	c.refreshFn = func() (string, error) { refreshes++; return "new", nil }

	if err := c.doRequest("GET", "/x", nil, nil); err == nil {
		t.Error("expected an error when the 401 persists after refresh")
	}
	if refreshes != 1 {
		t.Errorf("refresh must run at most once, ran %d", refreshes)
	}
	if hits != 2 {
		t.Errorf("want 2 requests (initial + one retry), got %d", hits)
	}
}
