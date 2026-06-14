package vmutil

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/egress"
)

// Guest paths for the injected egress-proxy configuration. Both live in the
// persistent upper, so they survive stop/start. The dockerd drop-in targets
// the IN-GUEST docker daemon — never the host's.
const (
	egressProfilePath    = "/etc/profile.d/10-shed-egress.sh"
	egressDockerDropIn   = "/etc/systemd/system/docker.service.d/http-proxy.conf"
	egressManagedComment = "# Managed by shed egress-control. Do not edit."
)

// EgressProxyEnv is the per-shed forward-proxy configuration injected into a
// guest so cooperating clients (curl/git/dockerd) route egress through the
// host-side proxy.
type EgressProxyEnv struct {
	Gateway string // host IP the guest reaches the proxy on
	Port    int    // per-shed listener port
	Token   string // per-shed proxy-auth token (basic-auth username)
	NoProxy string // comma-separated NO_PROXY list
}

// ProxyURL is the http://<token>@<gateway>:<port> forward-proxy URL. The token
// becomes the basic-auth username that the proxy validates per shed.
func (e EgressProxyEnv) ProxyURL() string {
	if e.Token != "" {
		return fmt.Sprintf("http://%s@%s:%d", e.Token, e.Gateway, e.Port)
	}
	return fmt.Sprintf("http://%s:%d", e.Gateway, e.Port)
}

// EnvPairs returns the proxy environment as KEY=VALUE pairs, in both lower- and
// upper-case (tools disagree on which they read).
func (e EgressProxyEnv) EnvPairs() []string {
	u := e.ProxyURL()
	return []string{
		"HTTP_PROXY=" + u, "http_proxy=" + u,
		"HTTPS_PROXY=" + u, "https_proxy=" + u,
		"NO_PROXY=" + e.NoProxy, "no_proxy=" + e.NoProxy,
	}
}

// BuildNoProxy assembles the NO_PROXY list: loopback, the backend subnet, the
// host gateway, and .local. Empty subnet/gateway entries are skipped so the
// list stays well-formed on backends that don't expose them.
func BuildNoProxy(gateway, subnet string) string {
	parts := []string{"localhost", "127.0.0.1", "::1"}
	if subnet != "" {
		parts = append(parts, subnet)
	}
	if gateway != "" {
		parts = append(parts, gateway)
	}
	parts = append(parts, ".local")
	return strings.Join(parts, ",")
}

// renderEgressProfile is the /etc/profile.d script exporting the proxy env for
// login shells (shed exec, provisioning, interactive sessions).
func renderEgressProfile(e EgressProxyEnv) string {
	var b strings.Builder
	b.WriteString(egressManagedComment + "\n")
	for _, kv := range e.EnvPairs() {
		b.WriteString("export " + kv + "\n")
	}
	return b.String()
}

// renderDockerProxyDropIn is the in-GUEST dockerd systemd drop-in so the guest
// docker daemon's pulls route through (and are audited by) the proxy.
func renderDockerProxyDropIn(e EgressProxyEnv) string {
	u := e.ProxyURL()
	return fmt.Sprintf("%s\n[Service]\nEnvironment=\"HTTP_PROXY=%s\"\nEnvironment=\"HTTPS_PROXY=%s\"\nEnvironment=\"NO_PROXY=%s\"\n",
		egressManagedComment, u, u, e.NoProxy)
}

// SetupEgress is the shared body of the per-backend ConfigureEgressProxy hook:
// it opens (or reuses) this shed's proxy listener, registers its teardown on
// cleanup so a later failure tears it back down, and injects the proxy env into
// the guest. existingPort/Token are the persisted assignment (0/"" on first
// create). Returns the effective port + token (for the caller to persist in
// backend metadata) and the env pairs to thread into the clone exec. A failure
// returns a non-nil error so the orchestrator aborts + unwinds.
func SetupEgress(ctx context.Context, mgr *egress.Manager, agent *AgentClient, name string, existingPort int, existingToken, gateway, subnet string, specs []egress.ProfileSpec, cleanup *backend.Cleanup) (port int, token string, env []string, err error) {
	port, token, err = mgr.Configure(name, existingPort, existingToken, gateway, specs)
	if err != nil {
		return 0, "", nil, err
	}
	cleanup.Register("close egress listener", func() error { return mgr.Remove(name) })
	pe := EgressProxyEnv{Gateway: gateway, Port: port, Token: token, NoProxy: BuildNoProxy(gateway, subnet)}
	if err := InjectEgressProxy(ctx, agent, pe); err != nil {
		return 0, "", nil, err
	}
	return port, token, pe.EnvPairs(), nil
}

