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

/// The client kind the broker sends with every bootstrap: advisory audit metadata the
/// server records in the issued certificate (Go `authtoken.ClientBroker`).
pub const CLIENT_KIND_BROKER: &str = "broker";

/// One bootstrap exchange (mirrors Go `bootstrap.Params`). Host/Port address the
/// server's SSH endpoint; `known_hosts_path` is the pinned trust root
/// (`~/.shed/known_hosts`); `scope` is the token scope (`"control"`/`"credentials"`);
/// `client_kind` is advisory audit metadata (`"broker"`/`"cli"`/`"desktop"`) and
/// may be empty.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Params {
    pub host: String,
    pub port: u16,
    pub known_hosts_path: String,
    pub scope: String,
    pub client_kind: String,
    /// A standard-base64 PKCS#10 CertificationRequest DER, sent as the `csr=<value>`
    /// request argument so an mtls-mode server can issue a client certificate. Empty
    /// means "no CSR" — the legacy, token-only shape (mirror Go `Params.CSRBase64`).
    ///
    /// Callers do not build this themselves: [`crate::minter::CredentialMinter`]
    /// generates the keypair and fills it in. It is a field (rather than an internal
    /// detail) only so [`validate`] covers it on the same path as every other argv
    /// element.
    pub csr: String,
    /// The one key ssh may offer (mirror Go `Params.IdentityFile`): the server entry's
    /// `identity_file`, or empty for none. When set, the argv gains `-i <path> -o
    /// IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none` after the fixed options
    /// and before `-l` ([`ssh_args`]), so the entry enrols with that key first, with no
    /// key that is only in the agent, and never over a shared connection another key
    /// opened. [`validate`] holds it to the value rule
    /// ([`shed_core::identityfile::check_value`]) and [`SystemSshRunner`] checks the file
    /// before it spawns ssh. Empty leaves the argv exactly as it is without the field.
    pub identity_file: String,
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
    /// The credential shape this bundle carries (`"token"` / `"mtls"`). **ABSENT MEANS
    /// TOKEN** — a server built before client-certificate support omits the key entirely,
    /// so `""` must decode as the bearer-token shape. [`Bundle::is_mtls`] is the single
    /// place that rule is decided (mirror Go `sdk.Bundle.Mode`).
    #[serde(default)]
    pub auth_mode: String,
    #[serde(default)]
    pub http_port: u32,
    #[serde(default)]
    pub https_port: u32,
    #[serde(default)]
    pub tls_cert_fingerprint: String,
    #[serde(default)]
    pub token: String,
    /// The PEM leaf the server's internal CA issued for the submitted CSR. Set in mtls
    /// mode only (mirror Go `sdk.Bundle.ClientCert`).
    #[serde(default)]
    pub client_cert: String,
    #[serde(default)]
    pub scope: String,
    #[serde(default)]
    pub token_id: String,
    /// The issued certificate's serial in lower-case hex. Opaque to the client — it
    /// exists for logs and rotation proofs (mirror Go `sdk.Bundle.CertSerial`).
    #[serde(default)]
    pub cert_serial: String,
    /// Parsed to unix seconds; `None` when absent, empty, or Go's zero time
    /// (`0001-01-01T00:00:00Z`) — the states for which Go omits `token.response`'s
    /// `expires_at`. A present-but-malformed value fails the decode.
    #[serde(default, deserialize_with = "de_expires_at")]
    pub expires_at: Option<i64>,
}

/// The `auth_mode` literal for a client-certificate bundle (mirror `sdk.AuthModeMTLS`).
const AUTH_MODE_MTLS: &str = "mtls";

