package roostprovider

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// Remote.SetAgentHooks over the fake-ssh rig (fakessh_test.go): roost's real
// exec chain, a real NDJSON dialogue with an unsolicited event frame in front
// of every answer, and roost's own vendored reply as the bytes on the wire.
// `shed attach`'s start rung is its one caller, and cmd/shed's rig covers what
// that caller does with each outcome; this file is the op itself.

// TestSetAgentHooksThroughFakeSSH: the exact request line the raise puts on
// the wire, and roost's own reply decoded through the presence check.
func TestSetAgentHooksThroughFakeSSH(t *testing.T) {
	r := newRig(t, rigOpts{})

	got, err := r.remote.SetAgentHooks(e2eContext(t), r.target(), "shed-cli")
	if err != nil {
		t.Fatalf("SetAgentHooks: %v", err)
	}
	if !reflect.DeepEqual(got, vectorHooksOutcome) {
		t.Errorf("outcome:\n got %+v\nwant %+v", got, vectorHooksOutcome)
	}
	assertSeq(t, "request lines", r.requestLines(), []string{
		`{"id":"1","op":"session.set_agent_hooks","params":` +
			`{"agents":["claude","codex","cursor","grok","opencode"],"client":"shed-cli"}}`,
	})
}

// TestSetAgentHooksRefusesAMalformedReply is the presence check, end to end: a
// well-formed `ok:true` envelope whose outcome is missing a field is a
// MalformedReplyError naming the field — never an empty outcome, which a
// caller would print as "nothing to wire", a claim about the host the reply
// never made. Same shape Identify gives a missing `session_protocol`, and the
// same refusal to become a provider row.
func TestSetAgentHooksRefusesAMalformedReply(t *testing.T) {
	cases := []struct {
		name, reply, wantReason string
	}{
		{"an empty result", `{"id":"1","ok":true,"result":{}}`, "no wired"},
		{"a null result", `{"id":"1","ok":true,"result":null}`, "no wired"},
		{
			"a result missing one field",
			`{"id":"1","ok":true,"result":{"wired":["grok"],"refreshed":[],"removed":[],"errors":[]}}`,
			"no skipped",
		},
		{
			"a field present as null",
			`{"id":"1","ok":true,"result":{"wired":[],"refreshed":[],"removed":[],"skipped":[],"errors":null}}`,
			"no errors",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			r.replaceReply("sethooks", tc.reply)

			got, err := r.remote.SetAgentHooks(e2eContext(t), r.target(), "shed-cli")
			var malformed *MalformedReplyError
			if !errors.As(err, &malformed) {
				t.Fatalf("err = %v (%T), want a MalformedReplyError; outcome %+v", err, err, got)
			}
			if malformed.Op != opSessionSetAgentHooks || malformed.Reason != tc.wantReason {
				t.Errorf("MalformedReplyError = %+v, want op %q reason %q", malformed, opSessionSetAgentHooks, tc.wantReason)
			}
			if !reflect.DeepEqual(got, AgentHooksOutcome{}) {
				t.Errorf("a refused reply still produced an outcome: %+v", got)
			}
			if row, ok := RowForError(Token{Shed: "dev", Server: "my-server"}, err); ok {
				t.Errorf("malformed far-side output became a row: %+v", row)
			}
		})
	}

	// The control: a reply with every group present and empty is NOT malformed
	// — it is the real "nothing to wire" answer, and must decode as one.
	t.Run("every group present and empty is a real answer", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.replaceReply("sethooks",
			`{"id":"1","ok":true,"result":{"wired":[],"refreshed":[],"removed":[],"skipped":[],"errors":[]}}`)
		got, err := r.remote.SetAgentHooks(e2eContext(t), r.target(), "shed-cli")
		if err != nil {
			t.Fatalf("SetAgentHooks: %v", err)
		}
		if len(got.Wired)+len(got.Refreshed)+len(got.Removed)+len(got.Skipped)+len(got.Errors) != 0 {
			t.Errorf("outcome = %+v, want every group empty", got)
		}
	})
}

// TestSetAgentHooksSurfacesARefusal: an `ok:false` arrives as roost's own code.
func TestSetAgentHooksSurfacesARefusal(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.replaceReply("sethooks",
		`{"id":"1","ok":false,"error":{"code":"invalid-param","message":"agents must not be empty"}}`)

	_, err := r.remote.SetAgentHooks(e2eContext(t), r.target(), "shed-cli")
	var refusal *ResponseError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v (%T), want a *ResponseError", err, err)
	}
	if refusal.Code != "invalid-param" {
		t.Errorf("code = %q", refusal.Code)
	}
}

// TestWiredAgentsIsACopy: the list is a policy, and no caller can rewrite it
// through the slice it was handed. The expected list is spelled out rather than
// read from wiredAgents on purpose: an accessor that returned an alias would
// mutate wiredAgents too, and a comparison against it would still pass.
func TestWiredAgentsIsACopy(t *testing.T) {
	first := WiredAgents()
	first[0] = "gx"
	if got := WiredAgents(); !slices.Equal(got, []string{"claude", "codex", "cursor", "grok", "opencode"}) {
		t.Errorf("mutating one WiredAgents() result changed the next: %q", got)
	}
}
