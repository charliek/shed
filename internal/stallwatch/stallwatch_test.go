package stallwatch

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// recorder collects logged lines; safe for the watcher goroutine.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// setVars swaps the package configuration for one test and restores it after.
func setVars(t *testing.T, first, every time.Duration, logf func(string, ...any)) {
	t.Helper()
	oldFirst, oldEvery, oldLogf := FirstAfter, RepeatEvery, Logf
	t.Cleanup(func() { FirstAfter, RepeatEvery, Logf = oldFirst, oldEvery, oldLogf })
	FirstAfter, RepeatEvery, Logf = first, every, logf
}

// The timing tests run in a synctest bubble: time is virtual and only moves
// when every goroutine in the bubble is blocked, so the assertions are exact
// and immune to scheduler noise. synctest.Wait returns once the watcher has
// handled everything due at the current instant.

func TestStopBeforeFirstLine(t *testing.T) {
	tests := []struct {
		name      string
		stopAfter time.Duration
	}{
		{"immediately", 0},
		{"just before the first line", 10*time.Second - time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &recorder{}
				setVars(t, 10*time.Second, 60*time.Second, rec.logf)

				stop := Watch("checking state directory", "/p")
				time.Sleep(tt.stopAfter)
				synctest.Wait()
				stop()

				time.Sleep(time.Hour)
				synctest.Wait()
				if got := rec.snapshot(); len(got) != 0 {
					t.Fatalf("logged %d line(s) for a watch stopped in time: %q", len(got), got)
				}
			})
		})
	}
}

func TestRepeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		setVars(t, 10*time.Second, 60*time.Second, rec.logf)

		stop := Watch("checking state directory", "/Volumes/ext/shed")
		defer stop()

		steps := []struct {
			advance   time.Duration // from the previous step
			wantLines int
			wantAfter string // in the newest line
		}{
			{10*time.Second - time.Millisecond, 0, ""},
			{2 * time.Millisecond, 1, "after 10s"},
			{60*time.Second - 2*time.Millisecond, 1, ""},
			{2 * time.Millisecond, 2, "after 1m10s"},
			{60 * time.Second, 3, "after 2m10s"},
		}
		for i, step := range steps {
			time.Sleep(step.advance)
			synctest.Wait()
			got := rec.snapshot()
			if len(got) != step.wantLines {
				t.Fatalf("step %d: %d line(s), want %d: %q", i, len(got), step.wantLines, got)
			}
			if step.wantAfter == "" {
				continue
			}
			want := "shed-server: startup is still waiting on /Volumes/ext/shed (checking state directory) " + step.wantAfter
			if newest := got[len(got)-1]; !strings.HasPrefix(newest, want) {
				t.Errorf("step %d: line %q, want prefix %q", i, newest, want)
			}
		}
	})
}

func TestConcurrentAndRepeatedStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		setVars(t, 10*time.Second, 60*time.Second, rec.logf)

		stop := Watch("checking state directory", "/p")
		time.Sleep(11 * time.Second)
		synctest.Wait()

		const callers = 8
		returned := make(chan struct{}, callers)
		for range callers {
			go func() {
				stop()
				returned <- struct{}{}
			}()
		}
		for range callers {
			<-returned
		}
		stop() // and once more, after it is long stopped

		time.Sleep(time.Hour)
		synctest.Wait()
		if got := rec.snapshot(); len(got) != 1 {
			t.Fatalf("got %d line(s), want 1 (logged before stop): %q", len(got), got)
		}
	})
}

// TestStopWaitsForInFlightLine pins the synchronous contract when stop races
// a line: stop must not return while Logf is still running.
func TestStopWaitsForInFlightLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var calls atomic.Int32
		setVars(t, 10*time.Second, 60*time.Second, func(string, ...any) {
			calls.Add(1)
			entered <- struct{}{}
			<-release
		})

		stop := Watch("checking state directory", "/p")
		time.Sleep(10 * time.Second)
		<-entered // the watcher is inside Logf

		stopped := make(chan struct{})
		go func() {
			stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned while a line was still being logged")
		default:
		}

		close(release)
		<-stopped
		time.Sleep(time.Hour)
		synctest.Wait()
		if n := calls.Load(); n != 1 {
			t.Fatalf("Logf called %d times, want 1", n)
		}
	})
}

func TestTimingsCapturedAtWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		setVars(t, 10*time.Second, 60*time.Second, rec.logf)

		stop := Watch("checking state directory", "/p")
		defer stop()

		// None of this may reach the running watch.
		FirstAfter = time.Millisecond
		RepeatEvery = time.Millisecond
		Logf = func(string, ...any) { t.Error("a running watch used a Logf set after Watch") }

		time.Sleep(10*time.Second - time.Millisecond)
		synctest.Wait()
		if got := rec.snapshot(); len(got) != 0 {
			t.Fatalf("first line before the captured FirstAfter: %q", got)
		}
		time.Sleep(2 * time.Millisecond)
		synctest.Wait()
		time.Sleep(60 * time.Second)
		synctest.Wait()
		if got := rec.snapshot(); len(got) != 2 {
			t.Fatalf("got %d line(s) at 70s, want 2 on the captured schedule: %q", len(got), got)
		}
	})
}

func TestContextDoneEndsWatch(t *testing.T) {
	t.Run("logs with its phase until cancelled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rec := &recorder{}
			setVars(t, 10*time.Second, 60*time.Second, rec.logf)

			ctx, cancel := context.WithCancel(context.Background())
			stop := WatchContext(ctx, Resume, "listing sheds", "/i")
			defer stop()

			time.Sleep(11 * time.Second)
			synctest.Wait()
			got := rec.snapshot()
			want := "shed-server: resuming running sheds is still waiting on /i (listing sheds) after 10s"
			if len(got) != 1 || !strings.HasPrefix(got[0], want) {
				t.Fatalf("got %q, want one line with prefix %q", got, want)
			}

			cancel()
			time.Sleep(time.Hour)
			synctest.Wait()
			if got := rec.snapshot(); len(got) != 1 {
				t.Fatalf("logged after the context was done: %q", got)
			}
		})
	})
	t.Run("already done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rec := &recorder{}
			setVars(t, 10*time.Second, 60*time.Second, rec.logf)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			stop := WatchContext(ctx, Resume, "listing sheds", "/i")
			time.Sleep(time.Hour)
			synctest.Wait()
			stop()
			if got := rec.snapshot(); len(got) != 0 {
				t.Fatalf("a watch on a done context logged: %q", got)
			}
		})
	})
}

// TestNoLineAfterStopReturns races stop against lines on the real clock, so
// the race detector sees genuine interleavings. Once any stop has returned,
// the watch must never call Logf again.
func TestNoLineAfterStopReturns(t *testing.T) {
	var stopped atomic.Bool
	var late atomic.Int32
	setVars(t, 0, 0, nil)
	for i := range 200 {
		stopped.Store(false)
		FirstAfter = time.Duration(rand.IntN(200)) * time.Microsecond
		RepeatEvery = 20 * time.Microsecond
		Logf = func(string, ...any) {
			if stopped.Load() {
				late.Add(1)
			}
		}
		stop := Watch("checking state directory", fmt.Sprintf("/p/%d", i))
		time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)

		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stop()
				stopped.Store(true)
			}()
		}
		wg.Wait()
	}
	time.Sleep(5 * time.Millisecond)
	if n := late.Load(); n != 0 {
		t.Fatalf("Logf ran %d time(s) after stop had returned", n)
	}
}

