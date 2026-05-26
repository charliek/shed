package backend

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// PhaseTimer measures the wall-clock duration of each phase of a
// long-running backend operation (e.g. CreateShed). Phases are delimited
// by ProgressEvents: each event closes the previous phase and opens the
// one it names. Consecutive events with the same phase are merged.
//
// Timing is captured server-side against a single monotonic clock and is
// intended for the server log only — it never travels on the SSE wire.
// SSE remains the user-facing CLI progress channel; the millisecond
// breakdown is a developer signal reachable via the server log (over SSH
// for remote hosts). See
// docs/discovery/platform-runtime-optimization.md.
type PhaseTimer struct {
	op  string
	now func() time.Time

	mu      sync.Mutex
	start   time.Time
	last    time.Time
	current string
	spans   []PhaseSpan
	guest   []PhaseSpan
}

// PhaseSpan is one measured phase.
type PhaseSpan struct {
	Phase    string
	Duration time.Duration
}

// NewPhaseTimer starts a timer for op. now defaults to time.Now when nil
// (a custom clock is used by tests).
func NewPhaseTimer(op string, now func() time.Time) *PhaseTimer {
	if now == nil {
		now = time.Now
	}
	t := &PhaseTimer{op: op, now: now}
	t.start = t.now()
	t.last = t.start
	return t
}

// Track is a ProgressFunc. Each event ends the current phase (recording
// its duration) and begins the phase it names. The interval before the
// first event is recorded as "setup". Safe for concurrent use.
func (t *PhaseTimer) Track(e ProgressEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.Phase == t.current {
		// Same phase emitting another message — keep accumulating into
		// the current span rather than splitting it.
		return
	}
	n := t.now()
	t.spans = append(t.spans, PhaseSpan{Phase: t.label(), Duration: n.Sub(t.last)})
	t.last = n
	t.current = e.Phase
}

// AddGuestSpans records sub-phases measured inside the guest (e.g. the
// initramfs mkfs/overlay split), reported as guest:[...] in the summary.
// Reserved for the guest-timing feature; the host does not populate it yet.
func (t *PhaseTimer) AddGuestSpans(spans ...PhaseSpan) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.guest = append(t.guest, spans...)
}

// Finish closes the final phase and returns a single-line summary for the
// server log. err annotates the outcome (nil => ok).
func (t *PhaseTimer) Finish(err error) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.now()
	t.spans = append(t.spans, PhaseSpan{Phase: t.label(), Duration: n.Sub(t.last)})
	total := n.Sub(t.start)

	var b strings.Builder
	fmt.Fprintf(&b, "%s total=%s", t.op, ms(total))
	for _, s := range t.spans {
		fmt.Fprintf(&b, " %s=%s", s.Phase, ms(s.Duration))
	}
	if len(t.guest) > 0 {
		b.WriteString(" guest:[")
		for i, s := range t.guest {
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%s=%s", s.Phase, ms(s.Duration))
		}
		b.WriteByte(']')
	}
	if err != nil {
		fmt.Fprintf(&b, " err=%q", err.Error())
	} else {
		b.WriteString(" err=<nil>")
	}
	return b.String()
}

// label returns the current phase name, defaulting to "setup" for the
// interval before any phase has been announced. Caller holds t.mu.
func (t *PhaseTimer) label() string {
	if t.current == "" {
		return "setup"
	}
	return t.current
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%dms", d.Milliseconds())
}
