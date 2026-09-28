package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostctl"
	"github.com/charliek/shed/internal/roostprovider"
)

// The start rung (plan 023 §3.4, ticket shed#383): when roost reports that a
// reachable shed simply has no roost-session, `shed attach` starts one over the
// shed's OWN ssh instead of telling the user to go and do it by hand.
//
// Same three fixtures as the rest of this package's roost tests — the roostctl
// shim is the local app, the fake shed is the far side's roost-session over a
// fake ssh, and the fake clock is the wall clock — plus the fake shed's `start`
// subcommand, which is what makes a verdict stageable.

// forStartRung gives the fence the shed identity attach() records before step
// 5. A test that drives connectHost directly has to hand it over itself; the
// rung is structurally unreachable without it (connectHost's gate).
func (r *roostRig) forStartRung() {
	r.attach.shedName, r.attach.shedEntry = "myproj", r.shedEntry()
}

// settlingRows is the `disconnected`-with-a-reason row repeated exactly as many
// times as the fence must see it before the persistence window closes.
//
// Derived from the two bounds rather than counted by hand: the fence settles on
// the first poll at which the row has been unchanged for connectSettle, which
// is one poll to establish it plus connectSettle/connectPoll to outlast the
// window. A hand-written count would silently stop settling if either bound
// moved, and the test would then fail at the 20 s cap with a confusing message.
func settlingRows(generation uint64, reason string) []string {
	polls := int(defaultRoostConnectSettle/defaultRoostConnectPoll) + 1
	rows := make([]string, 0, polls)
	for range polls {
		rows = append(rows, hostStatusDoc(statusRow(generation, roostctl.StateDisconnected, reason)))
	}
	return rows
}

// readyStart is the verdict a start that worked prints.
var readyStart = startReply{stdout: "ready pid=4242\n"}

// The start rung's agent-hook raise (plan 024 D4, shed#387), as the bridge log
// and stderr see it.
const (
	// opSetAgentHooks is the raise's op, as the bridge log records it.
	opSetAgentHooks = "session.set_agent_hooks"
	// wantHooksRequest is the one request line the raise puts on the wire:
	// roost's `{agents, client}`, shed's own five names (never the vendored
	// request vector's two-name example), labelled "shed-cli".
	wantHooksRequest = `{"id":"1","op":"session.set_agent_hooks","params":` +
		`{"agents":["claude","codex","cursor","grok","opencode"],"client":"shed-cli"}}`
	// wantStartingLine is the rung's progress line, which precedes the hooks
	// line on stderr.
	wantStartingLine = "starting roost-session on shed-myproj…\n"
	// wantVectorHooksLine is the summary of roost's own vendored reply (the
	// fake's default): two wired, two skipped.
	wantVectorHooksLine = "agent hooks on shed-myproj: wired claude, codex; " +
		"skipped cursor (not allowed), grok (not installed)\n"
)

// stageStartRungAttach stages the local app for a whole `shed attach` in which
// the rung fires: roost cannot connect because nothing is listening on the
// shed, the rung starts one (readyStart), roost connects on the second
// attempt, and the tab flow runs to a focused `h3.5`.
func stageStartRungAttach(t *testing.T, rig *roostRig) {
	t.Helper()
	rig.shed.stageStart(readyStart)
	rig.shim.reply("identify", roostVectorResult(t, "identify.response.json"))
	rig.shim.replySeq("host.status", append(
		[]string{hostStatusDoc()}, // Available()'s probe, before the flow starts
		// Attempt two, after the start: connected.
		rungStatusRows(hostStatusDoc(statusRow(2, roostctl.StateConnected, "")))...)...)
	stageTabFlow(rig)
}

// rungStatusRows is the fence's view of a shed the rung fires on: the
// baseline, then attempt one settling on "reachable, and nothing is listening
// over there" — followed by whatever the test stages for after the start.
func rungStatusRows(after ...string) []string {
	rows := []string{hostStatusDoc(statusRow(0, roostctl.StateDisconnected, ""))}
	rows = append(rows, settlingRows(1, roostReasonNoSession)...)
	return append(rows, after...)
}

// stageRungConnect stages a connectHost-level drive of the rung: the shed
// identity, the start's reply, the one `host connect`, and rungStatusRows.
func stageRungConnect(rig *roostRig, start startReply, after ...string) {
	rig.forStartRung()
	rig.shed.stageStart(start)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("host.status", rungStatusRows(after...)...)
}

