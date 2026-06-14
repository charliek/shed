package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
)

// egressFakeBackend reuses the system-test fakeBackend (all methods panic) and
// overrides only GetShed, which is all handleEgressShow needs.
type egressFakeBackend struct {
	fakeBackend
	shed *config.Shed
}

func (f *egressFakeBackend) GetShed(_ context.Context, _ string) (*config.Shed, error) {
	return f.shed, nil
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
