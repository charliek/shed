// Package sshagent implements the ssh/agent.Agent interface by proxying
// requests through shed's plugin message bus.
package sshagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/protocol"
)

// Agent implements ssh/agent.Agent by forwarding operations through the
// shed plugin message bus.
type Agent struct {
	bus *sdk.BusClient
}

// Option configures an Agent.
type Option func(*Agent)

// WithPublishURL sets the shed-agent publish endpoint URL.
func WithPublishURL(url string) Option {
	return func(a *Agent) {
		a.bus.PublishURL = url
	}
}

// New creates a new Agent that publishes requests to the given URL.
func New(opts ...Option) *Agent {
	a := &Agent{
		bus: sdk.NewBusClient(sdk.DefaultPublishURL, sdk.DefaultBusTimeout),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// List returns the identities known to the agent.
func (a *Agent) List() ([]*agent.Key, error) {
	req := protocol.SSHListRequest{Operation: protocol.SSHOpList}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling list request: %w", err)
	}

	respPayload, err := a.bus.Publish(context.Background(), protocol.NamespaceSSHAgent, payload)
	if err != nil {
		return nil, fmt.Errorf("list request failed: %w", err)
	}

	// Check for error response
	var errResp protocol.SSHErrorResponse
	if json.Unmarshal(respPayload, &errResp) == nil && errResp.Code != "" {
		return nil, fmt.Errorf("host error: %s (%s)", errResp.Error, errResp.Code)
	}

	var listResp protocol.SSHListResponse
	if err := json.Unmarshal(respPayload, &listResp); err != nil {
		return nil, fmt.Errorf("parsing list response: %w", err)
	}

	keys := make([]*agent.Key, 0, len(listResp.Keys))
	for _, k := range listResp.Keys {
		blob, err := base64.StdEncoding.DecodeString(k.Blob)
		if err != nil {
			continue
		}
		keys = append(keys, &agent.Key{
			Format:  k.Format,
			Blob:    blob,
			Comment: k.Comment,
		})
	}
	return keys, nil
}

// Sign has the agent sign the data using a protocol message.
func (a *Agent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return a.signWithFlags(key, data, 0)
}

// SignWithFlags implements agent.ExtendedAgent for flag-aware signing.
func (a *Agent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	return a.signWithFlags(key, data, flags)
}

func (a *Agent) signWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	pubKeyStr := base64.StdEncoding.EncodeToString(key.Marshal())
	req := protocol.SSHSignRequest{
		Operation: protocol.SSHOpSign,
		PublicKey: pubKeyStr,
		Data:      base64.StdEncoding.EncodeToString(data),
		Flags:     uint32(flags),
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling sign request: %w", err)
	}

	respPayload, err := a.bus.Publish(context.Background(), protocol.NamespaceSSHAgent, payload)
	if err != nil {
		return nil, fmt.Errorf("sign request failed: %w", err)
	}

	// Check for error response
	var errResp protocol.SSHErrorResponse
	if json.Unmarshal(respPayload, &errResp) == nil && errResp.Code != "" {
		return nil, fmt.Errorf("host error: %s (%s)", errResp.Error, errResp.Code)
	}

	var signResp protocol.SSHSignResponse
	if err := json.Unmarshal(respPayload, &signResp); err != nil {
		return nil, fmt.Errorf("parsing sign response: %w", err)
	}

	blob, err := base64.StdEncoding.DecodeString(signResp.Blob)
	if err != nil {
		return nil, fmt.Errorf("decoding signature: %w", err)
	}

	return &ssh.Signature{
		Format: signResp.Format,
		Blob:   blob,
	}, nil
}

// Ping sends a health check ping and returns nil if the host agent responds.
func (a *Agent) Ping(timeout time.Duration) error {
	return a.bus.Ping(context.Background(), protocol.NamespaceSSHAgent, timeout)
}

// Unsupported agent.ExtendedAgent methods — the SSH agent protocol requires
// these but we only need List and Sign for credential brokering.

func (a *Agent) Add(_ agent.AddedKey) error     { return agent.ErrExtensionUnsupported }
func (a *Agent) Remove(_ ssh.PublicKey) error   { return agent.ErrExtensionUnsupported }
func (a *Agent) RemoveAll() error               { return agent.ErrExtensionUnsupported }
func (a *Agent) Lock(_ []byte) error            { return agent.ErrExtensionUnsupported }
func (a *Agent) Unlock(_ []byte) error          { return agent.ErrExtensionUnsupported }
func (a *Agent) Signers() ([]ssh.Signer, error) { return nil, agent.ErrExtensionUnsupported }
func (a *Agent) Extension(_ string, _ []byte) ([]byte, error) {
	return nil, agent.ErrExtensionUnsupported
}