// stageTabFlow stages everything after the fence: the saved host, the second
// `host connect`, and the sidebar/focus/activate steps that select `h3.5`.
func stageTabFlow(rig *roostRig) {
	rig.shim.reply("host.list", hostStatusDoc())
	rig.shim.reply("host.add", hostAddDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("rpc.app.sidebar_dump",
		sidebarDumpDoc(testHostID),
		sidebarDumpDoc(testHostID, "h3.5"),
	)
	rig.shim.reply("tab.focus", "{}\n")
	rig.shim.reply("rpc.app.activate", "{}\n")
}

// runRigAttach runs the whole `shed attach myproj` through attachShed against
// the rig. The caller has already called resetAttachState.
func runRigAttach(t *testing.T, rig *roostRig) error {
	t.Helper()
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}
	newRoostAttach = func() *roostAttach { return rig.attach }
	execSSH = func(string, []string, []string) error {
		t.Error("the roost path must never exec ssh -t")
		return nil
	}
	return attachShed("myproj", "mini3", rig.shedEntry(), rig.shedConfig())
}

// assertTabSelected is the attach's own success: the tab this attach opened is
// the one the local app was told to select, and the result line printed.
func assertTabSelected(t *testing.T, rig *roostRig) {
	t.Helper()
	focus := rig.shim.runsOf("tab", "focus")
	if len(focus) != 1 {
		t.Fatalf("want exactly one `tab focus`, got %v", focus)
	}
	assertShimArgv(t, focus[0], []string{"tab", "focus", "--tab", "h3.5", "--json"})
	if !strings.Contains(rig.out.String(), "attached shed-myproj › default in roost") {
		t.Errorf("the attach did not finish: %q", rig.out.String())
	}
}

// assertHookRaises pins how many agent-hook raises reached the shed's bridge.
func assertHookRaises(t *testing.T, rig *roostRig, want int) {
	t.Helper()
	if n := rig.shed.countRequests(opSetAgentHooks); n != want {
		t.Errorf("saw %d session.set_agent_hooks raises on the shed, want exactly %d (bridge log: %q)",
			n, want, rig.shed.requestOps())
	}
	if want == 0 && strings.Contains(rig.errOut.String(), "agent hooks") {
		t.Errorf("want no raise, yet stderr reports one: %q", rig.errOut.String())
	}
}

// TestStartRungStartsAStoppedDaemonAndAttaches is the scenario the rung exists
// for, end to end through attachShed: roost cannot connect because nothing is
// listening on the shed, shed starts one over the shed's own ssh, raises the
// shed's agent hooks, roost connects on the second attempt, and the attach
// finishes normally — the tab is opened on the far side and SELECTED in the
// local app.
//
// Every layer below is the real one: the real fence, the real StartCommand
// through roost's real ladder, the real `session.identify` and
// `session.set_agent_hooks` over the real NDJSON wire, and the real tab flow
// after them.
func TestStartRungStartsAStoppedDaemonAndAttaches(t *testing.T) {
	resetAttachState(t)
	rig := newRoostRig(t, nil, true)
	stageStartRungAttach(t, rig)

	if err := runRigAttach(t, rig); err != nil {
		t.Fatalf("attachShed: %v", err)
	}

	// THE assertion: the tab this attach opened is the one the local app was
	// told to select. Everything else here is how it got there.
	assertTabSelected(t, rig)

	// The rung really ran, exactly once, and said so.
	if n := rig.shed.startCalls(); n != 1 {
		t.Errorf("want exactly one `roost-session start` on the shed, got %d", n)
	}
	if !strings.Contains(rig.errOut.String(), "starting roost-session on shed-myproj") {
		t.Errorf("the rung was silent; stderr was %q", rig.errOut.String())
	}
	// And it proved the session it started is one this shed speaks to.
	if n := rig.shed.countRequests("session.identify"); n != 1 {
		t.Errorf("want exactly one post-start session.identify, got %d", n)
	}

	// D4: exactly one raise, carrying exactly shed's own request.
	assertHookRaises(t, rig, 1)
	if got := rig.shed.requestFor(opSetAgentHooks); got != wantHooksRequest {
		t.Errorf("the raise's request line:\n got %s\nwant %s", got, wantHooksRequest)
	}
	// The ORDER, from the one log that holds both halves: step 6's tab ops go
	// over the same client-bridge as the raise, so "hooks wired after the
	// compatible identify and before the attach's tab opens" is this sequence
	// exactly — identify, then the raise, then the first tab.list.
	wantOps := []string{"session.identify", opSetAgentHooks, "tab.list", "tab.open", "tab.set_title"}
	if got := rig.shed.requestOps(); strings.Join(got, " ") != strings.Join(wantOps, " ") {
		t.Errorf("bridge op order:\n got %q\nwant %q", got, wantOps)
	}
	// And stderr is the progress line and the summary, one line each, and
	// nothing else.
	if got, want := rig.errOut.String(), wantStartingLine+wantVectorHooksLine; got != want {
		t.Errorf("stderr:\n got %q\nwant %q", got, want)
	}
}

