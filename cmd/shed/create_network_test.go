package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/config"
)

func TestParseNetworkFlag(t *testing.T) {
	for _, tt := range []struct {
		value   string
		changed bool
		want    string
		wantErr bool
	}{
		{"", false, "", false},
		{"open", true, "open", false},
		{" isolated ", true, "isolated", false},
		{"", true, "", true},
		{"none", true, "", true},
		{"Isolated", true, "", true},
	} {
		got, err := parseNetworkFlag(tt.value, tt.changed)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseNetworkFlag(%q, %v) = %q, %v", tt.value, tt.changed, got, err)
		}
	}
	if config.FeatureNetworkProfiles != api.FeatureNetworkProfiles {
		t.Error("the CLI's feature token differs from the server's")
	}
}

// runCreateNetwork runs `shed -s fake create box --network <network>`.
func runCreateNetwork(t *testing.T, f *fakeCreateServer, network string) error {
	t.Helper()
	origConfig, origServer := clientConfig, serverFlag
	t.Cleanup(func() {
		clientConfig, serverFlag = origConfig, origServer
		createNetwork = ""
		if fl := createCmd.Flags().Lookup("network"); fl != nil {
			fl.Changed = false
		}
	})
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
		"fake": {Host: "127.0.0.1", APIURL: f.URL},
	}}
	serverFlag = "fake"
	if err := createCmd.Flags().Set("network", network); err != nil {
		t.Fatal(err)
	}
	return runCreate(createCmd, []string{"box"})
}

func TestCreateNetworkIsolatedRefusedWithoutFeature(t *testing.T) {
	f := newFakeCreateServer(t, []string{"overview", "mount_selection"})
	err := runCreateNetwork(t, f, "isolated")
	if err == nil || !strings.Contains(err.Error(), "--network") || !strings.Contains(err.Error(), "network_profiles") || !strings.Contains(err.Error(), "fake") {
		t.Fatalf("err = %v, want a refusal naming the server, the flag and the feature", err)
	}
	if len(f.creates) != 0 {
		t.Errorf("the create was sent anyway: %v", f.creates)
	}
}

func TestCreateNetworkSentWithFeature(t *testing.T) {
	f := newFakeCreateServer(t, []string{"overview", "network_profiles"})
	_ = runCreateNetwork(t, f, "isolated")
	if len(f.creates) != 1 || !strings.Contains(f.creates[0], `"network":"isolated"`) {
		t.Fatalf("creates = %v, want one with network: isolated", f.creates)
	}
}

// --network open against a server without profiles sends nothing: such a
// server creates an open shed anyway.
func TestCreateNetworkOpenWithoutFeature(t *testing.T) {
	f := newFakeCreateServer(t, nil)
	_ = runCreateNetwork(t, f, "open")
	if len(f.creates) != 1 || strings.Contains(f.creates[0], `"network"`) {
		t.Fatalf("creates = %v, want one without a network field", f.creates)
	}
}

func TestListRendersNetwork(t *testing.T) {
	created := time.Date(2026, 11, 11, 1, 2, 3, 0, time.UTC)
	sheds := []listedShed{
		{server: "srv", shed: config.Shed{Name: "iso", Status: config.StatusStopped, CreatedAt: created, NetworkProfile: "isolated"}},
		{server: "srv", shed: config.Shed{Name: "open", Status: config.StatusStopped, CreatedAt: created, NetworkProfile: "open"}},
		{server: "srv", shed: config.Shed{Name: "old-server", Status: config.StatusStopped, CreatedAt: created}},
	}
	var buf bytes.Buffer
	renderShedTableVerbose(&buf, sheds, false)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	header := strings.Fields(lines[0])
	col := -1
	for i, h := range header {
		if h == "NETWORK" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("no NETWORK column in %q", lines[0])
	}
	want := map[string]string{"iso": "isolated", "open": "open", "old-server": "-"}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if f[col] != want[f[0]] {
			t.Errorf("%s: network %q, want %q", f[0], f[col], want[f[0]])
		}
	}

	buf.Reset()
	renderShedDetails(&buf, sheds[:1], false)
	if !strings.Contains(buf.String(), "Network:        isolated") {
		t.Errorf("-vv lacks the network:\n%s", buf.String())
	}
}
