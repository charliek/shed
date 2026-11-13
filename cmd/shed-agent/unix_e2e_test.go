//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/vmutil"
	"github.com/mdlayher/vsock"
)

// The vsock library's conn has CloseWrite, so the tcpproxy half-close fix
// applies on Firecracker and VZ as well as on unix sockets.
var (
	_ interface{ CloseWrite() error } = (*vsock.Conn)(nil)
	_ interface{ CloseWrite() error } = (*net.UnixConn)(nil)
)

// unixDialer reaches an agent listening with --listen unix:<dir>, the way a
// link does.
type unixDialer struct{ dir string }

func (d unixDialer) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", filepath.Join(d.dir, guest.SocketName(port)))
}

// startUnixAgent runs a real agent Server on unix sockets in a temp dir.
func startUnixAgent(t *testing.T) (*Server, unixDialer) {
	t.Helper()
	base, err := os.MkdirTemp("", "sa-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	dir := filepath.Join(base, "agent")

	lc, err := parseListen("unix:"+dir, "")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(guest.ConsolePort, guest.NotifyPort, guest.TCPProxyPort, 0, "e2e")
	s.listenCfg = lc
	s.info = guest.AgentInfo{Version: "v-e2e", Protocol: guest.AgentProtocol, Layout: guest.Layout}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s, unixDialer{dir: dir}
}

func TestUnixAgentEndToEnd(t *testing.T) {
	s, d := startUnixAgent(t)
	client := vmutil.NewAgentClient(d, guest.ConsolePort, guest.NotifyPort)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("probe reports version protocol and layout", func(t *testing.T) {
		info, err := client.Probe(ctx)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if info != s.info {
			t.Errorf("Probe = %+v, want %+v", info, s.info)
		}
		if !guest.ProtocolCompatible(info.Protocol) || !guest.LayoutSupported(info.Layout) {
			t.Errorf("Probe = %+v not compatible", info)
		}
		if err := client.WaitForHealth(ctx, 5*time.Second); err != nil {
			t.Fatalf("WaitForHealth: %v", err)
		}
		if s.currentMsgConn() != nil {
			t.Error("a probe was promoted to the message channel")
		}
	})

	t.Run("exec", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := client.Exec(ctx, backend.ExecOptions{
			Cmd:    []string{"/bin/sh", "-c", "read x; echo out:$x; echo err >&2"},
			Stdin:  io.NopCloser(strings.NewReader("hi\n")),
			Stdout: vmutil.NopWriteCloser(&stdout),
			Stderr: vmutil.NopWriteCloser(&stderr),
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if stdout.String() != "out:hi\n" || stderr.String() != "err\n" {
			t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
		var exitErr *vmutil.ExitError
		err = client.Exec(ctx, backend.ExecOptions{Cmd: []string{"/bin/sh", "-c", "exit 3"}})
		if !errors.As(err, &exitErr) || exitErr.Code != 3 {
			t.Errorf("Exec exit 3: %v", err)
		}
	})

	// HTTP/1.0 style: the guest service writes and closes without reading.
	// The host never closes its write side, so it sees EOF only if the
	// agent half-closes the proxied connection.
	t.Run("proxy guest-to-host EOF", func(t *testing.T) {
		port := serveTCP(t, func(c net.Conn) {
			_, _ = c.Write([]byte("hello"))
		})
		conn, err := vmutil.DialService(ctx, d, guest.TCPProxyPort, port)
		if err != nil {
			t.Fatalf("DialService: %v", err)
		}
		defer conn.Close()
		got, err := readAllWithin(conn, 3*time.Second)
		if err != nil {
			t.Fatalf("read to EOF: %v (got %q)", err, got)
		}
		if got != "hello" {
			t.Errorf("got %q, want hello", got)
		}
	})

	// Client-first half-close: the guest service reads to EOF, then answers.
	t.Run("proxy host-to-guest EOF then reply", func(t *testing.T) {
		port := serveTCP(t, func(c net.Conn) {
			data, _ := io.ReadAll(c)
			_, _ = fmt.Fprintf(c, "got:%d", len(data))
		})
		conn, err := vmutil.DialService(ctx, d, guest.TCPProxyPort, port)
		if err != nil {
			t.Fatalf("DialService: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		cw, ok := conn.(interface{ CloseWrite() error })
		if !ok {
			t.Fatalf("%T has no CloseWrite", conn)
		}
		if err := cw.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		got, err := readAllWithin(conn, 3*time.Second)
		if err != nil {
			t.Fatalf("read reply: %v (got %q)", err, got)
		}
		if got != "got:4" {
			t.Errorf("got %q, want got:4", got)
		}
	})

	t.Run("message channel survives probes", func(t *testing.T) {
		conn, err := d.Dial(ctx, guest.NotifyPort)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		sendEnvelope(t, conn, healthRequest(t, nil))
		env, err := readEnvelope(t, conn, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// The reply and the first heartbeat may come in either order.
		for !isHeartbeat(env) {
			if env, err = readEnvelope(t, conn, 2*time.Second); err != nil {
				t.Fatal(err)
			}
		}
		for range 3 {
			if _, err := client.Probe(ctx); err != nil {
				t.Fatalf("Probe: %v", err)
			}
		}
		if s.currentMsgConn() == nil {
			t.Fatal("message channel lost after probes")
		}
	})
}

// serveTCP runs handle for each connection to a loopback listener and
// closes the connection when handle returns.
func serveTCP(t *testing.T, handle func(net.Conn)) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

// readAllWithin reads to EOF, failing if EOF does not come in time. It uses
// a timer and Close rather than a deadline, so it works on any conn.
func readAllWithin(conn net.Conn, d time.Duration) (string, error) {
	timer := time.AfterFunc(d, func() { _ = conn.Close() })
	defer timer.Stop()
	data, err := io.ReadAll(conn)
	if err != nil {
		return string(data), err
	}
	if !timer.Stop() {
		return string(data), fmt.Errorf("no EOF within %v", d)
	}
	return string(data), nil
}
