//go:build linux
// +build linux

package firecracker

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hugelgupf/p9/p9"
)

func TestP9Server_StartStop(t *testing.T) {
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}

	// Verify the server has a valid address before starting
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("Addr() returned empty string")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error = %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", host)
	}
	if port == "" || port == "0" {
		t.Errorf("port = %q, want a dynamically assigned port", port)
	}

	srv.Start()

	// Verify we can connect to the listening address
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to server at %s: %v", addr, err)
	}
	conn.Close()

	// Close the server
	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Verify the server is no longer accepting connections
	_, err = net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		t.Error("expected connection to fail after Close(), but it succeeded")
	}
}

func TestP9Server_ReadWrite(t *testing.T) {
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	// Connect a 9P client
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
	if err != nil {
		conn.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	// Attach to get the root directory
	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}

	// Walk with empty names to clone the root fid before Create mutates it.
	// After root.Create(), root becomes the created file (9P semantics), so
	// we need a separate fid to walk back to the file for reading.
	_, root2, err := root.Walk(nil)
	if err != nil {
		root.Close()
		t.Fatalf("Walk(nil) to clone root fid: %v", err)
	}

	// Create a file (this consumes root, which now represents the new file)
	testContent := []byte("hello 9P world")
	_, _, _, err = root.Create("testfile.txt", p9.ReadWrite, 0644, p9.NoUID, p9.NoGID)
	if err != nil {
		root.Close()
		root2.Close()
		t.Fatalf("Create() error = %v", err)
	}

	// Write content via root (which is now the created file)
	n, err := root.WriteAt(testContent, 0)
	if err != nil {
		root.Close()
		root2.Close()
		t.Fatalf("WriteAt() error = %v", err)
	}
	if n != len(testContent) {
		root.Close()
		root2.Close()
		t.Errorf("WriteAt() wrote %d bytes, want %d", n, len(testContent))
	}
	root.Close()

	// Walk to the file from the cloned root fid and read it
	_, readFile, err := root2.Walk([]string{"testfile.txt"})
	if err != nil {
		root2.Close()
		t.Fatalf("Walk() error = %v", err)
	}
	defer readFile.Close()
	root2.Close()

	_, _, err = readFile.Open(p9.ReadOnly)
	if err != nil {
		t.Fatalf("Open(ReadOnly) error = %v", err)
	}

	buf := make([]byte, 1024)
	n, err = readFile.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt() error = %v", err)
	}

	got := string(buf[:n])
	want := string(testContent)
	if got != want {
		t.Errorf("ReadAt() = %q, want %q", got, want)
	}
}

func TestP9Server_ReadOnly(t *testing.T) {
	// The readOnly field on P9Server is stored but not enforced at the server
	// level (the localfs attacher doesn't respect it). The readOnly flag is
	// used for mount options in the guest (MS_RDONLY). This test verifies the
	// server still starts and operates even when readOnly=true, and that the
	// field is correctly stored.
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", true, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	if !srv.readOnly {
		t.Error("readOnly should be true")
	}

	// Connect and verify the server works
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
	if err != nil {
		conn.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	defer root.Close()

	// The server still allows writes because readOnly is not enforced at
	// the 9P server level -- it's enforced by the guest mount (MS_RDONLY).
	// Verify we can at least read the (empty) directory.
	_, _, _, err = root.GetAttr(p9.AttrMask{Mode: true})
	if err != nil {
		t.Fatalf("GetAttr() error = %v", err)
	}
}

func TestP9Server_MultipleConnections(t *testing.T) {
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	const numClients = 5
	var wg sync.WaitGroup
	errs := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
			if err != nil {
				errs <- err
				return
			}

			client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
			if err != nil {
				conn.Close()
				errs <- err
				return
			}
			defer client.Close()

			root, err := client.Attach("")
			if err != nil {
				errs <- err
				return
			}
			defer root.Close()

			// Each client reads the root directory attributes
			_, _, _, err = root.GetAttr(p9.AttrMask{Mode: true, Size: true})
			if err != nil {
				errs <- err
				return
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent client error: %v", err)
	}
}
