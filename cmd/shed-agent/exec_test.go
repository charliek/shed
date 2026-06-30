//go:build linux
// +build linux

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// send writes a framed message to c, failing the test on error. It sets a write
// deadline so that a regression which makes the pump stop reading produces a
// clean failure instead of hanging the test until the package timeout
// (net.Pipe is synchronous: a write blocks until the pump reads).
func send(t *testing.T, c net.Conn, msgType byte, data []byte) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, msgType, data); err != nil {
		t.Fatalf("write 0x%02x: %v", msgType, err)
	}
}

// startTestPump runs the production input pump over a net.Pipe and returns the
// client end (the test writes framed messages into it) plus a stop() that drives
// the real teardown (clientPump.stop) and closes the pipe. net.Pipe is
// synchronous and supports deadlines (Go 1.10+), which is what the teardown
// relies on.
func startTestPump(t *testing.T, h messageHandlers) (client net.Conn, stop func()) {
	t.Helper()
	c, s := net.Pipe()
	// Fail fast (rather than hang to the package timeout) if a regression makes
	// the pump stop reading while a test is still writing raw frames into c.
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	pump := startClientPump(s, h, nil)
	return c, func() {
		pump.stop()
		_ = c.Close()
		_ = s.Close()
	}
}

func TestPumpClientMessages(t *testing.T) {
	// Fidelity guard: a single frame whose header and payload are delivered
	// across several separate writes must be reassembled and handed to onData
	// complete and in order. This proves the blocking reader reassembles a split
	// frame correctly; the timing-specific #222 regression (losing bytes when a
	// per-read deadline fires mid-frame) is covered by slowPayloadNotTimedOut
	// below and by the one-time pre-fix demo recorded in the PR.
	t.Run("splitFrameDeliveredIntact", func(t *testing.T) {
		var got []byte
		client, stop := startTestPump(t, messageHandlers{
			onData:         func(b []byte) { got = append(got, b...) },
			stopOnStdinEOF: true,
		})

		payload := make([]byte, 9000) // larger than a single pipe read chunk
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		hdr := make([]byte, 5)
		hdr[0] = MsgTypeData
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))

		// Deliver the frame in pieces: header, then the payload split across two
		// writes, so the agent's io.ReadFull must reassemble across reads.
		if _, err := client.Write(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := client.Write(payload[:4000]); err != nil {
			t.Fatalf("write payload part 1: %v", err)
		}
		if _, err := client.Write(payload[4000:]); err != nil {
			t.Fatalf("write payload part 2: %v", err)
		}
		send(t, client, MsgTypeStdinEOF, nil) // stop the pump
		stop()                                // wg.Wait inside establishes happens-before for got

		if !bytes.Equal(got, payload) {
			t.Fatalf("payload corrupted/lost: got %d bytes, want %d", len(got), len(payload))
		}
	})

	// Timing regression guard tied to #222: the input pump must NOT impose a
	// per-read deadline. A frame whose payload arrives after a pause longer than
	// the removed 500ms poll interval must still be delivered intact. A
	// regression to deadline-based polling would time out mid-frame and discard
	// the bytes consumed so far (the pre-fix behavior, demonstrated against the
	// old code in the PR). This is the only intentionally slow subtest (~600ms).
	t.Run("slowPayloadNotTimedOut", func(t *testing.T) {
		var got []byte
		client, stop := startTestPump(t, messageHandlers{
			onData:         func(b []byte) { got = append(got, b...) },
			stopOnStdinEOF: true,
		})
		payload := []byte("payload-after-a-long-pause")
		hdr := make([]byte, 5)
		hdr[0] = MsgTypeData
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))

		if _, err := client.Write(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		// Pause longer than the removed 500ms poll deadline before the payload.
		// The blocking pump waits; a deadline-polling regression would not.
		time.Sleep(600 * time.Millisecond)
		if _, err := client.Write(payload); err != nil {
			t.Fatalf("write payload: %v", err)
		}
		send(t, client, MsgTypeStdinEOF, nil)
		stop()

		if string(got) != string(payload) {
			t.Fatalf("payload lost across pause: got %q want %q", got, payload)
		}
	})

	// After the process exits the caller's pump.stop() sets a read deadline; the
	// blocked pump must return promptly. Proves the teardown contract is
	// deterministic (and exercises the real clientPump.stop path).
	t.Run("teardownDeadlineUnblocks", func(t *testing.T) {
		c, s := net.Pipe()
		defer c.Close()
		defer s.Close()
		pump := startClientPump(s, messageHandlers{}, nil) // blocks: nothing is ever written
		done := make(chan struct{})
		go func() { defer close(done); pump.stop() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("pump.stop did not return (SetReadDeadline failed to unblock the blocking read)")
		}
	})

	// Non-PTY semantics: MsgTypeStdinEOF stops the pump on its own. The cleanup
	// callback fires when the pump returns, so closing it signals "stopped".
	t.Run("stdinEOFStopsNonPTY", func(t *testing.T) {
		c, s := net.Pipe()
		defer c.Close()
		defer s.Close()
		done := make(chan struct{})
		startClientPump(s, messageHandlers{stopOnStdinEOF: true}, func() { close(done) })
		send(t, c, MsgTypeStdinEOF, nil)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("pump did not stop on MsgTypeStdinEOF")
		}
	})

	// PTY semantics: MsgTypeStdinEOF is ignored (no separate stdin pipe), so the
	// pump keeps running and still delivers later data frames.
	t.Run("stdinEOFIgnoredPTY", func(t *testing.T) {
		var got []byte
		client, stop := startTestPump(t, messageHandlers{
			onData:         func(b []byte) { got = append(got, b...) },
			stopOnStdinEOF: false,
		})
		send(t, client, MsgTypeStdinEOF, nil)
		send(t, client, MsgTypeData, []byte("after-eof"))
		stop()
		if string(got) != "after-eof" {
			t.Fatalf("expected data delivered after ignored StdinEOF, got %q", got)
		}
	})

	// onData failing/continuing must NOT stop the pump: a process that closed its
	// own stdin can still be running and must keep receiving forwarded signals.
	// This pins the resolved review contradiction (log-and-continue, not stop).
	t.Run("dataFrameDoesNotStopSignalForwarding", func(t *testing.T) {
		sigCh := make(chan int, 4)
		client, stop := startTestPump(t, messageHandlers{
			onData:         func(b []byte) { /* simulate a process that ignores stdin */ },
			onSignal:       func(sig int) { sigCh <- sig },
			stopOnStdinEOF: true,
		})
		send(t, client, MsgTypeData, []byte("ignored"))
		send(t, client, MsgTypeSignal, mustJSON(t, SignalMessage{Signal: 2}))
		send(t, client, MsgTypeStdinEOF, nil)
		stop()
		close(sigCh)
		var sigs []int
		for s := range sigCh {
			sigs = append(sigs, s)
		}
		if len(sigs) != 1 || sigs[0] != 2 {
			t.Fatalf("expected signal 2 forwarded after data frame, got %v", sigs)
		}
	})

	// Signal dispatch: valid numbers reach onSignal; out-of-range numbers are
	// logged and dropped (never forwarded).
	t.Run("signalDispatchAndValidation", func(t *testing.T) {
		sigCh := make(chan int, 8)
		client, stop := startTestPump(t, messageHandlers{
			onSignal:       func(sig int) { sigCh <- sig },
			stopOnStdinEOF: true,
		})
		for _, n := range []int{15, 0, 65, 9} { // 0 and 65 are invalid
			send(t, client, MsgTypeSignal, mustJSON(t, SignalMessage{Signal: n}))
		}
		send(t, client, MsgTypeStdinEOF, nil)
		stop()
		close(sigCh)
		var sigs []int
		for s := range sigCh {
			sigs = append(sigs, s)
		}
		want := []int{15, 9}
		if len(sigs) != len(want) || sigs[0] != want[0] || sigs[1] != want[1] {
			t.Fatalf("expected only valid signals %v forwarded, got %v", want, sigs)
		}
	})

	// Resize dispatch (PTY path).
	t.Run("resizeDispatch", func(t *testing.T) {
		type size struct{ rows, cols uint16 }
		szCh := make(chan size, 2)
		client, stop := startTestPump(t, messageHandlers{
			onResize:       func(rows, cols uint16) { szCh <- size{rows, cols} },
			stopOnStdinEOF: true,
		})
		send(t, client, MsgTypeResize, mustJSON(t, ResizeMessage{Rows: 40, Cols: 120}))
		send(t, client, MsgTypeStdinEOF, nil)
		stop()
		select {
		case got := <-szCh:
			if got.rows != 40 || got.cols != 120 {
				t.Fatalf("resize = %dx%d, want 40x120", got.rows, got.cols)
			}
		default:
			t.Fatal("onResize was not called")
		}
	})

	// Unknown/future message types are logged and skipped — never written into
	// the child via onData.
	t.Run("unknownTypeIgnored", func(t *testing.T) {
		dataCalled := false
		client, stop := startTestPump(t, messageHandlers{
			onData:         func(b []byte) { dataCalled = true },
			stopOnStdinEOF: true,
		})
		send(t, client, 0x7F, []byte("mystery"))
		send(t, client, MsgTypeStdinEOF, nil)
		stop()
		if dataCalled {
			t.Fatal("unknown message type was routed to onData")
		}
	})
}
