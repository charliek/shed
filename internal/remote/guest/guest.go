// Package guest names the parts of a cloud shed's guest that the server
// relies on: users, paths, unit names, the agent's sockets, the instance
// metadata the boot unit reads, and the version numbers both sides compare.
//
// It is the one place these names live. The agent (cmd/shed-agent), the
// transports (internal/remote/transport/...), the providers
// (internal/remote/provider/...) and the image scripts (gce/) must agree on
// them, so each takes them from here (the scripts by copying; a test keeps
// them in step).
//
// The package is a leaf: it imports nothing from shed, so the agent, the
// config package and the providers can all import it without a cycle.
//
// A change to anything a baked image carries (a path, a unit name, a user, a
// socket mode, a metadata key the boot unit reads) is a new guest layout:
// raise Layout and bake a new image. Raising AgentProtocol is separate; it
// changes what the agent and the server say to each other.
package guest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Version numbers the server and the guest compare.
const (
	// Layout is the guest layout this server expects. An image records its
	// layout as a decimal integer in LayoutFile; the agent reports it; the
	// server copies it to the instance label shed_layout.
	Layout = 1
	// MinLayout is the oldest layout this server can drive. A shed whose
	// layout is outside [MinLayout, Layout] is refused at start with a
	// message telling the user to recreate it.
	MinLayout = 1

	// AgentProtocol is the agent wire protocol this build speaks. It covers
	// the framing and messages on the three agent ports.
	AgentProtocol = 1
	// MinAgentProtocol is the oldest agent protocol the server accepts. An
	// agent that reports nothing (every agent before the probe reply
	// existed) is protocol 0.
	MinAgentProtocol = 1
)

// Users and groups in the guest.
const (
	// LinkUser is the system user the server logs in as over SSH. It has no
	// password, a real shell (sshd runs session commands through it) and is
	// the only name in sshd's AllowUsers.
	LinkUser = "shed-link"
	// LinkGroup is LinkUser's primary group. The agent's socket directory
	// and sockets belong to it, which is what lets sshd, running as
	// LinkUser, open them.
	LinkGroup = "shed-link"
	// ShedUser is the workspace user (UID 1000, passwordless sudo). The
	// agent runs user commands as it. It cannot log in over the guest's
	// sshd.
	ShedUser = "shed"
	// SSHPort is the guest sshd port. It is the only port that listens off
	// loopback.
	SSHPort = 22
)

// Files and units a layout-1 image carries.
const (
	// LayoutFile holds the image's layout number: decimal digits and a
	// newline, mode 0644. A missing file means layout 0.
	LayoutFile = "/etc/shed/layout"
	// AgentBinaryPath is the agent binary, root-owned, mode 0755.
	AgentBinaryPath = "/usr/local/bin/shed-agent"
	// AgentInstallHelper is the root-owned helper (mode 0755) that replaces
	// the agent binary and restarts AgentUnit. See InstallCommand.
	AgentInstallHelper = "/usr/local/sbin/shed-agent-install"
	// SudoersFile grants LinkUser the right to run AgentInstallHelper as
	// root without a password, and nothing else. Its one rule is
	// SudoersRule.
	SudoersFile = "/etc/sudoers.d/shed-link"
	// SudoersRule is the exact line in SudoersFile.
	SudoersRule = LinkUser + " ALL=(root) NOPASSWD: " + AgentInstallHelper
	// AuthorizedKeysFile is sshd's AuthorizedKeysFile for LinkUser. The
	// directory and file are root-owned (0755 and 0644) and the boot unit
	// rewrites the file on every boot from MetadataAuthorizedKeys.
	AuthorizedKeysFile = "/etc/ssh/shed-link/authorized_keys"
	// SSHDConfigFile is the sshd drop-in that carries the policy. The low
	// number makes it win over the stock cloud drop-ins, because sshd keeps
	// the first value it reads.
	SSHDConfigFile = "/etc/ssh/sshd_config.d/10-shed.conf"
	// HostKeyFile is the one host key the guest presents and publishes. The
	// image bakes none; the boot unit creates it on first boot.
	HostKeyFile = "/etc/ssh/ssh_host_ed25519_key"
	// HostKeyType is the only host key algorithm the server accepts.
	HostKeyType = "ssh-ed25519"

	// IdentityUnit is the boot unit. It runs on every boot before sshd and
	// the agent.
	IdentityUnit = "shed-identity.service"
	// IdentityEnvFile is written by IdentityUnit and loaded by AgentUnit. It
	// defines SHED_NAME and SHED_ID.
	IdentityEnvFile = "/run/shed/identity.env"
	// AgentUnit is the agent's systemd unit. It sets KillMode=process, so
	// restarting the agent leaves the commands it started running.
	AgentUnit = "shed-agent.service"
)

