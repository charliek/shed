package rc

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The golden fixture is byte-identical to shed-remote-agent's
// packages/shared/src/schemas/rcSessionDto.golden.json and cmd/shed's copy. Both repos
// assert it decodes in their respective layer (Zod there, encoding/json here) — the
// single guard that the shed-ext-rc stdout contract stays in lockstep across tools.
// The `capabilities` block was added deliberately here (list envelope + DTO) alongside
// docs/reference/rc-session-convention.md; keep the three copies and the convention doc
// in sync when this shape changes.
func TestGoldenFixtureDecodes(t *testing.T) {
	data, err := os.ReadFile("testdata/rcSessionDto.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp ListResponse
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields() // a stray/renamed field would fail the contract
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("golden fixture failed to decode into the Go DTO: %v", err)
	}
	if len(resp.RCSessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(resp.RCSessions))
	}

	full := resp.RCSessions[0]
	if full.Kind != KindClaudeRC || full.State != StateReady || !full.Managed ||
		full.ID == "" || !strings.Contains(full.URL, "session_") || full.TargetLabel == "" {
		t.Fatalf("fully-populated session mismatch: %+v", full)
	}

	minimal := resp.RCSessions[1]
	if minimal.Kind != KindClaudeBroker || minimal.Managed ||
		minimal.DisplayName != "" || minimal.Workdir != "" || minimal.URL != "" || minimal.ID != "" {
		t.Fatalf("minimal session should have optionals omitted: %+v", minimal)
	}

	caps := resp.Capabilities
	if caps == nil {
		t.Fatal("list envelope must carry the capabilities block")
	}
	if caps.RCVersion != CapabilityVersion {
		t.Errorf("rc_version = %d, want %d", caps.RCVersion, CapabilityVersion)
	}
	if len(caps.Kinds) != len(allKinds) {
		t.Errorf("capabilities.kinds = %d, want %d", len(caps.Kinds), len(allKinds))
	}
	if info, ok := caps.Agents["claude"]; !ok || !info.Installed || info.Version == "" {
		t.Errorf("claude agent info wrong: %+v", caps.Agents["claude"])
	}
	if info, ok := caps.Agents["cursor"]; !ok || info.Installed || info.Version != "" {
		t.Errorf("uninstalled cursor should have no version: %+v", info)
	}
	if kf, ok := caps.KindFeatures[KindCodex]; !ok || !kf.PostInput || kf.Approvals != "tui" {
		t.Errorf("codex kind_features wrong: %+v", kf)
	}
}

// A minimal DTO re-marshals with its optional fields OMITTED (absent, not null) —
// the wire contract the Swift Codable + TS Zod consumers rely on.
func TestSessionMarshalOmitsEmptyOptionals(t *testing.T) {
	out, err := json.Marshal(Session{
		Slug: "x", TmuxSession: "rc-x", Kind: KindShell, State: StateStarting, Managed: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, omitted := range []string{"display_name", "workdir", "url", "id", "created_by", "created_at", "target_label", "null"} {
		if strings.Contains(s, omitted) {
			t.Errorf("expected %q to be omitted, got %s", omitted, s)
		}
	}
	// managed is always present (even when false).
	if !strings.Contains(s, `"managed":false`) {
		t.Errorf("managed must always be present: %s", s)
	}
}
