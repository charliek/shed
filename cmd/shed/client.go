package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

const (
	// DefaultTimeout for quick API operations (list, stop, delete, etc.)
	DefaultTimeout = 30 * time.Second
)

// APIClient provides methods for interacting with the shed server API.
type APIClient struct {
	baseURL       string
	httpClient    *http.Client
	createTimeout time.Duration
}

// NewAPIClient creates a new API client for the given host and port.
func NewAPIClient(host string, port int, createTimeout time.Duration) *APIClient {
	return &APIClient{
		baseURL: fmt.Sprintf("http://%s:%d", host, port),
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		createTimeout: createTimeout,
	}
}

// NewAPIClientFromEntry creates a new API client from a server entry.
func NewAPIClientFromEntry(entry *config.ServerEntry, createTimeout time.Duration) *APIClient {
	return NewAPIClient(entry.Host, entry.HTTPPort, createTimeout)
}

// doRequest performs an HTTP request with JSON body and response handling.
// It handles connection errors, status code validation, and JSON decoding.
func (c *APIClient) doRequest(method, path string, body, result interface{}, expectedStatus ...int) error {
	var bodyReader io.Reader
	if body != nil {
		bodyData, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to encode request: %w", err)
		}
		bodyReader = bytes.NewReader(bodyData)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
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

	// Create a client without a Timeout for long-running requests.
	// Important: When both http.Client.Timeout and context deadline are set,
	// the shorter one wins. Since c.httpClient has a 30s timeout, we must use
	// a separate client here to allow the context timeout (potentially minutes)
	// to control cancellation.
	client := &http.Client{}
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

// CreateShed creates a new shed.
func (c *APIClient) CreateShed(req *config.CreateShedRequest) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/sheds", req, &shed, c.createTimeout, http.StatusCreated, http.StatusOK); err != nil {
		return nil, err
	}
	return &shed, nil
}

// CreateShedWithProgress creates a new shed and streams progress events via SSE.
func (c *APIClient) CreateShedWithProgress(req *config.CreateShedRequest, onProgress func(backend.ProgressEvent)) (*config.Shed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.createTimeout)
	defer cancel()

	bodyData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/sheds", bytes.NewReader(bodyData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	client := &http.Client{}
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

	return c.readSSEStream(resp.Body, onProgress)
}

// readSSEStream parses an SSE event stream, calling onProgress for progress events
// and returning the final Shed from the complete event.
//
// This implements the key parts of the SSE specification:
//   - "event:" sets the event type for the next dispatch
//   - "data:" lines are concatenated (with newlines) to form the event payload
//   - Lines starting with ":" are comments (used for keep-alive pings)
//   - A blank line dispatches the accumulated event
func (c *APIClient) readSSEStream(body io.Reader, onProgress func(backend.ProgressEvent)) (*config.Shed, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	var eventType string
	var dataBuf strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		// Blank line dispatches the accumulated event
		if line == "" {
			if dataBuf.Len() > 0 {
				data := dataBuf.String()
				dataBuf.Reset()

				switch eventType {
				case "progress":
					var event backend.ProgressEvent
					if err := json.Unmarshal([]byte(data), &event); err == nil && onProgress != nil {
						onProgress(event)
					}
				case "complete":
					var shed config.Shed
					if err := json.Unmarshal([]byte(data), &shed); err != nil {
						return nil, fmt.Errorf("failed to parse complete event: %w", err)
					}
					return &shed, nil
				case "error":
					var apiErr config.APIError
					if err := json.Unmarshal([]byte(data), &apiErr); err != nil {
						return nil, fmt.Errorf("server error: %s", data)
					}
					return nil, fmt.Errorf("%s: %s", apiErr.Error.Code, apiErr.Error.Message)
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

// DeleteShed deletes a shed.
func (c *APIClient) DeleteShed(name string, keepVolume bool) error {
	path := "/api/sheds/" + name
	if keepVolume {
		path += "?keep_volume=true"
	}
	return c.doRequest(http.MethodDelete, path, nil, nil, http.StatusNoContent, http.StatusOK)
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

// Ping checks if the server is reachable.
func (c *APIClient) Ping() bool {
	client := &http.Client{
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get(c.baseURL + "/api/info")
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
