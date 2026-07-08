package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveEgressProxyBin_SiblingFound(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, egressProxyBinName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveEgressProxyBin(dir)
	if err != nil {
		t.Fatalf("resolveEgressProxyBin: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want sibling %q", got, bin)
	}
}

func TestResolveEgressProxyBin_NotFound(t *testing.T) {
	// Empty dir and (almost certainly) not on PATH ⇒ error. Guard against a
	// real shed-egress-proxy being on PATH in some dev environment.
	if _, err := exec.LookPath(egressProxyBinName); err == nil {
		t.Skip("shed-egress-proxy is on PATH in this environment")
	}
	if _, err := resolveEgressProxyBin(t.TempDir()); err == nil {
		t.Error("expected error when binary is absent from dir and PATH")
	}
}
