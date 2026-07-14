//! The SSH `_bootstrap` credential/control-token mint — a faithful Rust port of the
//! Go `sdk/bootstrap` package (`sdk/bootstrap/bootstrap.go`). It mints a shed HTTP
//! token over a server's reserved `_bootstrap` SSH channel by invoking the **system
//! `ssh` client** (never an in-process SSH library), so the exchange transparently
//! honors the user's agent, macOS Keychain, 1Password/Secretive `IdentityAgent`,
//! hardware keys, and `~/.ssh/config` — exactly as `shed server add`/`shed attach` do.
//!
//! `ssh` remains the security enforcement point: `StrictHostKeyChecking=yes` against
//! the shed-only `known_hosts` (global file disabled) is what refuses a MITM. This
//! module only *classifies* a failure as terminal (a confirmed host-key change) vs
//! retryable, so a caller can fail closed without re-implementing host-key
//! verification. It never logs or returns stdout, which carries the freshly minted
//! token.
//!
//! **Wire-compat is load-bearing:** [`ssh_args`] is byte-identical to Go `sshArgs`
//! (`bootstrap.go:109`) — the differential harness captures the argv both daemons
//! emit and compares them (after home-normalizing the one env-dependent element).
//!
//! **Injectable seam:** the [`BootstrapRunner`] trait mirrors Go's
//! `CredentialMinter.bootstrapRun` field — unit tests inject a fake runner so minting
//! is driven without spawning `ssh` or standing up a server; the production
//! [`SystemSshRunner`] shells the real client.

use std::path::PathBuf;
use std::process::Stdio;
use std::time::Duration;

use serde::de::Error as _;
use serde::Deserialize;
use thiserror::Error;
use tokio::io::AsyncReadExt;

/// The server's reserved SSH username (mirrors `internal/sshd reservedBootstrapUser`):
/// connecting as it mints + returns an HTTP token bundle.
const BOOTSTRAP_USER: &str = "_bootstrap";

/// Bounds the whole ssh exchange (Go `DefaultTimeout = 15s`). A field on
/// [`SystemSshRunner`] (Go makes it a package `var`) so tests can shorten it. The
/// bound is real even for a cancel-only caller: on expiry the read futures + child are
/// dropped (killing ssh), so a mint can never hang on a wedged ProxyCommand, a slow
/// agent, or a touch-required key with no one present. (This is the outer bound; Go
/// additionally uses `cmd.WaitDelay` to force pipes closed ~3s after ssh exits — see
/// the `kill_on_drop` note in `run`.)
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(15);

/// Caps captured stdout/stderr (Go `maxOutputBytes = 64<<10`) so a misbehaving ssh /
/// helper / ProxyCommand can't balloon memory. A bundle is well under this.
const MAX_OUTPUT_BYTES: usize = 64 << 10;

/// Bounds how much ssh stderr is echoed into a returned error (Go `maxErrStderr =
/// 2<<10`), so a chatty helper can't dump unbounded (possibly sensitive) output.
const MAX_ERR_STDERR: usize = 2 << 10;

/// OpenSSH's banner for a *changed* host key — the one failure mode that is a
/// confirmed pin mismatch (possible MITM). Stable, unlocalized text; we additionally
/// force `LC_ALL=C`. A bare "Host key verification failed." (no banner) is NOT this —
/// it also covers a missing entry — so only this marker latches terminal (Go
/// `hostKeyChangedMarker`).
const HOST_KEY_CHANGED_MARKER: &[u8] = b"REMOTE HOST IDENTIFICATION HAS CHANGED";

/// One bootstrap exchange (mirrors Go `bootstrap.Params`). Host/Port address the
/// server's SSH endpoint; `known_hosts_path` is the pinned trust root
/// (`~/.shed/known_hosts`); `scope` is the token scope (`"control"`/`"credentials"`);
/// `client_kind` is advisory audit metadata (`"host-agent"`/`"cli"`/`"desktop"`) and
/// may be empty.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Params {
    pub host: String,
    pub port: u16,
    pub known_hosts_path: String,
    pub scope: String,
    pub client_kind: String,
}

/// The ssh stdout JSON (mirrors Go `sdk.Bundle`, `sdk/bundle.go:16`). Every field is
/// `#[serde(default)]` because Go's `json.Unmarshal` leaves zero values for missing
/// fields, which [`decode_bundle`] THEN validates — so a Bundle missing e.g. both
/// ports must reach the "no usable API port" error, not a deserialization failure.
/// `expires_at` is parsed via [`de_expires_at`] to the same `Option<i64>` unix-seconds
/// shape Go's `time.Time` collapses to (absent / Go-zero → `None`; malformed → a
/// decode error, matching Go's `json.Unmarshal` into `time.Time` failing).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Bundle {
    #[serde(default)]
    pub http_port: u32,
    #[serde(default)]
    pub https_port: u32,
    #[serde(default)]
    pub tls_cert_fingerprint: String,
    #[serde(default)]
    pub token: String,
    #[serde(default)]
    pub scope: String,
    #[serde(default)]
    pub token_id: String,
    /// Parsed to unix seconds; `None` when absent, empty, or Go's zero time
    /// (`0001-01-01T00:00:00Z`) — the states for which Go omits `token.response`'s
    /// `expires_at`. A present-but-malformed value fails the decode.
    #[serde(default, deserialize_with = "de_expires_at")]
    pub expires_at: Option<i64>,
}

