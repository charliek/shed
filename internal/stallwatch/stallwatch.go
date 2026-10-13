// Package stallwatch makes a blocked filesystem touch visible in the server
// log.
//
// A watch logs one line naming the path it is waiting on after FirstAfter,
// then again every RepeatEvery, until it is stopped. It never cancels, times
// out or exits: the operation it watches is left to finish on its own. On
// macOS the usual cause is a permission prompt for an external drive that
// nobody has answered yet, and a server that gave up and was restarted would
// only stack more prompts and throw away a permission granted late (#304). On
// Linux the same silent stall comes from a wedged network filesystem.
//
// Call discipline: start a watch right before the operation and call stop
// right after it returns, on every path. Never defer stop to the end of a
// long-lived function, which would keep the watch alive after the operation
// is done.
package stallwatch

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FirstAfter, RepeatEvery and Logf configure every watch started after they
// are set. Watch captures all three when it is called, so changing them never
// affects a watch that is already running.
var (
	// FirstAfter is how long an operation may run before the first line.
	FirstAfter = 10 * time.Second

	// RepeatEvery is the interval between later lines.
	RepeatEvery = 60 * time.Second

	// Logf receives each line.
	Logf = log.Printf
)

// Phase names what the server is doing while it waits. It opens the logged
// line ("<phase> is still waiting on ...").
type Phase string

const (
	// Startup is the phase of a watch on the path to "Shed server is ready".
	Startup Phase = "startup"

	// Resume is the phase of a watch in the background walk that re-opens
	// running sheds' message channels after a restart.
	Resume Phase = "resuming running sheds"
)

// Watch starts a Startup watch on an operation that touches path. what says
// what the operation is doing there, e.g. "checking state directory".
//
// stop is idempotent and safe to call from several goroutines. It is
// synchronous: once it returns, the watch's timer is stopped, its goroutine
// has exited, and it will log nothing more.
func Watch(what, path string) (stop func()) {
	return WatchContext(context.Background(), Startup, what, path)
}

// WatchContext is Watch with an explicit phase, for a watch that runs inside
// a cancellable operation. The watch also ends, silently, when ctx is done: a
// syscall blocked on an unresponsive filesystem may never return, so the
// caller's stop may never run. stop is still required on every path.
func WatchContext(ctx context.Context, phase Phase, what, path string) (stop func()) {
	first, every, logf := FirstAfter, RepeatEvery, Logf
	if runtime.GOOS == "darwin" {
		resolveTarget()
	}
	start := time.Now()
	quit := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		timer := time.NewTimer(first)
		defer timer.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			// A stop that arrived together with the tick wins: when select
			// has several ready cases it picks one at random.
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			default:
			}
			logf("%s", hint(runtime.GOOS, currentTarget(), phase, what, path, time.Since(start)))
			timer.Reset(every)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() { close(quit) })
		// Every caller waits for the goroutine, so a line being logged as
		// stop is called is finished before any stop returns.
		<-done
	}
}

// The Full Disk Access target is resolved once, in the background. Resolving
// it touches the filesystem, and the watch goroutine must never block on
// anything but its own timer: stop waits for that goroutine to exit.
var (
	targetOnce     sync.Once
	targetResolved = make(chan struct{})
	resolvedTarget atomic.Pointer[grant]
)

// resolveTarget starts resolving the Full Disk Access target, once per process.
func resolveTarget() {
	targetOnce.Do(func() {
		go func() {
			g := grantTarget(executable())
			resolvedTarget.Store(&g)
			close(targetResolved)
		}()
	})
}

// currentTarget returns the resolved target when it is ready, and otherwise
// the unresolved executable (or its bundle, judged from the path alone). It
// never touches the filesystem.
func currentTarget() grant {
	if g := resolvedTarget.Load(); g != nil {
		return *g
	}
	return bundleOrSelf(executable())
}

// FullDiskAccessTarget names what an operator should grant Full Disk Access
// to for the running server: its app bundle when it runs from one, otherwise
// its executable. Symlinks are resolved first, so a launch through a bin/
// symlink into a bundle names the bundle. It waits up to two seconds for the
// resolution, then falls back to the unresolved path.
func FullDiskAccessTarget() string {
	resolveTarget()
	select {
	case <-targetResolved:
	case <-time.After(2 * time.Second):
	}
	return currentTarget().path
}

// executable returns the running binary's path, or its name when the OS
// cannot say. It does not touch the filesystem: on darwin the path comes from
// the process's launch arguments, on Linux from /proc.
func executable() string {
	p, err := os.Executable()
	if err != nil {
		return "shed-server"
	}
	return p
}

// appBundleMarker is what a path contains when its executable runs from inside
// a macOS app bundle.
const appBundleMarker = ".app/Contents/MacOS/"

// grant is what the stall line tells an operator: the path to grant Full Disk
// Access to, and the name macOS shows in its access prompt for it.
type grant struct {
	path   string
	prompt string
}

// grantTarget resolves exePath's symlinks and returns the enclosing .app when
// the resolved path is inside a bundle, otherwise the resolved executable. A
// path that cannot be resolved is used as given.
func grantTarget(exePath string) grant {
	resolved, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		resolved = exePath
	}
	return bundleOrSelf(resolved)
}

// bundleOrSelf returns the enclosing .app when p is inside a bundle, otherwise
// p itself. It is purely lexical. macOS names the responsible process in its
// prompt: the bundle's display name ("Shed Server", from ShedServer.app's
// Info.plist) for a bundle, the executable's own name for a bare binary such
// as a development build.
func bundleOrSelf(p string) grant {
	// LastIndex picks the innermost bundle, the one that is executing.
	if i := strings.LastIndex(p, appBundleMarker); i >= 0 {
		return grant{path: p[:i+len(".app")], prompt: "Shed Server"}
	}
	return grant{path: p, prompt: filepath.Base(p)}
}

// hint is the line a stalled watch logs. goos selects the wording: on darwin
// the likely cause is an unanswered permission prompt, elsewhere an
// unresponsive filesystem. target is what to grant Full Disk Access to and the
// prompt's name for it, already resolved; hint itself touches nothing.
func hint(goos string, target grant, phase Phase, what, path string, elapsed time.Duration) string {
	waiting := fmt.Sprintf("shed-server: %s is still waiting on %s (%s) after %s",
		phase, path, what, formatElapsed(elapsed))
	if goos != "darwin" {
		return waiting + "; the filesystem holding it may not be responding."
	}
	return waiting + ". macOS may be waiting for permission to access it " +
		"(external drives, and folders like Documents, need it). " +
		"Answer the " + strconv.Quote(target.prompt) + " prompt on this Mac's screen, or grant Full Disk Access to " +
		target.path +
		" in System Settings → Privacy & Security → Full Disk Access. " +
		"It continues on its own once access is granted."
}

// formatElapsed rounds to whole seconds, keeping milliseconds only below one
// second.
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
