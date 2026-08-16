package rc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testdata/rcSessionDto.golden.json is the CANONICAL copy of the shed-ext-rc stdout
// contract. Byte-identical copies live at:
//
//   - cmd/shed/testdata/rcSessionDto.golden.json (the CLI's decode guard),
//   - crates/fixtures/rcSessionDto.golden.json (the Rust core's include_str! copy —
//     crates-local on purpose: `make -C desktop core-linux` mounts only crates/),
//   - desktop/Tests/ShedKitTests/Fixtures/rcSessionDto.golden.json (the Swift guard),
//   - shed-remote-agent's packages/shared/src/schemas/rcSessionDto.golden.json
//     (out of repo; it also owns the convention doc, docs/reference/
//     rc-session-convention.md — that doc is NOT in this repo).
//
// golden_parity_test.go byte-compares the in-repo copies, so an edit here that is not
// mirrored fails the build rather than drifting silently. Each consumer asserts the
// fixture decodes in its own layer (encoding/json here, serde in Rust, Codable in
// Swift, Zod in shed-remote-agent) — together they are the guard that the contract
// stays in lockstep across tools.
func TestGoldenFixtureDecodes(t *testing.T) {
	var resp ListResponse
	decodeGoldenStrict(t, "rcSessionDto.golden.json", &resp)
	if len(resp.RCSessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(resp.RCSessions))
	}

	full := resp.RCSessions[0]
	if full.Kind != KindClaudeRC || full.State != StateReady || !full.Managed ||
		full.ID == "" || !strings.Contains(full.URL, "session_") || full.TargetLabel == "" {
		t.Fatalf("fully-populated session mismatch: %+v", full)
	}
	// The additive Phase-C activity fields decode inside the rc block.
	if full.Activity != ActivityWorking || full.ActivityAt == "" || full.LastMessage == "" {
		t.Fatalf("activity fields not decoded: %+v", full)
	}

	minimal := resp.RCSessions[1]
	if minimal.Kind != KindClaudeBroker || minimal.Managed ||
		minimal.DisplayName != "" || minimal.Workdir != "" || minimal.URL != "" || minimal.ID != "" {
		t.Fatalf("minimal session should have optionals omitted: %+v", minimal)
	}
	// Absent activity fields decode to the zero value (not present on the wire).
	if minimal.Activity != "" || minimal.ActivityAt != "" || minimal.LastMessage != "" {
		t.Fatalf("minimal session should omit activity fields: %+v", minimal)
	}
	// lane (contract v2) is present on EVERY row — the managed one and the
	// legacy/unmanaged one alike — so a client never distinguishes absent from "tui".
	for i, s := range resp.RCSessions {
		if s.Lane != LaneTUI {
			t.Errorf("session[%d] lane = %q, want %q (every kind is a TUI lane this phase)", i, s.Lane, LaneTUI)
		}
	}
	// pending_approvals is hub-layer-only in this phase — it must be ABSENT from
	// the one-shot `list` fixture, not merely empty (an `"pending_approvals": []`
	// fixture would be a wire-visible field the omitempty tag promises not to emit,
	// so nil-check rather than len-check).
	if full.PendingApprovals != nil || minimal.PendingApprovals != nil {
		t.Errorf("no producer emits pending_approvals in this phase: %+v / %+v",
			full.PendingApprovals, minimal.PendingApprovals)
	}

	caps := resp.Capabilities
	if caps == nil {
		t.Fatal("list envelope must carry the capabilities block")
	}
	if caps.RCVersion != CapabilityVersion {
		t.Errorf("rc_version = %d, want %d", caps.RCVersion, CapabilityVersion)
	}
	// The golden's feature list must be EXACTLY what this binary advertises — the
	// goldens must never advertise a capability ahead of (or behind) the code, which
	// is the whole reason the version bump and the `contract-v2` token land in the
	// same commit as the routes they describe.
	if !reflect.DeepEqual(caps.Features, capabilityFeatures) {
		t.Errorf("golden features = %v, want %v", caps.Features, capabilityFeatures)
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
	// The golden's kind_features rows must be exactly what kindFeatures() produces.
	// Chained with capabilities_test.go's TestKindFeatures (which pins the same rows
	// against literal values), this pins the fixture to the normative matrix by value
	// without restating it here; strict decoding above pins the JSON key names.
	live := kindFeatures()
	if len(caps.KindFeatures) != len(live) {
		t.Errorf("golden kind_features has %d rows, kindFeatures() produces %d", len(caps.KindFeatures), len(live))
	}
	for kind, want := range live {
		got, ok := caps.KindFeatures[kind]
		if !ok {
			t.Errorf("golden kind_features missing %q", kind)
			continue
		}
		if got != want {
			t.Errorf("golden kind_features[%q] = %+v, want %+v", kind, got, want)
		}
	}
}

