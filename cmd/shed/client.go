package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/charliek/shed/internal/config"
)

// APIClient provides methods for interacting with the shed server API.
type APIClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewAPIClient creates a new API client for the given host and port.
func NewAPIClient(host string, port int) *APIClient {
	return &APIClient{
		baseURL: fmt.Sprintf("http://%s:%d", host, port),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewAPIClientFromEntry creates a new API client from a server entry.
func NewAPIClientFromEntry(entry *config.ServerEntry) *APIClient {
	return NewAPIClient(entry.Host, entry.HTTPPort)
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
	if err := c.doRequest(http.MethodPost, "/api/sheds", req, &shed, http.StatusCreated, http.StatusOK); err != nil {
		return nil, err
	}
	return &shed, nil
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
	if err := c.doRequest(http.MethodPost, "/api/sheds/"+name+"/start", nil, &shed); err != nil {
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
