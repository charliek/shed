package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultServerURL    = "http://localhost:8080"
	maxReconnectBackoff = 30 * time.Second
	initialBackoff      = 1 * time.Second
)

// Subscription connection states reported by HostClient.Status.
const (
	ConnConnected    = "connected"
	ConnReconnecting = "reconnecting"
	ConnStopped      = "stopped"
)

// SubStatus is a snapshot of one namespace subscription's connection state, so
// callers can report health (e.g. `shed-host-agent status --live`) without
// scraping logs.
type SubStatus struct {
	Namespace string
	State     string    // ConnConnected | ConnReconnecting | ConnStopped
	LastError string    // last connection error (empty while connected)
	Since     time.Time // when the current state began
}

// HostClient connects to shed-server's plugin API to receive and respond to messages.
// Used by host-side extension binaries.
type HostClient struct {
	serverURL  string
	httpClient *http.Client
	logger     *slog.Logger

	mu     sync.Mutex
	states map[string]SubStatus
}

// HostClientOption configures a HostClient.
type HostClientOption func(*HostClient)

// WithServerURL sets the shed-server URL.
func WithServerURL(url string) HostClientOption {
	return func(c *HostClient) {
		c.serverURL = strings.TrimRight(url, "/")
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) HostClientOption {
	return func(c *HostClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) HostClientOption {
	return func(c *HostClient) {
		c.logger = logger
	}
}

// NewHostClient creates a new HostClient with the given options.
func NewHostClient(opts ...HostClientOption) *HostClient {
	c := &HostClient{
		serverURL:  defaultServerURL,
		httpClient: &http.Client{},
		logger:     slog.Default(),
		states:     make(map[string]SubStatus),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Subscribe connects to the SSE stream for the given namespace and returns a
// channel of envelopes. The channel is closed when the context is cancelled
// or the connection is permanently lost. Reconnects automatically on transient
// failures with exponential backoff.
func (c *HostClient) Subscribe(ctx context.Context, namespace string) <-chan *Envelope {
	ch := make(chan *Envelope, 32)
	go c.subscribeLoop(ctx, namespace, ch)
	return ch
}

// Status returns a snapshot of each namespace subscription's connection state,
// sorted by namespace. Safe for concurrent use.
func (c *HostClient) Status() []SubStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SubStatus, 0, len(c.states))
	for _, s := range c.states {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}

// setState records a namespace's connection state, stamping Since only when the
// state actually changes (so a snapshot shows how long it's been in that state).
func (c *HostClient) setState(namespace, state string, cause error) {
	es := ""
	if cause != nil {
		es = cause.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.states[namespace]; ok && prev.State == state {
		prev.LastError = es
		c.states[namespace] = prev
		return
	}
	c.states[namespace] = SubStatus{Namespace: namespace, State: state, LastError: es, Since: time.Now()}
}

func (c *HostClient) subscribeLoop(ctx context.Context, namespace string, ch chan<- *Envelope) {
	defer close(ch)
	defer c.setState(namespace, ConnStopped, nil)
	c.setState(namespace, ConnReconnecting, nil) // "connecting" until the first attempt resolves

	backoff := initialBackoff
	downLogged := false
	for {
		start := time.Now()
		connected := false
		err := c.streamMessages(ctx, namespace, ch, func() {
			connected = true
			downLogged = false
			c.setState(namespace, ConnConnected, nil)
			c.logger.Info("SSE connected", "namespace", namespace)
		})
		if ctx.Err() != nil {
			return
		}
		c.setState(namespace, ConnReconnecting, err)

		// Reset backoff only after a connection that held for a while, so a
		// flapping server doesn't keep resetting it.
		if connected && time.Since(start) > 60*time.Second {
			backoff = initialBackoff
		}

		// Log loudly on a down-transition / first failure, then quietly while it
		// stays down — a persistently-unreachable server must not flood the log
		// with an identical WARN every backoff cycle.
		if !downLogged {
			c.logger.Warn("SSE connection lost, reconnecting",
				"namespace", namespace, "error", err, "backoff", backoff)
			downLogged = true
		} else {
			c.logger.Debug("SSE still down, retrying",
				"namespace", namespace, "error", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxReconnectBackoff)
	}
}

func (c *HostClient) streamMessages(ctx context.Context, namespace string, ch chan<- *Envelope, onConnect func()) error {
	url := fmt.Sprintf("%s/api/plugins/listeners/%s/messages", c.serverURL, namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	onConnect()

	scanner := bufio.NewScanner(resp.Body)
	const maxSSEEventSize = 1 << 20 // 1 MiB — envelopes can carry large payloads
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSEEventSize)
	var dataBuf bytes.Buffer
	dataLines := 0

	for scanner.Scan() {
		line := scanner.Text()

		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			// Per SSE spec, multiple data: lines are joined with newlines
			if dataLines > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(data)
			dataLines++
			continue
		}

		// Empty line signals end of event
		if line == "" && dataBuf.Len() > 0 {
			var env Envelope
			if err := json.Unmarshal(dataBuf.Bytes(), &env); err != nil {
				c.logger.Warn("failed to parse SSE event", "error", err)
			} else {
				select {
				case ch <- &env:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			dataBuf.Reset()
			dataLines = 0
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stream: %w", err)
	}
	return errors.New("stream closed by server")
}

// Respond sends a response envelope back to shed-server for routing to the
// originating shed.
func (c *HostClient) Respond(ctx context.Context, namespace string, env *Envelope) error {
	url := fmt.Sprintf("%s/api/plugins/listeners/%s/respond", c.serverURL, namespace)

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling response: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
