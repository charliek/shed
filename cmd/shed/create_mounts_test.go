package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/config"
)

func TestParseMountsFlag(t *testing.T) {
	tests := []struct {
		value   string
		changed bool
		want    *[]string // nil: field not sent
		wantErr string
	}{
		{"", false, nil, ""},
		{"all", true, nil, ""},
		{"none", true, &[]string{}, ""},
		{" none ", true, &[]string{}, ""},
		{"claude", true, &[]string{"claude"}, ""},
		{"claude, gh", true, &[]string{"claude", "gh"}, ""},
		{"", true, nil, "must not be empty"},
		{"claude,,gh", true, nil, "empty name"},
		{"claude,none", true, nil, "cannot be combined"},
		{"all,gh", true, nil, "cannot be combined"},
		{"gh,gh", true, nil, "twice"},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseMountsFlag(tt.value, tt.changed)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			if got != nil && strings.Join(*got, ",") != strings.Join(*tt.want, ",") {
				t.Errorf("got %v, want %v", *got, *tt.want)
			}
			if got != nil && *got == nil {
				t.Error("none must be an empty list, not nil (it marshals to [] and means no mounts)")
			}
		})
	}
	none, _ := parseMountsFlag("none", true)
	b, _ := json.Marshal(config.CreateShedRequest{Name: "x", Mounts: none})
	if !strings.Contains(string(b), `"mounts":[]`) {
		t.Errorf("--mounts none must send mounts: [], got %s", b)
	}
	all, _ := parseMountsFlag("all", true)
	b, _ = json.Marshal(config.CreateShedRequest{Name: "x", Mounts: all})
	if strings.Contains(string(b), `"mounts"`) {
		t.Errorf("--mounts all must omit the field, got %s", b)
	}
}

func TestFeatureTokenMatchesServer(t *testing.T) {
	if config.FeatureMountSelection != api.FeatureMountSelection {
		t.Fatalf("CLI checks %q, server advertises %q", config.FeatureMountSelection, api.FeatureMountSelection)
	}
}

func TestFormatShedMounts(t *testing.T) {
	tests := []struct {
		sel  *config.MountSelection
		want string
	}{
		{nil, "-"},
		{&config.MountSelection{Mode: config.MountModeAll}, "all"},
		{&config.MountSelection{Mode: config.MountModeSelected}, "none"},
		{&config.MountSelection{Mode: config.MountModeSelected, Names: []string{}}, "none"},
		{&config.MountSelection{Mode: config.MountModeSelected, Names: []string{"claude", "gh"}}, "claude,gh"},
		{&config.MountSelection{Mode: "bogus"}, "none"},
	}
	for _, tt := range tests {
		if got := formatShedMounts(tt.sel); got != tt.want {
			t.Errorf("formatShedMounts(%+v) = %q, want %q", tt.sel, got, tt.want)
		}
	}
}

// fakeCreateServer answers /api/info with features and records the body of
// any create request (answering it with a 500, which ends runCreate).
type fakeCreateServer struct {
	*httptest.Server
	mu      sync.Mutex
	creates []string
}

func newFakeCreateServer(t *testing.T, features []string) *fakeCreateServer {
	t.Helper()
	f := &fakeCreateServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/info":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config.ServerInfo{Name: "fake", Backend: config.BackendFirecracker, Features: features})
		case r.Method == http.MethodPost && r.URL.Path == "/api/sheds":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.creates = append(f.creates, string(body))
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"BACKEND_ERROR","message":"fake stops here"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// runCreateAgainst runs `shed -s fake create box --mounts <mounts>` against
// the fake server.
func runCreateAgainst(t *testing.T, f *fakeCreateServer, mounts string) error {
	t.Helper()
	origConfig, origServer := clientConfig, serverFlag
	t.Cleanup(func() {
		clientConfig, serverFlag = origConfig, origServer
		createMounts = ""
		if fl := createCmd.Flags().Lookup("mounts"); fl != nil {
			fl.Changed = false
		}
	})
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
		"fake": {Host: "127.0.0.1", APIURL: f.URL},
	}}
	serverFlag = "fake"
	if err := createCmd.Flags().Set("mounts", mounts); err != nil {
		t.Fatal(err)
	}
	return runCreate(createCmd, []string{"box"})
}

