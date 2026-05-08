package vmutil

import (
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestBuildKnownHosts_NilConfig(t *testing.T) {
	got := BuildKnownHosts(nil)
	if !strings.Contains(got, "github.com ssh-ed25519") {
		t.Errorf("expected GitHub ed25519 default in output, got:\n%s", got)
	}
	if !strings.Contains(got, "github.com ecdsa-sha2-nistp256") {
		t.Errorf("expected GitHub ecdsa default in output, got:\n%s", got)
	}
	if !strings.Contains(got, "github.com ssh-rsa") {
		t.Errorf("expected GitHub rsa default in output, got:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got:\n%q", got)
	}
}

func TestBuildKnownHosts_NilGit(t *testing.T) {
	got := BuildKnownHosts(&config.ServerConfig{Name: "test"})
	if !strings.Contains(got, "github.com ssh-ed25519") {
		t.Errorf("expected GitHub default in output even when Git is nil")
	}
}

func TestBuildKnownHosts_AppendsExtras(t *testing.T) {
	extra := "gitlab.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf"
	cfg := &config.ServerConfig{
		Git: &config.GitConfig{ExtraKnownHosts: []string{extra}},
	}
	got := BuildKnownHosts(cfg)
	if !strings.Contains(got, "github.com ssh-ed25519") {
		t.Errorf("expected GitHub default to remain")
	}
	if !strings.Contains(got, extra) {
		t.Errorf("expected operator extra in output, got:\n%s", got)
	}
}

func TestBuildKnownHosts_DeduplicatesExactMatch(t *testing.T) {
	dup := "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	cfg := &config.ServerConfig{
		Git: &config.GitConfig{ExtraKnownHosts: []string{dup, dup}},
	}
	got := BuildKnownHosts(cfg)
	if n := strings.Count(got, dup); n != 1 {
		t.Errorf("expected dup line to appear exactly once, found %d times in:\n%s", n, got)
	}
}

func TestBuildKnownHosts_TrimsCRLF(t *testing.T) {
	cfg := &config.ServerConfig{
		Git: &config.GitConfig{ExtraKnownHosts: []string{
			"gitlab.com ssh-ed25519 AAAAC3...\r\n",
		}},
	}
	got := BuildKnownHosts(cfg)
	if strings.Contains(got, "\r") {
		t.Errorf("output must not contain carriage returns, got:\n%q", got)
	}
}