impl Bundle {
    /// Whether this bundle carries a client certificate rather than a bearer token.
    ///
    /// Absent, empty, and any UNRECOGNIZED future value all decode as token — matching
    /// Go's `sdk.Bundle.Mode`: an unknown mode is not something this client can act on,
    /// and treating it as mtls (the branch that expects a certificate) would fail more
    /// confusingly than treating it as the shape whose fields are actually populated.
    pub fn is_mtls(&self) -> bool {
        self.auth_mode == AUTH_MODE_MTLS
    }
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
    /// A public-key auth failure: the server refused every key ssh offered, for one of
    /// several causes ssh cannot tell apart, which the text names. Retryable (Go
    /// `ErrNoSSHIdentities`). The text is Go's (`ErrNoSSHIdentities` and
    /// `noSSHIdentitiesHint`) byte for byte; a Go test in `sdk/bootstrap` reads this line.
    #[error("sdk/bootstrap: the server refused every key ssh offered (no key ssh can reach, for example an agent ssh cannot find — see IdentityAgent; not a user's key; a passphrase key not in the agent; or the server's user list not in force. A key listed moments ago can be refused once: try again): {0}")]
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
    /// The identity file cannot be handed to ssh: its value breaks the rule, or the file
    /// cannot be used now (mirror Go's `*IdentityFileError` wrapped as `sdk/bootstrap: …`).
    /// Returned before ssh is spawned.
    #[error("sdk/bootstrap: {0}")]
    IdentityFile(shed_core::identityfile::IdentityFileError),
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
    ];
    // The entry's own key, when it has one: after the fixed options, before `-l`, exactly
    // where Go's `sshArgs` puts `IdentityArgs`. Nothing at all when it has none.
    args.extend(shed_core::identityfile::identity_args(&p.identity_file));
    args.extend([
        "-l".to_string(),
        BOOTSTRAP_USER.to_string(),
        p.host.clone(),
        p.scope.clone(),
    ]);
    if !p.client_kind.is_empty() {
        args.push(p.client_kind.clone());
    }
    // The server parses everything after the scope order-independently, so the CSR is
    // appended LAST and a kind-less request (`control csr=…`) is equally valid. A
    // pre-mtls server only ever inspected position 1, so the extra argument is silently
    // ignored there — which is what makes always sending it safe against every server
    // generation. Byte-identical to Go `sshArgs` (`bootstrap.go:155`).
    if !p.csr.is_empty() {
        args.push(format!("{CSR_ARG_PREFIX}{}", p.csr));
    }
    args
}

/// Names the CSR request argument. MUST match the server's `csrArgKey`
/// (`internal/sshd/bootstrap.go`) and Go's client-side `csrArgPrefix`.
const CSR_ARG_PREFIX: &str = "csr=";

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
    // The identity file's value, under the one rule every reader applies (the file itself
    // is checked by the runner, at its time of use).
    shed_core::identityfile::check_entry_value("", &p.identity_file)
        .map_err(BootstrapError::IdentityFile)?;
    validate_csr_arg(&p.csr)
}

/// Enforce that the CSR argument is a single argv token of standard-base64 characters
/// (mirror Go `validateCSRArg`). Empty (no CSR) is valid.
///
/// The CSR rides in ONE argv element (`csr=<base64>`) that the server splits on the
/// FIRST `=` and then base64-decodes. Whitespace would split it into two request
/// arguments (the second of which the server would read as a client kind), and a NUL
/// would truncate the element at exec. Neither can occur in standard base64, so anything
/// outside that alphabet means the value was not produced by `ClientKeyPair` and must not
/// reach exec.
///
/// It scans the ALPHABET rather than attempting a decode: base64 decoders commonly skip
/// `\r`/`\n`, so a successful decode would prove nothing about the element staying one
/// token. The value itself is never echoed — it is long, and a corrupted one is no more
/// legible printed in full.
fn validate_csr_arg(csr: &str) -> Result<(), BootstrapError> {
    for (i, c) in csr.bytes().enumerate() {
        let ok = c.is_ascii_alphanumeric() || c == b'+' || c == b'/' || c == b'=';
        if !ok {
            return Err(BootstrapError::Validate(format!(
                "invalid csr argument: byte {i} is not standard base64"
            )));
        }
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
        // The refusal of every key keeps ssh's words as its cause, and a client shows
        // them: escaped (`diag_text`, Go `diagText`), never raw, so an ESC or a line
        // break a ProxyCommand wrote cannot reach a terminal or a log line.
        return BootstrapError::NoSshIdentities(diag_text(stderr_text.trim(), MAX_ERR_STDERR));
    }
    if !msg.is_empty() {
        return BootstrapError::Ssh { exit, msg };
    }
    BootstrapError::SshFailed(format!("ssh exited {exit}"))
}

