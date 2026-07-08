package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// DefaultTimeout for quick API operations (list, stop, delete, etc.)
const DefaultTimeout = 30 * time.Second

// APIClient provides methods for interacting with the shed server API.
type APIClient struct {
	baseURL       string
	httpClient    *http.Client
	transport     http.RoundTripper // non-nil when TLS-pinned; shared by every client below
	createTimeout time.Duration
	// tokens holds the bearer token and, in secure mode, transparently re-mints
	// it (proactively near expiry, reactively on a 401). Static for open servers,
	// plain-HTTP clients, and legacy fixed tokens. Never nil.
	tokens *clienttoken.Source
}

// setAuth adds the bearer token header when the client has a non-empty token.
func (c *APIClient) setAuth(req *http.Request) {
	if tok := c.tokens.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// currentToken returns the client's current in-memory bearer (control) token. It
// may have been re-minted since construction — proactively near expiry or
// reactively on a 401 — even when persisting the refresh back to config was
// skipped (ambiguous alias) or failed. The tunnel path reads it so a forwarded
// connection dials with the freshest token rather than the possibly-stale one in
// the config entry.
func (c *APIClient) currentToken() string {
	return c.tokens.Token()
}

// TokenSource returns the client's token source, so a long-lived consumer (the
// tunnel daemon) can seed its own Source from the freshly-refreshed token+expiry
// this client obtained during setup.
func (c *APIClient) TokenSource() *clienttoken.Source {
	return c.tokens
}

// newHTTPClient builds an *http.Client carrying the pinning transport (if any),
// so every request path — including the long-running, ad-hoc clients used for
// SSE and timeouts — verifies the pinned TLS cert. timeout 0 means no
// client-level timeout (the caller bounds it with a context).
func (c *APIClient) newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: c.transport}
}

// pinnedTransport returns a transport that verifies the server's leaf cert
// against the pinned "sha256:<hex>" fingerprint instead of a CA chain, or nil
// when there is no pin (plain HTTP). It delegates to servertls.PinnedTransport
// so the CLI, the Connect tunnel, and the startup probe build the pinned
// transport identically from one place.
func pinnedTransport(fingerprint string) http.RoundTripper {
	return servertls.PinnedTransport(fingerprint)
}

// NewAPIClient creates a plain-HTTP API client for the given host and port
// (the bootstrap path, before any TLS pin is known).
func NewAPIClient(host string, port int, createTimeout time.Duration) *APIClient {
	return newAPIClient(fmt.Sprintf("http://%s:%d", host, port), "", "", createTimeout)
}

// newAPIClient is the shared constructor: an explicit base URL, an optional
// bearer token, and an optional TLS pin fingerprint.
func newAPIClient(baseURL, token, tlsFingerprint string, createTimeout time.Duration) *APIClient {
	c := &APIClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		transport:     pinnedTransport(tlsFingerprint),
		createTimeout: createTimeout,
		tokens:        clienttoken.Static(token),
	}
	c.httpClient = c.newHTTPClient(DefaultTimeout)
	return c
}

// NewAPIClientFromEntry creates an API client from a server entry, honoring its
// api_url/TLS pin and control token. When the entry carries a bootstrap-minted
// control token (ControlTokenExpiresAt non-zero), the client transparently
// re-mints it over SSH — proactively before expiry and reactively on a 401 —
// persisting it back to the config entry it came from.
func NewAPIClientFromEntry(entry *config.ServerEntry, createTimeout time.Duration) *APIClient {
	c := newAPIClient(entry.BaseURL(), entry.ControlToken, entry.TLSCertFingerprint, createTimeout)
	if entry.ControlTokenExpiresAt.IsZero() {
		return c // static/legacy token or open server — no refresh
	}
	// Resolve which config entry this is, so a refreshed token can be persisted.
	// "" means no unambiguous match (a one-off entry, or duplicate aliases to the
	// same endpoint) — the refresh still works for this client's lifetime, it
	// just isn't saved.
	name := serverNameForEntry(entry)
	c.tokens = clienttoken.New(entry.ControlToken, entry.ControlTokenExpiresAt,
		controlTokenRefresh(entry.Host, entry.SSHPort, name))
	// Proactively re-mint a near-expiry token so a request never races expiry. A
	// mint failure here is non-fatal (EnsureFresh keeps the stale token); the
	// reactive 401-retry surfaces any error on the next request.
	c.tokens.EnsureFresh()
	return c
}