/// Outcome sentinels for a bootstrap exchange (mirror Go's `Err*` vars). Only
/// [`BootstrapError::HostKeyMismatch`] is terminal — the rest are retryable so a
/// caller fails closed without permanently wedging a healthy server. The `Display`
/// text is what a caller ultimately surfaces; stdout (the token) never appears here.
#[derive(Debug, Error)]
pub enum BootstrapError {
    /// A confirmed server SSH host-key change — a hard, fail-closed trust failure (a
    /// possible MITM). Callers treat this as terminal and refuse any fallback to a
    /// weaker credential (Go `ErrHostKeyMismatch`).
    #[error("sdk/bootstrap: host key mismatch: {0}")]
    HostKeyMismatch(String),
    /// A host-key verification failure that is NOT a confirmed change (a racing/garbled
    /// known_hosts, or a missing entry when the caller did not pre-check). Retryable
    /// (Go `ErrHostKeyVerificationFailed`).
    #[error("sdk/bootstrap: ssh host key verification failed: {0}")]
    HostKeyVerificationFailed(String),
    /// A public-key auth failure: the daemon offered no identity ssh could use, or the
    /// offered key is not on the server allowlist. Retryable (Go `ErrNoSSHIdentities`).
    #[error("sdk/bootstrap: ssh could not authenticate with any available identity (the daemon may have no SSH identity available — see IdentityAgent docs — or the key may not be on the server allowlist): {0}")]
    NoSshIdentities(String),
    /// The ssh exchange was aborted (our own timeout, or a caller cancel). Retryable —
    /// the server is not implicated.
    #[error("sdk/bootstrap: ssh exchange aborted: {0}")]
    Aborted(String),
    /// ssh exited non-zero with diagnostics that don't match a more specific class.
    #[error("sdk/bootstrap: ssh exited {exit}: {msg}")]
    Ssh { exit: i32, msg: String },
    /// ssh failed to run (spawn error) with no useful stderr.
    #[error("sdk/bootstrap: ssh failed: {0}")]
    SshFailed(String),
    /// Invalid [`Params`] (would break argv construction / inject ssh options).
    #[error("sdk/bootstrap: {0}")]
    Validate(String),
    /// ssh stdout was not a single valid bootstrap bundle (Go
    /// `"ssh produced no valid bootstrap bundle"` + the decode-time validations).
    #[error("sdk/bootstrap: {0}")]
    Decode(String),
    /// The ssh binary was not found on PATH (nor the macOS fallback).
    #[error("sdk/bootstrap: ssh binary not found on PATH")]
    SshNotFound,
}

impl BootstrapError {
    /// Whether this is the terminal host-key-mismatch (a possible MITM) that a caller
    /// must latch and never retry. The Rust analog of Go's
    /// `errors.Is(err, ErrHostKeyMismatch)`.
    pub fn is_host_key_mismatch(&self) -> bool {
        matches!(self, BootstrapError::HostKeyMismatch(_))
    }
}

/// Build the `ssh` argv for a bootstrap exchange — **byte-identical** to Go `sshArgs`
/// (`bootstrap.go:109`); the 17 `-o` options pin ssh to a strict, non-interactive,
/// publickey-only exchange against the shed known_hosts as the SOLE trust root.
/// `~/.ssh/config` is intentionally NOT disabled (it is how a user points the daemon
/// at an `IdentityAgent`/`IdentityFile`), but a matching Host stanza must not add a
/// side effect during the unattended mint, so agent forwarding, port forwardings, and
/// LocalCommand hooks are force-disabled. Copy the vector from Go; do not re-derive.
pub fn ssh_args(p: &Params) -> Vec<String> {
    let mut args: Vec<String> = vec![
        "-T".into(),
        "-p".into(),
        p.port.to_string(),
        "-o".into(),
        "BatchMode=yes".into(),
        "-o".into(),
        "StrictHostKeyChecking=yes".into(),
        "-o".into(),
        format!("UserKnownHostsFile={}", p.known_hosts_path),
        "-o".into(),
        "GlobalKnownHostsFile=/dev/null".into(),
        "-o".into(),
        "VerifyHostKeyDNS=no".into(),
        "-o".into(),
        "KnownHostsCommand=none".into(),
        "-o".into(),
        "UpdateHostKeys=no".into(),
        "-o".into(),
        "CheckHostIP=no".into(),
        "-o".into(),
        "PreferredAuthentications=publickey".into(),
        "-o".into(),
        "PubkeyAuthentication=yes".into(),
        "-o".into(),
        "PasswordAuthentication=no".into(),
        "-o".into(),
        "KbdInteractiveAuthentication=no".into(),
        "-o".into(),
        "ChallengeResponseAuthentication=no".into(),
        "-o".into(),
        "NumberOfPasswordPrompts=0".into(),
        "-o".into(),
        "ForwardAgent=no".into(),
        "-o".into(),
        "ClearAllForwardings=yes".into(),
        "-o".into(),
        "PermitLocalCommand=no".into(),
        "-l".into(),
        BOOTSTRAP_USER.into(),
        p.host.clone(),
        p.scope.clone(),
    ];
    if !p.client_kind.is_empty() {
        args.push(p.client_kind.clone());
    }
    args
}

