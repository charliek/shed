package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
)

// The CLI's side of delivery: the DELIVERY column of `shed list` (only when
// some shed has a delivery to speak of), the record `shed provision
// --status` prints, and the start warnings.

func deliveredRecord() *delivery.Record {
	r := &delivery.Record{
		Steps: &delivery.Steps{
			Stage1: delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonWriteFailed}, Clone: delivery.Step{State: delivery.StepDone},
			Stage2: delivery.Step{State: delivery.StepOwed}, Install: delivery.Step{State: delivery.StepOwed},
		},
		Last: &delivery.Last{At: "2026-11-23T10:00:00Z", Reason: delivery.ReasonCreate, Outcome: delivery.OutcomeAccepted,
			Failure:  &delivery.Failure{Reason: delivery.ReasonWriteFailed, Item: "github", Result: delivery.ResultIsDirectory},
			Source:   &delivery.RecordSource{Kind: delivery.SourceKindMachine, MachineID: "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b", MachineName: "ada-laptop"},
			Problems: []delivery.Problem{{Provider: "api-keys", Code: "missing_input", Message: "no providers.toml"}}},
		Items: []delivery.RecordItem{
			{Name: "ssh-key", Provider: "ssh-key", Stage: 1, Kind: delivery.KindFile, Base: delivery.BaseHome, Target: ".ssh/id_ed25519", Fingerprint: "k1", WrittenAt: "2026-11-23T10:00:02Z", Result: delivery.ResultOK},
			{Name: "github", Provider: "github", Stage: 1, Kind: delivery.KindFile, Base: delivery.BaseHome, Target: ".config/gh/hosts.yml", Fingerprint: "g1", ExpiresAt: "2026-11-24T10:00:00Z", Result: delivery.ResultIsDirectory},
			{Name: "api-key", Provider: "api-keys", Stage: 1, Kind: delivery.KindEnv, Target: "EXAMPLE_API_KEY", Fingerprint: "a1", ExpiresAt: "2026-11-23T11:00:00Z", WrittenAt: "2026-11-23T10:00:02Z", Result: delivery.ResultOK},
			{Name: "project-env", Provider: "files", Stage: 2, Kind: delivery.KindFile, Base: delivery.BaseProject, Target: ".envrc", Fingerprint: "p1", Result: delivery.ResultNotWritten},
		},
	}
	r.State = delivery.DeriveState(r)
	return r
}

