package api

import (
	"bufio"
	"bytes"
	"io"
	"sync"
	"time"
)

// hubBreaker is a per-shed circuit breaker over rc hub start attempts. After
// rcHubBreakerThreshold failed starts within rcHubBreakerWindow, allow() reports
// false for the remainder of the window so a shed whose hub refuses to start
// (e.g. FC loopback-unreachable, a broken binary) can't drive an exec storm — the
// proxy returns 503 immediately instead. A successful start resets the shed.
//
// now is injectable so the window/threshold behavior is testable without sleeps.
type hubBreaker struct {
	mu  sync.Mutex
	m   map[string]*breakerEntry
	now func() time.Time
}

type breakerEntry struct {
	failures    int
	windowStart time.Time
	openUntil   time.Time
}

func newHubBreaker() *hubBreaker {
	return &hubBreaker{m: map[string]*breakerEntry{}, now: time.Now}
}

// allow reports whether a start attempt for shed is permitted right now (the
// breaker is closed).
func (b *hubBreaker) allow(shed string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[shed]
	if e == nil {
		return true
	}
	return !b.now().Before(e.openUntil)
}

// failure records a failed start. Failures are counted within a sliding window;
// crossing the threshold opens the breaker until the window elapses.
func (b *hubBreaker) failure(shed string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	e := b.m[shed]
	if e == nil {
		e = &breakerEntry{}
		b.m[shed] = e
	}
	// Reset the window if the last failure aged out.
	if e.windowStart.IsZero() || now.Sub(e.windowStart) > rcHubBreakerWindow {
		e.windowStart = now
		e.failures = 0
	}
	e.failures++
	if e.failures >= rcHubBreakerThreshold {
		e.openUntil = now.Add(rcHubBreakerWindow)
	}
}

// success clears a shed's breaker state (a hub is confirmed up).
func (b *hubBreaker) success(shed string) { b.reset(shed) }

// reset drops a shed's breaker entry entirely. Called on hub-confirmed-up
// (success) and on shed lifecycle transitions (stop/start/reset/delete — see
// Server.invalidateShedRC): a transitioned shed's past start failures no longer
// predict anything, and dropping the entry also keeps the map from accumulating
// entries for failing or deleted sheds (the rcCaps.invalidate precedent).
func (b *hubBreaker) reset(shed string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, shed)
}

// newLineCapReadCloser wraps r so no single line exceeds maxLine bytes. It is used
// on the proxied SSE events path where the whole-body 2 MiB cap is inappropriate
// (the stream is unbounded in length) but an individual unbounded line would be a
// memory hazard on the reading side. A line longer than maxLine is truncated to
// maxLine bytes followed by a newline; the underlying stream keeps flowing.
func newLineCapReadCloser(r io.ReadCloser, maxLine int) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer r.Close()
		br := bufio.NewReader(r)
		lineLen := 0      // bytes emitted for the current (not-yet-terminated) line
		overLine := false // current line already hit the cap; drop until '\n'
		buf := make([]byte, 32<<10)
		for {
			n, err := br.Read(buf)
			seg := buf[:n]
			for len(seg) > 0 {
				// Emit up to the next newline (inclusive), capping the line.
				nl := bytes.IndexByte(seg, '\n')
				var chunk []byte
				var hadNL bool
				if nl >= 0 {
					chunk = seg[:nl] // content before '\n'
					seg = seg[nl+1:]
					hadNL = true
				} else {
					chunk = seg
					seg = nil
				}
				if !overLine {
					room := maxLine - lineLen
					if len(chunk) > room {
						chunk = chunk[:room]
						overLine = true
					}
					if len(chunk) > 0 {
						if _, werr := pw.Write(chunk); werr != nil {
							return
						}
						lineLen += len(chunk)
					}
				}
				if hadNL {
					if _, werr := pw.Write([]byte{'\n'}); werr != nil {
						return
					}
					lineLen = 0
					overLine = false
				}
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr
}