/// Reject inputs that could break argv construction or inject ssh options before they
/// reach exec (mirror Go `validate`): a host that is empty, starts with `-` (option
/// injection), or contains whitespace/`@`/NUL; a port out of range; a scope/client_kind
/// that isn't a single token (the server parses `"<scope> [<kind>]"`).
pub fn validate(p: &Params) -> Result<(), BootstrapError> {
    if p.host.is_empty() {
        return Err(BootstrapError::Validate("host required".into()));
    }
    if p.host.starts_with('-') {
        return Err(BootstrapError::Validate(format!(
            "invalid host {:?} (looks like an option)",
            p.host
        )));
    }
    if p.host.contains([' ', '\t', '\r', '\n', '\0', '@']) {
        return Err(BootstrapError::Validate(format!(
            "invalid host {:?}",
            p.host
        )));
    }
    // Port is a u16 so 0 is the only out-of-range value Go's 1..=65535 check rejects.
    if p.port == 0 {
        return Err(BootstrapError::Validate(format!("invalid port {}", p.port)));
    }
    if p.known_hosts_path.is_empty() {
        return Err(BootstrapError::Validate("known_hosts path required".into()));
    }
    if p.scope.is_empty() || p.scope.contains([' ', '\t', '\r', '\n']) {
        return Err(BootstrapError::Validate(format!(
            "invalid scope {:?}",
            p.scope
        )));
    }
    if p.client_kind.contains([' ', '\t', '\r', '\n']) {
        return Err(BootstrapError::Validate(format!(
            "invalid client kind {:?}",
            p.client_kind
        )));
    }
    Ok(())
}

/// Map a non-zero ssh exit to a typed error (mirror Go `classify`). Only a confirmed
/// host-key change (exit 255 AND the CHANGED banner seen anywhere in the full stderr
/// stream) is the terminal [`BootstrapError::HostKeyMismatch`]; everything else is
/// retryable. The stderr surfaced in an error is clipped; stdout (the token) is never
/// referenced. `exit` is ssh's exit code (-1 when the process did not exit normally).
fn classify(exit: i32, host_key_changed: bool, stderr_text: &str) -> BootstrapError {
    let msg = clip(stderr_text.trim(), MAX_ERR_STDERR);
    if exit == 255 && host_key_changed {
        return BootstrapError::HostKeyMismatch(first_line(&msg));
    }
    if stderr_text.contains("Host key verification failed") {
        return BootstrapError::HostKeyVerificationFailed(msg);
    }
    if stderr_text.contains("Permission denied (publickey")
        || stderr_text.contains("No more authentication methods")
    {
        return BootstrapError::NoSshIdentities(msg);
    }
    if !msg.is_empty() {
        return BootstrapError::Ssh { exit, msg };
    }
    BootstrapError::SshFailed(format!("ssh exited {exit}"))
}

/// Validate ssh stdout: a single JSON object, no trailing garbage, a non-empty token,
/// a usable API port, an https port paired with a fingerprint, and (when the server
/// echoes one) a matching scope. Mirrors Go `decodeBundle`, IN THE SAME ORDER. The raw
/// stdout is NEVER included in an error — it carries the token.
pub fn decode_bundle(out: &[u8], want_scope: &str) -> Result<Bundle, BootstrapError> {
    let mut de = serde_json::Deserializer::from_slice(out);
    let b = Bundle::deserialize(&mut de)
        .map_err(|_| BootstrapError::Decode("ssh produced no valid bootstrap bundle".into()))?;
    // Require EOF after the single object (trailing whitespace is fine) — Go reads the
    // next token and insists the stream is exhausted.
    de.end().map_err(|_| {
        BootstrapError::Decode("unexpected trailing data after bootstrap bundle".into())
    })?;
    if b.token.trim().is_empty() {
        return Err(BootstrapError::Decode(
            "bootstrap returned an empty token".into(),
        ));
    }
    if b.https_port == 0 && b.http_port == 0 {
        return Err(BootstrapError::Decode(
            "bootstrap bundle has no usable API port".into(),
        ));
    }
    if b.https_port != 0 && b.tls_cert_fingerprint.trim().is_empty() {
        return Err(BootstrapError::Decode(
            "bootstrap bundle advertises HTTPS without a TLS fingerprint to pin".into(),
        ));
    }
    if !b.scope.is_empty() && b.scope != want_scope {
        return Err(BootstrapError::Decode(format!(
            "scope mismatch: requested {want_scope:?}, got {:?}",
            b.scope
        )));
    }
    Ok(b)
}