/// Validate ssh stdout: a single JSON object, no trailing garbage, the credential its
/// declared mode requires, and (when the server echoes one) a matching scope. Mirrors Go
/// `decodeBundle`, IN THE SAME ORDER. The raw stdout is NEVER included in an error — it
/// carries the credential.
///
/// Mode dispatch is the subtle part: a bundle with NO `auth_mode` key is a pre-mtls
/// server's token bundle and must validate exactly as it always did — see
/// [`Bundle::is_mtls`], which owns that rule.
pub fn decode_bundle(out: &[u8], want_scope: &str) -> Result<Bundle, BootstrapError> {
    let mut de = serde_json::Deserializer::from_slice(out);
    let b = Bundle::deserialize(&mut de)
        .map_err(|_| BootstrapError::Decode("ssh produced no valid bootstrap bundle".into()))?;
    // Require EOF after the single object (trailing whitespace is fine) — Go reads the
    // next token and insists the stream is exhausted.
    de.end().map_err(|_| {
        BootstrapError::Decode("unexpected trailing data after bootstrap bundle".into())
    })?;
    if b.is_mtls() {
        validate_mtls_bundle(&b)?;
    } else {
        validate_token_bundle(&b)?;
    }
    if !b.scope.is_empty() && b.scope != want_scope {
        return Err(BootstrapError::Decode(format!(
            "scope mismatch: requested {want_scope:?}, got {:?}",
            b.scope
        )));
    }
    Ok(b)
}

/// The pre-mtls validation ladder, unchanged: what a legacy bundle (no `auth_mode`) and
/// an explicit token bundle both go through (mirror Go `validateTokenBundle`).
fn validate_token_bundle(b: &Bundle) -> Result<(), BootstrapError> {
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
    Ok(())
}

