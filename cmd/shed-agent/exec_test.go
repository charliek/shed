//go:build linux
// +build linux

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"os/exec"
	"sync"
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
// relies on. stop()'s wg.Wait establishes happens-before for reading whatever
// the handlers recorded.
func startTestPump(t *testing.T, h messageHandlers) (client net.Conn, stop func()) {
	t.Helper()
	c, s := net.Pipe()
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
			onData: func(b []byte) { got = append(got, b...) },
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
		stop() // wg.Wait inside establishes happens-before for got

		if !bytes.Equal(got, payload) {
			t.Fatalf("payload corrupted/lost: got %d bytes, want %d", len(got), len(payload))
		}
	})

	// Timing regression guard tied to #222: the input pump must NOT impose a
	// per-read deadline. A frame whose payload arrives after a pause longer than
	// the removed 500ms poll interval must still be delivered intact. A
	// regression to deadline-based polling would time out mid-frame and discard
	// the bytes consumed so far. This is the only intentionally slow subtest.
	t.Run("slowPayloadNotTimedOut", func(t *testing.T) {
		var got []byte
		client, stop := startTestPump(t, messageHandlers{
			onData: func(b []byte) { got = append(got, b...) },
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

	// MsgTypeStdinEOF closes stdin (fires onStdinEOF) but does NOT stop the pump:
	// a command can keep running after stdin closes and must still receive later
	// signal frames. Pins the resolved review finding (close-once, keep pumping).
	t.Run("stdinEOFClosesStdinKeepsPumping", func(t *testing.T) {
		eofCount := 0
		sigCh := make(chan int, 2)
		client, stop := startTestPump(t, messageHandlers{
			onStdinEOF: func() { eofCount++ },
			onSignal:   func(sig int) { sigCh <- sig },
		})
		send(t, client, MsgTypeStdinEOF, nil)
		// Pump must still be alive to forward this signal after stdin EOF.
		send(t, client, MsgTypeSignal, mustJSON(t, SignalMessage{Signal: 2}))
		stop()
		if eofCount != 1 {
			t.Fatalf("onStdinEOF fired %d times, want 1", eofCount)
		}
		select {
		case got := <-sigCh:
			if got != 2 {
				t.Fatalf("signal after stdin EOF = %d, want 2", got)
			}
		default:
			t.Fatal("signal after stdin EOF was not forwarded (pump stopped too early)")
		}
	})

	// PTY semantics: onStdinEOF is nil, so MsgTypeStdinEOF is a no-op and the pump
	// keeps delivering later data frames.
	t.Run("stdinEOFIgnoredPTY", func(t *testing.T) {
		var got []byte
		client, stop := startTestPump(t, messageHandlers{
			onData: func(b []byte) { got = append(got, b...) },
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
	t.Run("dataFrameDoesNotStopSignalForwarding", func(t *testing.T) {
		sigCh := make(chan int, 4)
		client, stop := startTestPump(t, messageHandlers{
			onData:   func(b []byte) { /* simulate a process that ignores stdin */ },
			onSignal: func(sig int) { sigCh <- sig },
		})
		send(t, client, MsgTypeData, []byte("ignored"))
		send(t, client, MsgTypeSignal, mustJSON(t, SignalMessage{Signal: 2}))
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
			onSignal: func(sig int) { sigCh <- sig },
		})
		for _, n := range []int{15, 0, 65, 9} { // 0 and 65 are invalid
			send(t, client, MsgTypeSignal, mustJSON(t, SignalMessage{Signal: n}))
		}
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
			onResize: func(rows, cols uint16) { szCh <- size{rows, cols} },
		})
		send(t, client, MsgTypeResize, mustJSON(t, ResizeMessage{Rows: 40, Cols: 120}))
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
			onData: func(b []byte) { dataCalled = true },
		})
		send(t, client, 0x7F, []byte("mystery"))
		stop()
		if dataCalled {
			t.Fatal("unknown message type was routed to onData")
		}
	})

	// Host disconnect (the peer closes the connection) fires onDisconnect so the
	// caller can terminate the command instead of orphaning it.
	t.Run("onDisconnectOnConnClose", func(t *testing.T) {
		called := make(chan struct{}, 1)
		client, stop := startTestPump(t, messageHandlers{
			onDisconnect: func() { called <- struct{}{} },
		})
		_ = client.Close() // peer closes → non-timeout read error (io.EOF)
		select {
		case <-called:
		case <-time.After(2 * time.Second):
			t.Fatal("onDisconnect was not called when the host closed the connection")
		}
		stop()
	})

	// The teardown read deadline (a timeout) must NOT be treated as a disconnect:
	// the process has already exited and the command must not be re-signaled.
	// stop() drives the real clientPump teardown (SetReadDeadline now).
	t.Run("noDisconnectOnTeardownDeadline", func(t *testing.T) {
		disconnected := false
		_, stop := startTestPump(t, messageHandlers{
			onDisconnect: func() { disconnected = true },
		})
		stop() // teardown deadline → timeout, not a disconnect
		if disconnected {
			t.Fatal("onDisconnect fired on the teardown read deadline (should only fire on host close)")
		}
	})
}

// readOutputFrames reads framed output messages from c, folding MsgTypeData into
// gotData and MsgTypeStderr into gotStderr, until the exit-code frame (or a read
// error/EOF for callers that close the conn without one). Unexpected non-output
// frame types fail the test.
func readOutputFrames(t *testing.T, c net.Conn) (gotData, gotStderr []byte) {
	t.Helper()
	var data, stderr bytes.Buffer
	for {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		mt, payload, err := readMessage(c)
		if err != nil {
			break
		}
		switch mt {
		case MsgTypeData:
			data.Write(payload)
		case MsgTypeStderr:
			stderr.Write(payload)
		case MsgTypeExitCode:
			return data.Bytes(), stderr.Bytes()
		default:
			t.Errorf("unexpected frame type 0x%02x on output channel", mt)
		}
	}
	return data.Bytes(), stderr.Bytes()
}

// TestCopyToConnTagsStreams is the deterministic guard: the shared output copier
// frames stdout chunks as MsgTypeData and stderr chunks as MsgTypeStderr, and no
// stderr byte ever lands in a MsgTypeData frame. This is the separation that
// keeps a binary stdout protocol (Zed Remote-SSH, SFTP) uncorrupted by
// interleaved stderr (issue #222 follow-on). It drives the writer directly (no
// process), so it can't flake on scheduling and asserts by frame type, not order.
func TestCopyToConnTagsStreams(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()

	w := &connWriter{conn: s, disconnect: func() {}}
	stdoutPayload := []byte("stdout-bytes-0123456789")
	stderrPayload := []byte("STDERR-bytes-abcdefghij")

	// Two concurrent copiers serialized by connWriter, exactly like runWithoutPTY.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyToConn(w, bytes.NewReader(stdoutPayload), "stdout", MsgTypeData) }()
	go func() { defer wg.Done(); copyToConn(w, bytes.NewReader(stderrPayload), "stderr", MsgTypeStderr) }()
	go func() { wg.Wait(); _ = s.Close() }()

	gotData, gotStderr := readOutputFrames(t, c)
	if !bytes.Equal(gotData, stdoutPayload) {
		t.Errorf("MsgTypeData stream = %q, want %q", gotData, stdoutPayload)
	}
	if !bytes.Equal(gotStderr, stderrPayload) {
		t.Errorf("MsgTypeStderr stream = %q, want %q", gotStderr, stderrPayload)
	}
	if bytes.Contains(gotData, stderrPayload) {
		t.Error("stderr bytes leaked into the MsgTypeData (stdout) stream — fold regression")
	}
}

// TestRunWithoutPTYSeparatesStderr drives the real non-PTY handler with a command
// that writes to both streams and asserts stdout arrives as MsgTypeData and
// stderr as MsgTypeStderr — asserted by frame TYPE (not interleave order), read
// until the exit-code frame.
func TestRunWithoutPTYSeparatesStderr(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()

	cmd := exec.Command("sh", "-c", "printf OUT; printf ERR 1>&2")
	go runWithoutPTY(s, cmd)

	gotData, gotStderr := readOutputFrames(t, c)
	if string(gotData) != "OUT" {
		t.Errorf("stdout (MsgTypeData) = %q, want %q", gotData, "OUT")
	}
	if string(gotStderr) != "ERR" {
		t.Errorf("stderr (MsgTypeStderr) = %q, want %q", gotStderr, "ERR")
	}
	if bytes.Contains(gotData, []byte("ERR")) {
		t.Error("stderr leaked into the stdout stream (fold regression)")
	}
}

// TestRunWithPTYNoStderrFrames asserts the PTY path never emits a MsgTypeStderr
// frame: a pty master is a single merged stream (stdout+stderr), correctly framed
// as MsgTypeData. Exact PTY text is intentionally not asserted (the path may drop
// a small tail on close).
func TestRunWithPTYNoStderrFrames(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()

	cmd := exec.Command("sh", "-c", "printf OUT; printf ERR 1>&2")
	go runWithPTY(s, cmd, 24, 80)

	if _, gotStderr := readOutputFrames(t, c); len(gotStderr) > 0 {
		t.Fatalf("PTY path emitted %d MsgTypeStderr byte(s); PTY output must be a single merged MsgTypeData stream", len(gotStderr))
	}
}