/// Resolve the ssh binary: `$PATH` search, falling back to the standard macOS path so a
/// launchd/Homebrew daemon with a sparse PATH still finds it (mirror Go `lookSSH`).
fn look_ssh() -> Result<PathBuf, BootstrapError> {
    if let Some(p) = std::env::var_os("PATH").and_then(|path| find_on_path("ssh", &path)) {
        return Ok(p);
    }
    let fallback = PathBuf::from("/usr/bin/ssh");
    if fallback.is_file() {
        return Ok(fallback);
    }
    Err(BootstrapError::SshNotFound)
}

/// Search `path` (a `$PATH`-shaped value) for an executable named `name` (the
/// `exec.LookPath` subset `look_ssh` needs). Returns the first entry that exists as a
/// file. Takes `path` as a param so it is unit-testable without mutating the process env.
fn find_on_path(name: &str, path: &std::ffi::OsStr) -> Option<PathBuf> {
    for dir in std::env::split_paths(path) {
        if dir.as_os_str().is_empty() {
            continue;
        }
        let candidate = dir.join(name);
        if candidate.is_file() {
            return Some(candidate);
        }
    }
    None
}

/// serde field deserializer for `Bundle.expires_at`: `None` for absent/null/empty/zero;
/// `Some(secs)` for a valid instant; a serde error for a malformed non-empty value (so
/// the whole `Bundle::deserialize` fails, matching Go's `json.Unmarshal` into
/// `time.Time` erroring on a bad RFC3339).
fn de_expires_at<'de, D>(d: D) -> Result<Option<i64>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let raw: Option<String> = Option::deserialize(d)?;
    let Some(s) = raw else { return Ok(None) };
    let s = s.trim();
    if s.is_empty() {
        return Ok(None);
    }
    match crate::status::parse_rfc3339_to_unix(s) {
        Ok(v) => Ok(v),
        Err(()) => Err(D::Error::custom("invalid RFC3339 expires_at")),
    }
}

/// Runs one bootstrap exchange (mirror Go `bootstrap.Run`). A field/trait so tests can
/// inject a fake without spawning `ssh` or standing up a server (Go's
/// `CredentialMinter.bootstrapRun`). `Send + Sync` so a caller can hold it as
/// `Arc<dyn BootstrapRunner>` across the async token path.
#[async_trait::async_trait]
pub trait BootstrapRunner: Send + Sync {
    async fn run(&self, params: &Params) -> Result<Bundle, BootstrapError>;
}

/// The production runner: shells the system `ssh` client via `tokio::process`.
pub struct SystemSshRunner {
    timeout: Duration,
    /// A test-only override for the ssh binary path, so shim tests point at an absolute
    /// fake `ssh` without mutating the process-global `$PATH`.
    #[cfg(test)]
    ssh_override: Option<PathBuf>,
}

impl SystemSshRunner {
    pub fn new() -> Self {
        Self {
            timeout: DEFAULT_TIMEOUT,
            #[cfg(test)]
            ssh_override: None,
        }
    }

    /// A runner with a shortened timeout + an explicit ssh binary path for tests (Go
    /// shortens the `DefaultTimeout` package var + injects `bootstrapRun`).
    #[cfg(test)]
    fn with_shim(ssh: PathBuf, timeout: Duration) -> Self {
        Self {
            timeout,
            ssh_override: Some(ssh),
        }
    }
}

impl Default for SystemSshRunner {
    fn default() -> Self {
        Self::new()
    }
}

