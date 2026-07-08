package main

import (
	"context"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// bootstrapFn is the bootstrap entry point used by the token-refresh path,
// overridable in tests to avoid spawning a real ssh subprocess.
var bootstrapFn = bootstrapServer

// bootstrapServer mints a token over the server's reserved `_bootstrap` SSH
// channel and returns the bundle. The server's SSH host key must already be
// pinned in known_hosts (the trust decision stays with confirmHostKey, run
// earlier in `server add`); ssh enforces that pin via StrictHostKeyChecking=yes.
// The shared sdk/bootstrap helper owns the argv, timeout, and error
// classification so the CLI and the host-agent reach a server identically.
func bootstrapServer(host string, sshPort int, scope, clientKind string) (sdk.Bundle, error) {
	return sdkbootstrap.Run(context.Background(), sdkbootstrap.Params{
		Host:           host,
		Port:           sshPort,
		KnownHostsPath: config.GetKnownHostsPath(),
		Scope:          scope,
		ClientKind:     clientKind,
	})
}
