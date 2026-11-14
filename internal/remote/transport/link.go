// Package transport is the seam between the remote backend and whatever
// carries bytes to a shed's agent. A Link is the server's connection to one
// shed's agent; a Factory makes Links. The backend holds only these two
// interfaces, so a provider that cannot be reached by SSH can bring another
// transport without touching the backend.
//
// Implementations live in sub-packages: sshlink (one SSH connection per shed,
// agent sockets reached as forwarded unix sockets) and locallink (a real
// agent on the server host, for tests and the development driver).
//
// The package imports internal/vmutil, which imports internal/config, so
// internal/config must not import it. Config validates remote.transport and
// remote.agent_install against the literal strings, which are the constants
// below.
package transport

import (
	"context"
	"errors"
	"io"
	"path"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/vmutil"
)

// Transport names, the values of remote.transport in the server config.
const (
	NameSSH   = "ssh"
	NameLocal = "local"
)

// Where the agent listens in a cloud guest, and under which names. The
// values are defined in internal/remote/guest, which the agent imports; they
// are repeated here so a transport needs one import.
const (
	// GuestSocketDir is the directory the cloud image's agent unit passes
	// to --listen unix:<dir>.
	GuestSocketDir = guest.AgentSocketDir

	// PortConsole carries exec and console connections ("1024.sock").
	PortConsole = guest.ConsolePort
	// PortNotify carries the message channel and health probes
	// ("1026.sock").
	PortNotify = guest.NotifyPort
	// PortTCPProxy carries proxied connections to guest services
	// ("1028.sock").
	PortTCPProxy = guest.TCPProxyPort
)

// SocketName returns the agent socket's file name for a port: "<port>.sock".
func SocketName(port uint32) string { return guest.SocketName(port) }

// SocketPath returns the agent socket's path for a port under dir. A
// transport to a cloud guest passes GuestSocketDir.
func SocketPath(dir string, port uint32) string {
	return path.Join(dir, guest.SocketName(port))
}

// AgentBinaryName returns the file name of the agent build for an
// architecture inside remote.agent_dir: "shed-agent-linux-amd64".
func AgentBinaryName(arch string) string { return "shed-agent-linux-" + arch }

// Link is the server's connection to one shed's agent: a vmutil.Dialer that
// can be closed. Everything the server already does with a Dialer (exec, the
// message channel, proxied services) works over a Link unchanged.
//
// Dial opens a new stream to the agent port and may be called from many
// goroutines at once. A link connects lazily: the first Dial, and the first
// Dial after the connection was lost, establish it, asking Target.Addr for
// the address each time. Failed connection attempts are spaced at least
// MinConnectInterval apart; a Dial inside that interval returns the last
// error at once, because health checks call Dial every 50 ms while a guest
// boots.
//
// Streams returned by Dial:
//   - implement CloseWrite() error, and it reaches the agent as end-of-file
//     on its read side while the other direction stays open;
//   - may return an error from SetDeadline and its variants, so callers bound
//     an exchange with the context passed to Dial and with Close, never with
//     a deadline;
//   - return from a blocked Read once Close is called on the stream or on the
//     link.
//
// Errors from Dial wrap one of ErrUnreachable, ErrAuth, ErrHostKeyMismatch,
// ErrNoHostKey or ErrClosed when the cause is one of those, so the backend
// can tell a guest that is still booting from one it must not trust.
//
// Close ends the link and every stream opened from it. It is idempotent, and
// Dial after Close returns ErrClosed. The backend closes a shed's link when
// the shed stops or is deleted, before it stops the message channel.
//
// A link never accepts a connection from the guest and never forwards
// anything to it: every stream is opened by the server.
type Link interface {
	vmutil.Dialer
	Close() error
}

// MinConnectInterval is the least time between two failed attempts to
// establish a link's underlying connection.
const MinConnectInterval = 500 * time.Millisecond

// Target tells a Factory which guest a Link is for and how to trust it.
type Target struct {
	// ShedID and ShedName identify the shed, for logs and errors.
	ShedID   string
	ShedName string
	// Addr returns where the guest can be reached now: a host, or
	// host:port. With no port, guest.SSHPort is used. The link calls it
	// before every connection attempt and never caches the answer, because
	// a cloud guest's address changes at each start. A transport that
	// needs no address (locallink) does not call it, and it may be nil
	// there.
	Addr func(ctx context.Context) (string, error)
	// HostKeys decides which host key the link accepts. Required by
	// sshlink; unused by locallink.
	HostKeys HostKeyStore
}