// controlTokenRefresh returns a refresh callback that mints a control token over
// the SSH bootstrap channel and, when persistName != "", best-effort persists it
// to that config entry. It is shared by NewAPIClientFromEntry (persisting) and
// the tunnel daemon (persistName "" — mint-only, so a long-lived daemon never
// rewrites the user's config from its own stale in-memory copy).
func controlTokenRefresh(host string, sshPort int, persistName string) func() (string, time.Time, error) {
	return func() (string, time.Time, error) {
		bundle, err := bootstrapFn(host, sshPort, "control", "cli")
		if err != nil {
			return "", time.Time{}, err
		}
		if persistName != "" {
			// Best-effort: a Save failure never wastes the mint or fails the
			// request (the next process just re-mints).
			persistControlToken(persistName, bundle.Token, bundle.ExpiresAt)
		}
		return bundle.Token, bundle.ExpiresAt, nil
	}
}

// configMu serializes refresh-path access to the shared clientConfig global —
// the serverNameForEntry lookup and the token persist (map write + Save). The
// `--all` fan-out (forEachServer) constructs clients, and thus refreshes,
// concurrently across goroutines; without this, two near-expiry refreshes would
// race on the Servers map (a fatal "concurrent map writes") and clobber each
// other's config.yaml save.
var configMu sync.Mutex

// persistControlToken writes a freshly re-minted control token back to the named
// config entry and saves. It is best-effort: name == "" (no unambiguous config
// entry) is a no-op, and a Save failure is warned-but-not-fatal — the token is
// already valid in memory, so the command proceeds and the next process re-mints
// rather than the request failing. configMu serializes the map write + Save
// against concurrent refreshes from the `--all` fan-out.
func persistControlToken(name, token string, expiresAt time.Time) {
	if name == "" {
		return
	}
	configMu.Lock()
	defer configMu.Unlock()
	e := clientConfig.Servers[name]
	e.ControlToken = token
	e.ControlTokenExpiresAt = expiresAt
	clientConfig.Servers[name] = e
	if err := clientConfig.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist refreshed token for %q: %v\n", name, err)
	}
}

// serverNameForEntry returns the config name whose stored entry UNIQUELY matches
// e by its stable identity (host + ssh port + api_url), or "" when there is no
// match (a one-off entry) or more than one (duplicate aliases to the same
// endpoint). Returning "" on ambiguity is deliberate: a refreshed token is only
// persisted when the target entry is unambiguous, so it can never be written to
// the wrong alias. Used so a refreshed token can be written back without
// threading the name through every call site.
//
// ControlToken is deliberately NOT part of the key: the refresh path rewrites
// it, so matching on it would make an entry stop matching its own config row
// after the first re-mint.
func serverNameForEntry(e *config.ServerEntry) string {
	configMu.Lock()
	defer configMu.Unlock()
	match := ""
	for n, se := range clientConfig.Servers {
		if se.Host == e.Host && se.SSHPort == e.SSHPort && se.APIURL == e.APIURL {
			if match != "" {
				return "" // ambiguous — refuse to persist to the wrong alias
			}
			match = n
		}
	}
	return match
}

// sendRequest builds and sends a single JSON request. It is the per-attempt
// work factored out of doRequest so the 401 path can retry with a refreshed
// token.
func (c *APIClient) sendRequest(method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to encode request: %w", err)
		}
		bodyReader = bytes.NewReader(bodyData)
	}
	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setAuth(req)
	return c.httpClient.Do(req)
}

// refreshedOn401 is the shared on-401 re-mint for the request paths. When resp is
// a 401 and the token source can refresh, it closes resp.Body, re-mints the token
// that failed (sentToken — the generation the caller sent, so a concurrent
// refresh isn't double-minted), and returns retry=true so the caller re-runs its
// request exactly once (panel decision #11). A static token (not Refreshable) or
// a non-401 returns retry=false. Only a failed re-mint returns an error; each
// caller wraps its own resend error as it sees fit.
func (c *APIClient) refreshedOn401(resp *http.Response, sentToken string) (retry bool, err error) {
	if resp.StatusCode != http.StatusUnauthorized || !c.tokens.Refreshable() {
		return false, nil
	}
	_ = resp.Body.Close()
	if _, rerr := c.tokens.Refresh(sentToken); rerr != nil {
		return false, fmt.Errorf("re-authenticating after 401: %w", rerr)
	}
	return true, nil
}

