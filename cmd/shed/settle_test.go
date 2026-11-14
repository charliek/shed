package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// settleServer answers GET /api/sheds/a1 with the next status in seq (the
// last one repeats) and POST .../start with startStatus.
type settleServer struct {
	*httptest.Server
	mu          sync.Mutex
	seq         []config.Shed
	startStatus string
	starts      int
}

func newSettleServer(t *testing.T, startStatus string, seq ...config.Shed) (*settleServer, *APIClient) {
	t.Helper()
	f := &settleServer{seq: seq, startStatus: startStatus}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sheds/a1":
			s := f.seq[0]
			if len(f.seq) > 1 {
				f.seq = f.seq[1:]
			}
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sheds/a1/start":
			f.starts++
			_ = json.NewEncoder(w).Encode(config.Shed{Name: "a1", Status: f.startStatus})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	u, _ := url.Parse(f.URL)
	port, _ := strconv.Atoi(u.Port())
	return f, NewAPIClient(u.Hostname(), port, 5*time.Second)
}

func shortSettle(t *testing.T, timeout time.Duration) {
	t.Helper()
	oldT, oldI := settleTimeout, settlePollInterval
	settleTimeout, settlePollInterval = timeout, time.Millisecond
	t.Cleanup(func() { settleTimeout, settlePollInterval = oldT, oldI })
}

func sh(status, reason string) config.Shed {
	return config.Shed{Name: "a1", Backend: config.BackendRemote, Status: status, StatusReason: reason}
}

// local is a Firecracker shed in the given status.
func local(status string) config.Shed {
	return config.Shed{Name: "a1", Backend: config.BackendFirecracker, Status: status}
}

func TestWaitSettled(t *testing.T) {
	tests := []struct {
		name       string
		first      config.Shed
		seq        []config.Shed
		want       string
		wantErr    string
		wantOutput string
	}{
		{name: "settled is returned without a request", first: sh(config.StatusRunning, ""), seq: []config.Shed{sh(config.StatusError, "")}, want: config.StatusRunning},
		{name: "starting to running", first: sh(config.StatusStarting, ""), seq: []config.Shed{sh(config.StatusStarting, ""), sh(config.StatusRunning, "")}, want: config.StatusRunning, wantOutput: "Waiting for shed a1 (starting)..."},
		{name: "stopping to stopped", first: sh(config.StatusStopping, ""), seq: []config.Shed{sh(config.StatusStopped, "")}, want: config.StatusStopped, wantOutput: "(stopping)"},
		{name: "start refused", first: sh(config.StatusStarting, ""), seq: []config.Shed{sh(config.StatusStopped, config.StatusReasonQuota)}, want: config.StatusStopped},
		{name: "timeout", first: sh(config.StatusStarting, ""), seq: []config.Shed{sh(config.StatusStarting, "")}, wantErr: "still starting"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortSettle(t, 50*time.Millisecond)
			_, client := newSettleServer(t, "", tt.seq...)
			var out bytes.Buffer
			first := tt.first
			got, err := waitSettled(client, &first, &out)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got.Status != tt.want {
				t.Fatalf("got %+v, %v; want %s", got, err, tt.want)
			}
			if !strings.Contains(out.String(), tt.wantOutput) {
				t.Errorf("output %q lacks %q", out.String(), tt.wantOutput)
			}
		})
	}
}

