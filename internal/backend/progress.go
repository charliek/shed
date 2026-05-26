package backend

import "context"

// ProgressEvent represents a progress update during a long-running operation.
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

// Progress emits a progress event. No-op if no ProgressFunc is in the context.
func Progress(ctx context.Context, phase, message string) {
	if fn, ok := ctx.Value(progressKey).(ProgressFunc); ok && fn != nil {
		fn(ProgressEvent{Phase: phase, Message: message})
	}
}

// ProgressWarning emits a warning progress event. No-op if no ProgressFunc is in the context.
func ProgressWarning(ctx context.Context, phase, message string) {
	if fn, ok := ctx.Value(progressKey).(ProgressFunc); ok && fn != nil {
		fn(ProgressEvent{Phase: phase, Message: message, Warning: true})
	}
}

// TeeProgress returns a ProgressFunc that forwards each event to every
// non-nil fn, in order. It returns nil when no non-nil fn is supplied, so
// callers can pass the result straight to ContextWithProgress. This lets
// a single operation feed both a server-side consumer (e.g. PhaseTimer)
// and the SSE writer from one progress stream.
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