// doRequest performs an HTTP request with JSON body and response handling. It
// handles connection errors, status validation, and JSON decoding, and
// transparently re-mints + retries once on a 401 (an expired bootstrap token).
func (c *APIClient) doRequest(method, path string, body, result interface{}, expectedStatus ...int) error {
	sent := c.tokens.Token()
	resp, err := c.sendRequest(method, path, body)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	retry, rerr := c.refreshedOn401(resp, sent)
	if rerr != nil {
		return rerr
	}
	if retry {
		resp, err = c.sendRequest(method, path, body)
		if err != nil {
			return fmt.Errorf("failed to connect to server: %w", err)
		}
	}
	defer resp.Body.Close()

	// Check for expected status codes
	validStatus := false
	if len(expectedStatus) == 0 {
		validStatus = resp.StatusCode == http.StatusOK
	} else {
		for _, s := range expectedStatus {
			if resp.StatusCode == s {
				validStatus = true
				break
			}
		}
	}
	if !validStatus {
		return c.parseError(resp)
	}

	// Decode result if provided
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}
	}

	return nil
}

// doRequestWithTimeout performs an HTTP request with a custom timeout using context.
// Used for long-running operations like create and start that may need more time.
func (c *APIClient) doRequestWithTimeout(method, path string, body, result interface{}, timeout time.Duration, expectedStatus ...int) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyData, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to encode request: %w", err)
		}
		bodyReader = bytes.NewReader(bodyData)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.setAuth(req)

	// Create a client without a Timeout for long-running requests.
	// Important: When both http.Client.Timeout and context deadline are set,
	// the shorter one wins. Since c.httpClient has a 30s timeout, we must use
	// a separate client here to allow the context timeout (potentially minutes)
	// to control cancellation. It still carries the pinning transport.
	client := c.newHTTPClient(0)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("request timed out after %v (use --timeout to increase)", timeout)
		}
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	defer resp.Body.Close()

	// Check for expected status codes
	validStatus := false
	if len(expectedStatus) == 0 {
		validStatus = resp.StatusCode == http.StatusOK
	} else {
		for _, s := range expectedStatus {
			if resp.StatusCode == s {
				validStatus = true
				break
			}
		}
	}
	if !validStatus {
		return c.parseError(resp)
	}

	// Decode result if provided
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}
	}

	return nil
}

// GetInfo retrieves server information.
func (c *APIClient) GetInfo() (*config.ServerInfo, error) {
	var info config.ServerInfo
	if err := c.doRequest(http.MethodGet, "/api/info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// GetSSHHostKey retrieves the server's SSH host key.
func (c *APIClient) GetSSHHostKey() (*config.SSHHostKeyResponse, error) {
	var hostKey config.SSHHostKeyResponse
	if err := c.doRequest(http.MethodGet, "/api/ssh-host-key", nil, &hostKey); err != nil {
		return nil, err
	}
	return &hostKey, nil
}

// ListSheds retrieves all sheds from the server.
func (c *APIClient) ListSheds() (*config.ShedsResponse, error) {
	var sheds config.ShedsResponse
	if err := c.doRequest(http.MethodGet, "/api/sheds", nil, &sheds); err != nil {
		return nil, err
	}
	return &sheds, nil
}

// ListImages returns available image variants from the server.
func (c *APIClient) ListImages() (*config.ImagesResponse, error) {
	var images config.ImagesResponse
	if err := c.doRequest(http.MethodGet, "/api/images", nil, &images); err != nil {
		return nil, err
	}
	return &images, nil
}

// CreateShed creates a new shed.
func (c *APIClient) CreateShed(req *config.CreateShedRequest) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/sheds", req, &shed, c.createTimeout, http.StatusCreated, http.StatusOK); err != nil {
		return nil, err
	}
	return &shed, nil
}

