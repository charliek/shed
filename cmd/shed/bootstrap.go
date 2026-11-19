package main

import (
	"context"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// bootstrapCredentialFn is the bootstrap entry point used by the credential
// refresh path, overridable in tests to avoid spawning a real ssh subprocess.
// identityFile is the entry's identity_file ("" for none), already checked by
// the caller (bootstrapEntryCredential).
var bootstrapCredentialFn = bootstrapServerCredential

// bootstrapEntryCredential is how every CLI path reaches the bootstrap seam
// for an entry (add, --refetch, the refresh, the tunnel daemon's mint): the
// entry's identity_file is checked first (config.IdentityFileKey: its value,
// the file, and its public key, now), and one that cannot be used fails here,
// named for the entry, before ssh is spawned. entry names the entry in that
// message.
//
// entryKey is the fingerprint of the identity file's public key ("" without
// one), learned BEFORE ssh runs: every caller holds the certificate the
// server issues to it (config.IssuedCertificateKey), so the entry's API
// identity is that key and no other (plan 037 P-a).
//
// An identity-file error from any layer below (the SDK checks the file again,
// at its own time of use) comes back as the one typed refusal,
// *config.IdentityFileError named for the entry, so a caller tells it apart by
// its type, never by the words of a message that can hold a path.
func bootstrapEntryCredential(entry, host string, sshPort int, identityFile string) (res sdk.Credential, entryKey string, err error) {
	if entryKey, err = config.IdentityFileKey(entry, identityFile); err != nil {
		return sdk.Credential{}, "", err
	}
	res, err = bootstrapCredentialFn(host, sshPort, identityFile, "control", "cli")
	if named, ok := config.NameIdentityFileError(entry, err); ok {
		return sdk.Credential{}, "", named
	}
	return res, entryKey, err
}

// bootstrapParams is the SDK request the CLI's bootstrap sends: the server's
// SSH endpoint, the shed known_hosts as the pin, and the entry's identity file.
func bootstrapParams(host string, sshPort int, identityFile, scope, clientKind string) sdkbootstrap.Params {
	return sdkbootstrap.Params{
		Host:           host,
		Port:           sshPort,
		KnownHostsPath: config.GetKnownHostsPath(),
		Scope:          scope,
		ClientKind:     clientKind,
		IdentityFile:   identityFile,
	}
}

// bootstrapServerCredential mints a control credential over the server's
// reserved `_bootstrap` SSH channel and returns it. The server's SSH host key
// must already be pinned in known_hosts (the trust decision stays with
// confirmHostKey, run earlier in `server add`); ssh enforces that pin via
// StrictHostKeyChecking=yes. The shared sdk/bootstrap helper owns the argv,
// timeout, and error classification so the CLI and the desktop app's broker
// reach a server identically.
//
// RunCredential (not Run) is used unconditionally: it submits a CSR, which a
// token-mode or pre-mtls server ignores and an mtls-mode server signs. The CLI
// therefore never has to know a server's auth.mode in advance, and a server
// whose mode changed since the entry was written still answers usefully.
func bootstrapServerCredential(host string, sshPort int, identityFile, scope, clientKind string) (sdk.Credential, error) {
	return sdkbootstrap.RunCredential(context.Background(), bootstrapParams(host, sshPort, identityFile, scope, clientKind))
}
