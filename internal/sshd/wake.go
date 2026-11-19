package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// asyncWakeTimeout is how long an SSH or SFTP session waits for a shed on an
// asynchronous backend (remote) to settle: a stop in flight (about 50 s on
// GCE) followed by a start (about 40 s), with slack. A var so tests can
// shorten it.
var asyncWakeTimeout = 150 * time.Second

// wakeError is a failure ensureRunning reports to the session's stderr as
// "Error: <msg>".
type wakeError struct{ msg string }

func (e *wakeError) Error() string { return e.msg }

func wakeErrorf(format string, args ...any) error {
	return &wakeError{msg: fmt.Sprintf(format, args...)}
}

// ensureRunning returns the named shed once it is running, starting it when
// it is stopped. It is the one wake path the shell session and the SFTP
// subsystem share. Every error is a *wakeError whose text the caller prints
// after "Error: ". announce prints "Starting shed '<name>'..." before an
// auto-start (the shell session does; SFTP never has).
//
// A Firecracker or VZ shed is started synchronously and polled for at most
// containerReadyTimeout, exactly as before. A shed on an AsyncLifecycle
// backend is waited on through WaitSettled for at most asyncWakeTimeout, with
// the job's progress written to stderr; a stopping shed is waited on until it
// is stopped and then started.
//
// The right to enter the shed is checked first, before anything waits or
// wakes it: a caller who may see the shed but not enter it (an admin and
// another user's shed) is told so, and a caller who may not see it is told it
// does not exist.
//
// Everything after acts on the shed that was checked, with the ownership it
// was checked against: right after the check, ctx is pinned (pinEntry) to
// the shed's incarnation (creation time) and to its reported owner and user,
// and the returned context carries those pins for the guarded exec that
// enters the shed. Every guarded call of the wake (a start, a poll, a wait
// for the job to settle) refuses a shed that no longer matches them, and
// every record the wake reads is checked against them too (stillTheShed): a
// shed whose owner or user changed at any point between the check and the
// exec (reassigned, or a client's shed whose owner a reconcile could no
// longer resolve) is refused as one that is not there, never entered as the
// shed it has become.
//
// user is the username the connection authenticated as; it is turned into
// the shed's name by shedNameFor, before any lookup, and one that names no
// shed (git, a case variant, a special name) is told the shed does not exist,
// as an unknown shed is.
func (s *Server) ensureRunning(ctx context.Context, user string, stderr io.Writer, announce bool) (context.Context, *config.Shed, error) {
	name, ok := shedNameFor(user)
	if !ok {
		log.Printf("Refused shed lookup: user=%s is not a shed name", logUser(user))
		return nil, nil, wakeErrorf("shed '%s' not found", user)
	}
	shed, err := s.guard().Check(ctx, name, backend.ActionEnter)
	if err != nil {
		log.Printf("Failed to get shed %s: %v", name, err)
		if errors.Is(err, config.ErrForbiddenSentinel) {
			return nil, nil, &wakeError{msg: forbiddenMessage(name, err)}
		}
		return nil, nil, wakeErrorf("shed '%s' not found", name)
	}
	ctx = pinEntry(ctx, shed)
	if al, ok := backend.IsAsync(s.backend); ok {
		shed, err = s.ensureRunningAsync(ctx, al, shed, stderr, announce)
	} else {
		shed, err = s.ensureRunningSync(ctx, shed, stderr, announce)
	}
	if err != nil {
		return nil, nil, err
	}
	return ctx, shed, nil
}

// stillTheShed returns a "not found" wakeError when shed, a record the wake
// read, is not the one ctx is pinned to (pinEntry): another incarnation, or
// another reported owner or user. nil otherwise.
func stillTheShed(ctx context.Context, shed *config.Shed) error {
	if err := backend.CheckIdentity(ctx, shed.Name, shed.Owner, shed.User, shed.CreatedAt); err != nil {
		log.Printf("Shed %s changed while it was being woken: %v", shed.Name, err)
		return wakeErrorf("shed '%s' not found", shed.Name)
	}
	return nil
}

// wakeFailed is the wakeError for err, a failed guarded call of the wake on
// the named shed: a shed that is not there (or no longer the one that was
// checked, which the guard answers the same way) is "not found"; anything
// else is format, with err.
func wakeFailed(name string, err error, format string) error {
	if errors.Is(err, config.ErrShedNotFoundSentinel) {
		return wakeErrorf("shed '%s' not found", name)
	}
	return wakeErrorf(format, err)
}

