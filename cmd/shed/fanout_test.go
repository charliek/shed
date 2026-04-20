package main

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestForEachServer_AllSucceed(t *testing.T) {
	servers := map[string]config.ServerEntry{
		"alpha": {Host: "a.example.com", HTTPPort: 8080},
		"bravo": {Host: "b.example.com", HTTPPort: 8081},
	}

	var calls int32
	results := forEachServer(servers, func(name string, _ config.ServerEntry) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "OK:" + name, nil
	})

	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	// Alphabetical order.
	if results[0].ServerName != "alpha" || results[1].ServerName != "bravo" {
		t.Errorf("unexpected order: %+v", results)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("server %q: unexpected err %v", r.ServerName, r.Err)
		}
		if r.Value != "OK:"+r.ServerName {
			t.Errorf("server %q: value = %q", r.ServerName, r.Value)
		}
	}
}

func TestForEachServer_OneOffline(t *testing.T) {
	servers := map[string]config.ServerEntry{
		"online":  {Host: "ok.example.com", HTTPPort: 8080},
		"offline": {Host: "bad.example.com", HTTPPort: 8080},
	}

	sentinel := errors.New("connection refused")
	results := forEachServer(servers, func(name string, _ config.ServerEntry) (string, error) {
		if name == "offline" {
			return "", sentinel
		}
		return "OK", nil
	})

	byName := make(map[string]ServerResult[string], len(results))
	for _, r := range results {
		byName[r.ServerName] = r
	}
	if byName["online"].Err != nil {
		t.Errorf("online: unexpected err %v", byName["online"].Err)
	}
	if byName["online"].Value != "OK" {
		t.Errorf("online: value = %q", byName["online"].Value)
	}
	if byName["offline"].Err == nil {
		t.Errorf("offline: expected error, got nil")
	}
	if !errors.Is(byName["offline"].Err, sentinel) {
		t.Errorf("offline: err = %v, want sentinel", byName["offline"].Err)
	}
}

func TestForEachServer_AllOffline(t *testing.T) {
	servers := map[string]config.ServerEntry{
		"a": {Host: "a.example.com", HTTPPort: 8080},
		"b": {Host: "b.example.com", HTTPPort: 8080},
	}

	sentinel := errors.New("connection refused")
	results := forEachServer(servers, func(_ string, _ config.ServerEntry) (string, error) {
		return "", sentinel
	})

	for _, r := range results {
		if r.Err == nil {
			t.Errorf("server %q: expected error, got nil", r.ServerName)
		}
	}
}

func TestForEachServer_EmptyMap(t *testing.T) {
	results := forEachServer(map[string]config.ServerEntry{}, func(_ string, _ config.ServerEntry) (string, error) {
		return "", nil
	})
	if len(results) != 0 {
		t.Errorf("expected zero results, got %d", len(results))
	}
}
