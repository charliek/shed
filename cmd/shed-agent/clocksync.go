package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Clock-sync keeps the guest wall clock correct across host sleep. A VZ guest's
// CLOCK_REALTIME is derived from a counter (arch_sys_counter) that freezes while
// the host sleeps: the guest is paused externally and never sees a resume event,
// so neither the kernel nor systemd-timesyncd re-syncs it on wake, and shed-agent
// keeps running (it doesn't restart). The host-backed RTC keeps real time across
// the pause, so the agent periodically steps CLOCK_REALTIME forward to it. See
// issue #236.
//
// The pure decision/parse logic lives here (untagged) so it unit-tests on macOS
// under `make test`; the syscall glue, the loop, and the runtime tunables
// (interval, threshold, sysfs path) live in clocksync_linux.go.

// minPlausibleClock and maxPlausibleClock bound a sane current-era wall clock. An
// RTC read outside this window is treated as garbage and never used to step the
// clock: the floor rejects a zero/1970 read (which would jump the clock backward),
// and the ceiling rejects a corrupted huge value (which would also overflow time
// arithmetic — e.g. t.UnixNano(), defined only for ~years 1678-2262 — before
// reaching the syscall). The window is intentionally wide; it only has to exclude
// garbage. A time.Time cannot be a const.
var (
	minPlausibleClock = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	maxPlausibleClock = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// shouldStepClock reports whether CLOCK_REALTIME should be stepped forward to the
// host-backed RTC. Forward-only by design: the failure this fixes is the guest
// clock freezing *behind* real time during host sleep. An ahead-of-real clock is
// rare and is left to systemd-timesyncd (which steps both directions over the
// network); forward-only avoids a large backward wall-clock jump disrupting build
// tools, caches, or TLS validity. An RTC read outside [minPlausibleClock,
// maxPlausibleClock) is rejected as garbage.
func shouldStepClock(sys, rtc time.Time, threshold time.Duration) bool {
	if !rtc.After(minPlausibleClock) || !rtc.Before(maxPlausibleClock) {
		return false
	}
	return rtc.Sub(sys) > threshold
}

// readRTCFrom reads a Linux RTC `since_epoch` sysfs node (UTC epoch seconds) and
// returns the wall-clock time it represents. ok=false on a missing/unreadable
// node or non-positive/garbage contents — which is how a backend without an RTC
// (Firecracker x86) self-disables the resync. Pure file read + parse (no
// syscall), so it unit-tests on all platforms.
func readRTCFrom(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}

// clockSyncOutcome records what one reconcile pass did, for logging and tests.
type clockSyncOutcome struct {
	rtcPresent bool
	stepped    bool
	from       time.Time // system clock before the step
	to         time.Time // RTC time we stepped to
	err        error     // non-nil if the step syscall failed
}

// reconcileClock performs one RTC->system reconcile pass using injected
// dependencies, so the read -> decide -> step path is unit-testable without
// touching the real clock. It reads the RTC, decides via shouldStepClock, and
// steps if warranted, returning what it did.
func reconcileClock(
	now func() time.Time,
	readRTC func() (time.Time, bool),
	step func(time.Time) error,
	threshold time.Duration,
) clockSyncOutcome {
	rtc, ok := readRTC()
	if !ok {
		return clockSyncOutcome{rtcPresent: false}
	}
	sys := now()
	if !shouldStepClock(sys, rtc, threshold) {
		return clockSyncOutcome{rtcPresent: true}
	}
	if err := step(rtc); err != nil {
		return clockSyncOutcome{rtcPresent: true, from: sys, to: rtc, err: err}
	}
	return clockSyncOutcome{rtcPresent: true, stepped: true, from: sys, to: rtc}
}
