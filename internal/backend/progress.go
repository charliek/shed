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
// The wire shape stays the same; old `shed` CLIs continue to render
// progress identically because they only read `Message` + `Warning`
// (see `cmd/shed/shed.go` for the rendering loop).
type ProgressEvent struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Warning bool   `json:"warning,omitempty"`
}

// ProgressFunc is a callback for receiving progress events.
type ProgressFunc func(ProgressEvent)

type progressKeyType struct{}

var progressKey = progressKeyType{}

// ContextWithProgress returns a new context with the given progress function.
func ContextWithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey, fn)
}

// Phase advances the PhaseTimer to a new phase. No user-visible SSE
// message is emitted. Use this when the boundary matters but you don't
// have a useful display string yet (or a separate Status call describes
// what's happening).
func Phase(ctx context.Context, name string) {
	if fn, ok := ctx.Value(progressKey).(ProgressFunc); ok && fn != nil {
		fn(ProgressEvent{Phase: name})
	}
}

// Status emits a user-visible progress message tied to the current
// phase. The PhaseTimer ignores status-only events; they show up as
// SSE lines on the `shed` CLI but do not split or rename a phase span.
func Status(ctx context.Context, message string) {
	if fn, ok := ctx.Value(progressKey).(ProgressFunc); ok && fn != nil {
		fn(ProgressEvent{Message: message})
	}
}

// StatusWarning is Status with the Warning flag set. The CLI renders
// it with a "Warning:" prefix on stderr.
func StatusWarning(ctx context.Context, message string) {
	if fn, ok := ctx.Value(progressKey).(ProgressFunc); ok && fn != nil {
		fn(ProgressEvent{Message: message, Warning: true})
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