func TestListDeliveryColumn(t *testing.T) {
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	plain := []listedShed{
		{shed: config.Shed{Name: "a", Backend: "firecracker", Status: config.StatusStopped, CreatedAt: at, Delivery: config.NewShedDelivery(nil, at)}, server: "s"},
		{shed: config.Shed{Name: "b", Backend: "firecracker", Status: config.StatusStopped, CreatedAt: at,
			Delivery: &config.ShedDelivery{Record: delivery.Record{State: delivery.StateNotApplicable}}}, server: "s"},
	}
	var buf bytes.Buffer
	renderShedTable(&buf, plain, false)
	if strings.Contains(buf.String(), "DELIVERY") {
		t.Fatalf("a listing with no delivery to speak of grew the column:\n%s", buf.String())
	}

	withOne := append(plain, listedShed{shed: config.Shed{Name: "c", Backend: "firecracker", Status: config.StatusStopped, CreatedAt: at,
		Delivery: config.NewShedDelivery(deliveredRecord(), at)}, server: "s"})
	buf.Reset()
	renderShedTable(&buf, withOne, false)
	out := buf.String()
	t.Logf("shed list:\n%s", out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.Contains(lines[0], "DELIVERY") || !strings.Contains(lines[1], " - ") || !strings.Contains(lines[2], "not_applicable") || !strings.Contains(lines[3], "partial") {
		t.Fatalf("shed list:\n%s", out)
	}
	buf.Reset()
	renderShedTableVerbose(&buf, withOne, false)
	if !strings.Contains(buf.String(), "DELIVERY") {
		t.Fatalf("shed list -v:\n%s", buf.String())
	}
	buf.Reset()
	renderShedDetails(&buf, withOne[2:], false)
	if !strings.Contains(buf.String(), "Delivery:       partial (owed: stage2, install)") {
		t.Fatalf("shed list -vv:\n%s", buf.String())
	}
}

// TestProvisionStatusFromTheService: a delivery the account service answered
// is printed as from it.
func TestProvisionStatusFromTheService(t *testing.T) {
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	rec := deliveredRecord()
	rec.Last.Source = delivery.ServiceSource()
	var buf bytes.Buffer
	renderDeliveryStatus(&buf, &config.Shed{Name: "widgets", LandingDir: "/home/shed/widgets", Delivery: config.NewShedDelivery(rec, at)})
	if !strings.Contains(buf.String(), "Last:      2026-11-23T10:00:00Z (create) accepted from the account service\n") {
		t.Fatalf("--status:\n%s", buf.String())
	}
}

func TestProvisionStatus(t *testing.T) {
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	shed := &config.Shed{Name: "widgets", LandingDir: "/home/shed/widgets", Delivery: config.NewShedDelivery(deliveredRecord(), at)}
	var buf bytes.Buffer
	renderDeliveryStatus(&buf, shed)
	out := buf.String()
	t.Logf("shed provision widgets --status:\n%s", out)
	for _, want := range []string{
		"Delivery:  partial",
		"Last:      2026-11-23T10:00:00Z (create) accepted from ada-laptop",
		"Failure:   write_failed: github is_directory",
		"  stage1   failed (write_failed)",
		"  stage2   owed",
		"~/.ssh/id_ed25519",
		"$EXAMPLE_API_KEY",
		"/home/shed/widgets/.envrc",
		"api-keys: missing_input: no providers.toml",
		"Expired:   api-key",
		"Owed:      stage2, install. Run: shed provision widgets",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--status lacks %q", want)
		}
	}
}

func TestStartWarnsForEachExpiredItem(t *testing.T) {
	at := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	shed := &config.Shed{Name: "widgets", Delivery: config.NewShedDelivery(deliveredRecord(), at)}
	var buf bytes.Buffer
	warnExpiredItems(&buf, shed)
	t.Logf("shed start warnings:\n%s", buf.String())
	// The github item failed to write, so only the written api-key is in
	// the shed to expire.
	want := "Warning: delivered item api-key has expired. Run: shed provision widgets\n"
	if buf.String() != want {
		t.Fatalf("warnings = %q, want %q", buf.String(), want)
	}
	buf.Reset()
	warnExpiredItems(&buf, &config.Shed{Name: "old"})
	if buf.Len() != 0 {
		t.Fatalf("a shed with no delivery warned %q", buf.String())
	}
}

// The provision stream: the POST carries no body and asks for SSE, the
// warning arrives as a progress warning, and the complete event's shed is
// returned.
func TestProvisionShedWithProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sheds/widgets/provision" || r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: progress\ndata: {\"message\":\"Credentials were not delivered (source_offline). Run: shed provision widgets\",\"warning\":true}\n\n")
		fmt.Fprint(w, "event: complete\ndata: {\"name\":\"widgets\",\"status\":\"running\",\"delivery\":{\"state\":\"not_delivered\",\"owed\":[\"stage2\",\"install\"]}}\n\n")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	var warnings []string
	shed, err := NewAPIClient(u.Hostname(), port, 5*time.Second).ProvisionShedWithProgress("widgets", func(ev backend.ProgressEvent) {
		if ev.Warning {
			warnings = append(warnings, ev.Message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "Credentials were not delivered (source_offline)") {
		t.Fatalf("warnings = %q", warnings)
	}
	if shed.Delivery == nil || shed.Delivery.State != delivery.StateNotDelivered || len(shed.Delivery.Owed) != 2 {
		t.Fatalf("shed = %+v", shed.Delivery)
	}
}
