//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// handleTCPProxyConnection handles a single TCP proxy connection.
// Protocol: client sends "CONNECT <port>\n", server responds "OK\n" or "ERR <msg>\n",
// then bidirectional raw TCP follows.
func handleTCPProxyConnection(conn net.Conn) {
	defer conn.Close()

	// Read the CONNECT line with a deadline.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 64)
	line, err := reader.ReadString('\n')
	if err != nil {
		writeProxyErr(conn, "failed to read command")
		return
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear deadline

	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "CONNECT ") {
		writeProxyErr(conn, "expected CONNECT <port>")
		return
	}

	portStr := strings.TrimPrefix(line, "CONNECT ")
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		writeProxyErr(conn, "invalid port: "+portStr)
		return
	}

	// Dial the target inside the VM.
	target := net.JoinHostPort("127.0.0.1", strconv.FormatUint(port, 10))
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		writeProxyErr(conn, fmt.Sprintf("dial %s: %s", target, err.Error()))
		return
	}

	// Success — send OK and begin proxying.
	if _, err := conn.Write([]byte("OK\n")); err != nil {
		targetConn.Close()
		return
	}

	// Bidirectional copy. Uses reader (not conn directly) for client->target
	// to drain any data buffered during the CONNECT handshake read.
	//
	// Each direction half-closes its destination when its source ends, so
	// an end-of-file crosses the proxy in both directions without a full
	// close. Host-side conns are vsock (vsock.Conn) or unix (net.UnixConn);
	// both have CloseWrite, as does the TCP target. What the host then sees
	// is up to what carries the conn: a unix socket delivers the EOF, while
	// Firecracker's vsock device records the shutdown and tells the host
	// nothing until full close (see firecracker.Client.DialService).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetConn, reader)
		closeWrite(targetConn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, targetConn)
		closeWrite(conn)
	}()
	wg.Wait()
	targetConn.Close()
}

// closeWrite shuts down the write side of c when c supports it.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func writeProxyErr(conn net.Conn, msg string) {
	log.Printf("TCP proxy: %s", msg)
	_, _ = conn.Write([]byte("ERR " + msg + "\n"))
}
