package roostprovider

import "strings"

// SSHClass is roost's own classification of a failed ssh exec — a verbatim Go
// port of `roost_ipc::ssh::SshFailure` / `classify_ssh_failure` at the pinned
// rev (crates/Cargo.toml's `roost-ipc` rev).
//
// It is a PORT, not a reimplementation with the same spirit: the shed provider
// execs roost's own `exec_chain_command` and therefore sees roost's own stderr,
// so the two sides must read the same bytes the same way or the provider's copy
// contradicts the desktop app's. The shared golden
// crates/fixtures/roost-vectors/stderr-classes.json pins every case here
// against the real classifier from the Rust side.
type SSHClass string

// The six classes, in the classifier's own precedence order (see
// ClassifySSHFailure — the order is load-bearing, not alphabetical).
const (
	// ClassChangedHostKey — `REMOTE HOST IDENTIFICATION HAS CHANGED`. First,
	// and first for a reason: this is what a machine-in-the-middle looks like
	// from here, and every later rule would also match the same blob (ssh
	// prints "Host key verification failed." right after it).
	ClassChangedHostKey SSHClass = "changed-host-key"
	// ClassHostKeyUnknown — `Host key verification failed`, with no prior pin
	// to contradict.
	ClassHostKeyUnknown SSHClass = "host-key-unknown"
	// ClassAuth — `Permission denied`.
	ClassAuth SSHClass = "auth"
	// ClassNoSession — `client-bridge: no session`: roost-session is
	// INSTALLED on the far side but is not running. Ahead of ClassNotFound
	// because the bridge's own refusal is more specific than the generic
	// "command not found" / 127 pair below it.
	ClassNoSession SSHClass = "no-session"
	// ClassNotFound — `command not found` in stderr, or exit 127. This is
	// what falling off the end of roost's candidate ladder produces.
	ClassNotFound SSHClass = "not-found"
	// ClassTransport — none of the above. Carries the last non-empty trimmed
	// stderr line as its detail.
	//
	// NOTE roost does NOT special-case exit 255 (ssh's own "something went
	// wrong" code): 255 with an unrecognized stderr lands here, as Transport.
	// The provider's "unreachable" row is a SEPARATE, provider-local mapping
	// of this class plus 255-or-timeout — see ProviderRow, which keeps the two
	// concepts apart on purpose.
	ClassTransport SSHClass = "transport"
)

// ClassifySSHFailure ports `roost_ipc::ssh::classify_ssh_failure`.
//
// exitCode is nil when the child never exited on its own (killed on a deadline,
// or signalled). detail is the last non-empty trimmed line of stderrTail, and
// is only populated for ClassTransport — the other five classes ARE the
// diagnosis, so quoting a line back beside them would be noise.
//
// The rule order below is roost's, byte for byte. Reordering it would change
// the answer for every stderr blob that matches two rules (a changed host key
// also says "Host key verification failed"; a bridge that answers "no session"
// can still exit 127), and those overlaps are exactly what the golden's
// precedence cases pin.
func ClassifySSHFailure(exitCode *int, stderrTail string) (SSHClass, string) {
	switch {
	case strings.Contains(stderrTail, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return ClassChangedHostKey, ""
	case strings.Contains(stderrTail, "Host key verification failed"):
		return ClassHostKeyUnknown, ""
	case strings.Contains(stderrTail, "Permission denied"):
		return ClassAuth, ""
	case strings.Contains(stderrTail, "client-bridge: no session"):
		return ClassNoSession, ""
	case strings.Contains(stderrTail, "command not found"), exitCode != nil && *exitCode == 127:
		return ClassNotFound, ""
	}
	return ClassTransport, lastNonEmptyLine(stderrTail)
}

// lastNonEmptyLine ports `roost_ipc::ssh::last_line`: the last non-empty line
// of a stderr tail, trimmed — the one thing worth quoting back out of a blob
// that is mostly login banner.
func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// RowKind names one of the provider's non-actionable rows (plan 019 §3.2 step
// 2). These are the PROVIDER's vocabulary, deliberately a different set from
// SSHClass: three roost classes collapse into one row here, and two rows
// (RowNoAgents, RowNoSSH) have no roost class at all.
type RowKind string

const (
	// RowNotInstalled — ClassNotFound.
	RowNotInstalled RowKind = "not-installed"
	// RowNotRunning — ClassNoSession.
	RowNotRunning RowKind = "not-running"
	// RowUnreachable — ClassTransport (the motivating cases being ssh's exit
	// 255 and a timed-out exec), AND the three host-key/auth classes.
	//
	// Folding auth and host-key failures in here is deliberate. §3.2 pins six
	// non-actionable rows and no more, and each of those three failures
	// already prints its own diagnosis as ssh's last stderr line — which is
	// exactly what this row's subtitle carries. A seventh row shape would be
	// inventing copy the plan did not pin; a silently-dropped class would be
	// worse.
	RowUnreachable RowKind = "unreachable"
	// RowProtocolMismatch — not an ssh failure at all: the bridge answered,
	// and said a protocol number this build does not speak (pin P6: report,
	// never restart).
	RowProtocolMismatch RowKind = "protocol-mismatch"
	// RowNoAgents — the probe succeeded and found none of the six binaries.
	RowNoAgents RowKind = "no-agents"
	// RowNoSSH — there is no `ssh` on this machine where roost can see it.
	// Local, so no class.
	RowNoSSH RowKind = "no-ssh"
)

// ProviderRow maps a roost SSHClass onto the provider's row vocabulary.
//
// The whole function is four lines and could be inlined at its two call sites;
// it is a named mapping precisely so the seam stays visible — "which roost
// class is this" and "which row does the user see" are different questions, and
// the golden asserts them separately.
func ProviderRow(class SSHClass) RowKind {
	switch class {
	case ClassNotFound:
		return RowNotInstalled
	case ClassNoSession:
		return RowNotRunning
	default:
		return RowUnreachable
	}
}

// sshOwnExitCode is 255, the code ssh reserves for its own failures (it cannot
// distinguish that from a remote command that happens to exit 255, and neither
// can anybody downstream — see BridgeRow for why this side takes ssh's reading).
const sshOwnExitCode = 255

// BridgeRow maps ONE BRIDGE-phase failure onto a row — the class, plus the two
// pieces of evidence the class cannot see.
//
// **A timeout or exit 255 outranks the stderr class.** ClassifySSHFailure reads
// substrings out of a blob that begins with somebody else's login shell, and a
// shell that prints `foo: command not found` on its way past a broken line in a
// dotfile makes a `session.identify` that HUNG classify as not-found. The row
// that falls out of that says roost-session is not installed — on a host where
// it was found and merely became unreachable, which is the opposite of the
// truth and sends the user to install something they already have. A timeout
// means nothing answered, and 255 is ssh saying the failure was its own; either
// is stronger evidence about what happened than a substring that may have come
// from a motd.
//
// Deliberately NOT folded into ProviderRow: that function is the pure
// class → row mapping the shared golden pins, and the evidence this one adds is
// not part of a class. It is also not applied to the probe phase — see
// RowForError, where every probe failure is already a reachability failure
// because a login shell cannot say anything about roost-session either way.
func BridgeRow(class SSHClass, exitCode *int, timedOut bool) RowKind {
	if timedOut || (exitCode != nil && *exitCode == sshOwnExitCode) {
		return RowUnreachable
	}
	return ProviderRow(class)
}
