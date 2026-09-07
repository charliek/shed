package rc

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// waitUntilLive's settle arithmetic on a FAKE clock. S2 (charliek/shed#324) replaced
// the pane classifier with liveness, so the only thing left to get wrong here is the
// timing: when the kickoff is typed, and against which origin. Driving it on a fake
// clock is what keeps these cells free of a real 5 s wait (rc-parity's two kickoff
// cells pay the real one, on purpose).

// waitClock is a manual clock whose now() advances by exactly the durations the
// injected sleep is asked for. That makes the loop's real cadence (defaultPollEvery)
// the tick size, so a test can count polls instead of guessing at wall time.
type waitClock struct{ t time.Time }

func (c *waitClock) now() time.Time            { return c.t }
func (c *waitClock) sleep(d time.Duration)     { c.t = c.t.Add(d) }
func (c *waitClock) advance(d time.Duration)   { c.t = c.t.Add(d) }
func (c *waitClock) since(t0 time.Time) string { return c.t.Sub(t0).String() }

// paneScript answers capture-pane from a per-call script (the last entry repeats) and
// records every send-keys argv. sendFails makes every send-keys fail, which is how the
// "a failed keystroke does not move the origin" case is driven.
type paneScript struct {
	panes     []string
	captures  int
	sends     [][]string
	sendFails bool
	clk       *waitClock
	// sendAt records the clock reading of each send-keys (the whole point of the
	// suite: WHEN the kickoff landed).
	sendAt []time.Time
}

func (p *paneScript) Run(args ...string) Result {
	switch args[0] {
	case "capture-pane":
		i := p.captures
		if i >= len(p.panes) {
			i = len(p.panes) - 1
		}
		p.captures++
		return Result{Code: 0, Stdout: p.panes[i]}
	case "send-keys", "load-buffer", "paste-buffer", "delete-buffer":
		p.sends = append(p.sends, append([]string(nil), args...))
		p.sendAt = append(p.sendAt, p.clk.t)
		if p.sendFails {
			return Result{Code: 1, Stderr: "send failed"}
		}
		return Result{Code: 0}
	}
	return Result{Code: 0}
}

// kickoffAt returns the clock offset from t0 at which the kickoff line was typed
// (`send-keys -l -- <text>`, or the paste-buffer path for a multiline one), and
// whether one was typed at all.
func (p *paneScript) kickoffAt(t0 time.Time, text string) (time.Duration, bool) {
	for i, c := range p.sends {
		if c[0] == "send-keys" && contains(c, "-l") && contains(c, text) {
			return p.sendAt[i].Sub(t0), true
		}
	}
	return 0, false
}

const livePane = "opencode\nAsk anything..."