// A minimal DTO re-marshals with its optional fields OMITTED (absent, not null) —
// the wire contract the Swift Codable + TS Zod consumers rely on. `managed` and (since
// contract v2) `lane` are the always-present exceptions.
func TestSessionMarshalOmitsEmptyOptionals(t *testing.T) {
	out, err := json.Marshal(Session{
		Slug: "x", TmuxSession: "rc-x", Kind: KindShell, State: StateStarting,
		Managed: false, Lane: LaneTUI,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, omitted := range []string{"display_name", "workdir", "url", "id", "created_by", "created_at", "target_label", "activity", "activity_at", "last_message", "pending_approvals", "null"} {
		if strings.Contains(s, omitted) {
			t.Errorf("expected %q to be omitted, got %s", omitted, s)
		}
	}
	// managed is always present (even when false).
	if !strings.Contains(s, `"managed":false`) {
		t.Errorf("managed must always be present: %s", s)
	}
	// lane is always present too — no omitempty, so a client can read it
	// unconditionally.
	if !strings.Contains(s, `"lane":"tui"`) {
		t.Errorf("lane must always be present: %s", s)
	}
}

// TestFeedMessageGoldenDecodes pins the feed's wire shape — the text/tool_use rows and
// the approval_request pair (a pending row and its resolution, the SAME approval id
// appended twice, which is the id-keyed last-write-wins folding rule clients
// implement). Strict-decoded, so a renamed/stray field fails the contract.
func TestFeedMessageGoldenDecodes(t *testing.T) {
	var resp hubMessagesResponse
	decodeGoldenStrict(t, "feedMessage.golden.json", &resp)
	if resp.Truncated {
		t.Error("the golden page is contiguous; truncated must be false")
	}
	if len(resp.Messages) != 4 {
		t.Fatalf("want 4 messages, got %d", len(resp.Messages))
	}

	// The pending row and its resolution share ONE id — the folding key, so it is a
	// single constant here rather than two literals that could drift apart.
	const approvalID = "call_01HQ8Z3K.tool:2"

	// want pins role/type/approval only; seq and ts are asserted structurally below.
	cases := []struct {
		name string
		got  feedMessage
		want feedMessage
	}{
		{"text", resp.Messages[0], feedMessage{Role: feedRoleUser, Type: feedTypeText}},
		{"tool_use", resp.Messages[1], feedMessage{Role: feedRoleTool, Type: feedTypeToolUse}},
		{"approval_pending", resp.Messages[2], feedMessage{
			Role: feedRoleTool, Type: feedTypeApprovalRequest,
			Approval: &FeedApproval{
				ID:        approvalID,
				Status:    approvalStatusPending,
				Decisions: []string{approvalDecisionAllow, approvalDecisionAllowAlways, approvalDecisionDeny},
			},
		}},
		{"approval_resolved", resp.Messages[3], feedMessage{
			Role: feedRoleTool, Type: feedTypeApprovalRequest,
			Approval: &FeedApproval{
				ID:       approvalID,
				Status:   approvalStatusResolved,
				Decision: approvalDecisionAllow,
			},
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got.Seq != uint64(i+1) {
				t.Errorf("seq = %d, want %d (monotonic from 1)", c.got.Seq, i+1)
			}
			if c.got.TS == "" {
				t.Error("ts must be present on every row")
			}
			if c.got.Role != c.want.Role || c.got.Type != c.want.Type {
				t.Errorf("role/type = %s/%s, want %s/%s", c.got.Role, c.got.Type, c.want.Role, c.want.Type)
			}
			// Covers both directions: the exact approval block on an approval_request
			// row, and its absence (nil) on every other row.
			if !reflect.DeepEqual(c.got.Approval, c.want.Approval) {
				t.Errorf("approval = %+v, want %+v", c.got.Approval, c.want.Approval)
			}
		})
	}
}

// decodeGoldenStrict reads a testdata golden and decodes it into v with unknown fields
// REJECTED — a stray or renamed field on either side fails the contract rather than
// being silently dropped. The fixture must also be exactly ONE JSON value: a second
// value after the first is rejected (same drain-to-EOF decision the hub's own body
// decoder makes), so a golden can't quietly carry trailing junk the decoder ignores.
func decodeGoldenStrict(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s failed to decode into the Go types: %v", name, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		t.Fatalf("%s must hold exactly one JSON value; trailing content (decode err = %v)", name, err)
	}
}
