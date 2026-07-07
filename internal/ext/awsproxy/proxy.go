// Package awsproxy implements the guest-side AWS container credential endpoint.
// It serves the HTTP format the AWS SDK expects and translates requests into
// message bus calls to the host agent. The proxy is a passthrough — it does
// not cache credentials.
package awsproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/protocol"
)

// awsSDKResponse is the exact JSON format the AWS SDK expects from a
// container credential endpoint. Field names must match exactly.
type awsSDKResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	// Expiration is omitted when empty so the guest AWS SDK treats the creds as
	// non-expiring (and re-fetches on a 403) rather than failing to parse "".
	Expiration string `json:"Expiration,omitempty"`
}

// awsSDKErrorResponse is returned on errors with an actionable message.
type awsSDKErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Proxy handles AWS credential requests from the SDK and translates them
// into message bus requests.
type Proxy struct {
	bus    *sdk.BusClient
	logger *slog.Logger
}

// Option configures a Proxy.
type Option func(*Proxy)

// WithPublishURL sets the shed-agent publish endpoint URL.
func WithPublishURL(url string) Option {
	return func(p *Proxy) {
		p.bus.PublishURL = url
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(p *Proxy) {
		p.logger = logger
	}
}

// New creates a new Proxy with the given options.
func New(opts ...Option) *Proxy {
	p := &Proxy{
		bus:    sdk.NewBusClient(sdk.DefaultPublishURL, sdk.DefaultBusTimeout),
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// HandleCredentials is the HTTP handler for GET /credentials.
func (p *Proxy) HandleCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req := protocol.AWSCredentialsRequest{Operation: protocol.AWSOpGetCredentials}
	payload, err := json.Marshal(req)
	if err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to marshal request", "")
		return
	}

	respPayload, err := p.bus.Publish(r.Context(), protocol.NamespaceAWSCredentials, payload)
	if err != nil {
		p.logger.Error("credential request failed", "error", err)
		p.writeError(w, http.StatusServiceUnavailable,
			"credential request timed out",
			"shed-host-agent not reachable. Is it running on the host machine?",
		)
		return
	}

	// Check for error response from host
	var errResp protocol.AWSErrorResponse
	if json.Unmarshal(respPayload, &errResp) == nil && errResp.Code != "" {
		p.logger.Error("host returned error", "error", errResp.Error, "code", errResp.Code)
		p.writeError(w, http.StatusServiceUnavailable, errResp.Error, "")
		return
	}

	var creds protocol.AWSCredentialsResponse
	if err := json.Unmarshal(respPayload, &creds); err != nil {
		p.logger.Error("failed to parse credentials response", "error", err)
		p.writeError(w, http.StatusBadGateway, "invalid response from host agent", "")
		return
	}

	// Translate to the AWS SDK expected format (PascalCase fields)
	sdkResp := awsSDKResponse{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		Token:           creds.SessionToken,
		Expiration:      creds.Expiration,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sdkResp); err != nil {
		p.logger.Error("failed to write response", "error", err)
	}
}

// Ping sends a health check ping and returns nil if the host agent responds.
func (p *Proxy) Ping(timeout time.Duration) error {
	return p.bus.Ping(context.Background(), protocol.NamespaceAWSCredentials, timeout)
}

func (p *Proxy) writeError(w http.ResponseWriter, status int, msg, hint string) {
	if hint == "" {
		hint = "Ensure shed-host-agent is running on the host machine."
	}
	resp := awsSDKErrorResponse{
		Error:   msg,
		Message: msg,
		Hint:    hint,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		p.logger.Error("failed to write error response", "error", err)
	}
}
