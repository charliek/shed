package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadDiscoveredServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
servers:
  mini2:
    host: mini2
    http_port: 8080
    ssh_port: 2222
  mini3:
    host: mini3
    ssh_port: 2222
  bad:
    host: ""
default_server: mini2
sheds:
  web:
    server: mini2
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDiscoveredServers(path)
	if err != nil {
		t.Fatalf("LoadDiscoveredServers: %v", err)
	}
	// Sorted by name; "bad" (empty host) skipped; mini3 defaults to port 8080.
	// SSH endpoint (host + ssh_port) is carried through for self-minting.
	want := []ServerTarget{
		{Name: "mini2", URL: "http://mini2:8080", SSHHost: "mini2", SSHPort: 2222},
		{Name: "mini3", URL: "http://mini3:8080", SSHHost: "mini3", SSHPort: 2222},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadDiscoveredServersTLS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// "tls" is a secure server (api_url + pin + ssh_port for self-minting); "plain"
	// keeps the legacy http form with no ssh_port (SSHPort 0 → can't self-mint).
	content := `
servers:
  plain:
    host: plainhost
    http_port: 8080
  tls:
    host: tlshost
    http_port: 8080
    ssh_port: 2222
    api_url: https://tlshost:8443
    tls_cert_fingerprint: sha256:abc123
    credentials_token: shed_credentials_xyz
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDiscoveredServers(path)
	if err != nil {
		t.Fatalf("LoadDiscoveredServers: %v", err)
	}
	// api_url overrides http://host:port; the pin + token are carried through.
	// SSHHost is always the host; SSHPort is 0 when ssh_port is absent (plain).
	want := []ServerTarget{
		{Name: "plain", URL: "http://plainhost:8080", SSHHost: "plainhost"},
		{Name: "tls", URL: "https://tlshost:8443", Token: "shed_credentials_xyz", TLSFingerprint: "sha256:abc123", SSHHost: "tlshost", SSHPort: 2222},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadDiscoveredServersMissingFile(t *testing.T) {
	got, err := LoadDiscoveredServers(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file should yield empty slice, got %v", got)
	}
}

func TestLoadDiscoveredServersMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("servers: [this is not a map\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDiscoveredServers(path); err == nil {
		t.Error("malformed YAML should return an error")
	}
}