func TestCreateMountsRefusedWithoutFeature(t *testing.T) {
	f := newFakeCreateServer(t, []string{"overview", "http_exec"})
	err := runCreateAgainst(t, f, "none")
	if err == nil || !strings.Contains(err.Error(), "--mounts") || !strings.Contains(err.Error(), "mount_selection") || !strings.Contains(err.Error(), "fake") {
		t.Fatalf("err = %v, want a refusal naming the server, the flag and the feature", err)
	}
	if len(f.creates) != 0 {
		t.Errorf("the create was sent anyway: %v", f.creates)
	}
}

func TestCreateMountsSentWithFeature(t *testing.T) {
	f := newFakeCreateServer(t, []string{"overview", "mount_selection"})
	err := runCreateAgainst(t, f, "none")
	if err == nil || !strings.Contains(err.Error(), "fake stops here") {
		t.Fatalf("err = %v, want the fake's create error", err)
	}
	if len(f.creates) != 1 || !strings.Contains(f.creates[0], `"mounts":[]`) {
		t.Fatalf("creates = %v, want one with mounts: []", f.creates)
	}
}

// --mounts all sends no mounts field, so it works against any server.
func TestCreateMountsAllNeedsNoFeature(t *testing.T) {
	f := newFakeCreateServer(t, nil)
	_ = runCreateAgainst(t, f, "all")
	if len(f.creates) != 1 || strings.Contains(f.creates[0], `"mounts"`) {
		t.Fatalf("creates = %v, want one without a mounts field", f.creates)
	}
}

func TestListRendersOwnerAndMounts(t *testing.T) {
	origConfig := clientConfig
	t.Cleanup(func() { clientConfig = origConfig })
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}

	created := time.Date(2026, 11, 11, 1, 2, 3, 0, time.UTC)
	sheds := []listedShed{
		{server: "srv", shed: config.Shed{Name: "admin-box", Status: config.StatusStopped, CreatedAt: created,
			Mounts: &config.MountSelection{Mode: config.MountModeAll}}},
		{server: "srv", shed: config.Shed{Name: "nocturn-t1", Status: config.StatusStopped, CreatedAt: created,
			Owner: "nocturn", Mounts: &config.MountSelection{Mode: config.MountModeSelected}}},
		{server: "srv", shed: config.Shed{Name: "picked", Status: config.StatusStopped, CreatedAt: created,
			Mounts: &config.MountSelection{Mode: config.MountModeSelected, Names: []string{"claude", "gh"}}}},
		{server: "srv", shed: config.Shed{Name: "old-server", Status: config.StatusStopped, CreatedAt: created}},
	}

	var buf bytes.Buffer
	renderShedTableVerbose(&buf, sheds, false)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	header := strings.Fields(lines[0])
	col := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		t.Fatalf("no %s column in %q", name, lines[0])
		return -1
	}
	ownerCol, mountsCol := col("OWNER"), col("MOUNTS")
	want := map[string][2]string{
		"admin-box":  {"-", "all"},
		"nocturn-t1": {"nocturn", "none"},
		"picked":     {"-", "claude,gh"},
		"old-server": {"-", "-"},
	}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		w, ok := want[f[0]]
		if !ok {
			t.Fatalf("unexpected row %q", l)
		}
		if f[ownerCol] != w[0] || f[mountsCol] != w[1] {
			t.Errorf("%s: owner %q mounts %q, want %q %q", f[0], f[ownerCol], f[mountsCol], w[0], w[1])
		}
	}

	buf.Reset()
	renderShedTableVerbose(&buf, sheds, true)
	if h := strings.Fields(strings.SplitN(buf.String(), "\n", 2)[0]); len(h) != len(header)+1 || h[1] != "SERVER" {
		t.Errorf("--all header = %v", h)
	}

	buf.Reset()
	renderShedDetails(&buf, sheds[1:2], false)
	out := buf.String()
	for _, want := range []string{"Owner:          nocturn\n", "Server Mounts:  none\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("-vv output lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	renderShedDetails(&buf, sheds[0:1], false)
	if !strings.Contains(buf.String(), "Owner:          -\n") || !strings.Contains(buf.String(), "Server Mounts:  all\n") {
		t.Errorf("-vv admin shed:\n%s", buf.String())
	}
}
