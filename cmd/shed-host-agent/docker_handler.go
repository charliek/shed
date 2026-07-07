package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/protocol"
)

// Audit result values for the docker-credentials get path. shed-desktop renders
// "ok"→green and "error"/"denied"→red, and any other value neutrally — so
// "anonymous" degrades gracefully to a neutral row without a desktop change.
const (
	// auditResultError marks a credential get that genuinely failed: an
	// allowlist deny (REGISTRY_NOT_ALLOWED) or a fault (HELPER_FAILED, …).
	auditResultError = "error"
	// auditResultAnonymous marks a get for an allowed registry that had no
	// credential to serve (CREDENTIALS_NOT_FOUND). This is not a failure — the
	// guest pulls the image anonymously — so it is recorded distinctly from a
	// real error.
	auditResultAnonymous = "anonymous"
	// auditCodeApprovalDenied is the audit/feed code for a request the approval
	// gate rejected — distinct from REGISTRY_NOT_ALLOWED, which is an allowlist
	// deny. The guest still receives REGISTRY_NOT_ALLOWED for both (preserving
	// its behavior); this code only disambiguates the cause on host/admin
	// surfaces, where "you weren't approved" and "this registry isn't permitted"
	// have different fixes.
	auditCodeApprovalDenied = "APPROVAL_DENIED"
)

// DockerHandler processes Docker credential requests from the plugin message
// bus for a single shed server.
type DockerHandler struct {
	backend  DockerBackend
	client   *sdk.HostClient
	approval ApprovalGate
	audit    *AuditLogger
	server   string
	logger   *slog.Logger
}

// NewDockerHandler creates a handler for the docker-credentials namespace on
// the named server.
func NewDockerHandler(backend DockerBackend, client *sdk.HostClient, approval ApprovalGate, audit *AuditLogger, server string, logger *slog.Logger) *DockerHandler {
	return &DockerHandler{
		backend:  backend,
		client:   client,
		approval: approval,
		audit:    audit,
		server:   server,
		logger:   logger,
	}
}

// Run subscribes to the docker-credentials namespace and processes messages until
// the context is cancelled.
func (h *DockerHandler) Run(ctx context.Context) {
	ch := h.client.Subscribe(ctx, protocol.NamespaceDockerCredentials)

	for env := range ch {
		h.handleMessage(ctx, env)
	}
}

func (h *DockerHandler) handleMessage(ctx context.Context, env *sdk.Envelope) {
	shedName := ""
	if env.Shed != nil {
		shedName = env.Shed.Name
	}

	var op struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(env.Payload, &op); err != nil {
		h.logger.Error("failed to parse operation", "error", err)
		h.sendError(ctx, env, "invalid payload", protocol.DockerCodeInternal)
		return
	}

	switch op.Operation {
	case protocol.DockerOpGet:
		h.handleGet(ctx, env, shedName)
	case protocol.DockerOpList:
		h.handleList(ctx, env, shedName)
	case protocol.DockerOpPing:
		h.handlePing(ctx, env, shedName)
	case protocol.DockerOpStatus:
		h.handleStatus(ctx, env, shedName)
	default:
		h.logger.Warn("unknown operation", "operation", op.Operation, "shed", shedName)
		h.sendError(ctx, env, fmt.Sprintf("unknown operation: %s", op.Operation), protocol.DockerCodeInternal)
	}
}