// ApplyEgressLive (re)configures a running shed's listener and re-injects the
// guest env — the live `shed egress set` path (no create-transaction cleanup).
// On an inject failure it rolls back the listener it just opened. Returns the
// effective port + token to persist.
func ApplyEgressLive(ctx context.Context, mgr *egress.Manager, agent *AgentClient, name string, existingPort int, existingToken, gateway, subnet string, specs []egress.ProfileSpec) (int, string, error) {
	port, token, err := mgr.Configure(name, existingPort, existingToken, gateway, specs)
	if err != nil {
		return 0, "", err
	}
	pe := EgressProxyEnv{Gateway: gateway, Port: port, Token: token, NoProxy: BuildNoProxy(gateway, subnet)}
	if err := InjectEgressProxy(ctx, agent, pe); err != nil {
		_ = mgr.Remove(name) // roll back the listener we just opened
		return 0, "", err
	}
	return port, token, nil
}

// ClearEgressLive removes a running shed's listener and its injected guest files
// — the live `shed egress off` path.
func ClearEgressLive(ctx context.Context, mgr *egress.Manager, agent *AgentClient, name string) error {
	_ = mgr.Remove(name)
	return RemoveEgressProxy(ctx, agent)
}

// InjectEgressProxy writes the proxy env into the guest's persistent upper (a
// login-shell profile + an in-GUEST dockerd drop-in) and reloads the guest
// docker so it picks up the change (best-effort; no-op if docker is absent).
func InjectEgressProxy(ctx context.Context, agent *AgentClient, e EgressProxyEnv) error {
	if err := writeGuestFileSudo(ctx, agent, egressProfilePath, renderEgressProfile(e)); err != nil {
		return fmt.Errorf("write egress profile: %w", err)
	}
	if err := writeGuestFileSudo(ctx, agent, egressDockerDropIn, renderDockerProxyDropIn(e)); err != nil {
		return fmt.Errorf("write docker proxy drop-in: %w", err)
	}
	reloadGuestDocker(ctx, agent)
	return nil
}

// RemoveEgressProxy deletes the injected files and reloads the guest docker
// back to no-proxy. Best-effort and idempotent (rm -f). Never touches host
// docker. Used when egress is turned off on a still-running shed.
func RemoveEgressProxy(ctx context.Context, agent *AgentClient) error {
	if err := execGuestSudo(ctx, agent, fmt.Sprintf("rm -f %s %s", egressProfilePath, egressDockerDropIn)); err != nil {
		return fmt.Errorf("remove egress files: %w", err)
	}
	reloadGuestDocker(ctx, agent)
	return nil
}

// writeGuestFileSudo creates a root-owned guest file at an absolute path with
// the given content (mode 0644, world-readable so the shed user's login shells
// can source the profile). The shed user has passwordless sudo; the redirect
// runs inside the root shell so /etc is writable.
func writeGuestFileSudo(ctx context.Context, agent *AgentClient, absPath, content string) error {
	script := fmt.Sprintf("mkdir -p %s && cat > %s && chmod 0644 %s", path.Dir(absPath), absPath, absPath)
	opts := backend.ExecOptions{
		Cmd:    []string{"sudo", "sh", "-c", script},
		Stdin:  io.NopCloser(strings.NewReader(content)),
		Stdout: NopWriteCloser(io.Discard),
		Stderr: NopWriteCloser(io.Discard),
		TTY:    false,
	}
	return agent.Exec(ctx, opts)
}

func execGuestSudo(ctx context.Context, agent *AgentClient, script string) error {
	opts := backend.ExecOptions{
		Cmd:    []string{"sudo", "sh", "-c", script},
		Stdout: NopWriteCloser(io.Discard),
		Stderr: NopWriteCloser(io.Discard),
		TTY:    false,
	}
	return agent.Exec(ctx, opts)
}

// reloadGuestDocker reloads the in-guest docker daemon (best-effort) so a
// proxy drop-in change takes effect. No-op when docker is not installed (base
// / extensions image variants).
func reloadGuestDocker(ctx context.Context, agent *AgentClient) {
	script := "command -v docker >/dev/null 2>&1 && { systemctl daemon-reload && systemctl reload-or-try-restart docker; } || true"
	_ = execGuestSudo(ctx, agent, script)
}
