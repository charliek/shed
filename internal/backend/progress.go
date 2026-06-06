package backend

import "context"

// ProgressEvent is what crosses the ContextWithProgress wire and, via
// the SSE handler, what crosses the HTTP wire to the `shed` CLI.
//
// Two pieces of information that are independent in the API:
//
//   - `Phase` — the name of the timer phase the operation is in.
//     Phase-only events (where `Message` is empty) advance the
//     PhaseTimer; the SSE handler drops them so they do not appear as
//     user-visible progress lines.
//   - `Message` — a human-readable status string. Status-only events
//     (where `Phase` is empty) are forwarded to SSE consumers but do
//     NOT advance the timer; they "attach" to the current phase.
//
// The base wire fields are unchanged; old `shed` CLIs continue to render
// progress identically because they only read `Message` + `Warning` (see
// `cmd/shed/shed.go` for the rendering loop).
//
// The `Kind=="blob"` fields are additive and optional (all `omitempty`).
// They carry structured per-blob byte progress for the CLI's live renderer.
// The server only forwards blob events to clients that opt in via the
// `?progress=blob` query param on the pull/create SSE request, so older and
// line-mode clients never receive them (no byte-tick spam) — see the SSE
// handlers in internal/api. A new CLI against an old server simply never
// sees a `Kind`, and stays in line mode.
type ProgressEvent struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Warning bool   `json:"warning,omitempty"`

	// Structured per-blob byte progress (Kind=="blob"); zero on plain
	// status/phase events. ID is the full digest (the renderer keys on it
	// and shortens for display); Current/Total are bytes.
	Kind    string `json:"kind,omitempty"`
	ID      string `json:"id,omitempty"`
	Status  string `json:"status,omitempty"`
	Current int64  `json:"current,omitempty"`
	Total   int64  `json:"total,omitempty"`
}

// KindBlob marks a ProgressEvent as structured per-blob byte progress.
const KindBlob = "blob"

// ProgressFunc is a callback for receiving progress events.
type ProgressFunc func(ProgressEvent)

type progressKeyType struct{}

var progressKey = progressKeyType{}

// ContextWithProgress returns a new context with the given progress function.
func ContextWithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey, fn)
}

// emit delivers ev to the context progress sink (nil-safe).
func emit(ctx context.Context, ev ProgressEvent) {
	if fn, ok := ctx.Value(progressKey).(ProgressFunc); ok && fn != nil {
		fn(ev)
	}
}

// Phase advances the PhaseTimer to a new phase. No user-visible SSE
// message is emitted. Use this when the boundary matters but you don't
// have a useful display string yet (or a separate Status call describes
// what's happening).
func Phase(ctx context.Context, name string) {
	emit(ctx, ProgressEvent{Phase: name})
}

// Status emits a user-visible progress message tied to the current
// phase. The PhaseTimer ignores status-only events; they show up as
// SSE lines on the `shed` CLI but do not split or rename a phase span.
func Status(ctx context.Context, message string) {
	emit(ctx, ProgressEvent{Message: message})
}

// StatusWarning is Status with the Warning flag set. The CLI renders
// it with a "Warning:" prefix on stderr.
func StatusWarning(ctx context.Context, message string) {
	emit(ctx, ProgressEvent{Message: message, Warning: true})
}

// BlobProgress emits a structured per-blob byte event WITHOUT advancing the
// PhaseTimer (Phase is forced empty), so it is safe to call concurrently
// from parallel download goroutines. The event must carry a non-empty
// Message (the SSE handler drops Message-less events); the renderer uses the
// structured fields and line-mode clients fall back to Message.
func BlobProgress(ctx context.Context, ev ProgressEvent) {
	ev.Phase = ""
	ev.Kind = KindBlob
	emit(ctx, ev)
}

// EmitProgress routes a progress event from an image operation (the backend
// bridges translate vmimage's progress events into this) to the context
// sink. Blob events (Kind=="blob") go straight through without advancing the
// phase; plain events preserve the historical two-event behavior — advance
// the phase, then emit a status line.
func EmitProgress(ctx context.Context, ev ProgressEvent) {
	if ev.Kind == KindBlob {
		BlobProgress(ctx, ev)
		return
	}
	if ev.Phase != "" {
		Phase(ctx, ev.Phase)
	}
	if ev.Message != "" {
		if ev.Warning {
			StatusWarning(ctx, ev.Message)
		} else {
			Status(ctx, ev.Message)
		}
	}
}

// TeeProgress returns a ProgressFunc that forwards each event to every
// non-nil fn, in order. It returns nil when no non-nil fn is supplied,
// so callers can pass the result straight to ContextWithProgress. This
// lets a single operation feed both a server-side consumer (the
// PhaseTimer) and the SSE writer from one progress stream.
func TeeProgress(fns ...ProgressFunc) ProgressFunc {
	live := make([]ProgressFunc, 0, len(fns))
	for _, fn := range fns {
		if fn != nil {
			live = append(live, fn)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(e ProgressEvent) {
		for _, fn := range live {
			fn(e)
		}
	}
}