// HostKeyStore holds the one SSH host key a link accepts from a guest. The
// backend implements it per shed, on top of the provider: the pin is instance
// metadata (provider.MetadataHostKeyPin), and learning consults the guest's
// published key (provider.HostKeyReader).
//
// Keys are in authorized_keys form with no comment: "ssh-ed25519 AAAA...".
type HostKeyStore interface {
	// Pin returns the pinned key, or "" when the shed has none yet.
	Pin(ctx context.Context) (string, error)

	// Learn is called during a connection attempt when Pin returned "".
	// presented is the key the guest's sshd offered. The implementation
	// accepts it only if it equals the key the guest published through the
	// provider, then stores it as the pin and reads the pin back. A nil
	// return means the pin now exists and equals presented.
	//
	// A link calls Learn only for a shed with no pin. Once a pin exists a
	// different key is ErrHostKeyMismatch: the link asks Addr again once,
	// in case the address was stale, and never learns a second key.
	Learn(ctx context.Context, presented string) error
}

// Factory makes Links. The server builds one Factory at start-up from
// remote.transport and uses it for every shed.
type Factory interface {
	// Name returns the transport's name: NameSSH or NameLocal.
	Name() string

	// Open returns a Link for the target. It does not touch the network
	// and does not fail because the guest is down: connecting is left to
	// the first Dial. The Link is the caller's to Close.
	Open(ctx context.Context, target Target) (Link, error)
}

// Agent install policies, the values of remote.agent_install.
const (
	// InstallIncompatible installs the server's agent only when the
	// guest's agent speaks a protocol the server cannot use. The default.
	InstallIncompatible = "incompatible"
	// InstallAlways installs whenever the guest's agent binary differs
	// from the server's, by sha256. The development loop.
	InstallAlways = "always"
	// InstallNever never installs; an incompatible agent fails the start.
	InstallNever = "never"
)

// MaxGuestReply caps how much of a guest's answer the server reads when the
// answer drives a decision (the version line, the hash, the installer's
// output).
const MaxGuestReply = 1 << 20

// Installed is what a guest reports about its agent.
type Installed struct {
	// Info is the parsed output of guest.VersionCommand.
	Info guest.AgentInfo
	// SHA256 is the lowercase hex hash of the installed binary, from
	// guest.HashCommand.
	SHA256 string
}

// AgentInstaller is an optional interface of a Link. A link implements it
// when it can run commands in the guest outside the agent, which is what
// makes it possible to replace a broken or outdated agent. The backend tests
// for it with a type assertion and, when it is missing, treats
// remote.agent_install as InstallNever.
//
// The guest's answers are hints, not facts: a guest could report anything.
// The backend therefore installs at most once per start.
type AgentInstaller interface {
	// Installed reports the agent in the guest. It works when the agent
	// is not running.
	Installed(ctx context.Context) (Installed, error)

	// Install replaces the guest's agent with binary and restarts it. The
	// guest verifies the stream against sha256Hex before the swap, so a
	// truncated upload leaves the old agent in place. Work the old agent
	// started keeps running. The caller waits for the agent to answer a
	// health probe afterwards.
	Install(ctx context.Context, binary io.Reader, sha256Hex string) error
}

// Errors a Link's Dial wraps.
var (
	// ErrUnreachable: the guest did not answer (a timeout, a refused
	// connection, no address yet). Expected while a guest boots, and when
	// the server's own public address has changed under a static firewall
	// rule.
	ErrUnreachable = errors.New("guest unreachable")
	// ErrAuth: the guest answered and refused the server's key.
	ErrAuth = errors.New("guest refused the server's key")
	// ErrHostKeyMismatch: the guest presented a key other than the pinned
	// one. Either the address belongs to another machine now, or the guest
	// is not the one the server created. Never retried into trust.
	ErrHostKeyMismatch = errors.New("guest host key does not match the pinned key")
	// ErrNoHostKey: the shed has no pin and HostKeyStore.Learn refused the
	// presented key, or the guest has not published one yet.
	ErrNoHostKey = errors.New("guest host key could not be verified")
	// ErrClosed: the link was closed.
	ErrClosed = errors.New("link is closed")
)