func TestEnsureRunningShedSettles(t *testing.T) {
	tests := []struct {
		name        string
		startStatus string
		seq         []config.Shed
		wantStarts  int
		wantErr     string
	}{
		{name: "running", seq: []config.Shed{sh(config.StatusRunning, "")}},
		{name: "starting waits, no start", seq: []config.Shed{sh(config.StatusStarting, ""), sh(config.StatusRunning, "")}},
		{name: "stopping waits then starts", startStatus: config.StatusStarting,
			seq: []config.Shed{sh(config.StatusStopping, ""), sh(config.StatusStopped, ""), sh(config.StatusRunning, "")}, wantStarts: 1},
		{name: "synchronous start (firecracker, vz)", startStatus: config.StatusRunning, seq: []config.Shed{sh(config.StatusStopped, "")}, wantStarts: 1},
		{name: "start refused names the reason", startStatus: config.StatusStarting,
			seq: []config.Shed{sh(config.StatusStopped, ""), sh(config.StatusStopped, config.StatusReasonCapacity)}, wantStarts: 1, wantErr: "stopped (capacity)"},
		{name: "error shed is not started", seq: []config.Shed{sh(config.StatusError, config.StatusReasonHostKey)}, wantErr: "error (host_key)"},
		// Firecracker and VZ are unchanged: any status but running is
		// started at once, with no wait and no refusal.
		{name: "local starting shed is started, not waited on", startStatus: config.StatusRunning, seq: []config.Shed{local(config.StatusStarting)}, wantStarts: 1},
		{name: "local error shed is started", startStatus: config.StatusRunning, seq: []config.Shed{local(config.StatusError)}, wantStarts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortSettle(t, 2*time.Second)
			origJSON := jsonFlag
			jsonFlag = true // keep stdout quiet
			t.Cleanup(func() { jsonFlag = origJSON })
			f, client := newSettleServer(t, tt.startStatus, tt.seq...)
			got, err := ensureRunningShed(client, "a1")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || got.Status != config.StatusRunning {
				t.Fatalf("got %+v, %v; want running", got, err)
			}
			if f.starts != tt.wantStarts {
				t.Errorf("starts = %d, want %d", f.starts, tt.wantStarts)
			}
		})
	}
}

func TestRemoteCreateRefusal(t *testing.T) {
	names := []string{"gh"}
	none := []string{}
	tests := []struct {
		name     string
		localDir string
		addDirs  []string
		snapshot string
		egress   []string
		mounts   *[]string
		want     string
	}{
		{name: "nothing", want: ""},
		{name: "egress off and mounts none", egress: []string{"off"}, mounts: &none, want: ""},
		{name: "local dir", localDir: "/x", want: "--local-dir"},
		{name: "add dir", addDirs: []string{"/x"}, want: "--add-dir"},
		{name: "snapshot", snapshot: "s", want: "--from-snapshot"},
		{name: "egress", egress: []string{"github"}, want: "--egress"},
		{name: "mounts", mounts: &names, want: "--mounts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := remoteCreateRefusal(tt.localDir, tt.addDirs, tt.snapshot, tt.egress, tt.mounts)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.want+" ") {
				t.Fatalf("err = %v, want it to name %s", err, tt.want)
			}
		})
	}
}

func TestResolveCreateBackendRemote(t *testing.T) {
	info := &config.ServerInfo{Backend: config.BackendRemote}
	got, _, err := resolveCreateBackend(info, config.BackendRemote, 3, 0)
	if err != nil || got != config.BackendRemote {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, _, err := resolveCreateBackend(info, config.BackendFirecracker, 0, 0); err == nil {
		t.Fatal("firecracker against a remote server must be refused")
	}
}

func TestListVerboseRemote(t *testing.T) {
	var buf bytes.Buffer
	renderShedTableVerbose(&buf, []listedShed{{shed: config.Shed{Name: "a1", ID: "abcdefghijklmnopqrstuvwxyz", Status: config.StatusStopped, StatusReason: config.StatusReasonRunLimit}}}, false)
	out := buf.String()
	for _, want := range []string{"\tID", "ID", "abcdefghijklmnopqrstuvwxyz", "stopped (run_limit)"} {
		if !strings.Contains(out, strings.TrimPrefix(want, "\t")) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	renderShedTableVerbose(&buf, []listedShed{{shed: config.Shed{Name: "a1", Status: config.StatusStopped}}}, false)
	if strings.Contains(buf.String(), " ID") {
		t.Errorf("a shed without an ID must not add the column:\n%s", buf.String())
	}
}