#[async_trait::async_trait]
impl BootstrapRunner for SystemSshRunner {
    async fn run(&self, p: &Params) -> Result<Bundle, BootstrapError> {
        validate(p)?;
        #[cfg(test)]
        let ssh = match &self.ssh_override {
            Some(path) => path.clone(),
            None => look_ssh()?,
        };
        #[cfg(not(test))]
        let ssh = look_ssh()?;

        let mut cmd = tokio::process::Command::new(&ssh);
        cmd.args(ssh_args(p));
        // Forward the full environment (a user ProxyCommand may need arbitrary vars —
        // this is the user's own env reaching the user's own ssh/config) and force the
        // locale to C so ssh emits stable, English diagnostics for classification (Go
        // `cLocaleEnv`: append `LC_ALL=C`, which overrides any inherited locale).
        cmd.env("LC_ALL", "C");
        cmd.stdin(Stdio::null());
        cmd.stdout(Stdio::piped());
        cmd.stderr(Stdio::piped());
        // On drop (including the timeout branch below) the direct `ssh` child is killed.
        // This is NOT a full `cmd.WaitDelay` analog: Go's WaitDelay force-closes the
        // pipes ~3s after ssh exits so a pipe-holding ProxyCommand *grandchild* can't
        // block `Wait`, whereas here the outer `timeout` is the only bound on such a
        // grandchild — the mint returns `Aborted` at the deadline rather than ~3s after
        // ssh exits. Both are bounded and retryable (no hang, no wire impact); only the
        // timing/error-class of that rare case differs from Go.
        cmd.kill_on_drop(true);

        let mut child = cmd
            .spawn()
            .map_err(|e| BootstrapError::SshFailed(e.to_string()))?;
        let mut stdout = child.stdout.take().expect("piped stdout");
        let mut stderr = child.stderr.take().expect("piped stderr");

        let collect = async {
            // Read both pipes to EOF (capping the buffers, scanning stderr for the
            // CHANGED marker over the FULL stream) concurrently with the child exit, so
            // ssh never blocks on a full pipe.
            let out_fut = read_capped(&mut stdout, MAX_OUTPUT_BYTES, None);
            let err_fut = read_capped(&mut stderr, MAX_OUTPUT_BYTES, Some(HOST_KEY_CHANGED_MARKER));
            let wait_fut = child.wait();
            let ((out, _), (err, marker_seen), status) = tokio::join!(out_fut, err_fut, wait_fut);
            (out, err, marker_seen, status)
        };

        match tokio::time::timeout(self.timeout, collect).await {
            Err(_elapsed) => {
                // Prompt kill + drop of `child`/pipes (they leave scope here) so the
                // bound is real even against a wedged ProxyCommand holding the pipes.
                let _ = child.start_kill();
                Err(BootstrapError::Aborted("context deadline exceeded".into()))
            }
            Ok((out, err, marker_seen, status)) => {
                let status = status.map_err(|e| BootstrapError::SshFailed(e.to_string()))?;
                if status.success() {
                    return decode_bundle(&out, &p.scope);
                }
                let exit = status.code().unwrap_or(-1);
                Err(classify(exit, marker_seen, &String::from_utf8_lossy(&err)))
            }
        }
    }
}

/// Read an async pipe to EOF, buffering up to `max` bytes (dropping the rest so ssh
/// never blocks on a full pipe) and — when `marker` is set — reporting whether that
/// byte sequence appeared anywhere in the FULL stream (even past the cap, even split
/// across reads). Mirrors Go's `capWriter`.
async fn read_capped<R>(reader: &mut R, max: usize, marker: Option<&[u8]>) -> (Vec<u8>, bool)
where
    R: AsyncReadExt + Unpin,
{
    let mut buf = Vec::new();
    let mut chunk = [0u8; 8192];
    let mut scanner = marker.map(MarkerScanner::new);
    loop {
        let n = match reader.read(&mut chunk).await {
            Ok(0) => break,
            Ok(n) => n,
            Err(_) => break,
        };
        let piece = &chunk[..n];
        if let Some(s) = scanner.as_mut() {
            s.feed(piece);
        }
        if buf.len() < max {
            let room = max - buf.len();
            buf.extend_from_slice(&piece[..room.min(piece.len())]);
        }
    }
    (buf, scanner.is_some_and(|s| s.seen))
}

/// Detects whether a byte `marker` appears anywhere in a stream fed chunk-by-chunk,
/// even when the marker is SPLIT across two reads (Go `capWriter.carry`): after each
/// chunk it retains the last `marker.len()-1` bytes so a boundary-straddling match is
/// still found on the next feed.
struct MarkerScanner<'a> {
    marker: &'a [u8],
    carry: Vec<u8>,
    seen: bool,
}

impl<'a> MarkerScanner<'a> {
    fn new(marker: &'a [u8]) -> Self {
        Self {
            marker,
            carry: Vec::new(),
            seen: false,
        }
    }

    fn feed(&mut self, chunk: &[u8]) {
        if self.seen || self.marker.is_empty() {
            return;
        }
        let mut hay = std::mem::take(&mut self.carry);
        hay.extend_from_slice(chunk);
        if hay.windows(self.marker.len()).any(|w| w == self.marker) {
            self.seen = true;
            return;
        }
        let keep = (self.marker.len() - 1).min(hay.len());
        self.carry = hay[hay.len() - keep..].to_vec();
    }
}

