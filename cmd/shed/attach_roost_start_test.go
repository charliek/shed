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

// TestStartRungStartsAStoppedDaemonAndAttaches is the scenario the rung exists
// for, end to end through attachShed: roost cannot connect because nothing is
// listening on the shed, shed starts one over the shed's own ssh, roost
// connects on the second attempt, and the attach finishes normally — the tab is
// opened on the far side and SELECTED in the local app.
//
// Every layer below is the real one: the real fence, the real StartCommand
// through roost's real ladder, the real `session.identify` over the real NDJSON
// wire, and the real tab flow after it.
func TestStartRungStartsAStoppedDaemonAndAttaches(t *testing.T) {
	resetAttachState(t)
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}

	rig := newRoostRig(t, nil, true)
	rig.shed.stageStart(readyStart)
	rig.shim.reply("identify", roostVectorResult(t, "identify.response.json"))
	rows := []string{
		hostStatusDoc(), // Available()'s probe, before the flow starts
		hostStatusDoc(statusRow(0, roostctl.StateDisconnected, "")), // the fence's baseline
	}
	// Attempt one: reachable, and nothing is listening over there.
	rows = append(rows, settlingRows(1, roostReasonNoSession)...)
	// Attempt two, after the start: connected.
	rows = append(rows, hostStatusDoc(statusRow(2, roostctl.StateConnected, "")))
	rig.shim.replySeq("host.status", rows...)
	rig.shim.reply("host.list", hostStatusDoc())
	rig.shim.reply("host.add", hostAddDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("rpc.app.sidebar_dump",
		sidebarDumpDoc(testHostID),
		sidebarDumpDoc(testHostID, "h3.5"),
	)
	rig.shim.reply("tab.focus", "{}\n")
	rig.shim.reply("rpc.app.activate", "{}\n")

	newRoostAttach = func() *roostAttach { return rig.attach }
	execSSH = func(string, []string, []string) error {
		t.Error("the roost path must never exec ssh -t")
		return nil
	}

	if err := attachShed("myproj", "mini3", rig.shedEntry(), rig.shedConfig()); err != nil {
		t.Fatalf("attachShed: %v", err)
	}

	// THE assertion: the tab this attach opened is the one the local app was
	// told to select. Everything else here is how it got there.
	focus := rig.shim.runsOf("tab", "focus")
	if len(focus) != 1 {
		t.Fatalf("want exactly one `tab focus`, got %v", focus)
	}
	assertShimArgv(t, focus[0], []string{"tab", "focus", "--tab", "h3.5", "--json"})
	if !strings.Contains(rig.out.String(), "attached shed-myproj › default in roost") {
		t.Errorf("the attach did not finish: %q", rig.out.String())
	}

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
		})
	}
}
