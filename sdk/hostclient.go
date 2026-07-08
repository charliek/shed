package sdk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
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
	// ConnRejected is terminal: the server refused the subscription with 409
	// (another listener already owns the namespace), so the loop stops instead
	// of hot-looping a retry. Distinct from ConnStopped (a clean ctx cancel).
	ConnRejected = "rejected"
)

// errSubscribeConflict is returned by streamMessages when the server answers the
// subscribe with 409 Conflict — the namespace already has an active listener.
// subscribeLoop treats it as terminal: a second broker must be observably
// rejected, not silently retry forever.
var errSubscribeConflict = errors.New("namespace already has an active subscriber")

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
	token      string
	// tokenProvider, when set, supplies a refreshing bearer token and takes
	// precedence over the static token. It is consulted per request (subscribe
	// reconnect + respond) so a re-minted token is picked up without rebuilding
	// the client; Invalidate is called after a 401.
	tokenProvider TokenProvider
	tlsPin        string // "sha256:<hex>" pin applied in NewHostClient, or ""

	mu     sync.Mutex
	states map[string]SubStatus
}

// TokenProvider supplies the bearer token for the bus client and refreshes it as
// needed. Implementations must be safe for concurrent use: the subscribe and
// respond paths call Token from different goroutines.
type TokenProvider interface {
	// Token returns the current bearer token, re-minting if it has expired. An
	// error means no token is currently available; the client sends unauthenticated.
	Token() (string, error)
	// Invalidate marks the current token stale so the next Token re-mints. The
	// client calls it after a 401 — the server rejected the cached token.
	Invalidate()
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

// WithTokenProvider sets a refreshing token source, used in preference to a
// static WithToken. The host-agent passes one backed by its SSH credential
// minter so the bus token is re-minted near expiry / on a 401 without rebuilding
// the client.
func WithTokenProvider(tp TokenProvider) HostClientOption {
	return func(c *HostClient) {
		c.tokenProvider = tp
	}
}

// WithToken sets the bearer token sent on requests (the host-agent's
// credentials-scoped token). Sent when non-empty.
func WithToken(token string) HostClientOption {
	return func(c *HostClient) {
		c.token = token
	}
}

// WithTLSPin pins shed-server's self-signed TLS certificate by the SHA-256
// fingerprint of its DER ("sha256:<hex>") — the trust model the HTTPS listener
// expects (no CA). Pass it alongside an https WithServerURL.
//
// Fail-closed: a pin only protects an https endpoint, so if the resolved
// serverURL is not https the client refuses to send (every request errors)
// rather than silently downgrading to unpinned plaintext.
//
// Composition with WithHTTPClient: order-independent. The pin is applied once,
// after all options, onto the resolved HTTP client — its Timeout and a custom
// *http.Transport's other settings (including any existing TLS config: client
// certs, ServerName, NextProtos) are preserved by cloning, never mutating the
// caller's client. If the custom client uses a non-*http.Transport
// RoundTripper, it owns its own TLS and the pin is skipped with a warning.
func WithTLSPin(fingerprint string) HostClientOption {
	return func(c *HostClient) {
		c.tlsPin = fingerprint
	}
}

// applyTLSPin installs cert pinning on the resolved HTTP client's transport.
// Called once after all options so it composes with WithHTTPClient regardless
// of option order.
func (c *HostClient) applyTLSPin() {
	if c.tlsPin == "" {
		return
	}
	// A pin only protects an https endpoint; with an http serverURL the TLS
	// config is never exercised and traffic would go plaintext. Fail closed.
	if !strings.HasPrefix(strings.ToLower(c.serverURL), "https://") {
		c.httpClient = &http.Client{
			Timeout: c.httpClient.Timeout,
			Transport: errorRoundTripper{fmt.Errorf(
				"WithTLSPin set but serverURL %q is not https; refusing to send unpinned plaintext", c.serverURL)},
		}
		return
	}
	var base *http.Transport
	switch t := c.httpClient.Transport.(type) {
	case *http.Transport:
		base = t.Clone()
	case nil:
		// Clone DefaultTransport to keep proxy/HTTP2 defaults; guard the
		// assertion in case it was globally replaced with another RoundTripper.
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			base = dt.Clone()
		} else {
			base = &http.Transport{}
		}
	default:
		c.logger.Warn("WithTLSPin ignored: custom HTTP client uses a non-*http.Transport RoundTripper that owns its own TLS")
		return
	}
	// Overlay the pin onto the caller's existing TLS settings rather than
	// replacing them wholesale, so client certs / ServerName / NextProtos /
	// a stricter MinVersion survive.
	tlsCfg := base.TLSClientConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	tlsCfg.InsecureSkipVerify = true
	tlsCfg.VerifyPeerCertificate = pinVerifier(c.tlsPin)
	base.TLSClientConfig = tlsCfg
	c.httpClient = &http.Client{
		Transport:     base,
		Timeout:       c.httpClient.Timeout,
		CheckRedirect: c.httpClient.CheckRedirect,
		Jar:           c.httpClient.Jar,
	}
}

// setAuth adds the bearer token header. With a tokenProvider it uses the current
// (possibly just-refreshed) token; otherwise the static token. Both no-op when
// empty (an open-mode, un-gated server).
func (c *HostClient) setAuth(req *http.Request) {
	tok := c.token
	if c.tokenProvider != nil {
		t, err := c.tokenProvider.Token()
		if err != nil {
			c.logger.Warn("token provider returned no token; sending unauthenticated", "error", err)
		}
		tok = t
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
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
	c.applyTLSPin()
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
	// Terminal state defaults to a clean stop (ctx cancel); a 409 rejection
	// overrides it to ConnRejected below so Status() reports it observably.
	stopState, stopCause := ConnStopped, error(nil)
	defer func() { c.setState(namespace, stopState, stopCause) }()
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
		// A 409 means another listener already owns this namespace. Retrying
		// would be a silent double-broker hot-loop, so stop terminally and let
		// Status() surface the rejection.
		if errors.Is(err, errSubscribeConflict) {
			stopState, stopCause = ConnRejected, err
			c.logger.Error("SSE subscription rejected; another listener owns this namespace — not retrying",
				"namespace", namespace, "error", err)
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
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode == http.StatusConflict {
			return fmt.Errorf("%w: %s", errSubscribeConflict, strings.TrimSpace(string(body)))
		}
		if resp.StatusCode == http.StatusUnauthorized && c.tokenProvider != nil {
			// Token rejected — re-mint so the backoff-reconnect authenticates fresh.
			c.tokenProvider.Invalidate()
		}
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

	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		c.setAuth(req)
		return c.httpClient.Do(req)
	}

	resp, err := send()
	if err != nil {
		return fmt.Errorf("sending response: %w", err)
	}
	// A credentials token can expire mid-session: on 401 re-mint once via the
	// provider and retry the response a single time (mirrors the CLI client).
	if resp.StatusCode == http.StatusUnauthorized && c.tokenProvider != nil {
		_ = resp.Body.Close()
		c.tokenProvider.Invalidate()
		resp, err = send()
		if err != nil {
			return fmt.Errorf("sending response: %w", err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
