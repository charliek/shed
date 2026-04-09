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
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 64)
	line, err := reader.ReadString('\n')
	if err != nil {
		writeProxyErr(conn, "failed to read command")
		return
	}
	conn.SetReadDeadline(time.Time{}) // clear deadline

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

	// Bidirectional copy with proper shutdown.
	var wg sync.WaitGroup
	wg.Add(2)

	// conn -> target
	go func() {
		defer wg.Done()
		io.Copy(targetConn, reader) // use reader to drain any buffered data
		// Half-close: signal target we're done sending.
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// target -> conn
	go func() {
		defer wg.Done()
		io.Copy(conn, targetConn)
		// Half-close: signal client we're done sending.
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
	targetConn.Close()
}

func writeProxyErr(conn net.Conn, msg string) {
	log.Printf("TCP proxy: %s", msg)
	conn.Write([]byte("ERR " + msg + "\n"))
}
