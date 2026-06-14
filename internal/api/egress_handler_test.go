package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
)

// egressFakeBackend reuses the system-test fakeBackend (all methods panic),
// overrides GetShed, and implements the egressController capability so the
// set/off handlers can be exercised.
type egressFakeBackend struct {
	fakeBackend
	shed          *config.Shed
	setCalledWith []string
	cleared       bool
}

func (f *egressFakeBackend) GetShed(_ context.Context, _ string) (*config.Shed, error) {
	return f.shed, nil
}

func (f *egressFakeBackend) SetShedEgress(_ context.Context, name string, profiles []string) (*config.Shed, error) {
	f.setCalledWith = profiles
	return &config.Shed{Name: name, EgressProfiles: profiles, EgressPort: 20002}, nil
}

func (f *egressFakeBackend) ClearShedEgress(_ context.Context, name string) (*config.Shed, error) {
	f.cleared = true
	return &config.Shed{Name: name}, nil
}

func TestHandleEgressShow(t *testing.T) {
	cfg := &config.ServerConfig{
		Name: "test-server",
		Egress: &config.EgressConfig{
			Enabled:  true,
			Profiles: map[string]config.EgressProfile{"base": {Allow: []string{"*.ubuntu.com"}}},
		},
	}
	be := &egressFakeBackend{shed: &config.Shed{Name: "web", EgressProfiles: []string{"base"}, EgressPort: 20001}}
	srv := NewServer(be, cfg, "", nil, nil)

	a, err := egress.OpenAuditLog(filepath.Join(t.TempDir(), "a.jsonl"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Record(egress.AuditRecord{Shed: "web", Host: "evil.com", Port: 443, Verdict: "deny", Reason: "default-deny"})
	a.Record(egress.AuditRecord{Shed: "other", Host: "x.com", Verdict: "deny"}) // different shed, must be filtered out
	srv.SetEgressAudit(a)

	r := httptest.NewRequest(http.MethodGet, "/api/egress/web", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got config.EgressStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Port != 20001 {
		t.Errorf("status enabled=%v port=%d, want true/20001", got.Enabled, got.Port)
	}
	if len(got.Profiles) != 1 || got.Profiles[0] != "base" {
		t.Errorf("profiles = %v, want [base]", got.Profiles)
	}
	if def, ok := got.Rules["base"]; !ok || len(def.Allow) != 1 || def.Allow[0] != "*.ubuntu.com" {
		t.Errorf("rules = %+v, want base allow *.ubuntu.com", got.Rules)
	}
	if len(got.Recent) != 1 || got.Recent[0].Host != "evil.com" {
		t.Errorf("recent = %+v, want only web's evil.com record", got.Recent)
	}
}

func TestHandleEgressShow_DisabledServer(t *testing.T) {
	be := &egressFakeBackend{shed: &config.Shed{Name: "web"}}
	srv := NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)

	r := httptest.NewRequest(http.MethodGet, "/api/egress/web", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got config.EgressStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Errorf("expected Enabled=false when server has no egress config")
	}
}

func TestHandleEgressSet(t *testing.T) {
	be := &egressFakeBackend{shed: &config.Shed{Name: "web"}}
	srv := NewServer(be, &config.ServerConfig{Name: "t", Egress: &config.EgressConfig{Enabled: true}}, "", nil, nil)

	r := httptest.NewRequest(http.MethodPost, "/api/egress/web", strings.NewReader(`{"profiles":["base","github"]}`))
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(be.setCalledWith) != 2 || be.setCalledWith[0] != "base" || be.setCalledWith[1] != "github" {
		t.Errorf("SetShedEgress called with %v, want [base github]", be.setCalledWith)
	}
}

func TestHandleEgressOff(t *testing.T) {
	be := &egressFakeBackend{shed: &config.Shed{Name: "web"}}
	srv := NewServer(be, &config.ServerConfig{Name: "t"}, "", nil, nil)

	r := httptest.NewRequest(http.MethodDelete, "/api/egress/web", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !be.cleared {
		t.Error("ClearShedEgress was not called")
	}
}

func TestHandleEgressStream(t *testing.T) {
	a, err := egress.OpenAuditLog(filepath.Join(t.TempDir(), "a.jsonl"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	be := &egressFakeBackend{shed: &config.Shed{Name: "web"}}
	srv := NewServer(be, &config.ServerConfig{Name: "t", Egress: &config.EgressConfig{Enabled: true}}, "", nil, nil)
	srv.SetEgressAudit(a)

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/egress/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Record repeatedly so the read can't lose a race with subscription setup.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				a.Record(egress.AuditRecord{Shed: "web", Host: "evil.com", Verdict: "deny"})
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "evil.com") {
			return // got the streamed decision
		}
	}
	t.Fatal("did not receive an egress decision over the SSE stream")
}