// CreateShedWithProgress creates a new shed and streams progress events via SSE.
func (c *APIClient) CreateShedWithProgress(req *config.CreateShedRequest, wantBlobProgress bool, onProgress func(backend.ProgressEvent)) (*config.Shed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.createTimeout)
	defer cancel()

	bodyData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	// Opt into structured per-blob byte events (the image-pull leg) only when
	// the caller can render them; older servers ignore the param.
	url := c.baseURL + "/api/sheds"
	if wantBlobProgress {
		url += "?progress=blob"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	c.setAuth(httpReq)

	client := c.newHTTPClient(0)
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("request timed out after %v (use --timeout to increase)", c.createTimeout)
		}
		return nil, fmt.Errorf("failed to connect to server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	shed, err := c.readShedSSEStream(resp.Body, onProgress)
	return shed, err
}

// readShedSSEStream is the create-shed wrapper around readSSEStream: it
// decodes the terminal "complete" payload into a config.Shed.
func (c *APIClient) readShedSSEStream(body io.Reader, onProgress func(backend.ProgressEvent)) (*config.Shed, error) {
	data, err := c.readSSEStream(body, onProgress)
	if err != nil {
		return nil, err
	}
	var shed config.Shed
	if err := json.Unmarshal(data, &shed); err != nil {
		return nil, fmt.Errorf("failed to parse complete event: %w", err)
	}
	return &shed, nil
}

// readSSEStream parses an SSE event stream, calling onProgress for progress
// events and returning the RAW JSON payload of the terminal "complete" event
// (the caller decodes it into the operation-specific type — a Shed for create,
// an ImagePullResponse for pull). An "error" event is surfaced as a Go error.
//
// This implements the key parts of the SSE specification:
//   - "event:" sets the event type for the next dispatch
//   - "data:" lines are concatenated (with newlines) to form the event payload
//   - Lines starting with ":" are comments (used for keep-alive pings)
//   - A blank line dispatches the accumulated event
func (c *APIClient) readSSEStream(body io.Reader, onProgress func(backend.ProgressEvent)) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	var eventType string
	var dataBuf strings.Builder

	dispatch := func(eventType, data string) (done bool, payload []byte, err error) {
		switch eventType {
		case "progress":
			var event backend.ProgressEvent
			if uerr := json.Unmarshal([]byte(data), &event); uerr == nil && onProgress != nil {
				onProgress(event)
			}
		case "complete":
			return true, []byte(data), nil
		case "error":
			var apiErr config.APIError
			if uerr := json.Unmarshal([]byte(data), &apiErr); uerr != nil {
				return true, nil, fmt.Errorf("server error: %s", data)
			}
			return true, nil, fmt.Errorf("%s: %s", apiErr.Error.Code, apiErr.Error.Message)
		}
		return false, nil, nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		// Blank line dispatches the accumulated event
		if line == "" {
			if dataBuf.Len() > 0 {
				data := dataBuf.String()
				dataBuf.Reset()
				if done, payload, err := dispatch(eventType, data); done {
					return payload, err
				}
			}
			eventType = ""
			continue
		}

		// Comments (including keep-alive pings)
		if strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
			continue
		}
	}

	// Handle a final event if EOF occurs before a trailing blank line.
	if dataBuf.Len() > 0 {
		if done, payload, err := dispatch(eventType, dataBuf.String()); done {
			return payload, err
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading event stream: %w", err)
	}

	return nil, fmt.Errorf("event stream ended without a complete or error event")
}