// TestStartRungRunsAtMostOnce is the two pins rule 5 makes, asserted by exact
// call count: ONE remote recovery and TWO `host connect`s per attach, whatever
// the second attempt settles on.
//
// Without the latch this is an unbounded loop — every pass settles on the same
// recoverable reason and starts the daemon again — so the worst case before the
// user sees an error would be forever rather than 60 s and one more poll.
func TestStartRungRunsAtMostOnce(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.forStartRung()
	rig.shed.stageStart(readyStart)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rows := []string{hostStatusDoc(statusRow(0, roostctl.StateDisconnected, ""))}
	rows = append(rows, settlingRows(1, roostReasonNoSession)...)
	// Round two settles on the same recoverable row. The rung must not fire
	// again, and the fence must not connect a third time.
	rows = append(rows, settlingRows(2, roostReasonNoSession)...)
	rig.shim.replySeq("host.status", rows...)

	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	if err == nil {
		t.Fatal("a second settled failure is the last word")
	}
	if err.Error() != wantNoSessionMessage {
		t.Errorf("message:\n got %q\nwant %q", err.Error(), wantNoSessionMessage)
	}
	if n := len(rig.shim.runsOf("host", "connect")); n != 2 {
		t.Errorf("made %d `host connect` calls, want exactly 2", n)
	}
	if n := rig.shed.startCalls(); n != 1 {
		t.Errorf("made %d remote recoveries, want exactly 1", n)
	}
	// The raise rides on the same latch: the one start this attach made gets
	// one raise, and a second settled failure gets none.
	assertHookRaises(t, rig, 1)
}

// TestStartRungWrongProtocolIsAHardFailure is pin P6 on this rung: a session
// that comes up and speaks a protocol this shed does not is REPORTED, never
// repaired and never retried.
//
// The sentence is shed's own — roost's would tell the user to run `roostctl
// session stop` on a machine shed manages — and the number in it is built from
// SpokenProtocol here for the same reason it is built from SpokenProtocol
// there: a hard-coded 6 on either side would survive the bump that makes it
// wrong.
func TestStartRungWrongProtocolIsAHardFailure(t *testing.T) {
	const theirs = 7
	rig := newRoostRig(t, nil, true)
	rig.forStartRung()
	rig.shed.setProtocol(theirs)
	rig.shed.stageStart(readyStart)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rows := []string{hostStatusDoc(statusRow(0, roostctl.StateDisconnected, ""))}
	rows = append(rows, settlingRows(1, roostReasonNoSession)...)
	rig.shim.replySeq("host.status", rows...)

	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	if err == nil {
		t.Fatal("a session shed cannot speak to must fail the attach")
	}
	want := fmt.Sprintf("roost-session on %s speaks protocol %d, this shed speaks %d — upgrade whichever is older",
		testHostLabel, theirs, roostprovider.SpokenProtocol)
	if err.Error() != want {
		t.Errorf("message:\n got %q\nwant %q", err.Error(), want)
	}
	// It is shed's own error, carrying the number, not a reworded string.
	var refusal *roostStartRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want a *roostStartRefusal, got %#v", err)
	}
	if refusal.Spoken != theirs {
		t.Errorf("refusal.Spoken = %d, want %d", refusal.Spoken, theirs)
	}
	// Never a retry: no second connect, and no second start.
	if n := len(rig.shim.runsOf("host", "connect")); n != 1 {
		t.Errorf("made %d `host connect` calls; a mismatch is not retried", n)
	}
	if n := rig.shed.startCalls(); n != 1 {
		t.Errorf("made %d starts; a mismatch is not retried", n)
	}
	// And it does NOT fall back to "install and start one", which is not the
	// problem and not the fix.
	if strings.Contains(err.Error(), "Cmd/Alt-Shift-P") {
		t.Errorf("the mismatch was reported as a missing session:\n%s", err)
	}
	// A session shed will not talk to is never handed shed's hooks.
	assertHookRaises(t, rig, 0)
}

// TestStartRungNotInstalledKeepsTodaysMessage: the far side has no
// roost-session BINARY, so the ladder falls off its end with 127 and there is
// nothing to start. The user gets §3.3's pinned message, byte for byte — the
// rung adds nothing to it, because "install and start one" is exactly the
// remedy for this case.
//
// The rung's own diagnosis still reaches stderr, where a person debugging can
// see which of the two failures this was.
func TestStartRungNotInstalledKeepsTodaysMessage(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.forStartRung()
	rig.shed.stageStart(notInstalledStart)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rows := []string{hostStatusDoc(statusRow(0, roostctl.StateDisconnected, ""))}
	rows = append(rows, settlingRows(1, roostReasonNoSession)...)
	rig.shim.replySeq("host.status", rows...)

	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	if err == nil {
		t.Fatal("a far side with no roost-session must still fail the attach")
	}
	if err.Error() != wantNoSessionMessage {
		t.Errorf("message:\n got %q\nwant the pinned one %q", err.Error(), wantNoSessionMessage)
	}
	if !strings.Contains(rig.errOut.String(), "could not start roost-session on shed-myproj") {
		t.Errorf("the rung's diagnosis never reached stderr: %q", rig.errOut.String())
	}
	// It tried once and gave up: a 127 is not a race to wait out.
	if n := rig.shed.startCalls(); n != 1 {
		t.Errorf("made %d starts, want exactly 1", n)
	}
	if n := len(rig.shim.runsOf("host", "connect")); n != 1 {
		t.Errorf("made %d `host connect` calls; a failed start has nothing to reconnect to", n)
	}
	assertHookRaises(t, rig, 0)
}

