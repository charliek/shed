package main

import (
	"context"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// bootstrapCredentialFn is the bootstrap entry point used by the credential
// refresh path, overridable in tests to avoid spawning a real ssh subprocess.
var bootstrapCredentialFn = bootstrapServerCredential

// bootstrapServerCredential mints a control credential over the server's
// reserved `_bootstrap` SSH channel and returns it. The server's SSH host key
// must already be pinned in known_hosts (the trust decision stays with
// confirmHostKey, run earlier in `server add`); ssh enforces that pin via
// StrictHostKeyChecking=yes. The shared sdk/bootstrap helper owns the argv,
// timeout, and error classification so the CLI and the host-agent reach a
// server identically.
//
// RunCredential (not Run) is used unconditionally: it submits a CSR, which a
// token-mode or pre-mtls server ignores and an mtls-mode server signs. The CLI
// therefore never has to know a server's auth.mode in advance, and a server
// whose mode changed since the entry was written still answers usefully.
func bootstrapServerCredential(host string, sshPort int, scope, clientKind string) (sdk.Credential, error) {
	return sdkbootstrap.RunCredential(context.Background(), sdkbootstrap.Params{
		Host:           host,
		Port:           sshPort,
		KnownHostsPath: config.GetKnownHostsPath(),
		Scope:          scope,
		ClientKind:     clientKind,
	})
}