// The agent's unix sockets. With --listen unix:<dir> the agent listens on one
// socket per port, named by SocketName.
const (
	// AgentSocketDir is <dir> in the cloud image's unit.
	AgentSocketDir = "/run/shed-agent"
	// AgentSocketDirMode and AgentSocketMode are the modes the agent sets
	// when it is given --socket-group: directory root:<group> 0750, sockets
	// root:<group> 0660. Connecting to a unix socket needs search on the
	// directory and write on the socket, and no more.
	AgentSocketDirMode = 0o750
	AgentSocketMode    = 0o660

	// ConsolePort carries exec and console connections.
	ConsolePort uint32 = 1024
	// NotifyPort carries the message channel and health probes.
	NotifyPort uint32 = 1026
	// TCPProxyPort carries proxied connections to services in the guest.
	TCPProxyPort uint32 = 1028
)

// SocketName returns the file name of the agent socket for a port:
// "1024.sock", "1026.sock", "1028.sock".
func SocketName(port uint32) string {
	return strconv.FormatUint(uint64(port), 10) + ".sock"
}

// Instance metadata keys the boot unit reads. All three are set at create and
// never hold a secret: any process in the guest can read instance metadata.
const (
	// MetadataShedName is the shed's name. The boot unit sets the hostname
	// from it and passes it to the agent as --shed-name.
	MetadataShedName = "shed-name"
	// MetadataShedID is the shed's permanent ID.
	MetadataShedID = "shed-id"
	// MetadataAuthorizedKeys is the server's SSH public key in
	// authorized_keys form, one key per line (two lines while a key is
	// being rotated). The boot unit writes it to AuthorizedKeysFile.
	MetadataAuthorizedKeys = "shed-link-authorized-keys"
)

// The guest attribute through which the boot unit publishes its host key.
// Any guest process can write guest attributes, so the server treats the
// value as a claim to compare against the key sshd presents at first contact,
// then pins that key in instance metadata and never reads the attribute
// again.
const (
	// HostKeyAttrNamespace and HostKeyAttrKey name the attribute
	// "shed/host-key". Its value is the public half of HostKeyFile in
	// authorized_keys form with no comment: "ssh-ed25519 AAAA...".
	HostKeyAttrNamespace = "shed"
	HostKeyAttrKey       = "host-key"
)

// VersionCommand is the command the server runs as LinkUser to learn what
// agent is installed. It prints one line of JSON, an AgentInfo, on stdout.
const VersionCommand = AgentBinaryPath + " version"

// HashCommand is the command the server runs as LinkUser to hash the
// installed agent. The first field of its output is the lowercase hex sha256.
const HashCommand = "sha256sum " + AgentBinaryPath

// InstallCommand returns the command the server runs as LinkUser to install a
// new agent. The binary is streamed on stdin. The helper writes it to a
// temporary file beside AgentBinaryPath, refuses it unless its sha256 equals
// sha256Hex (64 lowercase hex digits), renames it into place as root:root
// 0755, restarts AgentUnit, prints "installed <sha256Hex>" and exits 0. Any
// failure exits non-zero with a reason on stderr and leaves the old binary in
// place.
func InstallCommand(sha256Hex string) string {
	return "sudo -n " + AgentInstallHelper + " " + sha256Hex
}

// AgentInfo is what an agent reports about itself. It is the JSON object
// printed by "shed-agent version" and the payload of the reply to a health
// request that set probe:true. Readers ignore fields they do not know.
type AgentInfo struct {
	// Version is the build's version string (internal/version.Version),
	// for example "v0.9.2" or "dev".
	Version string `json:"version"`
	// Protocol is the agent's AgentProtocol. Zero means the agent predates
	// the field.
	Protocol int `json:"protocol"`
	// Layout is the integer in LayoutFile, or 0 when the file is missing.
	// It describes the image, not the agent binary.
	Layout int `json:"layout"`
}

// ParseAgentInfo decodes the output of "shed-agent version" or a probe reply
// payload. Empty input is the zero AgentInfo (protocol 0), which is what an
// agent older than the probe reply sends.
func ParseAgentInfo(data []byte) (AgentInfo, error) {
	var info AgentInfo
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return info, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &info); err != nil {
		return AgentInfo{}, fmt.Errorf("parse agent info: %w", err)
	}
	return info, nil
}

// ProtocolCompatible reports whether the server can talk to an agent that
// speaks the given protocol.
func ProtocolCompatible(protocol int) bool {
	return protocol >= MinAgentProtocol && protocol <= AgentProtocol
}

// LayoutSupported reports whether the server can drive a guest with the
// given layout.
func LayoutSupported(layout int) bool {
	return layout >= MinLayout && layout <= Layout
}