// TestStartRungBudgetBoundsTheStart: a start that hangs is ended by the rung's
// own budget, and the reason the user is told is mined from whatever the far
// side had already printed.
//
// Real time and a real deadline — what is under test is that the context cuts
// the exec short, which a fake clock cannot say anything about.
func TestStartRungBudgetBoundsTheStart(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.forStartRung()
	rig.attach.startCap = 300 * time.Millisecond
	// It says why it is unhappy, and then never finishes.
	rig.shed.stageStart(startReply{stdout: "error: could not bind /run/user/1000/roost/session.sock\n", sleep: 30})
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rows := []string{hostStatusDoc(statusRow(0, roostctl.StateDisconnected, ""))}
	rows = append(rows, settlingRows(1, roostReasonNoSession)...)
	rig.shim.replySeq("host.status", rows...)

	start := time.Now()
	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a start that never finishes must fail the attach")
	}
	if err.Error() != wantNoSessionMessage {
		t.Errorf("message:\n got %q\nwant the pinned one %q", err.Error(), wantNoSessionMessage)
	}
	if !strings.Contains(rig.errOut.String(), "could not bind /run/user/1000/roost/session.sock") {
		t.Errorf("the reason was not mined from the stdout that existed: %q", rig.errOut.String())
	}
	// Generous, because what it separates is 300 ms from the far side's 30 s
	// hang — the number an unbounded rung would have taken.
	if elapsed > 10*time.Second {
		t.Errorf("the rung took %s; the budget did not bound the start", elapsed)
	}
	// A failed start is not a start: nothing to raise hooks on.
	assertHookRaises(t, rig, 0)
}

// TestStartableRemotely is the classifier the rung gates on, against roost's
// own reason strings.
//
// The two rows that matter most are the last two. `stopped` is roost saying the
// LOCAL app will not reconnect until it is asked again, which starting a daemon
// over there does not change; and roost's `NotFound` copy means roost has
// already looked and found no binary, so the rung would spend a round trip
// rediscovering a 127 — and roost's sentence, which names the non-interactive
// PATH, is the better answer to it.
func TestStartableRemotely(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		reason string
		want   bool
	}{
		{name: "roost's NoSession copy", state: roostctl.StateDisconnected, reason: roostReasonNoSession, want: true},
		{name: "the exec chain's raw command-not-found", state: roostctl.StateDisconnected, reason: roostReasonCommandNotFound, want: true},
		{name: "roost's NotFound copy has its own remedy", state: roostctl.StateDisconnected, reason: roostReasonNotFoundCopy},
		{name: "an auth failure is not a missing session", state: roostctl.StateDisconnected, reason: roostReasonAuth},
		{name: "a changed host key is not a missing session", state: roostctl.StateDisconnected, reason: roostReasonChangedHostKey},
		{name: "a transport failure is not a missing session", state: roostctl.StateDisconnected, reason: roostReasonTransport},
		{name: "no reason at all", state: roostctl.StateDisconnected},
		{name: "stopped is about the local end", state: roostctl.StateStopped, reason: roostReasonNoSession},
		{name: "needs-restart is too", state: roostctl.StateNeedsRestart, reason: roostReasonNoSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := startableRemotely(roostctl.HostStatus{State: tc.state, Reason: tc.reason})
			if got != tc.want {
				t.Errorf("startableRemotely(%q, %q) = %v, want %v", tc.state, tc.reason, got, tc.want)
			}
		})
	}
}

