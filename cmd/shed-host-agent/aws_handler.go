package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/protocol"
)

// AWSHandler processes AWS credential requests from the plugin message bus for
// a single shed server.
type AWSHandler struct {
	backend  AWSBackend
	client   *sdk.HostClient
	approval ApprovalGate
	audit    *AuditLogger
	server   string
	logger   *slog.Logger
}

// NewAWSHandler creates a handler for the aws-credentials namespace on the
// named server.
func NewAWSHandler(backend AWSBackend, client *sdk.HostClient, approval ApprovalGate, audit *AuditLogger, server string, logger *slog.Logger) *AWSHandler {
	return &AWSHandler{
		backend:  backend,
		client:   client,
		approval: approval,
		audit:    audit,
		server:   server,
		logger:   logger,
	}
}

// Run subscribes to the aws-credentials namespace and processes messages until
// the context is cancelled.
func (h *AWSHandler) Run(ctx context.Context) {
	ch := h.client.Subscribe(ctx, protocol.NamespaceAWSCredentials)

	for env := range ch {
		h.handleMessage(ctx, env)
	}
}

func (h *AWSHandler) handleMessage(ctx context.Context, env *sdk.Envelope) {
	shedName := ""
	if env.Shed != nil {
		shedName = env.Shed.Name
	}

	var op struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(env.Payload, &op); err != nil {
		h.logger.Error("failed to parse operation", "error", err)
		h.sendError(ctx, env, "invalid payload", protocol.AWSCodeInternal)
		return
	}

	switch op.Operation {
	case protocol.AWSOpGetCredentials:
		h.handleGetCredentials(ctx, env, shedName)
	case protocol.AWSOpPing:
		h.handlePing(ctx, env, shedName)
	case protocol.AWSOpStatus:
		h.handleStatus(ctx, env, shedName)
	default:
		h.logger.Warn("unknown operation", "operation", op.Operation, "shed", shedName)
		h.sendError(ctx, env, fmt.Sprintf("unknown operation: %s", op.Operation), protocol.AWSCodeInternal)
	}
}

func (h *AWSHandler) handleGetCredentials(ctx context.Context, env *sdk.Envelope, shedName string) {
	// Approval gate (deny-all default fails closed).
	outcome, err := h.approval.Approve(h.server, shedName, "AWS credentials request")
	if err != nil {
		h.logger.Info("aws credentials denied by approval gate", "shed", shedName, "error", err)
		h.sendError(ctx, env, "approval denied", protocol.AWSCodeAssumeRoleFailed)
		h.audit.LogEntry(AuditEntry{
			Server: h.server, Shed: shedName, Namespace: protocol.NamespaceAWSCredentials, Operation: protocol.AWSOpGetCredentials,
			Result: "denied", Approval: h.approval.Method(),
			DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
		})
		return
	}

	creds, err := h.backend.GetCredentials(ctx, h.server, shedName)
	if err != nil {
		h.logger.Error("get credentials failed", "error", err, "server", h.server, "shed", shedName)
		h.sendError(ctx, env, "credential request failed", protocol.AWSCodeAssumeRoleFailed)
		h.audit.LogEntry(AuditEntry{
			Server: h.server, Shed: shedName, Namespace: protocol.NamespaceAWSCredentials, Operation: protocol.AWSOpGetCredentials,
			Result: "error", Detail: err.Error(), Approval: h.approval.Method(),
			DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
		})
		return
	}

	resp := protocol.AWSCredentialsResponse{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
	}
	// Leave Expiration empty (omitted on the wire) when unknown — passthrough
	// creds may carry no expiry hint, and a zero time must not serialize as
	// year-0001 or "", which the guest SDK can't parse.
	if !creds.Expiration.IsZero() {
		resp.Expiration = creds.Expiration.Format("2006-01-02T15:04:05Z")
	}

	h.sendResponse(ctx, env, resp)
	h.audit.LogEntry(AuditEntry{
		Server: h.server, Shed: shedName, Namespace: protocol.NamespaceAWSCredentials, Operation: protocol.AWSOpGetCredentials,
		Result: "ok", Detail: awsExpiryDetail(creds.Expiration), Approval: h.approval.Method(),
		DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
	})
	h.logger.Debug("credentials served", "server", h.server, "shed", shedName, "expires", awsExpiryDetail(creds.Expiration))
}

func (h *AWSHandler) handlePing(ctx context.Context, env *sdk.Envelope, shedName string) {
	resp := protocol.AWSPingResponse{Status: "ok"}
	h.sendResponse(ctx, env, resp)
	h.logger.Debug("ping", "shed", shedName)
}

func (h *AWSHandler) handleStatus(ctx context.Context, env *sdk.Envelope, shedName string) {
	role, cachedUntil := h.backend.Status(h.server, shedName)

	resp := protocol.AWSStatusResponse{
		Connected: true,
		Role:      role,
	}
	if cachedUntil != nil {
		resp.CachedUntil = cachedUntil.Format("2006-01-02T15:04:05Z")
	}

	h.sendResponse(ctx, env, resp)
	h.logger.Debug("status", "role", role, "shed", shedName)
}

func (h *AWSHandler) sendResponse(ctx context.Context, req *sdk.Envelope, payload any) {
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

func (h *AWSHandler) sendError(ctx context.Context, req *sdk.Envelope, msg, code string) {
	errResp := protocol.AWSErrorResponse{Error: msg, Code: code}
	h.sendResponse(ctx, req, errResp)
}

// awsExpiryDetail renders the audit detail for a successful credential vend,
// using "expires:none" when the credentials carry no expiry (passthrough without
// a hint) rather than a misleading expires:00:00.
func awsExpiryDetail(exp time.Time) string {
	if exp.IsZero() {
		return "expires:none"
	}
	return fmt.Sprintf("expires:%s", exp.Format("15:04"))
}