// Without a prompt there is nothing to settle for: the FIRST successful capture
// returns ready, with no keystroke drawn.
func TestWaitUntilLiveNoPromptReturnsImmediately(t *testing.T) {
	clk := &waitClock{t: time.Unix(1_700_000_000, 0).UTC()}
	t0 := clk.t
	sc := &paneScript{panes: []string{livePane}, clk: clk}

	state, _, err := waitUntilLive(sc, "rc-a", KindOpencode, "", false, clk.sleep, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if state != StateReady {
		t.Fatalf("state = %q, want ready", state)
	}
	if sc.captures != 1 {
		t.Fatalf("captures = %d, want exactly 1 (no settle without a prompt)", sc.captures)
	}
	if len(sc.sends) != 0 {
		t.Fatalf("a promptless wait must draw no keystroke, got %v", sc.sends)
	}
	if d := clk.t.Sub(t0); d != 0 {
		t.Fatalf("elapsed = %s, want 0", d)
	}
}

// With a prompt the loop settles kickoffSettle from the FIRST successful capture,
// then sleeps promptDeliverSettle once more — so the line lands at 6 s.
func TestWaitUntilLiveKickoffLandsAfterTheSettle(t *testing.T) {
	clk := &waitClock{t: time.Unix(1_700_000_000, 0).UTC()}
	t0 := clk.t
	sc := &paneScript{panes: []string{livePane}, clk: clk}

	state, _, err := waitUntilLive(sc, "rc-a", KindOpencode, "go", false, clk.sleep, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if state != StateReady {
		t.Fatalf("state = %q, want ready", state)
	}
	at, ok := sc.kickoffAt(t0, "go")
	if !ok {
		t.Fatalf("kickoff was never typed; sends = %v", sc.sends)
	}
	// The loop polls every 750 ms, so it breaks at the first tick at or past 5 s
	// (5.25 s), then adds promptDeliverSettle.
	wantMin := kickoffSettle + promptDeliverSettle
	wantMax := kickoffSettle + defaultPollEvery + promptDeliverSettle
	if at < wantMin || at > wantMax {
		t.Fatalf("kickoff at %s, want within [%s, %s]", at, wantMin, wantMax)
	}
}

// A control accept at 4.9 s RESTARTS the settle window: the kickoff must not be typed
// into the dialog's repaint. The pane shows codex's trust dialog until the Enter is
// sent, so the accept happens on a late poll.
func TestWaitUntilLiveLateAcceptRestartsTheSettle(t *testing.T) {
	clk := &waitClock{t: time.Unix(1_700_000_000, 0).UTC()}
	t0 := clk.t
	const dialog = "codex\nDo you trust the contents of this directory?\n1. Yes, continue"
	// Six polls of a clean pane (0 → 3.75 s), then the dialog appears at 4.5 s, then
	// the composer again.
	panes := make([]string, 0, 10)
	for range 6 {
		panes = append(panes, "codex\nbooting")
	}
	panes = append(panes, dialog, "codex\nAsk Codex to do anything")
	sc := &paneScript{panes: panes, clk: clk}

	if _, _, err := waitUntilLive(sc, "rc-a", KindCodex, "go", false, clk.sleep, clk.now); err != nil {
		t.Fatal(err)
	}
	at, ok := sc.kickoffAt(t0, "go")
	if !ok {
		t.Fatalf("kickoff was never typed; sends = %v", sc.sends)
	}
	// Exactly one Enter for the dialog (the accept is latched), and it happened at
	// 4.5 s. Only sends BEFORE the kickoff count — sendLine's own trailing Enter is
	// part of the delivery, not an accept.
	enters := 0
	var enterAt time.Duration
	for i, c := range sc.sends {
		if sc.sendAt[i].Sub(t0) >= at {
			break
		}
		if len(c) == 4 && c[0] == "send-keys" && c[3] == "Enter" {
			enters++
			enterAt = sc.sendAt[i].Sub(t0)
		}
	}
	if enters != 1 {
		t.Fatalf("trust Enter count = %d, want exactly 1: %v", enters, sc.sends)
	}
	// The origin moved to the accept, so the kickoff is >= accept + 5 s + 1 s —
	// strictly later than the 6 s it would have been off the first capture.
	if at < enterAt+kickoffSettle+promptDeliverSettle {
		t.Fatalf("kickoff at %s, want >= %s (the accept must restart the settle)",
			at, enterAt+kickoffSettle+promptDeliverSettle)
	}
}

// THE DELIVERY GATE, and the reason the accept latches only on success: when every
// send-keys fails the dialog never clears, so the kickoff must be REFUSED rather
// than typed into the modal. A latch-before-send would have made the loop ignore the
// dialog from the second poll on, and the old post-loop `state == ready` check would
// then have delivered straight into it.
func TestWaitUntilLiveRefusesToDeliverIntoADialogThatNeverCleared(t *testing.T) {
	clk := &waitClock{t: time.Unix(1_700_000_000, 0).UTC()}
	t0 := clk.t
	const dialog = "codex\nDo you trust the contents of this directory?\n1. Yes, continue"
	sc := &paneScript{panes: []string{dialog}, clk: clk, sendFails: true}

	state, _, err := waitUntilLive(sc, "rc-a", KindCodex, "go", false, clk.sleep, clk.now)
	if state != StateReady {
		t.Fatalf("state = %q, want ready (the session is live either way)", state)
	}
	if !errors.Is(err, ErrBadArgs) {
		t.Fatalf("err = %v, want the ErrBadArgs control-dialog refusal", err)
	}
	if !strings.Contains(err.Error(), "trust/bypass dialog") {
		t.Fatalf("err = %v, want the shared control-dialog refusal message", err)
	}
	if _, delivered := sc.kickoffAt(t0, "go"); delivered {
		t.Fatalf("the kickoff was typed into a live dialog: %v", sc.sends)
	}
	// The accept was RETRIED every poll (never latched on a failed send) rather than
	// latched once and forgotten.
	enters := 0
	for _, c := range sc.sends {
		if len(c) == 4 && c[0] == "send-keys" && c[3] == "Enter" {
			enters++
		}
	}
	if enters < 2 {
		t.Fatalf("accept Enter count = %d, want a retry on every poll: %v", enters, sc.sends)
	}
}

// The same refusal for claude's BYPASS dialog: a dialog the deadline ran out under
// must not receive the kickoff either.
func TestWaitUntilLiveRefusesToDeliverIntoABypassDialog(t *testing.T) {
	clk := &waitClock{t: time.Unix(1_700_000_000, 0).UTC()}
	t0 := clk.t
	const dialog = "claude\nBypass Permissions mode\n1. No, exit\n2. Yes, I accept"
	// The accepts "succeed" but the pane never changes — a TUI that redraws the same
	// dialog is indistinguishable from one that ignored the keypress.
	sc := &paneScript{panes: []string{dialog}, clk: clk}

	state, _, err := waitUntilLive(sc, "rc-a", KindClaudeRC, "go", true, clk.sleep, clk.now)
	if state != StateReady {
		t.Fatalf("state = %q, want ready", state)
	}
	if !errors.Is(err, ErrBadArgs) {
		t.Fatalf("err = %v, want the ErrBadArgs control-dialog refusal", err)
	}
	if _, delivered := sc.kickoffAt(t0, "go"); delivered {
		t.Fatalf("the kickoff was typed into a live bypass dialog: %v", sc.sends)
	}
}

// The settle is bounded by the existing 20 s deadline. A session that only starts
// drawing at 18 s has a settle window that would run past it — the kickoff is
// delivered AT the deadline rather than not at all.
func TestWaitUntilLiveSettleIsDeadlineBounded(t *testing.T) {
	clk := &waitClock{t: time.Unix(1_700_000_000, 0).UTC()}
	t0 := clk.t
	sc := &paneScript{panes: []string{livePane}, clk: clk}
	late := &lateRunner{inner: sc, clk: clk, liveAfter: t0.Add(18 * time.Second)}

	state, _, err := waitUntilLive(late, "rc-a", KindOpencode, "go", false, clk.sleep, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if state != StateReady {
		t.Fatalf("state = %q, want ready", state)
	}
	at, ok := sc.kickoffAt(t0, "go")
	if !ok {
		t.Fatalf("kickoff was never typed; sends = %v", sc.sends)
	}
	// The first capture landed at 18 s, so the 5 s settle would end at 23 s — past
	// the deadline. The loop left AT the deadline (the last poll sleep is clamped to
	// what was left, so there is no overshoot to add promptDeliverSettle on top of)
	// and delivered exactly one settle later.
	if want := defaultWaitTimeout + promptDeliverSettle; at != want {
		t.Fatalf("kickoff at %s, want exactly %s (the deadline plus one delivery settle)", at, want)
	}
}

// lateRunner fails capture-pane transiently until liveAfter, then delegates. It models
// a session whose agent takes most of the wait window to draw anything.
type lateRunner struct {
	inner     Runner
	clk       *waitClock
	liveAfter time.Time
}

func (l *lateRunner) Run(args ...string) Result {
	if args[0] == "capture-pane" && l.clk.t.Before(l.liveAfter) {
		return Result{Code: 1, Stderr: "tmux: no output yet"}
	}
	return l.inner.Run(args...)
}

// A session that never comes up (capture-pane keeps failing transiently) times out as
// `starting` with nothing delivered — unchanged from the classifier era.
func TestWaitUntilLiveTransientCaptureFailuresTimeOut(t *testing.T) {
	clk := &waitClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f := &fakeTmux{handler: func(args []string) Result {
		if args[0] == "capture-pane" {
			return Result{Code: 1, Stderr: "tmux: server exited unexpectedly"}
		}
		return Result{Code: 0}
	}}
	state, url, err := waitUntilLive(f, "rc-a", KindOpencode, "go", false, clk.sleep, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if state != StateStarting || url != "" {
		t.Fatalf("(state,url) = (%q,%q), want (starting,\"\")", state, url)
	}
	if f.callWith("send-keys") != nil {
		t.Fatal("nothing may be typed into a session that never captured")
	}
	// The loop leaves AT the deadline, not a poll past it.
	if got := clk.t.Sub(time.Unix(1_700_000_000, 0).UTC()); got != defaultWaitTimeout {
		t.Fatalf("elapsed = %s, want exactly the %s deadline", got, defaultWaitTimeout)
	}
}
