package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/protocol"
)

// SSHHandler processes SSH agent requests from the plugin message bus for a
// single shed server.
type SSHHandler struct {
	backend  SSHBackend
	client   *sdk.HostClient
	approval ApprovalGate
	audit    *AuditLogger
	server   string
	logger   *slog.Logger
}

// NewSSHHandler creates a handler for the ssh-agent namespace on the named
// server.
func NewSSHHandler(backend SSHBackend, client *sdk.HostClient, approval ApprovalGate, audit *AuditLogger, server string, logger *slog.Logger) *SSHHandler {
	return &SSHHandler{
		backend:  backend,
		client:   client,
		approval: approval,
		audit:    audit,
		server:   server,
		logger:   logger,
	}
}

// Run subscribes to the ssh-agent namespace and processes messages until the
// context is cancelled.
func (h *SSHHandler) Run(ctx context.Context) {
	ch := h.client.Subscribe(ctx, protocol.NamespaceSSHAgent)

	for env := range ch {
		h.handleMessage(ctx, env)
	}
}

func (h *SSHHandler) handleMessage(ctx context.Context, env *sdk.Envelope) {
	shedName := ""
	if env.Shed != nil {
		shedName = env.Shed.Name
	}

	// Parse operation from payload
	var op struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(env.Payload, &op); err != nil {
		h.logger.Error("failed to parse operation", "error", err)
		h.sendError(ctx, env, "invalid payload", protocol.SSHCodeInternal)
		return
	}

	switch op.Operation {
	case protocol.SSHOpList:
		h.handleList(ctx, env, shedName)
	case protocol.SSHOpSign:
		h.handleSign(ctx, env, shedName)
	case protocol.SSHOpPing:
		h.handlePing(ctx, env, shedName)
	case protocol.SSHOpStatus:
		h.handleStatus(ctx, env, shedName)
	default:
		h.logger.Warn("unknown operation", "operation", op.Operation, "shed", shedName)
		h.sendError(ctx, env, fmt.Sprintf("unknown operation: %s", op.Operation), protocol.SSHCodeInternal)
	}
}

func (h *SSHHandler) handleList(ctx context.Context, env *sdk.Envelope, shedName string) {
	keys, err := h.backend.List()
	if err != nil {
		h.logger.Error("list keys failed", "error", err, "shed", shedName)
		h.sendError(ctx, env, "key listing failed", protocol.SSHCodeInternal)
		h.audit.Log(h.server, shedName, protocol.NamespaceSSHAgent, protocol.SSHOpList, "error", err.Error(), "none")
		return
	}

	keyInfos := make([]protocol.SSHKeyInfo, len(keys))
	for i, k := range keys {
		keyInfos[i] = protocol.SSHKeyInfo{
			Format:  k.Format,
			Blob:    base64.StdEncoding.EncodeToString(k.Blob),
			Comment: k.Comment,
		}
	}

	resp := protocol.SSHListResponse{Keys: keyInfos}
	h.sendResponse(ctx, env, resp)
	h.audit.Log(h.server, shedName, protocol.NamespaceSSHAgent, protocol.SSHOpList, "ok", fmt.Sprintf("%d keys", len(keys)), "none")
	h.logger.Debug("list keys", "count", len(keys), "shed", shedName)
}

func (h *SSHHandler) handleSign(ctx context.Context, env *sdk.Envelope, shedName string) {
	var req protocol.SSHSignRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		h.sendError(ctx, env, "invalid sign request", protocol.SSHCodeInternal)
		return
	}

	// Approval gate (deny-all default fails closed).
	outcome, err := h.approval.Approve(h.server, shedName, "SSH sign request")
	if err != nil {
		h.logger.Info("sign denied by approval gate", "shed", shedName, "error", err)
		h.sendError(ctx, env, "approval denied", protocol.SSHCodeSignFailed)
		h.audit.LogEntry(AuditEntry{
			Server: h.server, Shed: shedName, Namespace: protocol.NamespaceSSHAgent, Operation: protocol.SSHOpSign,
			Result: "denied", Approval: h.approval.Method(),
			DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
		})
		return
	}
	approvalResult := h.approval.Method()

	// Decode the public key
	pubKeyBytes, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil {
		h.sendError(ctx, env, "invalid public key encoding", protocol.SSHCodeInternal)
		return
	}
	pubKey, err := ssh.ParsePublicKey(pubKeyBytes)
	if err != nil {
		h.sendError(ctx, env, "invalid public key", protocol.SSHCodeKeyNotFound)
		return
	}

	// Decode the challenge data
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		h.sendError(ctx, env, "invalid challenge data encoding", protocol.SSHCodeInternal)
		return
	}

	sig, err := h.backend.Sign(pubKey, data, agent.SignatureFlags(req.Flags))
	if err != nil {
		h.logger.Error("sign failed", "error", err, "shed", shedName)
		h.sendError(ctx, env, "sign operation failed", protocol.SSHCodeSignFailed)
		h.audit.LogEntry(AuditEntry{
			Server: h.server, Shed: shedName, Namespace: protocol.NamespaceSSHAgent, Operation: protocol.SSHOpSign,
			Result: "error", Detail: pubKey.Type(), Approval: approvalResult,
			DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
		})
		return
	}

	resp := protocol.SSHSignResponse{
		Format: sig.Format,
		Blob:   base64.StdEncoding.EncodeToString(sig.Blob),
	}
	h.sendResponse(ctx, env, resp)
	h.audit.LogEntry(AuditEntry{
		Server: h.server, Shed: shedName, Namespace: protocol.NamespaceSSHAgent, Operation: protocol.SSHOpSign,
		Result: "ok", Detail: pubKey.Type(), Approval: approvalResult,
		DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
	})
	h.logger.Debug("sign completed", "key_type", pubKey.Type(), "shed", shedName)
}

func (h *SSHHandler) handlePing(ctx context.Context, env *sdk.Envelope, shedName string) {
	resp := protocol.SSHPingResponse{Status: "ok"}
	h.sendResponse(ctx, env, resp)
	h.logger.Debug("ping", "shed", shedName)
}

func (h *SSHHandler) handleStatus(ctx context.Context, env *sdk.Envelope, shedName string) {
	keys, err := h.backend.List()
	keyCount := 0
	if err == nil {
		keyCount = len(keys)
	}

	resp := protocol.SSHStatusResponse{
		Connected: true,
		Mode:      h.backend.Mode(),
		KeyCount:  keyCount,
	}
	h.sendResponse(ctx, env, resp)
	h.logger.Debug("status", "mode", resp.Mode, "keys", keyCount, "shed", shedName)
}

func (h *SSHHandler) sendResponse(ctx context.Context, req *sdk.Envelope, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error("failed to marshal response", "error", err)
		return
	}

	resp := sdk.NewResponse(req.ID, req.Namespace, data)
	resp.Shed = req.Shed

	if err := h.client.Respond(ctx, req.Namespace, resp); err != nil {
		h.logger.Error("failed to send response", "error", err)
	}
}

func (h *SSHHandler) sendError(ctx context.Context, req *sdk.Envelope, msg, code string) {
	errResp := protocol.SSHErrorResponse{Error: msg, Code: code}
	h.sendResponse(ctx, req, errResp)
}
