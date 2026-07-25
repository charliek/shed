package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTunnelLogPaths(t *testing.T) {
	wantDir := filepath.Join(GetClientConfigDir(), "logs")
	if got := GetTunnelLogDir(); got != wantDir {
		t.Errorf("GetTunnelLogDir() = %q, want %q", got, wantDir)
	}
	wantPath := filepath.Join(wantDir, "tunnel-myproj.log")
	if got := GetTunnelLogPath("myproj"); got != wantPath {
		t.Errorf("GetTunnelLogPath(myproj) = %q, want %q", got, wantPath)
	}
}

func TestServerEntryBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		entry ServerEntry
		want  string
	}{
		{
			name:  "legacy plain http from host+port",
			entry: ServerEntry{Host: "host.example", HTTPPort: 8080},
			want:  "http://host.example:8080",
		},
		{
			name:  "api_url overrides scheme+host+port",
			entry: ServerEntry{Host: "host.example", HTTPPort: 8080, APIURL: "https://host.example:8443"},
			want:  "https://host.example:8443",
		},
		{
			name:  "api_url trailing slash trimmed",
			entry: ServerEntry{Host: "host.example", HTTPPort: 8080, APIURL: "https://host.example:8443/"},
			want:  "https://host.example:8443",
		},
		{
			// An unbracketed IPv6 literal produces "http://::1:8080", which no
			// http.Client or url.Parse can make sense of — and the open-mode add
			// path writes exactly this kind of entry (no api_url).
			name:  "ipv6 literal is bracketed",
			entry: ServerEntry{Host: "::1", HTTPPort: 8080},
			want:  "http://[::1]:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.entry.BaseURL()
			if got != tt.want {
				t.Errorf("BaseURL() = %q, want %q", got, tt.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("BaseURL() = %q does not parse: %v", got, err)
			}
			if u.Hostname() == "" || u.Port() == "" {
				t.Errorf("BaseURL() = %q parsed to host %q port %q", got, u.Hostname(), u.Port())
			}
		})
	}
}

// TestKnownHostsAddRemove: the add/remove pair is what lets `shed server add`
// pin a host key for the bootstrap and then take back exactly that line if the
// add fails.
func TestKnownHostsAddRemove(t *testing.T) {
	lines := func(t *testing.T) []string {
		t.Helper()
		data, err := os.ReadFile(GetKnownHostsPath())
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatal(err)
		}
		var out []string
		for _, ln := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(ln) != "" {
				out = append(out, ln)
			}
		}
		return out
	}
	const keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

	t.Run("removing an entry that was never added is a no-op", func(t *testing.T) {
		withHomeShed(t)
		if err := RemoveKnownHost("nope.test", 2222, keyA); err != nil {
			t.Errorf("removing from a missing known_hosts: %v", err)
		}
		if err := AddKnownHost("a.test", 2222, keyA); err != nil {
			t.Fatal(err)
		}
		if err := RemoveKnownHost("a.test", 2222, keyB); err != nil {
			t.Errorf("removing a key that is not there: %v", err)
		}
		if got := lines(t); len(got) != 1 {
			t.Errorf("known_hosts = %v, want the one line left alone", got)
		}
	})

	t.Run("removes only the matching line", func(t *testing.T) {
		withHomeShed(t)
		for _, add := range []struct {
			host string
			port int
			key  string
		}{
			{"a.test", 2222, keyA},
			{"b.test", 2222, keyB},
			{"a.test", 2022, keyB}, // same host, different port: a different line
			{"ssh.test", 22, keyA}, // port 22 uses the unbracketed form
		} {
			if err := AddKnownHost(add.host, add.port, add.key); err != nil {
				t.Fatal(err)
			}
		}
		if err := RemoveKnownHost("a.test", 2222, keyA); err != nil {
			t.Fatal(err)
		}
		got := lines(t)
		want := []string{
			"[b.test]:2222 " + keyB,
			"[a.test]:2022 " + keyB,
			"ssh.test " + keyA,
		}
		if len(got) != len(want) {
			t.Fatalf("known_hosts = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("line %d = %q, want %q", i, got[i], want[i])
			}
		}

		// The port-22 form round-trips too.
		if err := RemoveKnownHost("ssh.test", 22, keyA); err != nil {
			t.Fatal(err)
		}
		if got := lines(t); len(got) != 2 {
			t.Errorf("known_hosts = %v, want the port-22 line removed", got)
		}
	})

	t.Run("removes one occurrence of a duplicated line", func(t *testing.T) {
		withHomeShed(t)
		for i := 0; i < 2; i++ {
			if err := AddKnownHost("a.test", 2222, keyA); err != nil {
				t.Fatal(err)
			}
		}
		if err := RemoveKnownHost("a.test", 2222, keyA); err != nil {
			t.Fatal(err)
		}
		if got := lines(t); len(got) != 1 {
			t.Errorf("known_hosts = %v, want exactly one line removed", got)
		}
	})
}