func TestHint(t *testing.T) {
	const exe = "/nonexistent/stallwatch-test/bin/shed-server" // unresolvable: used as given
	tests := []struct {
		name    string
		goos    string
		phase   Phase
		what    string
		elapsed time.Duration
		want    string
	}{
		{
			name: "darwin startup", goos: "darwin", phase: Startup,
			what: "checking state directory", elapsed: 10 * time.Second,
			want: "shed-server: startup is still waiting on /Volumes/ext/shed (checking state directory) after 10s. " +
				"macOS may be waiting for permission to access it (external drives, and folders like Documents, need it). " +
				`Answer the "Shed Server" prompt on this Mac's screen, or grant Full Disk Access to ` + exe +
				" in System Settings → Privacy & Security → Full Disk Access. It continues on its own once access is granted.",
		},
		{
			name: "linux startup", goos: "linux", phase: Startup,
			what: "checking state directory", elapsed: 10 * time.Second,
			want: "shed-server: startup is still waiting on /Volumes/ext/shed (checking state directory) after 10s; " +
				"the filesystem holding it may not be responding.",
		},
		{
			name: "linux resume, minutes", goos: "linux", phase: Resume,
			what: "listing sheds", elapsed: 70*time.Second + 400*time.Millisecond,
			want: "shed-server: resuming running sheds is still waiting on /Volumes/ext/shed (listing sheds) after 1m10s; " +
				"the filesystem holding it may not be responding.",
		},
		{
			name: "sub-second keeps milliseconds", goos: "linux", phase: Startup,
			what: "loading the config", elapsed: 250 * time.Millisecond,
			want: "shed-server: startup is still waiting on /Volumes/ext/shed (loading the config) after 250ms; " +
				"the filesystem holding it may not be responding.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hint(tt.goos, exe, tt.phase, tt.what, "/Volumes/ext/shed", tt.elapsed)
			if got != tt.want {
				t.Errorf("hint()\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestGrantTarget(t *testing.T) {
	// Resolve the temp dir itself: on macOS it lives behind /var -> /private/var.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkfile := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	symlink := func(target, rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	bundleExe := mkfile("Cellar/shed/1.0/libexec/ShedServer.app/Contents/MacOS/shed-server")
	bundle := filepath.Join(root, "Cellar/shed/1.0/libexec/ShedServer.app")
	bareExe := mkfile("Cellar/shed/1.0/bin/shed-server-bare")
	nestedExe := mkfile("Outer.app/Contents/Helpers/Inner.app/Contents/MacOS/helper")

	tests := []struct {
		name string
		exe  string
		want string
	}{
		{"bundle executable", bundleExe, bundle},
		{"bin symlink into the bundle", symlink(bundleExe, "opt/shed/bin/shed-server"), bundle},
		{"relative symlink into the bundle", symlink("../../../Cellar/shed/1.0/libexec/ShedServer.app/Contents/MacOS/shed-server", "opt/shed2/bin/shed-server"), bundle},
		{"bare executable", bareExe, bareExe},
		{"symlink to a bare executable", symlink(bareExe, "bin/shed-server"), bareExe},
		{"innermost bundle", nestedExe, filepath.Join(root, "Outer.app/Contents/Helpers/Inner.app")},
		{"unresolvable path is used as given", filepath.Join(root, "missing/shed-server"), filepath.Join(root, "missing/shed-server")},
		{"unresolvable bundle path still names the bundle", filepath.Join(root, "gone/X.app/Contents/MacOS/x"), filepath.Join(root, "gone/X.app")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := grantTarget(tt.exe); got != tt.want {
				t.Errorf("grantTarget(%q) = %q, want %q", tt.exe, got, tt.want)
			}
		})
	}

	t.Run("darwin hint names the bundle", func(t *testing.T) {
		got := hint("darwin", grantTarget(filepath.Join(root, "opt/shed/bin/shed-server")), Startup, "checking state directory", "/p", 10*time.Second)
		if want := "grant Full Disk Access to " + bundle + " in System Settings"; !strings.Contains(got, want) {
			t.Errorf("hint %q does not contain %q", got, want)
		}
	})
}
