// Package backend's cleanup.go provides Cleanup — a LIFO rollback stack
// for the per-shed multi-step `CreateShed` (and `StartShed`, snapshot
// spawn, ...) lifecycle.
//
// The problem it replaces: today both backends' `CreateShed`
// implementations have ~10 inline rollback blocks per file that look
// like:
//
//	if err := step3(); err != nil {
//	    if stopErr := vm.Stop(...); stopErr != nil { log.Printf(...) }
//	    c.preserveConsoleLog(meta)
//	    if rmErr := meta.Delete(...); rmErr != nil { log.Printf(...) }
//	    if rmErr := DeleteUpper(...); rmErr != nil { log.Printf(...) }
//	    return nil, fmt.Errorf("step3: %w", err)
//	}
//
// VZ has ~19 such lines today, Firecracker has ~47 (more cleanups
// because of CID + IP + TAP device + 9P servers). Easy to forget one;
// easy to put two in the wrong order; harder still to keep the two
// backends in sync as new steps are added.
//
// The Cleanup pattern flips that around: each successful step
// `Register`s its own teardown closure at acquisition time, with a
// short name for the log. On any error the caller invokes
// `RunReverse(err)`, which runs every registered cleanup in
// last-in-first-out order, logs individual failures, and returns the
// original error unchanged so the call site reads:
//
//	cleanup := backend.NewCleanup()
//	defer cleanup.Run() // no-op after Commit
//
//	if err := allocateUpper(); err != nil {
//	    return nil, fmt.Errorf("allocate upper: %w", err)
//	}
//	cleanup.Register("delete upper", func() error {
//	    return DeleteUpper(c.cfg.UppersDir, req.Name)
//	})
//
//	if err := saveMeta(); err != nil {
//	    return nil, fmt.Errorf("save metadata: %w", err)
//	}
//	cleanup.Register("preserve console log + delete instance dir", func() error {
//	    c.preserveConsoleLog(meta)
//	    return meta.Delete(c.cfg.InstanceDir)
//	})
//
//	// ... etc ...
//	cleanup.Commit()
//	return shed, nil
//
// The defer guarantees cleanup runs on *any* return path, including
// panic recovery in callers (the rollback runs before the panic
// propagates). The `Commit` call on the happy path zeros the stack so
// the defer becomes a no-op.

package backend

import (
	"log"
	"sync"
)

// Cleanup is a LIFO rollback stack for multi-step lifecycle methods
// (e.g. CreateShed). Safe for concurrent use; the lifecycle methods
// don't share Cleanup instances across goroutines, but Register from
// inside an in-flight goroutine is legal.
//
// Two stacks live inside one Cleanup:
//
//   - `steps` — error-only cleanups (the typical `Register` case).
//     Run on `Run()`; cleared by `Commit()` so the success-path
//     `defer cleanup.Run()` is a no-op.
//   - `deferred` — always-run cleanups (the `AddDeferred` case).
//     Run on BOTH `Run()` (after the error-only stack unwinds) and
//     on `Commit()`. Semantically equivalent to a Go `defer` — useful
//     for protective bits (e.g., a `.creating` marker) whose lifetime
//     is the operation, not the resulting resource.
type Cleanup struct {
	mu        sync.Mutex
	steps     []cleanupStep
	deferred  []cleanupStep
	committed bool
}

type cleanupStep struct {
	name string
	fn   func() error
}

// NewCleanup returns an empty Cleanup stack.
func NewCleanup() *Cleanup {
	return &Cleanup{}
}

// Register pushes a cleanup onto the stack. `name` appears in the
// warning log emitted by Run when the cleanup fails — keep it short
// and operator-friendly (e.g. "stop VM", "delete upper layer").
//
// Cleanups registered after Commit are silently ignored, so a caller
// can defensively Register a cleanup for a step whose success-path
// teardown is the operation's job (e.g. cluster the cleanup with the
// step that needs it without worrying about the success-path).
func (c *Cleanup) Register(name string, fn func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.committed {
		return
	}
	c.steps = append(c.steps, cleanupStep{name: name, fn: fn})
}

// AddDeferred pushes a cleanup onto the always-run stack. Unlike
// `Register`, the cleanup runs on BOTH `Run()` (error return) and
// `Commit()` (success return) — semantically equivalent to a Go
// `defer` inside the lifecycle method's body.
//
// Use this for operations whose lifetime is bounded by the lifecycle
// method itself, not by the resource the method produces. The
// canonical example is a `.creating` marker that protects an image
// blob from a concurrent `shed image prune` for the duration of a
// `CreateShed` — it must be removed whether the create succeeded
// (the meta now keeps the digest pinned) or failed (no shed to
// protect anymore).
//
// `AddDeferred` cleanups run AFTER all error-only `Register` cleanups
// have unwound (on error) so they see the post-rollback state. On
// success they run after Commit's main work.
func (c *Cleanup) AddDeferred(name string, fn func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.committed {
		return
	}
	c.deferred = append(c.deferred, cleanupStep{name: name, fn: fn})
}

// Commit declares the operation successful — error-only cleanups are
// cleared so the deferred `Run()` is a no-op for them, but the
// always-run (`AddDeferred`) cleanups still execute in LIFO order
// from THIS call. Idempotent. Call from the happy path right before
// returning the successful result.
func (c *Cleanup) Commit() {
	c.mu.Lock()
	deferred := c.deferred
	c.deferred = nil
	c.steps = nil
	c.committed = true
	c.mu.Unlock()

	// Run the always-deferred cleanups (success path). Same panic-
	// recovery wrapper as Run uses, so a buggy deferred cleanup can
	// not stop a sibling from executing.
	for i := len(deferred) - 1; i >= 0; i-- {
		runCleanupStep(deferred[i])
	}
}

// Run invokes every registered cleanup in LIFO order. Individual
// cleanup errors are logged at warning level; cleanups after a failure
// still run, so a slow `vm.Stop` doesn't leak an upper-layer file.
// Idempotent: subsequent calls are no-ops.
//
// Cleanup closures are run inside an isolating panic-recovery wrapper —
// a panic in step N is logged and the remaining steps still execute.
// Without this, a buggy `vm.Stop` could leave an upper-layer file
// behind, which is precisely the class of leak this type exists to
// prevent.
//
// Designed to be called from `defer cleanup.Run()` at the top of a
// lifecycle method. The defer guarantees rollback on panic-mid-create
// too (the rollback runs before the panic propagates out).
func (c *Cleanup) Run() {
	c.mu.Lock()
	steps := c.steps
	deferred := c.deferred
	c.steps = nil
	c.deferred = nil
	c.committed = true
	c.mu.Unlock()

	// Error-only cleanups unwind first.
	for i := len(steps) - 1; i >= 0; i-- {
		runCleanupStep(steps[i])
	}
	// Always-deferred cleanups run after the rest of the rollback,
	// so they see the post-rollback state (a marker file's protected
	// blob is now gone, etc.).
	for i := len(deferred) - 1; i >= 0; i-- {
		runCleanupStep(deferred[i])
	}
}

// runCleanupStep wraps a single cleanup invocation in `defer recover`
// so a panicking closure does not abort the LIFO unwind. Reported
// errors and recovered panics share the same log shape.
func runCleanupStep(step cleanupStep) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Warning: cleanup %q panicked: %v", step.name, r)
		}
	}()
	if err := step.fn(); err != nil {
		log.Printf("Warning: cleanup %q failed: %v", step.name, err)
	}
}
