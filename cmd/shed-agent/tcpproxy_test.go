//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHandleTCPProxyConnection(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantPrefix string // expected response prefix
		wantOK     bool   // true if we expect OK and a working tunnel
	}{
		{
			name:       "valid port with listener",
			input:      "", // filled dynamically with a real listener port
			wantPrefix: "OK",
			wantOK:     true,
		},
		{
			name:       "port zero",
			input:      "CONNECT 0\n",
			wantPrefix: "ERR",
		},
		{
			name:       "port over 65535",
			input:      "CONNECT 99999\n",
			wantPrefix: "ERR",
		},
		{
			name:       "malformed command",
			input:      "HELLO\n",
			wantPrefix: "ERR",
		},
		{
			name:       "empty line",
			input:      "\n",
			wantPrefix: "ERR",
		},
		{
			name:       "missing port",
			input:      "CONNECT \n",
			wantPrefix: "ERR",
		},
		{
			name:       "connection refused",
			input:      "CONNECT 19999\n", // unlikely to be listening
			wantPrefix: "ERR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantOK {
				// Start a real TCP listener for the valid case.
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				defer ln.Close()
				port := ln.Addr().(*net.TCPAddr).Port

				// Accept one connection and echo data back.
				go func() {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					defer c.Close()
					io.Copy(c, c) // echo
				}()

				tt.input = fmt.Sprintf("CONNECT %d\n", port)
			}

			client, server := net.Pipe()
			defer client.Close()

			done := make(chan struct{})
			go func() {
				handleTCPProxyConnection(server)
				close(done)
			}()

			// Send the CONNECT command.
			client.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := client.Write([]byte(tt.input)); err != nil {
				t.Fatalf("write: %v", err)
			}

			// Read the response line.
			client.SetReadDeadline(time.Now().Add(5 * time.Second))
			reader := bufio.NewReader(client)
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			line = strings.TrimSpace(line)

			if !strings.HasPrefix(line, tt.wantPrefix) {
				t.Errorf("response = %q, want prefix %q", line, tt.wantPrefix)
			}

			if tt.wantOK {
				// Verify bidirectional data flow through the tunnel.
				testData := "hello tunnel\n"
				client.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if _, err := client.Write([]byte(testData)); err != nil {
					t.Fatalf("write through tunnel: %v", err)
				}

				client.SetReadDeadline(time.Now().Add(2 * time.Second))
				echoed, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("read echo: %v", err)
				}
				if echoed != testData {
					t.Errorf("echo = %q, want %q", echoed, testData)
				}
			}

			client.Close()
			<-done
		})
	}
}
