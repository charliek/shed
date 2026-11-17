//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const ptyPrompt = "Enrolment code (not echoed): "

// promptWriter is the prompt stream: it keeps what is written and, the
// moment the prompt is written, runs onPrompt, as a person who types the
// moment the prompt shows.
type promptWriter struct {
	mu       sync.Mutex
	b        bytes.Buffer
	onPrompt func()
	fired    bool
}

func (p *promptWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.b.Write(b)
	fire := !p.fired && strings.Contains(p.b.String(), ptyPrompt)
	p.fired = p.fired || fire
	p.mu.Unlock()
	if fire && p.onPrompt != nil {
		p.onPrompt()
	}
	return len(b), nil
}

func (p *promptWriter) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.b.String()
}

// ptyCase is a pseudo-terminal for one reading of the code: what the
// terminal shows is collected until the terminal is closed.
type ptyCase struct {
	t     *testing.T
	ptmx  *os.File
	tty   *os.File
	ttyFd int
	shown chan string
}

func newPtyCase(t *testing.T) *ptyCase {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	c := &ptyCase{t: t, ptmx: ptmx, tty: tty, ttyFd: int(tty.Fd()), shown: make(chan string, 1)}
	// Closing the master ends any read still waiting on the terminal.
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	if !c.echoOn() {
		t.Fatal("the terminal starts with echo off")
	}
	go func() {
		b, _ := io.ReadAll(ptmx)
		c.shown <- string(b)
	}()
	return c
}

func (c *ptyCase) echoOn() bool {
	c.t.Helper()
	tio, err := unix.IoctlGetTermios(c.ttyFd, unix.TCGETS)
	if err != nil {
		c.t.Fatal(err)
	}
	return tio.Lflag&unix.ECHO != 0
}

// typeIn writes s at the keyboard.
func (c *ptyCase) typeIn(s string) {
	if _, err := c.ptmx.Write([]byte(s)); err != nil {
		c.t.Error(err)
	}
}

type codeResult struct {
	code string
	err  error
}

// read runs readEnrolmentCode on the terminal under ctx and waits for it.
func (c *ptyCase) read(ctx context.Context, prompt *promptWriter) codeResult {
	c.t.Helper()
	done := make(chan codeResult, 1)
	go func() {
		code, err := readEnrolmentCode(ctx, c.tty, prompt)
		done <- codeResult{code, err}
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		c.t.Fatal("the read did not end")
		return codeResult{}
	}
}

// finish closes the terminal and returns everything it showed.
func (c *ptyCase) finish() string {
	c.t.Helper()
	c.tty.Close()
	select {
	case s := <-c.shown:
		return s
	case <-time.After(10 * time.Second):
		c.t.Fatal("the terminal did not close")
		return ""
	}
}

// TestReadEnrolmentCodeOnATerminal: when standard input is a terminal, echo
// is turned off before the prompt shows, so a code typed the moment the
// prompt appears never shows on the terminal; the code is read under the
// same 65-byte bound as a pipe's, the rest of an over-long line discarded
// (none of it is left for the shell); an interrupt at the prompt ends the
// read at once; echo is back on after each.
func TestReadEnrolmentCodeOnATerminal(t *testing.T) {
	t.Run("typed at the prompt", func(t *testing.T) {
		c := newPtyCase(t)
		const code = "code-typed-6e6e6e"
		prompt := &promptWriter{onPrompt: func() { c.typeIn(code + "\n") }}
		res := c.read(context.Background(), prompt)
		if res.err != nil || res.code != code {
			t.Fatalf("read %q, %v; want %q", res.code, res.err, code)
		}
		if !c.echoOn() {
			t.Error("echo is still off after the code was read")
		}
		if p := prompt.String(); !strings.HasPrefix(p, ptyPrompt) {
			t.Errorf("prompt %q", p)
		}
		if s := c.finish(); strings.Contains(s, code) {
			t.Errorf("the terminal showed the code: %q", s)
		}
	})

	t.Run("over-long", func(t *testing.T) {
		c := newPtyCase(t)
		long := strings.Repeat("0123456789", 10)
		prompt := &promptWriter{onPrompt: func() { c.typeIn(long + "\n") }}
		res := c.read(context.Background(), prompt)
		if !errors.Is(res.err, errCodeTooLong) {
			t.Fatalf("read %q, %v; want the code refused as too long", res.code, res.err)
		}
		if n, err := unix.IoctlGetInt(c.ttyFd, unix.TIOCINQ); err != nil || n != 0 {
			t.Errorf("%d bytes (%v) of the line are left for the next reader of the terminal", n, err)
		}
		if !c.echoOn() {
			t.Error("echo is still off")
		}
		if s := c.finish(); strings.Contains(s, long[:20]) {
			t.Errorf("the terminal showed the input: %q", s)
		}
	})

	t.Run("interrupted at the prompt", func(t *testing.T) {
		c := newPtyCase(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		prompt := &promptWriter{onPrompt: cancel}
		res := c.read(ctx, prompt)
		if !errors.Is(res.err, errInterrupted) {
			t.Fatalf("read %q, %v; want %v", res.code, res.err, errInterrupted)
		}
		if !c.echoOn() {
			t.Error("echo is still off after the interrupt")
		}
		// The abandoned read ends when a line comes; nothing more is shown
		// of it than the newline.
		c.typeIn("\n")
		c.finish()
	})
}