// ensureRunningSync is ensureRunning for a Firecracker or VZ backend.
func (s *Server) ensureRunningSync(ctx context.Context, shed *config.Shed, stderr io.Writer, announce bool) (*config.Shed, error) {
	name := shed.Name
	// Auto-start if stopped.
	if shed.Status == config.StatusStopped {
		log.Printf("Auto-starting stopped shed: %s", name)
		if announce {
			fmt.Fprintf(stderr, "Starting shed '%s'...\n", name)
		}
		started, err := s.backend.StartShed(ctx, name)
		if err != nil {
			log.Printf("Failed to start shed %s: %v", name, err)
			return nil, wakeFailed(name, err, "failed to start shed: %v")
		}
		if started != nil {
			if err := stillTheShed(ctx, started); err != nil {
				return nil, err
			}
		}
	}

	// Wait for a shed that is still starting up (either just auto-started
	// above, or already mid-startup from another concurrent connection).
	if shed.Status == config.StatusStopped || shed.Status == config.StatusStarting {
		if err := s.waitForReady(ctx, name); err != nil {
			log.Printf("Shed %s not ready: %v", name, err)
			return nil, wakeFailed(name, err, "shed not ready: %v")
		}
		var err error
		shed, err = s.backend.GetShed(ctx, name)
		if err != nil {
			log.Printf("Failed to get shed %s after start: %v", name, err)
			return nil, wakeFailed(name, err, "failed to get shed after start: %v")
		}
		if err := stillTheShed(ctx, shed); err != nil {
			return nil, err
		}
	}

	if shed.Status != config.StatusRunning {
		log.Printf("Shed %s is not running (status: %s)", name, shed.Status)
		return nil, wakeErrorf("shed '%s' is not running (status: %s)", name, shed.Status)
	}
	return shed, nil
}

// ensureRunningAsync is ensureRunning for an AsyncLifecycle backend.
func (s *Server) ensureRunningAsync(ctx context.Context, al backend.AsyncLifecycle, shed *config.Shed, stderr io.Writer, announce bool) (*config.Shed, error) {
	name := shed.Name
	if shed.Status == config.StatusRunning {
		return shed, nil
	}
	ctx, cancel := context.WithTimeout(ctx, asyncWakeTimeout)
	defer cancel()
	ctx = backend.ContextWithProgress(ctx, func(ev backend.ProgressEvent) {
		if ev.Message != "" && ev.Kind != backend.KindBlob {
			fmt.Fprintf(stderr, "%s\n", ev.Message)
		}
	})

	settle := func() (*config.Shed, error) {
		got, err := al.WaitSettled(ctx, name)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, wakeErrorf("shed not ready: timed out after %s (status: %s)", asyncWakeTimeout, shed.Status)
		case errors.Is(err, config.ErrShedNotFoundSentinel):
			return nil, wakeErrorf("shed '%s' not found", name)
		case err != nil:
			return nil, wakeErrorf("shed not ready: %v", err)
		}
		if err := stillTheShed(ctx, got); err != nil {
			return nil, err
		}
		return got, nil
	}

	var err error
	if shed.Status == config.StatusStopping {
		fmt.Fprintf(stderr, "Waiting for shed '%s' to stop...\n", name)
		if shed, err = settle(); err != nil {
			return nil, err
		}
	}
	if shed.Status == config.StatusStopped {
		log.Printf("Auto-starting stopped shed: %s", name)
		if announce {
			fmt.Fprintf(stderr, "Starting shed '%s'...\n", name)
		}
		// The start is a job the backend owns; a dropped session must not
		// cancel it.
		if shed, err = s.backend.StartShed(context.WithoutCancel(ctx), name); err != nil {
			log.Printf("Failed to start shed %s: %v", name, err)
			return nil, wakeFailed(name, err, "failed to start shed: %v")
		}
		if err := stillTheShed(ctx, shed); err != nil {
			return nil, err
		}
	}
	if shed.Status == config.StatusStarting {
		if shed, err = settle(); err != nil {
			return nil, err
		}
	}
	if shed.Status != config.StatusRunning {
		reason := ""
		if shed.StatusReason != "" {
			reason = ", reason: " + shed.StatusReason
		}
		log.Printf("Shed %s is not running (status: %s%s)", name, shed.Status, reason)
		return nil, wakeErrorf("shed '%s' is not running (status: %s%s)", name, shed.Status, reason)
	}
	return shed, nil
}
