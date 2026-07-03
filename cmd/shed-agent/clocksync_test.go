package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShouldStepClock(t *testing.T) {
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	const threshold = 30 * time.Second

	tests := []struct {
		name string
		sys  time.Time
		rtc  time.Time
		want bool
	}{
		{"system far behind RTC steps", base, base.Add(10 * time.Minute), true},
		{"system just over threshold behind steps", base, base.Add(31 * time.Second), true},
		{"system exactly at threshold does not step", base, base.Add(30 * time.Second), false},
		{"system within threshold does not step", base, base.Add(5 * time.Second), false},
		{"system ahead of RTC does not step (forward-only)", base, base.Add(-10 * time.Minute), false},
		// RTC below the plausibility floor is rejected even though it is far
		// "ahead" of an equally implausible system clock (proves the floor gates
		// a would-be forward step, not just the direction check).
		{"rtc below floor does not step", time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC), false},
		{"rtc at floor does not step", minPlausibleClock.Add(-time.Hour), minPlausibleClock, false},
		// RTC above the plausibility ceiling — a corrupted since_epoch that would
		// otherwise be seen as "far ahead" and overflow time arithmetic — is
		// rejected rather than stepped to.
		{"rtc above ceiling does not step", base, maxPlausibleClock.Add(time.Hour), false},
		{"rtc at an absurd future value does not step", base, time.Unix(1<<62, 0).UTC(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldStepClock(tt.sys, tt.rtc, threshold); got != tt.want {
				t.Errorf("shouldStepClock(sys=%v, rtc=%v) = %v, want %v", tt.sys, tt.rtc, got, tt.want)
			}
		})
	}
}

func TestReadRTCFrom(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name    string
		path    string
		wantOK  bool
		wantSec int64
	}{
		{"valid", write("valid", "1783058612"), true, 1783058612},
		{"trailing newline", write("nl", "1783058612\n"), true, 1783058612}, // real sysfs always has one
		{"surrounding whitespace", write("ws", "  1783058612 \n"), true, 1783058612},
		{"empty", write("empty", ""), false, 0},
		{"non-numeric", write("garbage", "not-a-number"), false, 0},
		{"negative", write("neg", "-5"), false, 0},
		{"zero", write("zero", "0"), false, 0},
		{"overflow", write("of", "999999999999999999999999"), false, 0},
		{"missing file", filepath.Join(dir, "does-not-exist"), false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := readRTCFrom(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("readRTCFrom(%s) ok = %v, want %v", tt.name, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Unix() != tt.wantSec {
				t.Errorf("readRTCFrom(%s) = %d, want %d", tt.name, got.Unix(), tt.wantSec)
			}
			if got.Location() != time.UTC {
				t.Errorf("readRTCFrom(%s) location = %v, want UTC", tt.name, got.Location())
			}
		})
	}
}

func TestReconcileClock(t *testing.T) {
	sysTime := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	const threshold = 30 * time.Second
	now := func() time.Time { return sysTime }

	t.Run("no RTC is a no-op", func(t *testing.T) {
		stepCalled := false
		out := reconcileClock(now,
			func() (time.Time, bool) { return time.Time{}, false },
			func(time.Time) error { stepCalled = true; return nil },
			threshold)
		if out.rtcPresent || out.stepped || stepCalled {
			t.Errorf("got %+v, stepCalled=%v; want no-op", out, stepCalled)
		}
	})

	t.Run("within threshold present but no step", func(t *testing.T) {
		stepCalled := false
		out := reconcileClock(now,
			func() (time.Time, bool) { return sysTime.Add(5 * time.Second), true },
			func(time.Time) error { stepCalled = true; return nil },
			threshold)
		if !out.rtcPresent || out.stepped || stepCalled {
			t.Errorf("got %+v, stepCalled=%v; want present, no step", out, stepCalled)
		}
	})

	t.Run("far behind steps forward to the RTC time", func(t *testing.T) {
		rtc := sysTime.Add(10 * time.Minute)
		var steppedTo time.Time
		out := reconcileClock(now,
			func() (time.Time, bool) { return rtc, true },
			func(to time.Time) error { steppedTo = to; return nil },
			threshold)
		if !out.stepped || !out.to.Equal(rtc) || !out.from.Equal(sysTime) {
			t.Errorf("got %+v; want stepped from %v to %v", out, sysTime, rtc)
		}
		if !steppedTo.Equal(rtc) {
			t.Errorf("step called with %v, want %v", steppedTo, rtc)
		}
	})

	// Regression: a corrupted RTC that reads a huge, parseable value must not be
	// stepped to (it would overflow the clock-set syscall / jump the clock wildly).
	// The node reads fine, so rtcPresent stays true; the value is just rejected.
	t.Run("implausibly-far-future RTC is present but not stepped to", func(t *testing.T) {
		stepCalled := false
		out := reconcileClock(now,
			func() (time.Time, bool) { return maxPlausibleClock.Add(24 * time.Hour), true },
			func(time.Time) error { stepCalled = true; return nil },
			threshold)
		if out.stepped || stepCalled {
			t.Errorf("got %+v, stepCalled=%v; want no step for implausible RTC", out, stepCalled)
		}
		if !out.rtcPresent {
			t.Error("got rtcPresent=false; want true (RTC readable, value out of range)")
		}
	})

	t.Run("step error is reported, not swallowed", func(t *testing.T) {
		rtc := sysTime.Add(10 * time.Minute)
		wantErr := errors.New("boom")
		out := reconcileClock(now,
			func() (time.Time, bool) { return rtc, true },
			func(time.Time) error { return wantErr },
			threshold)
		if out.stepped || !errors.Is(out.err, wantErr) {
			t.Errorf("got %+v; want stepped=false, err=%v", out, wantErr)
		}
	})
}