// TestStartRungIdentifyFailures is the post-start gate's classification: which
// `session.identify` failures are a hard refusal, and which are ordinary rung
// failures that end at §3.3's pinned message.
//
// The line between them is an assertion about the FAR SIDE. "It reported ready
// and then no session was there" is only true when the bridge ran and said so;
// a stalled identify — the shared 60 s budget expiring on a connection that
// went quiet — establishes only that shed never found out. Ending the attach on
// a hard refusal there would walk the user past the remedy over a slow network.
func TestStartRungIdentifyFailures(t *testing.T) {
	// A well-formed envelope with nothing in it: a session answered, and said
	// nothing this side can read (Remote.Identify's MalformedReplyError).
	const emptyIdentifyResult = `{"id":"1","ok":true,"result":{}}`

	cases := []struct {
		name string
		// stage arms the far side's identify.
		stage func(*fakeShed)
		// wantRefusal is whether this failure is the hard, pinned-message-
		// replacing kind.
		wantRefusal bool
		// wantStderr is a substring the rung's diagnosis must carry when it is
		// NOT a refusal.
		wantStderr string
	}{
		{
			name:        "the bridge says nothing is listening",
			stage:       func(f *fakeShed) { f.refuseIdentifyWithNoSession() },
			wantRefusal: true,
		},
		{
			name:       "a no-session line with a command-not-found exit is not an absent session",
			stage:      func(f *fakeShed) { f.refuseIdentifyWithNoSessionExit(127) },
			wantStderr: "confirming the roost-session on shed-myproj",
		},
		{
			name:       "a stalled identify is not an absent session",
			stage:      func(f *fakeShed) { f.stallIdentify(30) },
			wantStderr: "confirming the roost-session on shed-myproj",
		},
		{
			name:       "an answer this side cannot read is not an absent session",
			stage:      func(f *fakeShed) { f.setIdentifyReply(emptyIdentifyResult) },
			wantStderr: "no session_protocol",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRoostRig(t, nil, true)
			rig.forStartRung()
			rig.attach.startCap = 300 * time.Millisecond
			rig.shed.stageStart(readyStart)
			tc.stage(rig.shed)
			rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
			rows := []string{hostStatusDoc(statusRow(0, roostctl.StateDisconnected, ""))}
			rows = append(rows, settlingRows(1, roostReasonNoSession)...)
			rig.shim.replySeq("host.status", rows...)

			start := time.Now()
			err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("a session that cannot be confirmed must fail the attach")
			}
			var refusal *roostStartRefusal
			gotRefusal := errors.As(err, &refusal)
			if gotRefusal != tc.wantRefusal {
				t.Errorf("errors.As(*roostStartRefusal) = %v, want %v (err: %v)", gotRefusal, tc.wantRefusal, err)
			}
			if tc.wantRefusal {
				if !strings.Contains(err.Error(), "reported ready and then no session was there") {
					t.Errorf("message %q is not the post-start refusal", err.Error())
				}
			} else {
				// Today's pinned message, byte for byte — the remedy is still
				// the best advice available when shed could not find out.
				if err.Error() != wantNoSessionMessage {
					t.Errorf("message:\n got %q\nwant the pinned one %q", err.Error(), wantNoSessionMessage)
				}
				if !strings.Contains(rig.errOut.String(), tc.wantStderr) {
					t.Errorf("stderr %q does not carry %q", rig.errOut.String(), tc.wantStderr)
				}
			}
			// The budget bounds it either way. Generous, because what it
			// separates is 300 ms from the far side's 30 s stall.
			if elapsed > 10*time.Second {
				t.Errorf("the rung took %s; the budget did not bound the identify", elapsed)
			}
			// No compatible identify, no raise — whichever kind of failure
			// this was.
			assertHookRaises(t, rig, 0)
		})
	}
}

// TestStartRungErrorVerdictRaisesNoHooks: a start that answers roost's own
// `error: …` verdict did not start anything, so there is no session to raise
// hooks on — the other failed-start shape beside the budget expiring.
func TestStartRungErrorVerdictRaisesNoHooks(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	stageRungConnect(rig, startReply{stdout: "error: could not bind /run/user/1000/roost/session.sock\n", exit: 1})

	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	if err == nil || err.Error() != wantNoSessionMessage {
		t.Fatalf("an `error:` verdict ends at the pinned message; got %v", err)
	}
	if n := rig.shed.startCalls(); n != 1 {
		t.Errorf("made %d starts, want exactly 1", n)
	}
	assertHookRaises(t, rig, 0)
}

// TestStartRungRaisesOnceWhenAlreadyRunningIsAccepted is the Rust-parity claim,
// pinned: shed-core's PostStart runs Hooks after ANY compatible identify,
// including the one that follows an `already-running` verdict accepted at the
// retry cap (somebody else won the socket-lock race, and the post-start
// identify proved the winner speaks our protocol). That is still a start this
// rung performed, so it gets exactly one raise — not one per attempt.
func TestStartRungRaisesOnceWhenAlreadyRunningIsAccepted(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	// Every attempt loses the race (the last staged reply repeats); the second
	// connect succeeds.
	stageRungConnect(rig, startReply{stdout: "already-running pid=4242\n"},
		hostStatusDoc(statusRow(2, roostctl.StateConnected, "")))

	if err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID); err != nil {
		t.Fatalf("connectHost: %v", err)
	}
	// roostprovider's startRetries: the verdict was retried to the cap and
	// then accepted.
	if n := rig.shed.startCalls(); n != 5 {
		t.Errorf("made %d starts, want the retry cap's 5", n)
	}
	assertHookRaises(t, rig, 1)
	if got, want := rig.errOut.String(), wantStartingLine+wantVectorHooksLine; got != want {
		t.Errorf("stderr:\n got %q\nwant %q", got, want)
	}
}