func (h *DockerHandler) handleGet(ctx context.Context, env *sdk.Envelope, shedName string) {
	var req protocol.DockerGetRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		h.logger.Error("failed to parse get request", "error", err)
		h.sendError(ctx, env, "invalid get request", protocol.DockerCodeInternal)
		return
	}

	// Approval gate (deny-all default fails closed). The prompt names the
	// registry so the shed-desktop approval card shows what is being requested.
	outcome, err := h.approval.Approve(h.server, shedName, fmt.Sprintf("Docker credentials for %s", req.ServerURL))
	if err != nil {
		h.logger.Info("docker credentials denied by approval gate", "shed", shedName, "registry", req.ServerURL, "code", auditCodeApprovalDenied, "error", err)
		h.sendError(ctx, env, "approval denied", protocol.DockerCodeNotAllowed)
		h.audit.LogEntry(AuditEntry{
			Server: h.server, Shed: shedName, Namespace: protocol.NamespaceDockerCredentials, Operation: protocol.DockerOpGet,
			Result: "denied", Detail: req.ServerURL, Code: auditCodeApprovalDenied, Reason: err.Error(), Approval: h.approval.Method(),
			DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
		})
		return
	}

	cred, err := h.backend.GetCredentials(ctx, h.server, shedName, req.ServerURL)
	if err != nil {
		code := protocol.DockerCodeInternal
		if de, ok := err.(*dockerError); ok {
			code = de.code
		}

		// CREDENTIALS_NOT_FOUND for an allowed registry is not a failure: the
		// guest helper turns it into Docker's anonymous-pull fallback (public
		// images need no credential). Audit it as "anonymous" — not "error" — so
		// the durable log and the shed-desktop activity feed show a successful
		// anonymous pull rather than a red error, and log it at info level rather
		// than error. REGISTRY_NOT_ALLOWED (an explicit deny) and genuine faults
		// (HELPER_FAILED, INTERNAL_ERROR, …) stay "error" and log loudly.
		result := auditResultError
		if code == protocol.DockerCodeNotFound {
			result = auditResultAnonymous
			h.logger.Info("no credential for allowed registry; guest pulls anonymously",
				"server", h.server, "shed", shedName, "registry", req.ServerURL, "code", code)
		} else {
			h.logger.Error("get credentials failed", "error", err, "server", h.server, "shed", shedName, "registry", req.ServerURL, "code", code)
		}

		// The guest still receives the original error code; it decides how to act
		// (anonymous fallback for not-found, hard fail otherwise). Code/Reason
		// enrich the host/admin surfaces (durable log + shed-desktop). err is the
		// broker's own message — it never contains raw helper stderr (that stays
		// in the Debug log), so it is safe to surface here.
		h.sendError(ctx, env, "credential request failed", code)
		h.audit.LogEntry(AuditEntry{
			Server: h.server, Shed: shedName, Namespace: protocol.NamespaceDockerCredentials, Operation: protocol.DockerOpGet,
			Result: result, Detail: req.ServerURL, Code: code, Reason: err.Error(), Approval: h.approval.Method(),
			DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
		})
		return
	}

	resp := protocol.DockerGetResponse{
		ServerURL: cred.ServerURL,
		Username:  cred.Username,
		Secret:    cred.Secret,
	}

	h.sendResponse(ctx, env, resp)
	h.audit.LogEntry(AuditEntry{
		Server: h.server, Shed: shedName, Namespace: protocol.NamespaceDockerCredentials, Operation: protocol.DockerOpGet,
		Result: "ok", Detail: req.ServerURL, Approval: h.approval.Method(),
		DecidedBy: outcome.DecidedBy, Scope: outcome.Scope, TTL: outcome.TTL,
	})
	h.logger.Debug("credentials served", "shed", shedName, "registry", req.ServerURL)
}

func (h *DockerHandler) handleList(ctx context.Context, env *sdk.Envelope, shedName string) {
	registries, err := h.backend.ListCredentials(ctx, h.server, shedName)
	if err != nil {
		h.logger.Error("list credentials failed", "error", err, "shed", shedName)
		h.sendError(ctx, env, "list failed", protocol.DockerCodeInternal)
		return
	}

	resp := protocol.DockerListResponse{Registries: registries}
	h.sendResponse(ctx, env, resp)
	h.audit.Log(h.server, shedName, protocol.NamespaceDockerCredentials, protocol.DockerOpList, "ok", fmt.Sprintf("count:%d", len(registries)), "none")
	h.logger.Debug("list", "shed", shedName, "count", len(registries))
}

func (h *DockerHandler) handlePing(ctx context.Context, env *sdk.Envelope, shedName string) {
	resp := protocol.DockerPingResponse{Status: "ok"}
	h.sendResponse(ctx, env, resp)
	h.logger.Debug("ping", "shed", shedName)
}

func (h *DockerHandler) handleStatus(ctx context.Context, env *sdk.Envelope, shedName string) {
	allowAll, registryCount := h.backend.Status(h.server, shedName)

	resp := protocol.DockerStatusResponse{
		Connected:     true,
		AllowAll:      allowAll,
		RegistryCount: registryCount,
	}

	h.sendResponse(ctx, env, resp)
	h.logger.Debug("status", "shed", shedName, "allow_all", allowAll, "registry_count", registryCount)
}

func (h *DockerHandler) sendResponse(ctx context.Context, req *sdk.Envelope, payload any) {
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

func (h *DockerHandler) sendError(ctx context.Context, req *sdk.Envelope, msg, code string) {
	errResp := protocol.DockerErrorResponse{Error: msg, Code: code}
	h.sendResponse(ctx, req, errResp)
}