/// What an mtls bundle must carry (mirror Go `validateMTLSBundle`). The requirements are
/// strictly TIGHTER than the token shape's: the certificate is the entire credential, and
/// mtls exists only on the TLS listener, so an https port and a pin are not optional the
/// way they are for a token (which a legacy server may still serve over plain HTTP).
fn validate_mtls_bundle(b: &Bundle) -> Result<(), BootstrapError> {
    if b.client_cert.trim().is_empty() {
        return Err(BootstrapError::Decode(
            "mtls bundle carries no client certificate".into(),
        ));
    }
    if b.https_port == 0 {
        return Err(BootstrapError::Decode(
            "mtls bundle has no HTTPS port (mtls is only served over TLS)".into(),
        ));
    }
    if b.tls_cert_fingerprint.trim().is_empty() {
        return Err(BootstrapError::Decode(
            "mtls bundle advertises HTTPS without a TLS fingerprint to pin".into(),
        ));
    }
    // A bearer token alongside a certificate would be a server that does not know its own
    // mode; the mtls middleware never reads the Authorization header, so carrying one
    // could only mislead.
    if !b.token.trim().is_empty() {
        return Err(BootstrapError::Decode(
            "mtls bundle unexpectedly carries a bearer token".into(),
        ));
    }
    Ok(())
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
    match crate::time::parse_rfc3339_to_unix(s) {
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
        // The identity file is checked at the time of use, before ssh is spawned (Go
        // `run`): a key removed since it was set fails here with its own reason, not as an
        // ssh that "refused every key".
        if !p.identity_file.is_empty() {
            shed_core::identityfile::check_file(&p.identity_file)
                .map_err(BootstrapError::IdentityFile)?;
        }
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

/// Ends a text `clip` cut (Go `clipMarker`).
const CLIP_MARKER: &str = "…(truncated)";

/// Truncate `s` to at most `n` bytes (on a char boundary), appending an ellipsis marker
/// when cut (Go `clip`).
fn clip(s: &str, n: usize) -> String {
    match clip_head(s, n) {
        Some(head) => format!("{head}{CLIP_MARKER}"),
        None => s.to_string(),
    }
}

/// `s` cut to at most `n` bytes on a char boundary, or `None` when it fits (Go
/// `clipHead`, which cuts valid UTF-8 at the same byte).
fn clip_head(s: &str, n: usize) -> Option<&str> {
    if s.len() <= n {
        return None;
    }
    let mut end = n;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    Some(&s[..end])
}

/// ssh's stderr as a refusal keeps it for display (Go `diagText`, byte for byte, both
/// held to `crates/fixtures/bootstrap-stderr/cases.json`): cut to at most `n` bytes on a
/// char boundary, then escaped, then marked when cut. Printable ASCII (`' '` to `'~'`)
/// other than `\` is itself; `\` is `\\`, a line feed `\n`, a carriage return `\r`, a tab
/// `\t`; every other character (a control character such as ESC, any non-ASCII
/// character, the U+FFFD lossy decoding put where a byte was not UTF-8) is `\u{<hex>}`,
/// lowercase with no leading zeros. The text then cannot move the cursor, colour the
/// terminal, start a new line or reorder what follows.
fn diag_text(s: &str, n: usize) -> String {
    let (head, cut) = match clip_head(s, n) {
        Some(head) => (head, true),
        None => (s, false),
    };
    let mut out = String::with_capacity(head.len());
    for c in head.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            ' '..='~' => out.push(c),
            _ => out.push_str(&format!("\\u{{{:x}}}", c as u32)),
        }
    }
    if cut {
        out.push_str(CLIP_MARKER);
    }
    out
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
            client_kind: CLIENT_KIND_BROKER.into(),
            csr: String::new(),
            identity_file: String::new(),
        }
    }

    /// With an identity file, the exact vector: the four identity elements after the 17
    /// fixed options and before `-l`, nothing else moved (Go `sshArgs` with
    /// `Params.IdentityFile`).
    #[test]
    fn ssh_args_exact_vector_with_an_identity_file() {
        let mut p = params();
        p.identity_file = "/home/pat/.ssh/id work".into();
        let got = ssh_args(&p);
        let without = ssh_args(&params());
        let at = without.iter().position(|a| a == "-l").unwrap();
        let mut want = without[..at].to_vec();
        want.extend(
            [
                "-i",
                "/home/pat/.ssh/id work",
                "-o",
                "IdentitiesOnly=yes",
                "-o",
                "ControlMaster=no",
                "-o",
                "ControlPath=none",
            ]
            .map(String::from),
        );
        want.extend(without[at..].iter().cloned());
        assert_eq!(got, want);
        assert_eq!(got[36], "PermitLocalCommand=no");
        assert_eq!(got[37], "-i", "right after PermitLocalCommand=no");
        assert_eq!(got[45], "-l");
    }

    /// The Go-written fixture (`crates/fixtures/identityfile/`): every accepted entry
    /// gives Go's bootstrap argv byte for byte (scope `control`, client kind `cli`, no
    /// CSR, the fixture's known_hosts), and every refused entry is refused before ssh is
    /// spawned (a shim that records any spawn stays untouched). Each entry's
    /// `identity_file`, host and port are the fixture's decoded values (shed-core's
    /// reader decodes `config.yaml` to them: `config.rs`'s test of the same fixture).
    #[tokio::test]
    async fn the_go_fixture_gives_go_s_bootstrap_argv_and_its_refusals_spawn_nothing() {
        let root =
            std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../fixtures/identityfile");
        let expected: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(root.join("expected.json")).unwrap())
                .unwrap();
        let host = expected["host"].as_str().unwrap();
        let port = u16::try_from(expected["ssh_port"].as_u64().unwrap()).unwrap();
        let accepted = expected["accepted"].as_array().unwrap();
        for want in accepted {
            let name = want["entry"].as_str().unwrap();
            let p = Params {
                host: host.into(),
                port,
                known_hosts_path: expected["known_hosts"].as_str().unwrap().into(),
                scope: "control".into(),
                client_kind: "cli".into(),
                csr: String::new(),
                identity_file: want["identity_file"].as_str().unwrap().into(),
            };
            validate(&p).unwrap_or_else(|e| panic!("{name}: {e}"));
            let want_argv: Vec<String> = want["bootstrap_argv"]
                .as_array()
                .unwrap()
                .iter()
                .map(|a| a.as_str().unwrap().to_string())
                .collect();
            assert_eq!(ssh_args(&p), want_argv, "{name}");
        }

        let dir = tempfile::tempdir().unwrap();
        let spawned = dir.path().join("spawned");
        let body = format!(": > '{}'\nexit 0\n", spawned.display());
        let (_shim_dir, ssh) = write_shim(&body);
        let runner = SystemSshRunner::with_shim(ssh, Duration::from_secs(5));
        for want in expected["refused"].as_array().unwrap() {
            let name = want["entry"].as_str().unwrap();
            let p = Params {
                identity_file: want["identity_file"].as_str().unwrap().into(),
                ..params()
            };
            // The argv-safety gate refuses it on its own, and so does the runner.
            assert!(
                matches!(validate(&p), Err(BootstrapError::IdentityFile(_))),
                "{name}: validate"
            );
            match runner.run(&p).await {
                Err(BootstrapError::IdentityFile(e)) => {
                    assert_eq!(e.rule.map(|r| r.class()), want["class"].as_str(), "{name}")
                }
                other => panic!("{name}: {other:?}"),
            }
        }
        assert!(!spawned.exists(), "no ssh was spawned for a refused value");
    }

    /// A file that is missing at the time of use is refused before ssh is spawned, with
    /// its own reason, not as an ssh that refused every key.
    #[tokio::test]
    async fn a_missing_identity_file_is_refused_before_ssh() {
        let dir = tempfile::tempdir().unwrap();
        let spawned = dir.path().join("spawned");
        let (_shim_dir, ssh) = write_shim(&format!(": > '{}'\nexit 0\n", spawned.display()));
        let p = Params {
            identity_file: dir.path().join("gone").to_str().unwrap().into(),
            ..params()
        };
        let err = SystemSshRunner::with_shim(ssh, Duration::from_secs(5))
            .run(&p)
            .await
            .unwrap_err();
        assert!(
            err.to_string()
                .ends_with("cannot be used: the file does not exist"),
            "{err}"
        );
        assert!(!spawned.exists());
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
            "broker",
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

    /// The `csr=` element is ONE argv token appended AFTER scope and the optional kind,
    /// and a kind-less request keeps it directly after the scope (the server parses the
    /// post-scope arguments position-independently). Byte-compat with Go `sshArgs`.
    #[test]
    fn ssh_args_appends_csr_last() {
        let mut p = params();
        p.csr = "QUJD+/aa==".into();
        let got = ssh_args(&p);
        assert_eq!(
            &got[got.len() - 3..],
            &[
                "credentials".to_string(),
                "broker".to_string(),
                "csr=QUJD+/aa==".to_string()
            ]
        );

        // No kind → `<scope> csr=…`, still a single trailing token.
        p.client_kind = String::new();
        let got = ssh_args(&p);
        assert_eq!(
            &got[got.len() - 2..],
            &["credentials".to_string(), "csr=QUJD+/aa==".to_string()]
        );

        // Empty CSR → the legacy, token-only argv (no trailing element at all).
        p.csr = String::new();
        p.client_kind = CLIENT_KIND_BROKER.into();
        assert_eq!(ssh_args(&p), ssh_args(&params()));
    }

    /// A real generated CSR survives argv composition as one token the server can split
    /// on the FIRST `=` — the end-to-end shape check across the shed-core producer and
    /// this consumer.
    #[test]
    fn ssh_args_carries_a_real_generated_csr() {
        let kp = shed_core::csr::ClientKeyPair::generate().unwrap();
        let mut p = params();
        p.csr = kp.csr_base64();
        validate(&p).expect("a generated CSR passes validation");
        let arg = ssh_args(&p).pop().unwrap();
        let (key, value) = arg.split_once('=').unwrap();
        assert_eq!(key, "csr");
        // Splitting on the FIRST `=` keeps base64 padding in the VALUE (the classic
        // Split-vs-SplitN bug the server-side parser guards against).
        assert_eq!(value, kp.csr_base64());
        assert!(!arg.contains(char::is_whitespace));
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

    /// The `csr=` value gets the same argv-safety gate as every other element: a single
    /// token of standard base64. Whitespace would split it into TWO request arguments
    /// (the second read as a client kind) and a NUL would truncate the element at exec —
    /// neither can occur in the alphabet, so the scan is the gate (mirror Go
    /// `validateCSRArg`).
    #[test]
    fn validate_rejects_unsafe_csr_arguments() {
        let bad = |csr: &str| {
            let mut p = params();
            p.csr = csr.into();
            validate(&p).is_err()
        };
        assert!(bad("QUJD REVG"), "space");
        assert!(bad("QUJD\tREVG"), "tab");
        assert!(bad("QUJD\nREVG"), "newline");
        assert!(bad("QUJD\rREVG"), "carriage return");
        assert!(bad("QUJD\0REVG"), "NUL");
        assert!(bad("QUJD-REVG_"), "url-safe alphabet");
        assert!(bad("-oProxyCommand=x"), "option injection");

        // Standard base64 (with padding) and the empty value both pass.
        let mut ok = params();
        ok.csr = "QUJDREVG+/12==".into();
        assert!(validate(&ok).is_ok());
        ok.csr = String::new();
        assert!(validate(&ok).is_ok());

        // The rejection names the offending byte offset and never echoes the value.
        let mut p = params();
        p.csr = "AAAA BBBB".into();
        let err = validate(&p).unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("csr argument"), "{msg}");
        assert!(msg.contains("byte 4"), "{msg}");
        assert!(!msg.contains("AAAA"), "value must not be echoed: {msg}");
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
        // Its text names the refusal and each cause, then ssh's words; it names no
        // server setting (where a key is listed is the server's own choice).
        let text = classify(255, false, "mini3: Permission denied (publickey).").to_string();
        assert_eq!(
            text,
            "sdk/bootstrap: the server refused every key ssh offered (no key ssh can reach, \
             for example an agent ssh cannot find — see IdentityAgent; not a user's key; a \
             passphrase key not in the agent; or the server's user list not in force. A key \
             listed moments ago can be refused once: try again): mini3: Permission denied \
             (publickey)."
        );
        for old in [
            "allowlist",
            "github_users",
            "authorized_keys",
            "could not authenticate",
        ] {
            assert!(!text.contains(old), "{old:?} in {text}");
        }
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

    /// The cause the refusal of every key keeps, against the one hand-written table Go's
    /// `diagText` is held to as well (`TestNoSSHIdentitiesTextMatchesRust` in
    /// `sdk/bootstrap`): ssh's stderr escaped (an ESC sequence, line breaks, a backslash,
    /// non-ASCII) and clipped on a char boundary (a multi-byte character at the 2048-byte
    /// boundary), the same text in both languages.
    #[test]
    fn no_ssh_identities_cause_shared_table() {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../fixtures/bootstrap-stderr/cases.json");
        let text = std::fs::read_to_string(&path).expect("read the shared table");
        let table: serde_json::Value = serde_json::from_str(&text).expect("decode the table");
        let cases = table["cases"].as_array().expect("cases");
        assert!(
            cases.len() >= 10,
            "the shared table holds only {} cases",
            cases.len()
        );
        for case in cases {
            let name = case["name"].as_str().expect("name");
            let pad = "a".repeat(case.get("pad").and_then(|p| p.as_u64()).unwrap_or(0) as usize);
            let stderr = format!("{pad}{}", case["stderr"].as_str().expect("stderr"));
            let cause = format!("{pad}{}", case["cause"].as_str().expect("cause"));
            match classify(255, false, &stderr) {
                BootstrapError::NoSshIdentities(got) => assert_eq!(got, cause, "{name}"),
                other => panic!("{name}: classify = {other:?}, want NoSshIdentities"),
            }
        }
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

    /// The mtls leg of the D4 matrix: a certificate bundle decodes, and every shape that
    /// could not authenticate is refused.
    #[test]
    fn decode_bundle_mtls_matrix() {
        const CERT: &str = "-----BEGIN CERTIFICATE-----\\nMIIB\\n-----END CERTIFICATE-----\\n";
        let mtls = |extra: &str| {
            format!(
                r#"{{"auth_mode":"mtls","https_port":8443,"tls_cert_fingerprint":"sha256:abc",
                     "client_cert":"{CERT}","scope":"control","cert_serial":"0a1b",
                     "expires_at":"2030-01-01T00:00:00Z"{extra}}}"#
            )
        };

        // Happy path: the certificate + serial land on the bundle, the token stays empty.
        let b = decode_bundle(mtls("").as_bytes(), "control").unwrap();
        assert!(b.is_mtls());
        assert_eq!(b.cert_serial, "0a1b");
        assert_eq!(b.token, "");
        assert!(b.client_cert.contains("BEGIN CERTIFICATE"));
        assert_eq!(b.expires_at, Some(1_893_456_000));

        // A bearer token ALONGSIDE the certificate is a server that does not know its
        // own mode — refused rather than silently half-adopted.
        assert!(decode_bundle(mtls(r#","token":"tok""#).as_bytes(), "control").is_err());

        // Missing certificate / no https port / no fingerprint each fail closed.
        assert!(decode_bundle(
            br#"{"auth_mode":"mtls","https_port":8443,"tls_cert_fingerprint":"sha256:abc","scope":"control"}"#,
            "control"
        )
        .is_err());
        assert!(decode_bundle(
            br#"{"auth_mode":"mtls","http_port":8080,"tls_cert_fingerprint":"sha256:abc","client_cert":"x"}"#,
            "control"
        )
        .is_err());
        assert!(decode_bundle(
            br#"{"auth_mode":"mtls","https_port":8443,"client_cert":"x"}"#,
            "control"
        )
        .is_err());

        // The scope check is mode-independent.
        assert!(decode_bundle(mtls("").as_bytes(), "credentials").is_err());
    }

    /// Absent `auth_mode` means TOKEN: a pre-mtls server's bundle keeps validating (and
    /// decoding) exactly as it always did, and an UNRECOGNIZED future mode falls through
    /// to the same branch rather than being treated as a certificate bundle.
    #[test]
    fn decode_bundle_absent_auth_mode_is_token() {
        // Legacy bundle: no auth_mode key at all.
        let b = decode_bundle(bundle_json("tok", true, "control").as_bytes(), "control").unwrap();
        assert_eq!(b.auth_mode, "");
        assert!(!b.is_mtls());
        assert_eq!(b.token, "tok");
        assert_eq!(b.client_cert, "");

        // Explicit "token" behaves identically.
        let explicit = br#"{"auth_mode":"token","http_port":8080,"token":"tok"}"#;
        let b = decode_bundle(explicit, "control").unwrap();
        assert!(!b.is_mtls());
        assert_eq!(b.token, "tok");

        // An unknown future mode is validated as a token bundle (the shape whose fields
        // are actually populated), not as mtls.
        let future = br#"{"auth_mode":"quantum","http_port":8080,"token":"tok"}"#;
        assert!(!decode_bundle(future, "control").unwrap().is_mtls());
        // ...so a bundle claiming that mode with no token is still rejected.
        let future_empty = br#"{"auth_mode":"quantum","http_port":8080,"client_cert":"x"}"#;
        assert!(decode_bundle(future_empty, "control").is_err());
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

    /// Run a shim with a bounded ETXTBSY retry. Another test's fork can
    /// transiently inherit this shim's write fd across our exec — the fd table
    /// is process-wide, so a concurrent `Command::spawn` anywhere in this test
    /// binary races every freshly-written executable (the classic fork/exec
    /// race; hits ~100% on the 4-vCPU ubuntu runner, unreproducible on dev
    /// machines). The spawn errno is stringified away by
    /// `BootstrapError::SshFailed(e.to_string())`, so match the locale-stable
    /// `"os error 26"` suffix, never the English prose. Each attempt writes a
    /// FRESH shim (fresh tempdir); non-ETXTBSY results return immediately, so
    /// error-asserting tests (timeout, changed-host-key) are unaffected.
    async fn run_shim(body: &str, p: &Params, timeout: Duration) -> Result<Bundle, BootstrapError> {
        let mut delay = Duration::from_millis(10);
        for _ in 0..10 {
            let (_dir, ssh) = write_shim(body);
            match SystemSshRunner::with_shim(ssh, timeout).run(p).await {
                Err(BootstrapError::SshFailed(m)) if m.contains("os error 26") => {
                    tokio::time::sleep(delay).await;
                    delay = (delay * 2).min(Duration::from_millis(160));
                }
                r => return r,
            }
        }
        // Exhaustion must fail LOUDLY (mirrors docker_backend::exec_retrying): a
        // persistent ETXTBSY is not the transient fork/exec race, and returning
        // the SshFailed would let error-asserting callers mistake it for the
        // error they intended to provoke.
        panic!("persistent ETXTBSY exec'ing the ssh shim — not the transient fork/exec race");
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
        // `: >` truncates first so an ETXTBSY retry (run_shim) can't double the
        // captured argv via the appends below.
        let body = format!(
            ": > '{p}'\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '{p}'; done\nprintf '{{\"http_port\":8080,\"token\":\"t\"}}'\n",
            p = argv_file.display()
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