/// The first non-empty line of `s`, for a compact error (Go `firstLine`).
fn first_line(s: &str) -> String {
    for ln in s.split('\n') {
        let t = ln.trim();
        if !t.is_empty() {
            return t.to_string();
        }
    }
    s.to_string()
}

/// Truncate `s` to at most `n` bytes (on a char boundary), appending an ellipsis marker
/// when cut (Go `clip`).
fn clip(s: &str, n: usize) -> String {
    if s.len() <= n {
        return s.to_string();
    }
    let mut end = n;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…(truncated)", &s[..end])
}

#[cfg(test)]
mod tests {
    use super::*;

    fn params() -> Params {
        Params {
            host: "mini3".into(),
            port: 2222,
            known_hosts_path: "/home/x/.shed/known_hosts".into(),
            scope: "credentials".into(),
            client_kind: "host-agent".into(),
        }
    }

    #[test]
    fn ssh_args_exact_vector() {
        // Full-vector equality (Go's TestArgs is only Contains+ordering; this pins the
        // exact byte sequence the differential also compares).
        let got = ssh_args(&params());
        let want = vec![
            "-T",
            "-p",
            "2222",
            "-o",
            "BatchMode=yes",
            "-o",
            "StrictHostKeyChecking=yes",
            "-o",
            "UserKnownHostsFile=/home/x/.shed/known_hosts",
            "-o",
            "GlobalKnownHostsFile=/dev/null",
            "-o",
            "VerifyHostKeyDNS=no",
            "-o",
            "KnownHostsCommand=none",
            "-o",
            "UpdateHostKeys=no",
            "-o",
            "CheckHostIP=no",
            "-o",
            "PreferredAuthentications=publickey",
            "-o",
            "PubkeyAuthentication=yes",
            "-o",
            "PasswordAuthentication=no",
            "-o",
            "KbdInteractiveAuthentication=no",
            "-o",
            "ChallengeResponseAuthentication=no",
            "-o",
            "NumberOfPasswordPrompts=0",
            "-o",
            "ForwardAgent=no",
            "-o",
            "ClearAllForwardings=yes",
            "-o",
            "PermitLocalCommand=no",
            "-l",
            "_bootstrap",
            "mini3",
            "credentials",
            "host-agent",
        ];
        assert_eq!(got, want);
        // Exactly 17 `-o` flags (Codex: 17, not 20).
        assert_eq!(got.iter().filter(|a| a.as_str() == "-o").count(), 17);
    }

    #[test]
    fn ssh_args_omits_empty_client_kind() {
        let mut p = params();
        p.client_kind = String::new();
        let got = ssh_args(&p);
        assert_eq!(got.last().unwrap(), "credentials"); // scope is the final element
    }

    #[test]
    fn validate_rejects() {
        let base = params();
        let bad = |mutate: &dyn Fn(&mut Params)| {
            let mut p = base.clone();
            mutate(&mut p);
            validate(&p).is_err()
        };
        assert!(bad(&|p| p.host = String::new()), "empty host");
        assert!(bad(&|p| p.host = "-oProxyCommand=x".into()), "leading dash");
        assert!(bad(&|p| p.host = "a b".into()), "host whitespace");
        assert!(bad(&|p| p.host = "a@b".into()), "host @");
        assert!(bad(&|p| p.host = "a\0b".into()), "host NUL");
        assert!(bad(&|p| p.port = 0), "port 0");
        assert!(
            bad(&|p| p.known_hosts_path = String::new()),
            "no known_hosts"
        );
        assert!(bad(&|p| p.scope = String::new()), "empty scope");
        assert!(bad(&|p| p.scope = "a b".into()), "scope whitespace");
        assert!(bad(&|p| p.client_kind = "a b".into()), "kind whitespace");
        assert!(validate(&base).is_ok());
    }

    #[test]
    fn classify_matrix() {
        // 255 + banner → terminal.
        assert!(classify(
            255,
            true,
            "REMOTE HOST IDENTIFICATION HAS CHANGED\nsomething"
        )
        .is_host_key_mismatch());
        // Banner but WRONG exit code → NOT terminal (the AND-gate; CodeRabbit F3).
        assert!(!classify(1, true, "REMOTE HOST IDENTIFICATION HAS CHANGED").is_host_key_mismatch());
        // 255 without the banner → NOT terminal.
        assert!(!classify(255, false, "Host key verification failed.").is_host_key_mismatch());
        // Verification-failed (no banner) → retryable verification error.
        assert!(matches!(
            classify(255, false, "Host key verification failed."),
            BootstrapError::HostKeyVerificationFailed(_)
        ));
        // publickey / no-more-methods → no-identities.
        assert!(matches!(
            classify(255, false, "mini3: Permission denied (publickey)."),
            BootstrapError::NoSshIdentities(_)
        ));
        assert!(matches!(
            classify(255, false, "No more authentication methods to try."),
            BootstrapError::NoSshIdentities(_)
        ));
        // Other non-empty stderr → generic ssh-exited.
        assert!(matches!(
            classify(1, false, "kex_exchange_identification: banner"),
            BootstrapError::Ssh { exit: 1, .. }
        ));
        // No stderr → bare failure.
        assert!(matches!(
            classify(-1, false, ""),
            BootstrapError::SshFailed(_)
        ));
    }

