package tunnels

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/charliek/shed/internal/vmutil"
)

// ConnectClient dials shed VMs via the shed-server Connect API.
type ConnectClient struct {
	serverAddr string // host:port of shed-server HTTP API (e.g., "localhost:8080")
}

// NewConnectClient creates a new Connect API client.
func NewConnectClient(serverAddr string) *ConnectClient {
	return &ConnectClient{serverAddr: serverAddr}
}

// Dial opens a TCP tunnel to a shed VM port via the Connect API.
// It performs an HTTP upgrade handshake and returns the underlying raw TCP connection.
func (c *ConnectClient) Dial(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	// Use raw net.Dialer — not http.Client which has timeouts unsuitable for long-lived tunnels.
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to shed-server at %s: %w", c.serverAddr, err)
	}

	// Send HTTP upgrade request.
	path := fmt.Sprintf("/api/sheds/%s/connect/%d", shedName, port)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: shed-tcp\r\n\r\n",
		path, c.serverAddr)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send upgrade request: %w", err)
	}

	// Read the response.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.Body != nil {
			resp.Body.Close()
		}
		conn.Close()
		return nil, fmt.Errorf("connect API returned HTTP %d (expected 101)", resp.StatusCode)
	}

	return &vmutil.BufferedConn{Conn: conn, Reader: reader}, nil
}