// GetShed retrieves a specific shed by name.
func (c *APIClient) GetShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodGet, "/api/sheds/"+name, nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// EgressShow returns a shed's egress status (active profiles + recent decisions).
func (c *APIClient) EgressShow(name string) (*config.EgressStatus, error) {
	var status config.EgressStatus
	if err := c.doRequest(http.MethodGet, "/api/egress/"+name, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// EgressSet applies a profile selection to a shed (live on a running shed).
func (c *APIClient) EgressSet(name string, profiles []string) (*config.Shed, error) {
	var shed config.Shed
	req := config.EgressSetRequest{Profiles: profiles}
	if err := c.doRequest(http.MethodPost, "/api/egress/"+name, req, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// EgressOff turns egress control off for a shed.
func (c *APIClient) EgressOff(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodDelete, "/api/egress/"+name, nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// EgressProfilesList returns all egress profiles (config baseline + user store),
// each tagged with its source.
func (c *APIClient) EgressProfilesList() ([]config.EgressProfileInfo, error) {
	var infos []config.EgressProfileInfo
	if err := c.doRequest(http.MethodGet, "/api/egress/profiles", nil, &infos); err != nil {
		return nil, err
	}
	return infos, nil
}

// EgressProfileGet returns one egress profile by name.
func (c *APIClient) EgressProfileGet(name string) (*config.EgressProfileInfo, error) {
	var info config.EgressProfileInfo
	if err := c.doRequest(http.MethodGet, "/api/egress/profiles/"+name, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// EgressProfilePut creates or replaces a user profile (whole document).
func (c *APIClient) EgressProfilePut(name string, p config.EgressProfile) (*config.EgressProfileInfo, error) {
	var info config.EgressProfileInfo
	if err := c.doRequest(http.MethodPut, "/api/egress/profiles/"+name, p, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// EgressProfileDelete removes a user profile.
func (c *APIClient) EgressProfileDelete(name string) error {
	return c.doRequest(http.MethodDelete, "/api/egress/profiles/"+name, nil, nil)
}

// DeleteShedWithProgress deletes a shed and streams teardown progress via SSE.
// It uses no client-level timeout (context deadline only, like create), so an
// active event stream never trips the 30s quick-op timeout while a delete runs.
// It falls back cleanly to a plain delete when the server predates delete-SSE: a
// 204 No Content means the delete succeeded and there is no stream to read.
func (c *APIClient) DeleteShedWithProgress(name string, onProgress func(backend.ProgressEvent)) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.createTimeout)
	defer cancel()

	// The streaming client (no client-level timeout, context deadline only)
	// bypasses doRequest, so replicate its send + 401-refresh here. Rebuilding
	// per send is fine — a DELETE has no body. Delete, unlike create, has no
	// GetInfo pre-flight to refresh a stale bootstrap token, so this is the only
	// place a mid-session token expiry gets re-minted.
	client := c.newHTTPClient(0)
	send := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/sheds/"+name, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Accept", "text/event-stream")
		c.setAuth(httpReq)
		resp, err := client.Do(httpReq)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("request timed out after %v (use --timeout to increase)", c.createTimeout)
			}
			return nil, fmt.Errorf("failed to connect to server: %w", err)
		}
		return resp, nil
	}

	sent := c.tokens.Token()
	resp, err := send()
	if err != nil {
		return err
	}
	retry, rerr := c.refreshedOn401(resp, sent)
	if rerr != nil {
		return rerr
	}
	if retry {
		if resp, err = send(); err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	// Old server (no delete-SSE) returns a plain 204 — the delete succeeded and
	// there is no stream to read.
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return c.parseError(resp)
	}
	// A 200 without the SSE content type is a non-streaming responder (an old or
	// proxied plain delete that the pre-SSE client also accepted) — the delete
	// succeeded; don't feed a non-SSE body to the stream reader. Otherwise read
	// the SSE stream, ignoring the benign terminal "complete" payload.
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return nil
	}
	_, err = c.readSSEStream(resp.Body, onProgress)
	return err
}

// StartShed starts a stopped shed.
func (c *APIClient) StartShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/sheds/"+name+"/start", nil, &shed, c.createTimeout); err != nil {
		return nil, err
	}
	return &shed, nil
}