// TestAttachToARunningSessionRaisesNoHooks is owner decision D4's rejected
// shape, pinned from the other side: an attach whose host is ALREADY connected
// starts nothing, so it raises nothing (see raiseAgentHooks for why never on
// every attach).
func TestAttachToARunningSessionRaisesNoHooks(t *testing.T) {
	resetAttachState(t)
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("identify", roostVectorResult(t, "identify.response.json"))
	rig.shim.replySeq("host.status",
		hostStatusDoc(), // Available()'s probe
		hostStatusDoc(statusRow(4, roostctl.StateConnected, "")), // already connected
	)
	stageTabFlow(rig)

	if err := runRigAttach(t, rig); err != nil {
		t.Fatalf("attachShed: %v", err)
	}
	assertTabSelected(t, rig)
	if n := rig.shed.startCalls(); n != 0 {
		t.Errorf("made %d starts against a connected host", n)
	}
	assertHookRaises(t, rig, 0)
	if rig.errOut.Len() != 0 {
		t.Errorf("an attach to a running session printed to stderr: %q", rig.errOut.String())
	}
}

// TestStartRungHookFailuresNeverFailTheAttach is D4's "warn and continue",
// parity with shed-core's hooks.rs: a raise that fails is a missing enrichment,
// never a failed attach. Every failure shape exits 0, prints exactly the pinned
// warning line, and still selects the tab.
//
// The malformed replies are the presence check's reason for existing: an
// `ok:true` whose outcome is missing a field must never be reported as
// "nothing to wire" — that would be a claim about the host the reply never
// made.
func TestStartRungHookFailuresNeverFailTheAttach(t *testing.T) {
	cases := []struct {
		name    string
		stage   func(*fakeShed)
		wantErr string
	}{
		{
			name: "an ok:false refusal",
			stage: func(f *fakeShed) {
				f.setReply("sethooks", `{"id":"1","ok":false,"error":{"code":"invalid-param","message":"agents must not be empty"}}`)
			},
			wantErr: "session.set_agent_hooks: agents must not be empty (invalid-param)",
		},
		{
			// A dropped connection: ssh's own words on stderr, and ssh's 255.
			name: "a transport failure",
			stage: func(f *fakeShed) {
				f.setBridge("sethooks", "printf '%s\\n' 'ssh: connect to host mini3 port 2222: Connection refused' >&2\nexit 255\n")
			},
			wantErr: "session.set_agent_hooks failed (transport): ssh: connect to host mini3 port 2222: Connection refused",
		},
		{
			name:    "a malformed ok:true with an empty result",
			stage:   func(f *fakeShed) { f.setReply("sethooks", `{"id":"1","ok":true,"result":{}}`) },
			wantErr: "the far side answered session.set_agent_hooks with no wired",
		},
		{
			name: "a malformed ok:true missing one field",
			stage: func(f *fakeShed) {
				f.setReply("sethooks", `{"id":"1","ok":true,"result":{"wired":["grok"],"refreshed":[],"removed":[],"errors":[]}}`)
			},
			wantErr: "the far side answered session.set_agent_hooks with no skipped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAttachState(t)
			rig := newRoostRig(t, nil, true)
			stageStartRungAttach(t, rig)
			tc.stage(rig.shed)

			if err := runRigAttach(t, rig); err != nil {
				t.Fatalf("a hook failure failed the attach: %v", err)
			}
			assertTabSelected(t, rig)
			assertHookRaises(t, rig, 1)
			want := wantStartingLine + "warning: could not wire agent hooks on shed-myproj: " + tc.wantErr + "\n"
			if got := rig.errOut.String(); got != want {
				t.Errorf("stderr:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestStartRungHookStallIsBoundedByHooksCap: a far side that accepts the raise
// and never answers costs the attach hooksCap, not the rest of the rung's
// minute. Real time and a real deadline, in the shape of
// TestStartRungBudgetBoundsTheStart — what is under test is that the context
// cuts the exec short, which a fake clock cannot say anything about — so the
// ELAPSED time is asserted, not merely that the test finished.
func TestStartRungHookStallIsBoundedByHooksCap(t *testing.T) {
	resetAttachState(t)
	rig := newRoostRig(t, nil, true)
	// startCap pinned well above the 10 s elapsed bound below, so only
	// hooksCap can end the stall in time — a smaller rig default would let the
	// test pass with hooksCap not applied at all.
	rig.attach.startCap = 60 * time.Second
	rig.attach.hooksCap = 300 * time.Millisecond
	stageStartRungAttach(t, rig)
	rig.shed.setBridge("sethooks", "sleep 30\n")

	start := time.Now()
	err := runRigAttach(t, rig)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a stalled raise failed the attach: %v", err)
	}
	assertTabSelected(t, rig)
	assertHookRaises(t, rig, 1)
	want := wantStartingLine +
		"warning: could not wire agent hooks on shed-myproj: session.set_agent_hooks failed (transport): no answer before the deadline\n"
	if got := rig.errOut.String(); got != want {
		t.Errorf("stderr:\n got %q\nwant %q", got, want)
	}
	// Generous, because what it separates is 300 ms from the far side's 30 s
	// stall — which is what an attach whose raise ran on the rung's 60 s budget
	// alone would have taken.
	if elapsed > 10*time.Second {
		t.Errorf("the attach took %s; hooksCap did not bound the raise", elapsed)
	}
}

// hooksStderrBudget bounds the whole stderr of a start-rung attach — the
// progress line and the hooks line — for the replies these rows send, the flood
// row included. The worst case a far side can force is larger, and is pinned by
// TestHooksSummaryWorstCaseIsBounded.
const hooksStderrBudget = 4096

// TestStartRungHooksSummaryLine pins the summary line for the replies roost's
// vendored vector does not carry, each through a whole attach (exit 0, the tab
// selected). Every row also holds the line to ONE physical line with no raw
// control byte on it, and stderr to hooksStderrBudget bytes.
func TestStartRungHooksSummaryLine(t *testing.T) {
	// A flood: a replaced or hostile daemon naming 1000 agents and skipping
	// 1000 more, each skip reason past the per-value cap. Only the first
	// hooksGroupCap (5) of each group are named.
	var floodWired []string
	var floodSkipped []map[string]string
	for i := range 1000 {
		floodWired = append(floodWired, fmt.Sprintf("agent%04d", i))
		floodSkipped = append(floodSkipped, map[string]string{
			"agent": fmt.Sprintf("skip%04d", i), "reason": strings.Repeat("r", 200),
		})
	}
	flood := strings.TrimSuffix(compactShedLine(t, map[string]any{
		"id": "1", "ok": true,
		"result": map[string]any{
			"wired": floodWired, "refreshed": []string{}, "removed": []string{},
			"skipped": floodSkipped, "errors": []string{},
		},
	}), "\n")
	capped := strings.Repeat("r", remoteValueCap) + "…"

	cases := []struct {
		name, reply, wantLine string
	}{
		{
			// A PARTIAL success — the normal case, shown, and still exit 0.
			name: "per-agent errors",
			reply: `{"id":"1","ok":true,"result":{"wired":["grok"],"refreshed":[],"removed":[],"skipped":[],` +
				`"errors":[{"agent":"codex","error":"permission denied writing ~/.codex/config.toml"}]}}`,
			wantLine: "agent hooks on shed-myproj: wired grok; failed codex (permission denied writing ~/.codex/config.toml)",
		},
		{
			// Every named agent already wired and already announced to some
			// client answers with every group empty (roost's own
			// `a_second_client_is_told_nothing`): a real answer, printed as
			// exactly this rather than as a line with nothing after the colon.
			name:     "nothing to wire",
			reply:    `{"id":"1","ok":true,"result":{"wired":[],"refreshed":[],"removed":[],"skipped":[],"errors":[]}}`,
			wantLine: "agent hooks on shed-myproj: nothing to wire",
		},
		{
			// roost documents skip reasons and errors as free display strings.
			// Decoded, these carry a real newline, CR and ESC: a newline would
			// forge a second line and an escape sequence would reach the
			// user's terminal. Both arrive as visible, escaped text.
			name: "one physical line",
			reply: `{"id":"1","ok":true,"result":{"wired":["gr\u001b[31mok"],"refreshed":[],"removed":[],` +
				`"skipped":[{"agent":"cursor","reason":"not\ninstalled\u001b[31m"}],` +
				`"errors":[{"agent":"codex","error":"boom\r\nwarning: forged\u001b[0m"}]}}`,
			wantLine: `agent hooks on shed-myproj: wired gr\x1b[31mok; skipped cursor (not\ninstalled\x1b[31m); ` +
				`failed codex (boom\r\nwarning: forged\x1b[0m)`,
		},
		{
			name:  "an entry flood is capped per group",
			reply: flood,
			wantLine: "agent hooks on shed-myproj: wired agent0000, agent0001, agent0002, agent0003, agent0004 (+995 more); " +
				"skipped skip0000 (" + capped + "), skip0001 (" + capped + "), skip0002 (" + capped + "), " +
				"skip0003 (" + capped + "), skip0004 (" + capped + ") (+995 more)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAttachState(t)
			rig := newRoostRig(t, nil, true)
			stageStartRungAttach(t, rig)
			rig.shed.setReply("sethooks", tc.reply)

			if err := runRigAttach(t, rig); err != nil {
				t.Fatalf("attachShed: %v", err)
			}
			assertTabSelected(t, rig)
			stderr := rig.errOut.String()
			if want := wantStartingLine + tc.wantLine + "\n"; stderr != want {
				t.Errorf("stderr:\n got %q\nwant %q", stderr, want)
			}
			if n := strings.Count(stderr, "\n"); n != 2 {
				t.Errorf("stderr is %d physical lines, want exactly 2 (progress + summary): %q", n, stderr)
			}
			if strings.ContainsAny(stderr, "\x1b\r") {
				t.Errorf("a raw control byte reached stderr: %q", stderr)
			}
			if len(stderr) >= hooksStderrBudget {
				t.Errorf("stderr is %d bytes, want under %d", len(stderr), hooksStderrBudget)
			}
		})
	}
}

// hooksSummaryMaxBytes is the hooks line's hard ceiling. The worst case is five
// groups of five entries (hooksGroupCap) plus a `(+N more)` each, every value
// remoteValueCap runes of four bytes plus its ellipsis (483 bytes), a skipped or
// failed entry carrying two of them: about 2.4 KB for each plain group, 4.9 KB
// for each detailed one, ~17 KB in all. 20 KiB leaves room for the labels and
// separators without letting an uncapped group anywhere near it.
const hooksSummaryMaxBytes = 20 * 1024

// TestHooksSummaryWorstCaseIsBounded holds the hooks line under
// hooksSummaryMaxBytes whatever the far side sends: every group flooded, every
// value past the per-value cap and made of four-byte runes — the byte maximum,
// since an escaped control byte is ASCII. (The one-physical-line rule is
// TestStartRungHooksSummaryLine's.)
func TestHooksSummaryWorstCaseIsBounded(t *testing.T) {
	long := strings.Repeat("\U0001F600", 400)
	var names []string
	var skipped []roostprovider.AgentHooksSkip
	var failed []roostprovider.AgentHooksFailure
	for range 1000 {
		names = append(names, long)
		skipped = append(skipped, roostprovider.AgentHooksSkip{Agent: long, Reason: long})
		failed = append(failed, roostprovider.AgentHooksFailure{Agent: long, Error: long})
	}
	got := hooksSummary(roostprovider.AgentHooksOutcome{
		Wired: names, Refreshed: names, Removed: names, Skipped: skipped, Errors: failed,
	})
	if n := len(got); n > hooksSummaryMaxBytes {
		t.Errorf("the hooks line is %d bytes, want at most %d", n, hooksSummaryMaxBytes)
	}
	if min := hooksSummaryMaxBytes * 3 / 4; len(got) < min {
		t.Errorf("the hooks line is %d bytes — under %d, so this is not the worst case", len(got), min)
	}
	if want := 5; strings.Count(got, "(+995 more)") != want {
		t.Errorf("want every one of the %d groups capped with (+995 more)", want)
	}
}

// TestHooksSummaryGroupOrder pins the summary's grammar with every group
// present: wired, refreshed, removed, skipped, failed — in that order, joined
// by "; ", names by ", ".
func TestHooksSummaryGroupOrder(t *testing.T) {
	got := hooksSummary(roostprovider.AgentHooksOutcome{
		Wired:     []string{"claude", "codex"},
		Refreshed: []string{"grok"},
		Removed:   []string{"opencode"},
		Skipped: []roostprovider.AgentHooksSkip{
			{Agent: "cursor", Reason: "not allowed"},
			{Agent: "pi", Reason: "unknown"},
		},
		Errors: []roostprovider.AgentHooksFailure{{Agent: "amp", Error: "read-only home"}},
	})
	want := "wired claude, codex; refreshed grok; removed opencode; " +
		"skipped cursor (not allowed), pi (unknown); failed amp (read-only home)"
	if got != want {
		t.Errorf("hooksSummary:\n got %q\nwant %q", got, want)
	}
	if got := hooksSummary(roostprovider.AgentHooksOutcome{}); got != "nothing to wire" {
		t.Errorf("an empty outcome = %q, want %q", got, "nothing to wire")
	}
}

// TestSanitizeRemoteText pins the escaping and the cap.
func TestSanitizeRemoteText(t *testing.T) {
	long := strings.Repeat("a", remoteValueCap+30)
	cases := []struct {
		name, in, want string
	}{
		{"plain text is untouched", "not installed", "not installed"},
		{"non-ASCII text is untouched", "café ✓", "café ✓"},
		{"newline, return and tab by name", "a\nb\rc\td", `a\nb\rc\td`},
		{"ESC and the rest of C0 as hex", "\x1b[31mred\x00", `\x1b[31mred\x00`},
		{"DEL", "a\x7fb", `a\x7fb`},
		{"a C1 control (8-bit CSI)", "a\u009bb", `a\u009bb`},
		{"the Unicode line and paragraph separators", "a\u2028b\u2029c", `a\u2028b\u2029c`},
		{"exactly the cap is not cut", long[:remoteValueCap], long[:remoteValueCap]},
		{"past the cap is cut with an ellipsis", long, long[:remoteValueCap] + "…"},
		{
			// The cut falls between whole escapes, never inside one.
			"an escape that would straddle the cap is dropped whole",
			strings.Repeat("a", remoteValueCap-2) + "\x1bzzz",
			strings.Repeat("a", remoteValueCap-2) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRemoteText(tc.in); got != tc.want {
				t.Errorf("sanitizeRemoteText(%q):\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