    fn bundle_json(token: &str, https: bool, scope: &str) -> String {
        let fp = if https {
            r#""tls_cert_fingerprint":"sha256:abc","#
        } else {
            ""
        };
        let port = if https {
            r#""https_port":8443,"#
        } else {
            r#""http_port":8080,"#
        };
        format!(
            r#"{{{port}{fp}"token":"{token}","scope":"{scope}","token_id":"t1","expires_at":"2030-01-01T00:00:00Z"}}"#
        )
    }

    #[test]
    fn decode_bundle_matrix() {
        // Happy paths.
        let b = decode_bundle(bundle_json("tok", true, "control").as_bytes(), "control").unwrap();
        assert_eq!(b.token, "tok");
        assert_eq!(b.expires_at, Some(1_893_456_000)); // 2030-01-01T00:00:00Z
        decode_bundle(bundle_json("tok", false, "").as_bytes(), "control").unwrap();

        // Bad: empty input / not json / empty token / trailing data / no port /
        // https-without-fingerprint / scope mismatch.
        assert!(decode_bundle(b"", "control").is_err());
        assert!(decode_bundle(b"not json", "control").is_err());
        assert!(decode_bundle(bundle_json("", true, "control").as_bytes(), "control").is_err());
        let trailing = format!("{} garbage", bundle_json("tok", true, "control"));
        assert!(decode_bundle(trailing.as_bytes(), "control").is_err());
        assert!(decode_bundle(br#"{"token":"tok"}"#, "control").is_err()); // no port
        assert!(decode_bundle(br#"{"https_port":8443,"token":"tok"}"#, "control").is_err()); // https w/o fingerprint
        assert!(decode_bundle(
            bundle_json("tok", true, "credentials").as_bytes(),
            "control"
        )
        .is_err()); // scope mismatch
    }

    #[test]
    fn decode_bundle_trailing_whitespace_ok() {
        let s = format!("{}\n  \n", bundle_json("tok", false, ""));
        assert!(decode_bundle(s.as_bytes(), "credentials").is_ok());
    }

    #[test]
    fn decode_bundle_scope_absent_is_accepted() {
        // A bundle that omits `scope` is accepted for any want_scope (Go only checks a
        // NON-empty echoed scope).
        let b = decode_bundle(br#"{"http_port":8080,"token":"tok"}"#, "control").unwrap();
        assert_eq!(b.scope, "");
        assert_eq!(b.expires_at, None); // absent expires_at → None (non-expiring)
    }

    #[test]
    fn expires_at_zero_time_is_none() {
        // Go's zero time → omitted (IsZero) → None.
        let b = decode_bundle(
            br#"{"http_port":8080,"token":"tok","expires_at":"0001-01-01T00:00:00Z"}"#,
            "control",
        )
        .unwrap();
        assert_eq!(b.expires_at, None);
    }

    #[test]
    fn expires_at_malformed_fails_decode() {
        // A present-but-malformed expires_at fails the whole decode (Go: json.Unmarshal
        // into time.Time errors).
        assert!(decode_bundle(
            br#"{"http_port":8080,"token":"tok","expires_at":"not-a-time"}"#,
            "control"
        )
        .is_err());
    }

    #[test]
    fn marker_scanner_detects_split_across_reads() {
        // The CHANGED banner split across two feeds is still detected (Go capWriter).
        let mut s = MarkerScanner::new(HOST_KEY_CHANGED_MARKER);
        s.feed(b"noise REMOTE HOST IDENTIFICATION HAS ");
        assert!(!s.seen);
        s.feed(b"CHANGED! more noise");
        assert!(s.seen);
    }

    #[test]
    fn marker_scanner_absent_and_single_chunk() {
        let mut absent = MarkerScanner::new(HOST_KEY_CHANGED_MARKER);
        absent.feed(b"Host key verification failed.");
        absent.feed(b"Permission denied");
        assert!(!absent.seen);

        let mut whole = MarkerScanner::new(HOST_KEY_CHANGED_MARKER);
        whole.feed(b"@@@ REMOTE HOST IDENTIFICATION HAS CHANGED @@@");
        assert!(whole.seen);
    }

    #[test]
    fn find_on_path_resolves_and_misses() {
        // A dir containing an `ssh` file resolves; a PATH without it misses. No process
        // env mutation (path is passed in).
        use std::os::unix::fs::PermissionsExt as _;
        let dir = tempfile::tempdir().unwrap();
        let ssh = dir.path().join("ssh");
        std::fs::write(&ssh, "x").unwrap();
        std::fs::set_permissions(&ssh, std::fs::Permissions::from_mode(0o755)).unwrap();

        let with = std::env::join_paths(["/nonexistent-x", dir.path().to_str().unwrap()]).unwrap();
        assert_eq!(find_on_path("ssh", &with), Some(ssh));
        let without = std::env::join_paths(["/nonexistent-x", "/nonexistent-y"]).unwrap();
        assert_eq!(find_on_path("ssh", &without), None);
    }

    // --- real-runner tests (shell-shim `ssh` via the ssh_override seam — no PATH env
    // mutation, so tests stay hermetic + parallel-safe) -------------------------------

    /// Write a `#!/bin/sh` shim `ssh` into a fresh dir and return (dir, absolute path).
    fn write_shim(body: &str) -> (tempfile::TempDir, PathBuf) {
        use std::io::Write as _;
        use std::os::unix::fs::PermissionsExt as _;
        let dir = tempfile::tempdir().unwrap();
        let ssh = dir.path().join("ssh");
        let mut f = std::fs::File::create(&ssh).unwrap();
        write!(f, "#!/bin/sh\n{body}").unwrap();
        drop(f);
        std::fs::set_permissions(&ssh, std::fs::Permissions::from_mode(0o755)).unwrap();
        (dir, ssh)
    }

    async fn run_shim(body: &str, p: &Params, timeout: Duration) -> Result<Bundle, BootstrapError> {
        let (_dir, ssh) = write_shim(body);
        SystemSshRunner::with_shim(ssh, timeout).run(p).await
    }

    #[tokio::test]
    async fn system_runner_success_via_shim() {
        let bundle = r#"{"https_port":8443,"tls_cert_fingerprint":"sha256:abc","token":"minted","scope":"credentials","token_id":"t1","expires_at":"2030-01-01T00:00:00Z"}"#;
        let body = format!("printf '%s' '{bundle}'\nexit 0\n");
        let b = run_shim(&body, &params(), Duration::from_secs(5))
            .await
            .expect("shim mint");
        assert_eq!(b.token, "minted");
        assert_eq!(b.expires_at, Some(1_893_456_000));
    }

    #[tokio::test]
    async fn system_runner_captures_argv() {
        // The shim writes its argv to a file; assert it equals ssh_args (the real
        // subprocess sees exactly the argv the differential also compares).
        let dir = tempfile::tempdir().unwrap();
        let argv_file = dir.path().join("argv");
        let body = format!(
            "for a in \"$@\"; do printf '%s\\n' \"$a\" >> '{}'; done\nprintf '{{\"http_port\":8080,\"token\":\"t\"}}'\n",
            argv_file.display()
        );
        run_shim(&body, &params(), Duration::from_secs(5))
            .await
            .unwrap();
        let captured: Vec<String> = std::fs::read_to_string(&argv_file)
            .unwrap()
            .lines()
            .map(str::to_string)
            .collect();
        assert_eq!(captured, ssh_args(&params()));
    }

    #[tokio::test]
    async fn system_runner_changed_host_key_is_terminal() {
        // Emit the CHANGED banner on stderr + exit 255 → terminal, through the REAL
        // subprocess path (capWriter marker scan + classify).
        let body = "echo 'REMOTE HOST IDENTIFICATION HAS CHANGED' 1>&2\nexit 255\n";
        let err = run_shim(body, &params(), Duration::from_secs(5))
            .await
            .unwrap_err();
        assert!(err.is_host_key_mismatch(), "got {err:?}");
    }

    #[tokio::test]
    async fn system_runner_uses_c_locale() {
        // The shim echoes $LC_ALL back on stdout as the token; assert it is "C".
        let body = "printf '{\"http_port\":8080,\"token\":\"%s\"}' \"$LC_ALL\"\nexit 0\n";
        let b = run_shim(body, &params(), Duration::from_secs(5))
            .await
            .unwrap();
        assert_eq!(b.token, "C");
    }

    #[tokio::test]
    async fn system_runner_times_out_promptly_on_hung_child() {
        // A shim that sleeps far longer than the timeout must abort quickly (WaitDelay
        // analog: kill_on_drop + prompt start_kill).
        let body = "sleep 30\n";
        let start = std::time::Instant::now();
        let err = run_shim(body, &params(), Duration::from_millis(300))
            .await
            .unwrap_err();
        assert!(matches!(err, BootstrapError::Aborted(_)), "got {err:?}");
        assert!(
            start.elapsed() < Duration::from_secs(5),
            "did not abort promptly"
        );
    }
}