// StopShed stops a running shed.
func (c *APIClient) StopShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodPost, "/api/sheds/"+name+"/stop", nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// ResetShed resets the per-shed writable upper layer.
func (c *APIClient) ResetShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodPost, "/api/sheds/"+name+"/reset", nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// ListSessions retrieves all tmux sessions in a shed.
func (c *APIClient) ListSessions(shedName string) ([]config.Session, error) {
	var resp config.SessionsResponse
	if err := c.doRequest(http.MethodGet, "/api/sheds/"+shedName+"/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// ListAllSessions retrieves all tmux sessions across all sheds.
// Returns the full SessionsResponse including any warnings about sheds that couldn't be queried.
func (c *APIClient) ListAllSessions() (*config.SessionsResponse, error) {
	var resp config.SessionsResponse
	if err := c.doRequest(http.MethodGet, "/api/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// KillSession terminates a tmux session in a shed.
func (c *APIClient) KillSession(shedName, sessionName string) error {
	path := fmt.Sprintf("/api/sheds/%s/sessions/%s", shedName, sessionName)
	return c.doRequest(http.MethodDelete, path, nil, nil, http.StatusNoContent, http.StatusOK)
}

// imageIdentURL builds the request URL for an endpoint that targets a single
// image by identifier (a Docker ref, digest, or cosmetic tag). A Docker ref
// contains slashes, which can't ride in a single URL path segment (the
// server's chi {name} param stops at the first '/'), so slash-bearing
// identifiers are passed as a ?ref= query. Slash-free identifiers (digests,
// tags) keep the path form so a newer CLI still drives an older server.
func imageIdentURL(collection, ident string) string {
	if strings.Contains(ident, "/") {
		return collection + "?ref=" + url.QueryEscape(ident)
	}
	return collection + "/" + ident
}

// DeleteImage removes an image's addressability (Docker model). The blob is
// GC'd by PruneImages. ident may be a Docker ref, a digest, or a tag label.
func (c *APIClient) DeleteImage(ident string) error {
	return c.doRequest(http.MethodDelete, imageIdentURL("/api/images", ident), nil, nil, http.StatusNoContent, http.StatusOK)
}

// InspectImage returns full details for a ref, tag, or digest.
func (c *APIClient) InspectImage(ident string) (*config.ImageInspectResponse, error) {
	var resp config.ImageInspectResponse
	if err := c.doRequest(http.MethodGet, imageIdentURL("/api/images/inspect", ident), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// TagImage points newTag at the digest currently held by srcTagOrDigest.
func (c *APIClient) TagImage(src, dst string) error {
	body := config.ImageTagRequest{Source: src, Target: dst}
	return c.doRequest(http.MethodPost, "/api/images/tag", body, nil, http.StatusNoContent, http.StatusOK)
}

// PullImage pulls a Docker reference into the blob store under the named tag.
// platform is an optional override (e.g. "linux/arm64"); withLayers pulls the
// full image (false = boot-only, the default).
func (c *APIClient) PullImage(dockerRef, tag, platform string, withLayers bool) (*config.ImagePullResponse, error) {
	body := config.ImagePullRequest{DockerRef: dockerRef, Tag: tag, Platform: platform, WithLayers: withLayers}
	var resp config.ImagePullResponse
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/images/pull", body, &resp, 30*time.Minute); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PullImageWithProgress pulls a Docker reference and streams per-stage
// progress via SSE (mirrors CreateShedWithProgress). Falls back to the
// non-streaming PullImage path only if the server rejects the stream.
func (c *APIClient) PullImageWithProgress(dockerRef, tag, platform string, withLayers, wantBlobProgress bool, onProgress func(backend.ProgressEvent)) (*config.ImagePullResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	bodyData, err := json.Marshal(config.ImagePullRequest{DockerRef: dockerRef, Tag: tag, Platform: platform, WithLayers: withLayers})
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}
	// Opt into structured per-blob byte events only when the caller can
	// render them (interactive TTY). Older servers ignore the param and a
	// non-opted-in request keeps today's plain line stream.
	url := c.baseURL + "/api/images/pull"
	if wantBlobProgress {
		url += "?progress=blob"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	c.setAuth(httpReq)

	resp, err := c.newHTTPClient(0).Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("pull timed out after 30m")
		}
		return nil, fmt.Errorf("failed to connect to server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var out config.ImagePullResponse
	// Fall back to the non-streaming path against a server that ignores the
	// Accept header (e.g. a pre-SSE shed-server): it returns a plain JSON
	// ImagePullResponse with Content-Type application/json.
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("failed to decode pull response: %w", err)
		}
		return &out, nil
	}

	data, err := c.readSSEStream(resp.Body, onProgress)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("failed to parse complete event: %w", err)
	}
	return &out, nil
}

// PushImage uploads the manifest held by source (tag or digest) to a
// destination registry ref. Byte-perfect.
func (c *APIClient) PushImage(source, destination string) (*config.ImagePushResponse, error) {
	body := config.ImagePushRequest{Source: source, Destination: destination}
	var resp config.ImagePushResponse
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/images/push", body, &resp, 30*time.Minute); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PruneImages removes unused cached images.
// If dryRun is true, returns candidates without deleting.
func (c *APIClient) PruneImages(dryRun bool) (*config.PruneImagesResponse, error) {
	path := "/api/images/prune"
	if dryRun {
		path += "?dry_run=true"
	}
	var resp config.PruneImagesResponse
	if err := c.doRequest(http.MethodPost, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListSnapshots returns all snapshots managed by the server.
func (c *APIClient) ListSnapshots() (*config.SnapshotsResponse, error) {
	var resp config.SnapshotsResponse
	if err := c.doRequest(http.MethodGet, "/api/snapshots", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetSnapshot retrieves a snapshot by name.
func (c *APIClient) GetSnapshot(name string) (*config.Snapshot, error) {
	var snap config.Snapshot
	if err := c.doRequest(http.MethodGet, "/api/snapshots/"+name, nil, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// CreateSnapshot creates a new snapshot from a stopped shed.
// Returns the created snapshot and any non-fatal warnings the backend emitted
// during the operation (e.g., "--local-dir not captured").
func (c *APIClient) CreateSnapshot(req *config.SnapshotCreateRequest) (*config.Snapshot, []string, error) {
	var resp config.SnapshotCreateResponse
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/snapshots", req, &resp, c.createTimeout, http.StatusCreated, http.StatusOK); err != nil {
		return nil, nil, err
	}
	return resp.Snapshot, resp.Warnings, nil
}

// DeleteSnapshot removes a snapshot from the server.
func (c *APIClient) DeleteSnapshot(name string) error {
	return c.doRequest(http.MethodDelete, "/api/snapshots/"+name, nil, nil, http.StatusNoContent, http.StatusOK)
}

// GetSystemDF retrieves disk usage information for the server.
func (c *APIClient) GetSystemDF() (*config.DiskUsage, error) {
	var du config.DiskUsage
	if err := c.doRequest(http.MethodGet, "/api/system/df", nil, &du); err != nil {
		return nil, err
	}
	return &du, nil
}

// SystemPruneOptions mirrors backend.PruneOptions for the CLI → API path.
// Using a client-local struct avoids pulling the backend package into the
// CLI (which doesn't need the rest of the Backend interface).
type SystemPruneOptions struct {
	Images       bool
	Instances    bool
	Logs         bool
	Orphans      bool
	DryRun       bool
	Until        time.Duration
	LogTailBytes int64
}

// pruneTimeout bounds prune requests. Large fleets can exceed DefaultTimeout;
// use a generous ceiling modeled on create rather than 30s.
const pruneTimeout = 10 * time.Minute

// SystemPrune triggers a prune pass on the server and returns the report.
// Scope flags are added as repeatable `scope=` query params; an empty scope
// (all flags false) lets the server apply its default (images + instances
// + orphans, no logs).
func (c *APIClient) SystemPrune(opts SystemPruneOptions) (*config.PruneReport, error) {
	path := "/api/system/prune"
	q := make([]string, 0, 6)
	if opts.Images {
		q = append(q, "scope=images")
	}
	if opts.Instances {
		q = append(q, "scope=instances")
	}
	if opts.Logs {
		q = append(q, "scope=logs")
	}
	if opts.Orphans {
		q = append(q, "scope=orphans")
	}
	if opts.DryRun {
		q = append(q, "dry_run=true")
	}
	// Always send Until on the wire — a zero value from the user
	// means "prune any age" and must reach the handler, otherwise
	// the handler's 72h default would override their explicit intent.
	// The CLI's cobra default (72h) is still preserved: flag unset →
	// systemPruneFlagUntil == 72h → we send until=72h0m0s explicitly.
	q = append(q, "until="+opts.Until.String())
	if opts.LogTailBytes > 0 {
		q = append(q, fmt.Sprintf("log_tail_bytes=%d", opts.LogTailBytes))
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var report config.PruneReport
	if err := c.doRequestWithTimeout(http.MethodPost, path, nil, &report, pruneTimeout); err != nil {
		return nil, err
	}
	return &report, nil
}

// Ping checks if the server is reachable.
func (c *APIClient) Ping() bool {
	resp, err := c.newHTTPClient(2 * time.Second).Get(c.baseURL + "/api/info")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// parseError extracts the error message from an API error response.
func (c *APIClient) parseError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var apiErr config.APIError
	if err := json.Unmarshal(body, &apiErr); err != nil {
		// If not a structured error, return the body as-is
		return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
	}

	return fmt.Errorf("%s: %s", apiErr.Error.Code, apiErr.Error.Message)
}
