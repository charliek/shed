package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// DefaultPublishURL is the shed-agent publish endpoint inside the VM.
	DefaultPublishURL = "http://127.0.0.1:498/v1/publish"

	// DefaultBusTimeout is the request timeout for credential operations.
	DefaultBusTimeout = 3 * time.Second
)

// publishRequest is the body sent to the shed-agent /v1/publish endpoint.
type publishRequest struct {
	Namespace string          `json:"namespace"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// BusClient publishes messages to the shed plugin bus via the shed-agent
// HTTP endpoint. Used by guest-side extension binaries.
type BusClient struct {
	PublishURL string
	HTTPClient *http.Client
}

// NewBusClient creates a BusClient with the given publish URL and timeout.
func NewBusClient(publishURL string, timeout time.Duration) *BusClient {
	return &BusClient{
		PublishURL: publishURL,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// Publish sends a request to the given namespace and returns the response
// envelope's payload. The context controls cancellation; the HTTP client
// timeout controls the overall deadline.
func (c *BusClient) Publish(ctx context.Context, namespace string, payload json.RawMessage) (json.RawMessage, error) {
	req := publishRequest{
		Namespace: namespace,
		Type:      string(MessageTypeRequest),
		Payload:   payload,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling publish request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.PublishURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("publishing to bus: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("publish failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var env Envelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("parsing response envelope: %w", err)
	}

	return env.Payload, nil
}

// Ping sends a health check ping to the given namespace and returns nil
// if the host agent responds within the timeout.
func (c *BusClient) Ping(ctx context.Context, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, err := json.Marshal(map[string]string{"operation": "ping"})
	if err != nil {
		return fmt.Errorf("marshaling ping: %w", err)
	}

	_, err = c.Publish(ctx, namespace, payload)
	return err
}
