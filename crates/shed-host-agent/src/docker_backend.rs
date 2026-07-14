//! The Docker registry-credential backend — a faithful Rust port of
//! `cmd/shed-host-agent/docker_backend.go`.
//!
//! Resolves Docker registry credentials on the host by reading the host's Docker
//! `config.json` and, when a credential helper is configured, shelling out to
//! `docker-credential-<helper> get`. The allowlist policy is resolved **per
//! `(server, shed)` request** from [`crate::config::DockerConfig`] (not fixed at
//! construction), so the same backend serves every shed with the shed's own policy.
//!
//! **Resolution order (`get_credentials`, mirror `docker_backend.go:128`):**
//! 1. `normalize_registry(server_url)`.
//! 2. **Allowlist check FIRST** — a not-allowed registry is refused
//!    (`REGISTRY_NOT_ALLOWED`) *before* `config.json` is even read, so a blocked
//!    registry with a perfectly good local credential is still denied.
//! 3. `read_config()`.
//! 4. `credHelpers[reg]` → exec the per-registry helper and **return its result
//!    directly** (a per-registry helper error PROPAGATES — no fallback).
//! 5. `credsStore` (default helper) → exec, and **on error FALL THROUGH** to inline
//!    auths (the credsStore-vs-credHelpers asymmetry: only credsStore failure is
//!    swallowed).
//! 6. Inline `auths[reg].auth` → `decode_inline_auth`.
//! 7. None → `CREDENTIALS_NOT_FOUND`.
//!
//! **The unconfigured-backend asymmetry (the crux of this slice):** unlike the AWS
//! backend (which errors when `!enabled()`), [`new_docker_backend`] returns an error
//! **only** when an explicit `config_path` is set but cannot be stat'd. An empty /
//! absent `docker:` block (or a missing default `~/.docker/config.json`) yields a
//! **live backend, no error** — one that then denies every registry (empty
//! allowlist). So the docker-credentials bus namespace is subscribed for every server
//! in the common case, even unconfigured. See [`new_docker_backend`].
//!
//! **The helper-exec seam** ([`HelperExecutor`]) is the live-diffable process-spawning
//! boundary. The production [`RealHelperExecutor`] resolves the helper to an ABSOLUTE
//! path first (Go's `lookHelperPath`: `PATH` via [`look_path`], then the extra
//! well-known dirs with an executable-bit check), APPENDS the extra dirs to the
//! child's `PATH` ([`augment_path`]), pipes the raw `server_url` on stdin, and parses
//! the helper's `{"ServerURL","Username","Secret"}` reply — all under a 5 s timeout
//! with `kill_on_drop` so a wedged helper can never hang a mint. Unit tests inject a
//! fake executor; the real one is exercised by the shell-shim `exec_helper_*` tests.
//!
//! **Wiring:** this module is reached through the docker-credentials bus handler
//! (`bus.rs`) + `main.rs` (commit 3), which construct the backend via
//! [`new_docker_backend`] and dispatch `get`/`list`/`status` to it. Ported here in
//! commit 2, wired later — the "ported, wired by a later slice" posture the AWS
//! backend established.
//!
//! Wired in commit 3b: the bus references `new_docker_backend` / the `DockerBackend`
//! trait + the `DOCKER_CODE_*` codes (the AWS slice carried a blanket `allow(dead_code)`
//! in its commit 2, then dropped it on wiring — done here likewise).

use std::collections::{BTreeMap, HashSet};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use base64::Engine as _;
use serde::Deserialize;
use tokio::io::AsyncWriteExt as _;

use crate::bus::BusLog;
use crate::config::{expand_tilde, user_home_dir, DockerConfig};

// The Docker wire error codes (mirror `internal/ext/protocol/docker.go`). Duplicated
// here as string constants because the Rust daemon does not link the Go protocol
// package; the handler (commit 3) maps these onto `DockerErrorResponse.code`.
// NOT_FOUND / NOT_ALLOWED / INTERNAL are `pub(crate)` — the bus handler maps them onto
// the guest wire (the anonymous-vs-error audit split keys on NOT_FOUND; deny returns
// NOT_ALLOWED; unknown-op/invalid-payload/list-error/empty-code use INTERNAL).
// HELPER_FAILED is only ever set by the executor here, so it stays private (the handler
// forwards `DockerCredError.code` verbatim, never naming it).
pub(crate) const DOCKER_CODE_NOT_FOUND: &str = "CREDENTIALS_NOT_FOUND";
pub(crate) const DOCKER_CODE_NOT_ALLOWED: &str = "REGISTRY_NOT_ALLOWED";
const DOCKER_CODE_HELPER_FAILED: &str = "HELPER_FAILED";
pub(crate) const DOCKER_CODE_INTERNAL: &str = "INTERNAL_ERROR";

/// The list separator for `PATH`. The daemon targets macOS/Linux only (vsock/UDS), so
/// the unix `:` is correct; Go uses `os.PathListSeparator` for the same reason.
const PATH_LIST_SEP: &str = ":";

/// The default per-exec helper timeout (Go `5*time.Second`). A field on
/// [`RealHelperExecutor`] so tests can shorten it (the minter `DEFAULT_TIMEOUT`
/// precedent).
const DEFAULT_HELPER_TIMEOUT: Duration = Duration::from_secs(5);

/// A single Docker registry credential (mirror Go's `DockerCredential`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DockerCredential {
    pub server_url: String,
    pub username: String,
    pub secret: String,
}

/// A backend error carrying the wire `code` the handler maps (mirror Go's
/// `dockerError`). `Display` renders only `msg` (Go's `Error()`); `code` may be empty
/// for the plain-error cases Go builds with a bare `fmt.Errorf` (the inline-auth
/// decode failures) — the handler maps an empty code to `INTERNAL_ERROR`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DockerCredError {
    pub msg: String,
    pub code: String,
}

impl DockerCredError {
    fn new(msg: impl Into<String>, code: &str) -> Self {
        Self {
            msg: msg.into(),
            code: code.to_string(),
        }
    }

    /// A plain error with no wire code (mirror Go's bare `fmt.Errorf` — no
    /// `dockerError`), which the handler maps to `INTERNAL_ERROR`.
    fn plain(msg: impl Into<String>) -> Self {
        Self {
            msg: msg.into(),
            code: String::new(),
        }
    }
}

impl std::fmt::Display for DockerCredError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.msg)
    }
}

impl std::error::Error for DockerCredError {}

/// The host-side Docker credential operations (mirror Go's `DockerBackend`
/// interface). Async + `&self` so the bus handler can hold it as
/// `Arc<dyn DockerBackend>`. [`Self::status`] NEVER errors.
#[async_trait]
pub trait DockerBackend: Send + Sync {
    /// Credentials for `server_url`, applying the allowlist resolved for
    /// `(server, shed)`.
    async fn get_credentials(
        &self,
        server: &str,
        shed: &str,
        server_url: &str,
    ) -> Result<DockerCredential, DockerCredError>;

    /// Map of allowed registry hostnames → usernames (or a placeholder label) for the
    /// policy resolved for `(server, shed)`.
    async fn list_credentials(
        &self,
        server: &str,
        shed: &str,
    ) -> Result<BTreeMap<String, String>, String>;

    /// The allowlist mode and registry count resolved for `(server, shed)`.
    fn status(&self, server: &str, shed: &str) -> (bool, usize);
}

/// The credential-helper exec seam (mirror Go's `helperExecutor`) — the
/// live-diffable process-spawning boundary. A trait so unit tests inject a fake
/// without real binaries. `Send + Sync` for `Arc<dyn HelperExecutor>`.
#[async_trait]
pub trait HelperExecutor: Send + Sync {
    async fn exec_helper(
        &self,
        helper_name: &str,
        server_url: &str,
    ) -> Result<DockerCredential, DockerCredError>;
}

/// The relevant parts of `~/.docker/config.json` (mirror Go's `dockerConfig`). Every
/// field defaults so a missing key yields an empty map/string rather than a decode
/// error (Go's `json.Unmarshal` zero-values). Tags mirror Go's exact json tags.
#[derive(Debug, Default, Deserialize)]
struct DockerConfigFile {
    #[serde(default, rename = "credHelpers")]
    cred_helpers: BTreeMap<String, String>,
    #[serde(default, rename = "credsStore")]
    creds_store: String,
    #[serde(default)]
    auths: BTreeMap<String, DockerAuthEntry>,
}

/// One `auths` entry (mirror Go's `dockerAuthEntry`). `auth` is base64(user:pass).
#[derive(Debug, Default, Deserialize)]
struct DockerAuthEntry {
    #[serde(default)]
    auth: String,
}

/// The credential-helper protocol reply (mirror the anonymous struct in Go's
/// `execHelper`). The tags are the **Capitalized** docker-credential-helper protocol
/// names, pinned with **explicit** per-field renames — NOT `rename_all =
/// "PascalCase"`, which would emit `ServerUrl` and silently drop the `ServerURL`
/// field. This is a distinct struct from the guest wire's snake_case
/// `DockerGetResponse`; `docker_helper_struct_roundtrip_serverurl` guards the drift.
#[derive(Debug, Default, Deserialize)]
struct HelperCredential {
    #[serde(default, rename = "ServerURL")]
    server_url: String,
    #[serde(default, rename = "Username")]
    username: String,
    #[serde(default, rename = "Secret")]
    secret: String,
}

/// Credential-helper install locations that may be absent from the inherited PATH —
/// notably under a bare launchd PATH (`brew services`). Docker Desktop symlinks its
/// helper into `/usr/local/bin`; Homebrew installs into `/opt/homebrew/bin` (Apple
/// Silicon) or `/usr/local/bin` (Intel). Mirror Go's `extraHelperDirs`.
fn extra_helper_dirs() -> Vec<String> {
    vec![
        "/usr/local/bin".to_string(),
        "/opt/homebrew/bin".to_string(),
    ]
}

/// The concrete Docker backend (mirror Go's `dockerHelperBackend`). `config_path` is
/// `None` when no config file was resolved (Go's empty-string sentinel). The helper
/// dirs live on the [`RealHelperExecutor`] behind the [`HelperExecutor`] seam — the
/// exec path that reads them — not on the backend (Go merges the two because its
/// backend IS its own executor; the Rust seam split puts them where they are used).
pub struct DockerHelperBackend {
    config_path: Option<String>,
    cfg: DockerConfig,
    executor: Arc<dyn HelperExecutor>,
    log: Arc<dyn BusLog>,
}

/// Build a Docker backend that reads from the host's Docker credential store (mirror
/// `NewDockerBackend`, `docker_backend.go:88`). Returns an error **only** if an
/// explicit `config_path` is specified but cannot be stat'd; a missing default config
/// is NOT an error — the crux of the unconfigured-non-nil asymmetry (see the module
/// docs). `config_path`'s `~/` prefix is tilde-expanded here (Go's `LoadConfig`
/// stores it raw; the constructor expands).
///
/// Unwired in this slice — the handler + `main.rs` construct it in commit 3 (the same
/// "ported, wired by a later slice" posture as the AWS backend).
pub fn new_docker_backend(
    cfg: DockerConfig,
    log: Arc<dyn BusLog>,
) -> Result<DockerHelperBackend, String> {
    let mut config_path = cfg.config_path.clone();
    if config_path.is_empty() {
        config_path = find_docker_config();
    } else {
        // `expand_tilde` returns a non-`~/` path unchanged, so this covers both the
        // tilde and the plain-absolute explicit-path cases (Go's `expandTilde`).
        config_path = expand_tilde(&config_path);
    }

    // Error ONLY when an explicit config_path was set but is unstat'able. A default
    // path (from find_docker_config) that is missing is fine — find_docker_config
    // already returned "" for it, so config_path is non-empty here only when it exists
    // or when it was explicit. Stat guards the explicit case.
    if !cfg.config_path.is_empty() && !config_path.is_empty() {
        if let Err(e) = std::fs::metadata(&config_path) {
            return Err(format!("docker config not found at {config_path}: {e}"));
        }
    }

    let registry_info = if cfg.allow_all {
        "all (allow_all)".to_string()
    } else if !cfg.registries.is_empty() {
        cfg.registries.join(", ")
    } else {
        "none".to_string()
    };
    log.info(&format!(
        "Docker backend initialized: config={config_path:?} registries={registry_info}"
    ));

    Ok(DockerHelperBackend {
        config_path: if config_path.is_empty() {
            None
        } else {
            Some(config_path)
        },
        cfg,
        executor: Arc::new(RealHelperExecutor::new(extra_helper_dirs())),
        log,
    })
}

impl DockerHelperBackend {
    /// Read + parse the Docker `config.json` (mirror `readConfig`,
    /// `docker_backend.go:216`): no path → empty config; file missing (`NotFound`) →
    /// empty config, **no error**; other read error → error (raw); parse error →
    /// `"parsing <path>: <e>"`. Returns a plain `String` error the callers wrap.
    fn read_config(&self) -> Result<DockerConfigFile, String> {
        let Some(path) = &self.config_path else {
            return Ok(DockerConfigFile::default());
        };
        let data = match std::fs::read(path) {
            Ok(d) => d,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Ok(DockerConfigFile::default());
            }
            Err(e) => return Err(e.to_string()),
        };
        serde_json::from_slice(&data).map_err(|e| format!("parsing {path}: {e}"))
    }
}

#[async_trait]
impl DockerBackend for DockerHelperBackend {
    async fn get_credentials(
        &self,
        server: &str,
        shed: &str,
        server_url: &str,
    ) -> Result<DockerCredential, DockerCredError> {
        let normalized = normalize_registry(server_url);

        // (2) Allowlist check FIRST — before reading config.json — so a blocked
        // registry is refused even when the config is missing or unreadable, and even
        // when a perfectly good local credential exists for it.
        let resolved = self.cfg.resolve(server, shed);
        if !resolved.allow_all
            && !normalize_registry_set(&resolved.registries).contains(&normalized)
        {
            return Err(DockerCredError::new(
                format!("registry {server_url:?} not in allowlist"),
                DOCKER_CODE_NOT_ALLOWED,
            ));
        }

        // (3) read config.json.
        let cfg = self.read_config().map_err(|e| {
            DockerCredError::new(format!("reading docker config: {e}"), DOCKER_CODE_INTERNAL)
        })?;

        // (4) credHelpers first (per-registry helper). Look up both raw + normalized
        // forms since config keys may be "https://index.docker.io/v1/" while the guest
        // sends "index.docker.io". A per-registry helper error PROPAGATES.
        if let Some(helper) = lookup_config_map(&cfg.cred_helpers, server_url, &normalized) {
            return self.executor.exec_helper(helper, server_url).await;
        }

        // (5) credsStore (default helper). On error FALL THROUGH to inline auths (the
        // asymmetry: only credsStore failure is swallowed).
        if !cfg.creds_store.is_empty() {
            match self
                .executor
                .exec_helper(&cfg.creds_store, server_url)
                .await
            {
                Ok(cred) => return Ok(cred),
                Err(e) => {
                    self.log.debug(&format!(
                        "default credsStore helper failed, trying auths fallback: helper={} error={}",
                        cfg.creds_store, e
                    ));
                }
            }
        }

        // (6) inline auths.
        if let Some(entry) = lookup_config_map(&cfg.auths, server_url, &normalized) {
            if !entry.auth.is_empty() {
                return decode_inline_auth(server_url, &entry.auth);
            }
        }

        // (7) none.
        Err(DockerCredError::new(
            format!("no credentials found for {server_url:?}"),
            DOCKER_CODE_NOT_FOUND,
        ))
    }

    async fn list_credentials(
        &self,
        server: &str,
        shed: &str,
    ) -> Result<BTreeMap<String, String>, String> {
        let cfg = self
            .read_config()
            .map_err(|e| format!("reading docker config: {e}"))?;

        let resolved = self.cfg.resolve(server, shed);
        let allow_all = resolved.allow_all;
        let allowed = normalize_registry_set(&resolved.registries);

        let mut result = BTreeMap::new();

        // From credHelpers (labelled, never exec'd).
        for server_url in cfg.cred_helpers.keys() {
            if allow_all || allowed.contains(&normalize_registry(server_url)) {
                result.insert(server_url.clone(), "(credential helper)".to_string());
            }
        }

        // From inline auths (username decoded when possible, else a placeholder).
        for (server_url, auth) in &cfg.auths {
            if allow_all || allowed.contains(&normalize_registry(server_url)) {
                if !auth.auth.is_empty() {
                    if let Ok(cred) = decode_inline_auth(server_url, &auth.auth) {
                        result.insert(server_url.clone(), cred.username);
                        continue;
                    }
                }
                result.insert(server_url.clone(), "(auth entry)".to_string());
            }
        }

        Ok(result)
    }

    fn status(&self, server: &str, shed: &str) -> (bool, usize) {
        let resolved = self.cfg.resolve(server, shed);
        (
            resolved.allow_all,
            normalize_registry_set(&resolved.registries).len(),
        )
    }
}

/// Build a normalized lookup set from a registries list (mirror
/// `normalizeRegistrySet`). Deduplicates by normalized form, so `status`'s count is
/// the number of DISTINCT normalized registries.
fn normalize_registry_set(registries: &[String]) -> HashSet<String> {
    registries.iter().map(|r| normalize_registry(r)).collect()
}

/// Strip protocol prefix and trailing slash / `/v1` / `/v2` — in that order — to
/// produce a canonical hostname for allowlist matching (mirror `normalizeRegistry`,
/// `docker_backend.go:375`).
///
/// Uses `strip_prefix`/`strip_suffix` (a **single** occurrence each), NOT
/// `trim_*_matches`, to match Go's `TrimPrefix`/`TrimSuffix`: `ghcr.io//` → `ghcr.io/`
/// (one trailing slash stripped, not all). The trailing `/` is stripped BEFORE `/v1`,
/// so `https://index.docker.io/v1/` → `index.docker.io/v1/` → `index.docker.io/v1` →
/// `index.docker.io`.
fn normalize_registry(s: &str) -> String {
    let s = s.strip_prefix("https://").unwrap_or(s);
    let s = s.strip_prefix("http://").unwrap_or(s);
    let s = s.strip_suffix('/').unwrap_or(s);
    let s = s.strip_suffix("/v1").unwrap_or(s);
    let s = s.strip_suffix("/v2").unwrap_or(s);
    s.to_string()
}

/// Search a Docker config map using the raw key, then the normalized key (if it
/// differs), then a normalize-every-key scan (mirror `lookupConfigMap`,
/// `docker_backend.go:387`). This is what lets a config key stored as
/// `https://index.docker.io/v1/` match a guest `index.docker.io` request (and vice
/// versa). Generic over the value type (credHelpers = `String`, auths =
/// `DockerAuthEntry`).
fn lookup_config_map<'a, V>(
    m: &'a BTreeMap<String, V>,
    raw: &str,
    normalized: &str,
) -> Option<&'a V> {
    if let Some(v) = m.get(raw) {
        return Some(v);
    }
    if raw != normalized {
        if let Some(v) = m.get(normalized) {
            return Some(v);
        }
    }
    for (k, v) in m {
        if normalize_registry(k) == normalized {
            return Some(v);
        }
    }
    None
}

/// Decode a base64(user:pass) inline auth (mirror `decodeInlineAuth`,
/// `docker_backend.go:406`). Uses the **padded STANDARD** base64 engine (Go's
/// `StdEncoding` requires padding). The password may contain colons (split on the
/// FIRST colon only). Both failure modes return a **plain** (codeless) error mirroring
/// Go's bare `fmt.Errorf`:
/// * decode error → `"decoding auth for <url>: <e>"` — the PREFIX is exact-ported and
///   unit-pinned; the SUFFIX (`<e>`) is an impl-dependent divergence (Go's `illegal
///   base64 data at input byte N` vs the Rust `base64` crate's text), so the
///   malformed-base64 case is EXCLUDED from `docker_inline_auth.json` and never
///   live-diffed.
/// * no colon → `"invalid auth format for <url>"` — prefix-only, no runtime text,
///   golden-fixtured.
fn decode_inline_auth(
    server_url: &str,
    encoded: &str,
) -> Result<DockerCredential, DockerCredError> {
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .map_err(|e| DockerCredError::plain(format!("decoding auth for {server_url}: {e}")))?;
    let decoded = String::from_utf8_lossy(&decoded);
    let Some((username, secret)) = decoded.split_once(':') else {
        return Err(DockerCredError::plain(format!(
            "invalid auth format for {server_url}"
        )));
    };
    Ok(DockerCredential {
        server_url: server_url.to_string(),
        username: username.to_string(),
        secret: secret.to_string(),
    })
}

/// Return the Docker `config.json` path: `$DOCKER_CONFIG/config.json` if that exists,
/// else `~/.docker/config.json` if that exists, else `""` (mirror `findDockerConfig`,
/// `docker_backend.go:356`).
fn find_docker_config() -> String {
    if let Some(dir) = std::env::var_os("DOCKER_CONFIG") {
        if !dir.is_empty() {
            let p = Path::new(&dir).join("config.json");
            if std::fs::metadata(&p).is_ok() {
                return p.to_string_lossy().into_owned();
            }
        }
    }
    let p = user_home_dir().join(".docker").join("config.json");
    if std::fs::metadata(&p).is_ok() {
        return p.to_string_lossy().into_owned();
    }
    String::new()
}

/// Resolve a credential-helper binary to an ABSOLUTE path (mirror `lookHelperPath`,
/// `docker_backend.go:306`): the inherited `PATH` first (via [`look_path`]), then each
/// extra dir with a regular-file + executable-bit (`mode & 0o111 != 0`) check.
///
/// Absolute-path resolution is required because a bare-name spawn resolves against the
/// process PATH at construction — augmenting the child's `PATH` afterward only affects
/// the tools the helper ITSELF shells out to. Missing → an error naming the searched
/// dirs. Returns a plain `String`; the caller wraps it with `HELPER_FAILED`.
fn look_helper_path(bin: &str, extra_dirs: &[String]) -> Result<PathBuf, String> {
    if let Some(p) = look_path(bin) {
        return Ok(p);
    }
    for dir in extra_dirs {
        let candidate = Path::new(dir).join(bin);
        if is_executable_file(&candidate) {
            return Ok(candidate);
        }
    }
    Err(format!(
        "{bin:?} not found on PATH or in {}",
        extra_dirs.join(", ")
    ))
}

/// The `exec.LookPath` subset [`look_helper_path`] needs: search the process `$PATH`
/// for an executable regular file named `bin`, returning the first hit's `dir/bin`.
fn look_path(bin: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    for dir in std::env::split_paths(&path) {
        if dir.as_os_str().is_empty() {
            continue;
        }
        let candidate = dir.join(bin);
        if is_executable_file(&candidate) {
            return Some(candidate);
        }
    }
    None
}

/// Whether `p` is a regular file (not a directory) with any executable bit set
/// (`mode & 0o111 != 0`) — the `lookHelperPath` `findExecutable` check. Unix-only
/// (the daemon targets macOS/Linux).
fn is_executable_file(p: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt as _;
    match std::fs::metadata(p) {
        Ok(meta) => !meta.is_dir() && meta.permissions().mode() & 0o111 != 0,
        Err(_) => false,
    }
}

/// Return `current` (a `PATH`-shaped value) with `extra_dirs` APPENDED (skipping any
/// already present), joined by the list separator (mirror `augmentPATH`,
/// `docker_backend.go:323`, for the single-`PATH`-value case). An empty `current`
/// yields just the extra dirs with NO leading separator (an empty leading element
/// means "current directory", a footgun).
///
/// **Deliberate APPEND** (not prepend — contrast the minter/ssh shim which PREpends):
/// the extra dirs are a fallback for a sparse launchd PATH, not an override. Go's
/// last-of-duplicate-`PATH=`-wins logic is an artifact of its `[]string` env; the Rust
/// `Command` env is a map, so that sub-behavior is inherently absent — the
/// multiple-`PATH=`-entry and no-`PATH=`-entry Go subtests are labelled Go-only in the
/// golden (see `augment_path_go_only_note`).
fn augment_path(current: &str, extra_dirs: &[String]) -> String {
    let mut dirs: Vec<String> = if current.is_empty() {
        Vec::new()
    } else {
        current.split(PATH_LIST_SEP).map(str::to_string).collect()
    };
    let mut seen: HashSet<String> = dirs.iter().cloned().collect();
    for d in extra_dirs {
        if seen.insert(d.clone()) {
            dirs.push(d.clone());
        }
    }
    dirs.join(PATH_LIST_SEP)
}

/// The production [`HelperExecutor`]: shells `docker-credential-<helper> get` via
/// `tokio::process`, with the raw `server_url` on stdin and the extra dirs APPENDED to
/// the child's PATH. Holds `helper_dirs` (Go stores them on the backend; the Rust seam
/// split puts them on the exec path that reads them) + an injectable `timeout`.
pub struct RealHelperExecutor {
    helper_dirs: Vec<String>,
    timeout: Duration,
}

impl RealHelperExecutor {
    fn new(helper_dirs: Vec<String>) -> Self {
        Self {
            helper_dirs,
            timeout: DEFAULT_HELPER_TIMEOUT,
        }
    }

    /// A real executor over explicit dirs + a shortened timeout, for the shell-shim
    /// `exec_helper_*` tests (Go injects `helperDirs` on the struct + shortens the
    /// 5 s constant).
    #[cfg(test)]
    fn with_dirs(helper_dirs: Vec<String>, timeout: Duration) -> Self {
        Self {
            helper_dirs,
            timeout,
        }
    }
}

#[async_trait]
impl HelperExecutor for RealHelperExecutor {
    async fn exec_helper(
        &self,
        helper_name: &str,
        server_url: &str,
    ) -> Result<DockerCredential, DockerCredError> {
        let bin = format!("docker-credential-{helper_name}");

        // Resolve to an absolute path FIRST (a not-found here is the common cause of a
        // silent "no credentials found" under a bare launchd PATH — the caller logs it
        // loudly). Classified as HELPER_FAILED with the `<bin> not found: <e>` wrap.
        let helper_path = match look_helper_path(&bin, &self.helper_dirs) {
            Ok(p) => p,
            Err(e) => {
                return Err(DockerCredError::new(
                    format!("{bin} not found: {e}"),
                    DOCKER_CODE_HELPER_FAILED,
                ));
            }
        };

        // Augment PATH so any tools the helper itself shells out to are also found
        // under a bare launchd PATH; the rest of the parent env (HOME etc.) is
        // inherited by not clearing it (Go rebuilds os.Environ() with an augmented
        // PATH — equivalent to inherit-all + override-PATH).
        let augmented = augment_path(
            &std::env::var("PATH").unwrap_or_default(),
            &self.helper_dirs,
        );

        let mut cmd = tokio::process::Command::new(&helper_path);
        cmd.arg("get");
        cmd.env("PATH", augmented);
        cmd.stdin(Stdio::piped());
        cmd.stdout(Stdio::piped());
        cmd.stderr(Stdio::piped());
        // On drop (including the timeout branch) the child is killed, so a wedged
        // helper can never hang a mint past the deadline (minter WaitDelay precedent).
        cmd.kill_on_drop(true);

        let mut child = cmd.spawn().map_err(|e| {
            DockerCredError::new(format!("{bin} failed: {e}"), DOCKER_CODE_HELPER_FAILED)
        })?;
        let mut stdin = child.stdin.take().expect("piped stdin");

        // stdin = the raw server_url (NOT JSON), then EOF; the output is tiny so a
        // sequential write-then-collect can't deadlock on a full pipe.
        let server_url_owned = server_url.to_string();
        let collect = async move {
            let _ = stdin.write_all(server_url_owned.as_bytes()).await;
            let _ = stdin.shutdown().await;
            drop(stdin);
            child.wait_with_output().await
        };

        let output = match tokio::time::timeout(self.timeout, collect).await {
            // The timeout error text is unit-owned (impl-dependent), never diffed.
            Err(_) => {
                return Err(DockerCredError::new(
                    format!("{bin} failed: timed out after {:?}", self.timeout),
                    DOCKER_CODE_HELPER_FAILED,
                ));
            }
            Ok(Ok(output)) => output,
            Ok(Err(e)) => {
                return Err(DockerCredError::new(
                    format!("{bin} failed: {e}"),
                    DOCKER_CODE_HELPER_FAILED,
                ));
            }
        };

        if !output.status.success() {
            // A located helper that exits non-zero is expected control flow (for
            // credsStore, get_credentials falls back to inline auths). stderr is
            // logged locally by the caller path but NEVER propagated to the guest; the
            // exit-status SUFFIX is impl-dependent (`exit status 1` vs `exit status:
            // 1`), unit-owned, never diffed.
            return Err(DockerCredError::new(
                format!("{bin} failed: {}", output.status),
                DOCKER_CODE_HELPER_FAILED,
            ));
        }

        let cred: HelperCredential = serde_json::from_slice(&output.stdout).map_err(|e| {
            DockerCredError::new(
                format!("parsing {bin} output: {e}"),
                DOCKER_CODE_HELPER_FAILED,
            )
        })?;

        Ok(DockerCredential {
            server_url: cred.server_url,
            username: cred.username,
            secret: cred.secret,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    // ---- test doubles -----------------------------------------------------------

    struct SilentLog;
    impl BusLog for SilentLog {
        fn info(&self, _: &str) {}
        fn warn(&self, _: &str) {}
        fn debug(&self, _: &str) {}
        fn error(&self, _: &str) {}
    }
    fn silent() -> Arc<dyn BusLog> {
        Arc::new(SilentLog)
    }

    /// A fake [`HelperExecutor`] returning a canned outcome and recording each call
    /// (mirror Go's `mockExecutor` + the `callLog` on `mockDockerBackend`).
    struct FakeExecutor {
        outcome: Result<DockerCredential, DockerCredError>,
        calls: Mutex<Vec<(String, String)>>,
    }
    impl FakeExecutor {
        fn ok(cred: DockerCredential) -> Arc<Self> {
            Arc::new(Self {
                outcome: Ok(cred),
                calls: Mutex::new(Vec::new()),
            })
        }
        fn err(e: DockerCredError) -> Arc<Self> {
            Arc::new(Self {
                outcome: Err(e),
                calls: Mutex::new(Vec::new()),
            })
        }
    }
    #[async_trait]
    impl HelperExecutor for FakeExecutor {
        async fn exec_helper(
            &self,
            helper_name: &str,
            server_url: &str,
        ) -> Result<DockerCredential, DockerCredError> {
            self.calls
                .lock()
                .unwrap()
                .push((helper_name.to_string(), server_url.to_string()));
            self.outcome.clone()
        }
    }

    /// An executor that panics if called — proves a path never reaches the exec seam
    /// (inline-auth / not-found / not-allowed).
    struct PanicExecutor;
    #[async_trait]
    impl HelperExecutor for PanicExecutor {
        async fn exec_helper(
            &self,
            _helper_name: &str,
            _server_url: &str,
        ) -> Result<DockerCredential, DockerCredError> {
            panic!("exec_helper must not be called");
        }
    }
    fn panic_exec() -> Arc<dyn HelperExecutor> {
        Arc::new(PanicExecutor)
    }

    fn cred(user: &str, secret: &str) -> DockerCredential {
        DockerCredential {
            server_url: "registry.example.com".to_string(),
            username: user.to_string(),
            secret: secret.to_string(),
        }
    }

    fn b64(plain: &str) -> String {
        base64::engine::general_purpose::STANDARD.encode(plain.as_bytes())
    }

    /// Write `config.json` into a fresh temp dir and build a backend over it with the
    /// given cfg + executor (mirror the Go tests' direct `dockerHelperBackend{...}`
    /// struct literals). Returns the guard TempDir (kept alive) + the backend.
    fn backend_with(
        config_json: &str,
        cfg: DockerConfig,
        executor: Arc<dyn HelperExecutor>,
    ) -> (tempfile::TempDir, DockerHelperBackend) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.json");
        std::fs::write(&path, config_json).unwrap();
        let b = DockerHelperBackend {
            config_path: Some(path.to_string_lossy().into_owned()),
            cfg,
            executor,
            log: silent(),
        };
        (dir, b)
    }

    fn cfg_registries(regs: &[&str]) -> DockerConfig {
        DockerConfig {
            registries: regs.iter().map(|s| s.to_string()).collect(),
            ..Default::default()
        }
    }

    fn cfg_allow_all() -> DockerConfig {
        DockerConfig {
            allow_all: true,
            ..Default::default()
        }
    }

    fn block_on<F: std::future::Future>(f: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(f)
    }

    /// `exec_helper` with a bounded ETXTBSY retry — a concurrent test's fork can
    /// transiently inherit a freshly-written helper's write fd across our exec
    /// (the fd table is process-wide; the same fork/exec race as
    /// `bootstrap::tests::run_shim`). The spawn errno is stringified into the
    /// HELPER_FAILED message, so match the locale-stable `"os error 26"` suffix.
    /// Non-ETXTBSY results return immediately — which also stops a transient
    /// ETXTBSY from masquerading as the *intended* HELPER_FAILED in the
    /// error-asserting tests below.
    fn exec_retrying(
        exec: &RealHelperExecutor,
        helper: &str,
        url: &str,
    ) -> Result<DockerCredential, DockerCredError> {
        let mut delay = Duration::from_millis(10);
        for _ in 0..10 {
            match block_on(exec.exec_helper(helper, url)) {
                Err(e) if e.msg.contains("os error 26") => {
                    std::thread::sleep(delay);
                    delay = (delay * 2).min(Duration::from_millis(160));
                }
                r => return r,
            }
        }
        // Exhaustion must fail LOUDLY: a persistent ETXTBSY is not the transient
        // fork/exec race, and its error also carries HELPER_FAILED — returning it
        // would falsely satisfy the error-asserting tests below without ever
        // exercising the behavior they exist to check.
        panic!("persistent ETXTBSY exec'ing helper {helper:?} — not the transient fork/exec race");
    }

    // ---- normalize_registry (TestNormalizeRegistry) -----------------------------

    #[test]
    fn normalize_registry_matrix() {
        let cases = [
            ("us-docker.pkg.dev", "us-docker.pkg.dev"),
            ("https://us-docker.pkg.dev", "us-docker.pkg.dev"),
            ("https://index.docker.io/v1/", "index.docker.io"),
            ("https://index.docker.io/v1", "index.docker.io"),
            ("http://localhost:5000", "localhost:5000"),
            ("ghcr.io", "ghcr.io"),
            ("ghcr.io/", "ghcr.io"),
            // One-occurrence strip: only ONE trailing slash removed.
            ("ghcr.io//", "ghcr.io/"),
            // /v2 strip + strip-order (trailing `/` before `/v2`).
            ("https://reg.io/v2", "reg.io"),
            ("reg.io/v2/", "reg.io"),
        ];
        for (input, want) in cases {
            assert_eq!(normalize_registry(input), want, "normalize {input:?}");
        }
    }

    // ---- decode_inline_auth (TestDecodeInlineAuth*) -----------------------------

    #[test]
    fn decode_inline_auth_basic() {
        let c = decode_inline_auth("registry.example.com", &b64("myuser:mypass")).unwrap();
        assert_eq!(c.username, "myuser");
        assert_eq!(c.secret, "mypass");
        assert_eq!(c.server_url, "registry.example.com");
    }

    #[test]
    fn decode_inline_auth_colon_in_password() {
        let c = decode_inline_auth("registry.example.com", &b64("user:pass:word:extra")).unwrap();
        assert_eq!(c.username, "user");
        assert_eq!(c.secret, "pass:word:extra");
    }

    #[test]
    fn decode_inline_auth_no_colon_errors() {
        let e = decode_inline_auth("registry.example.com", &b64("nocolon")).unwrap_err();
        assert_eq!(e.msg, "invalid auth format for registry.example.com");
        assert!(e.code.is_empty(), "no-colon is a plain (codeless) error");
    }

    // ---- get_credentials --------------------------------------------------------

    #[test]
    fn get_allowlist_denies_blocked_even_with_cred() {
        // blocked.io has a perfectly good local credential — the allowlist must still
        // refuse it (deny checked BEFORE any lookup). allowed.io succeeds.
        let json = format!(
            r#"{{"auths":{{"allowed.io":{{"auth":"{}"}},"blocked.io":{{"auth":"{}"}}}}}}"#,
            b64("user:pass"),
            b64("user:secret")
        );
        let (_d, b) = backend_with(&json, cfg_registries(&["allowed.io"]), panic_exec());

        let c = block_on(b.get_credentials("srv", "shed", "allowed.io")).unwrap();
        assert_eq!(c.username, "user");

        let e = block_on(b.get_credentials("srv", "shed", "blocked.io")).unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_NOT_ALLOWED);
    }

    #[test]
    fn get_allow_all_bypasses() {
        let json = format!(
            r#"{{"auths":{{"any-registry.io":{{"auth":"{}"}}}}}}"#,
            b64("user:pass")
        );
        let (_d, b) = backend_with(&json, cfg_allow_all(), panic_exec());
        let c = block_on(b.get_credentials("srv", "shed", "any-registry.io")).unwrap();
        assert_eq!(c.username, "user");
    }

    #[test]
    fn get_via_cred_helper() {
        let json = r#"{"credHelpers":{"us-docker.pkg.dev":"gcloud"}}"#;
        let exec = FakeExecutor::ok(DockerCredential {
            server_url: "us-docker.pkg.dev".into(),
            username: "_json_key".into(),
            secret: "gcloud-token".into(),
        });
        let (_d, b) = backend_with(json, cfg_registries(&["us-docker.pkg.dev"]), exec.clone());
        let c = block_on(b.get_credentials("srv", "shed", "us-docker.pkg.dev")).unwrap();
        assert_eq!(c.username, "_json_key");
        assert_eq!(c.secret, "gcloud-token");
        // The helper was exec'd with the registry's helper name + raw server_url.
        assert_eq!(
            exec.calls.lock().unwrap().as_slice(),
            &[("gcloud".to_string(), "us-docker.pkg.dev".to_string())]
        );
    }

    #[test]
    fn get_via_creds_store() {
        let json = r#"{"credsStore":"osxkeychain"}"#;
        let exec = FakeExecutor::ok(cred("kc-user", "kc-secret"));
        let (_d, b) = backend_with(json, cfg_allow_all(), exec);
        let c = block_on(b.get_credentials("srv", "shed", "registry.example.com")).unwrap();
        assert_eq!(c.username, "kc-user");
    }

    #[test]
    fn get_cred_helper_wins_over_store_and_auths() {
        // credHelpers must win over credsStore and inline auths.
        let json = format!(
            r#"{{"credHelpers":{{"registry.example.com":"custom-helper"}},"credsStore":"osxkeychain","auths":{{"registry.example.com":{{"auth":"{}"}}}}}}"#,
            b64("inline:creds")
        );
        let exec = FakeExecutor::ok(cred("helper-user", "helper-secret"));
        let (_d, b) = backend_with(&json, cfg_allow_all(), exec);
        let c = block_on(b.get_credentials("srv", "shed", "registry.example.com")).unwrap();
        assert_eq!(c.username, "helper-user");
    }

    #[test]
    fn get_creds_store_failure_falls_through_to_auths() {
        // A credsStore helper FAILURE is swallowed → falls through to the inline auth.
        let json = format!(
            r#"{{"credsStore":"osxkeychain","auths":{{"registry.example.com":{{"auth":"{}"}}}}}}"#,
            b64("inline-user:inline-secret")
        );
        let exec = FakeExecutor::err(DockerCredError::new("boom", DOCKER_CODE_HELPER_FAILED));
        let (_d, b) = backend_with(&json, cfg_allow_all(), exec);
        let c = block_on(b.get_credentials("srv", "shed", "registry.example.com")).unwrap();
        assert_eq!(c.username, "inline-user");
        assert_eq!(c.secret, "inline-secret");
    }

    #[test]
    fn get_cred_helper_failure_propagates() {
        // A per-registry credHelper FAILURE PROPAGATES (no fallback to the present
        // inline auth) — the asymmetry vs credsStore.
        let json = format!(
            r#"{{"credHelpers":{{"registry.example.com":"custom-helper"}},"auths":{{"registry.example.com":{{"auth":"{}"}}}}}}"#,
            b64("inline-user:inline-secret")
        );
        let exec = FakeExecutor::err(DockerCredError::new(
            "helper boom",
            DOCKER_CODE_HELPER_FAILED,
        ));
        let (_d, b) = backend_with(&json, cfg_allow_all(), exec);
        let e = block_on(b.get_credentials("srv", "shed", "registry.example.com")).unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_HELPER_FAILED);
        assert_eq!(e.msg, "helper boom");
    }

    #[test]
    fn get_not_found_code() {
        let (_d, b) = backend_with("{}", cfg_allow_all(), panic_exec());
        let e = block_on(b.get_credentials("srv", "shed", "unknown.io")).unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_NOT_FOUND);
    }

    #[test]
    fn get_missing_config_surfaces_not_found() {
        // Misnamed in Go: a missing config FILE → empty config (NO read error) → the
        // allow_all path finds nothing → CREDENTIALS_NOT_FOUND (not a read error).
        let b = DockerHelperBackend {
            config_path: Some("/nonexistent/config.json".to_string()),
            cfg: cfg_allow_all(),
            executor: panic_exec(),
            log: silent(),
        };
        let e = block_on(b.get_credentials("srv", "shed", "any.io")).unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_NOT_FOUND);
    }

    // ---- list_credentials (TestListCredentials) ---------------------------------

    #[test]
    fn list_respects_allowlist() {
        let json = format!(
            r#"{{"credHelpers":{{"gcr.io":"gcloud","blocked.io":"helper"}},"auths":{{"ghcr.io":{{"auth":"{}"}}}}}}"#,
            b64("user:token")
        );
        let (_d, b) = backend_with(&json, cfg_registries(&["gcr.io", "ghcr.io"]), panic_exec());
        let result = block_on(b.list_credentials("srv", "shed")).unwrap();
        assert!(result.contains_key("gcr.io"));
        assert!(result.contains_key("ghcr.io"));
        assert_eq!(result.get("ghcr.io").map(String::as_str), Some("user")); // decoded username
        assert!(!result.contains_key("blocked.io"));
    }

    // ---- find_docker_config + constructor ---------------------------------------

    #[test]
    fn find_docker_config_env_var() {
        let _g = crate::env_lock();
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.json");
        std::fs::write(&path, "{}").unwrap();
        std::env::set_var("DOCKER_CONFIG", dir.path());
        let got = find_docker_config();
        std::env::remove_var("DOCKER_CONFIG");
        assert_eq!(got, path.to_string_lossy());
    }

    #[test]
    fn docker_config_env_missing_falls_back_to_home() {
        // DOCKER_CONFIG unset → $HOME/.docker/config.json (when it exists there).
        let _g = crate::env_lock();
        std::env::remove_var("DOCKER_CONFIG");
        let home = tempfile::tempdir().unwrap();
        let docker_dir = home.path().join(".docker");
        std::fs::create_dir_all(&docker_dir).unwrap();
        let path = docker_dir.join("config.json");
        std::fs::write(&path, "{}").unwrap();
        std::env::set_var("HOME", home.path());
        let got = find_docker_config();
        std::env::remove_var("HOME");
        assert_eq!(got, path.to_string_lossy());
    }

    #[test]
    fn find_docker_config_missing_returns_empty() {
        // Neither DOCKER_CONFIG nor ~/.docker/config.json present → "".
        let _g = crate::env_lock();
        std::env::remove_var("DOCKER_CONFIG");
        let home = tempfile::tempdir().unwrap(); // empty home, no .docker
        std::env::set_var("HOME", home.path());
        let got = find_docker_config();
        std::env::remove_var("HOME");
        assert_eq!(got, "");
    }

    #[test]
    fn new_docker_backend_absent_config_constructs() {
        // An absent docker: block + a missing default file → a LIVE backend, no error
        // (the unconfigured-non-nil crux). It then denies every registry.
        let _g = crate::env_lock();
        std::env::remove_var("DOCKER_CONFIG");
        let home = tempfile::tempdir().unwrap(); // no ~/.docker/config.json
        std::env::set_var("HOME", home.path());
        let b = new_docker_backend(DockerConfig::default(), silent());
        std::env::remove_var("HOME");
        let b = b.expect("unconfigured backend still constructs");
        // Denies every registry (empty allowlist, allow_all false).
        let e = block_on(b.get_credentials("srv", "shed", "any.io")).unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_NOT_ALLOWED);
    }

    #[test]
    fn explicit_missing_config_path_errors() {
        // An EXPLICIT config_path that can't be stat'd → construction ERRORS.
        let cfg = DockerConfig {
            config_path: "/definitely/not/here/config.json".to_string(),
            ..Default::default()
        };
        assert!(new_docker_backend(cfg, silent()).is_err());
    }

    #[test]
    fn config_path_tilde_expands() {
        // A `~/`-prefixed explicit path expands to $HOME; when the file exists there,
        // construction succeeds and reads it.
        let _g = crate::env_lock();
        let home = tempfile::tempdir().unwrap();
        let path = home.path().join("mydocker.json");
        std::fs::write(&path, "{}").unwrap();
        std::env::set_var("HOME", home.path());
        let cfg = DockerConfig {
            config_path: "~/mydocker.json".to_string(),
            ..Default::default()
        };
        let got = new_docker_backend(cfg, silent());
        std::env::remove_var("HOME");
        let b = got.expect("tilde path resolves to an existing file");
        assert_eq!(
            b.config_path.as_deref(),
            Some(path.to_string_lossy().as_ref())
        );
    }

    // ---- status + read_config + error -------------------------------------------

    #[test]
    fn status_count_and_allow_all() {
        let b = DockerHelperBackend {
            config_path: None,
            cfg: cfg_registries(&["a.io", "b.io"]),
            executor: panic_exec(),
            log: silent(),
        };
        let (allow_all, count) = b.status("srv", "shed");
        assert!(!allow_all);
        assert_eq!(count, 2);
    }

    #[test]
    fn read_config_empty_when_no_path() {
        let b = DockerHelperBackend {
            config_path: None,
            cfg: DockerConfig::default(),
            executor: panic_exec(),
            log: silent(),
        };
        let cfg = b.read_config().unwrap();
        assert!(cfg.cred_helpers.is_empty());
        assert!(cfg.creds_store.is_empty());
        assert!(cfg.auths.is_empty());
    }

    #[test]
    fn read_config_missing_file_is_empty_no_error() {
        // config_path SET but the file is NotFound → empty config, no error (distinct
        // from the empty-path branch above).
        let b = DockerHelperBackend {
            config_path: Some("/nonexistent/config.json".to_string()),
            cfg: DockerConfig::default(),
            executor: panic_exec(),
            log: silent(),
        };
        let cfg = b.read_config().unwrap();
        assert!(cfg.cred_helpers.is_empty());
        assert!(cfg.auths.is_empty());
    }

    #[test]
    fn docker_cred_error_display() {
        let e = DockerCredError::new("test error", "TEST_CODE");
        assert_eq!(e.to_string(), "test error");
        assert_eq!(e.code, "TEST_CODE");
    }

    // ---- look_helper_path (TestLookHelperPath) ----------------------------------

    fn write_exec(dir: &Path, name: &str, body: &str) -> PathBuf {
        use std::os::unix::fs::PermissionsExt as _;
        let p = dir.join(name);
        std::fs::write(&p, body).unwrap();
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
        p
    }

    #[test]
    fn look_helper_found_in_extra_dir() {
        let dir = tempfile::tempdir().unwrap();
        let bin = "docker-credential-faketest-look";
        let p = write_exec(dir.path(), bin, "#!/bin/sh\n");
        let got = look_helper_path(bin, &[dir.path().to_string_lossy().into_owned()]).unwrap();
        assert_eq!(got, p);
    }

    #[test]
    fn look_helper_missing_names_dirs() {
        let dir = tempfile::tempdir().unwrap();
        let dirs = vec![dir.path().to_string_lossy().into_owned()];
        let e = look_helper_path("docker-credential-does-not-exist", &dirs).unwrap_err();
        assert!(
            e.contains(dir.path().to_string_lossy().as_ref()),
            "err {e:?} names dir"
        );
    }

    #[test]
    fn look_helper_skips_non_executable() {
        use std::os::unix::fs::PermissionsExt as _;
        let dir = tempfile::tempdir().unwrap();
        let name = "docker-credential-faketest-plain";
        let p = dir.path().join(name);
        std::fs::write(&p, "x").unwrap();
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(look_helper_path(name, &[dir.path().to_string_lossy().into_owned()]).is_err());
    }

    #[test]
    fn look_helper_skips_directory() {
        let dir = tempfile::tempdir().unwrap();
        let name = "docker-credential-faketest-dir";
        std::fs::create_dir(dir.path().join(name)).unwrap();
        assert!(look_helper_path(name, &[dir.path().to_string_lossy().into_owned()]).is_err());
    }

    // ---- augment_path (TestAugmentPATH) -----------------------------------------

    #[test]
    fn augment_path_appends_missing() {
        let got = augment_path(
            "/usr/bin:/bin",
            &["/usr/local/bin".into(), "/opt/homebrew/bin".into()],
        );
        assert_eq!(got, "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin");
    }

    #[test]
    fn augment_path_no_duplicate() {
        // A dir already present is not duplicated (append-missing semantics).
        let got = augment_path("/usr/local/bin:/usr/bin", &["/usr/local/bin".into()]);
        assert_eq!(got, "/usr/local/bin:/usr/bin");
    }

    #[test]
    fn augment_path_empty_yields_only_extras() {
        // An empty PATH value yields JUST the extra dirs — no leading separator (an
        // empty leading element means "current directory", a footgun).
        let got = augment_path("", &["/opt/homebrew/bin".into()]);
        assert_eq!(got, "/opt/homebrew/bin");
    }

    #[test]
    fn augment_path_go_only_note() {
        // Go's TestAugmentPATH additionally covers a NO-`PATH=`-entry env and a
        // multiple-`PATH=`-entry env (last-wins augmented). Both are artifacts of Go's
        // `[]string` env; the Rust `Command` env is a MAP keyed by name, so there is no
        // "no PATH entry" (the caller passes "") and no "duplicate PATH entry" — those
        // subtests have no Rust runtime equivalent and are labelled go_only in
        // docker_path_augment.json. This asserts the single-value augmentation the Rust
        // seam DOES have: a value already containing the extra keeps a single copy.
        let got = augment_path("/a:/opt/homebrew/bin:/b", &["/opt/homebrew/bin".into()]);
        assert_eq!(got, "/a:/opt/homebrew/bin:/b");
    }

    // ---- exec_helper (real subprocess; skip if no /bin/sh) ----------------------

    fn have_sh() -> bool {
        Path::new("/bin/sh").exists()
    }

    #[test]
    fn exec_helper_resolves_via_extra_dir() {
        if !have_sh() {
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        // Fake helper: drains stdin (the server_url) and emits a credential JSON.
        let script = "#!/bin/sh\ncat >/dev/null\n\
            printf '%s' '{\"ServerURL\":\"registry.example.com\",\"Username\":\"fake-user\",\"Secret\":\"fake-secret\"}'\n";
        write_exec(dir.path(), "docker-credential-faketest", script);
        // dir is NOT on PATH, so this exercises the extra-dir fallback.
        let exec = RealHelperExecutor::with_dirs(
            vec![dir.path().to_string_lossy().into_owned()],
            Duration::from_secs(5),
        );
        let c = exec_retrying(&exec, "faketest", "registry.example.com").unwrap();
        assert_eq!(c.username, "fake-user");
        assert_eq!(c.secret, "fake-secret");
        assert_eq!(c.server_url, "registry.example.com");
    }

    #[test]
    fn exec_helper_missing_binary_helper_failed() {
        let dir = tempfile::tempdir().unwrap(); // empty dir, binary nowhere
        let exec = RealHelperExecutor::with_dirs(
            vec![dir.path().to_string_lossy().into_owned()],
            Duration::from_secs(5),
        );
        let e =
            block_on(exec.exec_helper("definitely-missing", "registry.example.com")).unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_HELPER_FAILED);
    }

    #[test]
    fn exec_helper_nonzero_exit_and_bad_json_helper_failed() {
        if !have_sh() {
            return;
        }
        // Non-zero exit → HELPER_FAILED.
        let dir = tempfile::tempdir().unwrap();
        write_exec(
            dir.path(),
            "docker-credential-failexit",
            "#!/bin/sh\ncat >/dev/null\nexit 1\n",
        );
        let exec = RealHelperExecutor::with_dirs(
            vec![dir.path().to_string_lossy().into_owned()],
            Duration::from_secs(5),
        );
        let e = exec_retrying(&exec, "failexit", "registry.example.com").unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_HELPER_FAILED);

        // Exit 0 but non-JSON stdout → HELPER_FAILED (parse error).
        let dir2 = tempfile::tempdir().unwrap();
        write_exec(
            dir2.path(),
            "docker-credential-badjson",
            "#!/bin/sh\ncat >/dev/null\nprintf 'not json'\n",
        );
        let exec2 = RealHelperExecutor::with_dirs(
            vec![dir2.path().to_string_lossy().into_owned()],
            Duration::from_secs(5),
        );
        let e2 = exec_retrying(&exec2, "badjson", "registry.example.com").unwrap_err();
        assert_eq!(e2.code, DOCKER_CODE_HELPER_FAILED);
        assert!(
            e2.msg
                .starts_with("parsing docker-credential-badjson output: "),
            "got {e2:?}"
        );
    }

    #[test]
    fn exec_helper_times_out_promptly() {
        if !have_sh() {
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        write_exec(
            dir.path(),
            "docker-credential-sleeper",
            "#!/bin/sh\nsleep 30\n",
        );
        let exec = RealHelperExecutor::with_dirs(
            vec![dir.path().to_string_lossy().into_owned()],
            Duration::from_millis(300),
        );
        let start = std::time::Instant::now();
        let e = exec_retrying(&exec, "sleeper", "registry.example.com").unwrap_err();
        assert_eq!(e.code, DOCKER_CODE_HELPER_FAILED);
        assert!(
            start.elapsed() < Duration::from_secs(5),
            "did not abort promptly"
        );
    }

    // ---- lookup_config_map (3-way) ----------------------------------------------

    #[test]
    fn lookup_config_map_3way() {
        let mut m: BTreeMap<String, String> = BTreeMap::new();
        m.insert(
            "https://index.docker.io/v1/".to_string(),
            "helperA".to_string(),
        );
        m.insert("ghcr.io".to_string(), "helperB".to_string());

        let lookup = |raw: &str| -> Option<String> {
            let n = normalize_registry(raw);
            lookup_config_map(&m, raw, &n).cloned()
        };

        // Exact raw match.
        assert_eq!(lookup("ghcr.io"), Some("helperB".to_string()));
        // Scan-all-keys: guest sends the normalized host, key is the full docker form.
        assert_eq!(lookup("index.docker.io"), Some("helperA".to_string()));
        // Normalized-key path: guest sends the full docker form, key is normalized.
        let mut m2: BTreeMap<String, String> = BTreeMap::new();
        m2.insert("index.docker.io".to_string(), "helperC".to_string());
        let n = normalize_registry("https://index.docker.io/v1/");
        assert_eq!(
            lookup_config_map(&m2, "https://index.docker.io/v1/", &n).cloned(),
            Some("helperC".to_string())
        );
        // Miss.
        assert_eq!(lookup("nope.io"), None);
    }

    // ---- helper struct serde roundtrip (tag-drift guard) ------------------------

    #[test]
    fn docker_helper_struct_roundtrip_serverurl() {
        // The Capitalized docker-credential-helper protocol tags must survive a
        // deserialize independently of the guest wire's snake_case struct. A struct
        // emitting `ServerUrl` (the rename_all="PascalCase" bug) would drop this.
        let json = r#"{"ServerURL":"reg.io","Username":"u","Secret":"s"}"#;
        let c: HelperCredential = serde_json::from_str(json).unwrap();
        assert_eq!(c.server_url, "reg.io");
        assert_eq!(c.username, "u");
        assert_eq!(c.secret, "s");
        // The snake_case guest tags must NOT populate this struct.
        let wrong = r#"{"server_url":"reg.io","username":"u","secret":"s"}"#;
        let c2: HelperCredential = serde_json::from_str(wrong).unwrap();
        assert!(
            c2.server_url.is_empty(),
            "snake_case must not fill ServerURL"
        );
    }

    // ---- golden runners (Rust half) ---------------------------------------------

    fn fixture(name: &str) -> serde_json::Value {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures")
            .join(name);
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        serde_json::from_str(&raw).unwrap()
    }

    #[test]
    fn golden_docker_normalize() {
        let fx = fixture("docker_normalize.json");
        assert_eq!(fx["protocol_version"], 1, "version skew");
        for v in fx["normalize_vectors"].as_array().unwrap() {
            let got = normalize_registry(v["input"].as_str().unwrap());
            assert_eq!(got, v["expected"].as_str().unwrap(), "normalize {v}");
        }
        for v in fx["lookup_vectors"].as_array().unwrap() {
            let mut m: BTreeMap<String, String> = BTreeMap::new();
            for (k, val) in v["map"].as_object().unwrap() {
                m.insert(k.clone(), val.as_str().unwrap().to_string());
            }
            let raw = v["raw"].as_str().unwrap();
            let n = normalize_registry(raw);
            let got = lookup_config_map(&m, raw, &n).cloned();
            let want = v["expected"].as_str().map(str::to_string);
            assert_eq!(got, want, "lookup {}", v["name"]);
        }
    }

    #[test]
    fn golden_docker_inline_auth() {
        let fx = fixture("docker_inline_auth.json");
        assert_eq!(fx["protocol_version"], 1, "version skew");
        for v in fx["valid_vectors"].as_array().unwrap() {
            let url = v["server_url"].as_str().unwrap();
            let encoded = b64(v["plain"].as_str().unwrap());
            let c = decode_inline_auth(url, &encoded).unwrap();
            assert_eq!(
                c.username,
                v["username"].as_str().unwrap(),
                "user {}",
                v["name"]
            );
            assert_eq!(
                c.secret,
                v["secret"].as_str().unwrap(),
                "secret {}",
                v["name"]
            );
            assert_eq!(c.server_url, url);
        }
        for v in fx["invalid_vectors"].as_array().unwrap() {
            let url = v["server_url"].as_str().unwrap();
            let encoded = b64(v["plain"].as_str().unwrap());
            let e = decode_inline_auth(url, &encoded).unwrap_err();
            assert_eq!(
                e.msg,
                v["expected_error"].as_str().unwrap(),
                "err {}",
                v["name"]
            );
        }
    }

    #[test]
    fn golden_docker_path_augment() {
        let fx = fixture("docker_path_augment.json");
        assert_eq!(fx["protocol_version"], 1, "version skew");
        for v in fx["vectors"].as_array().unwrap() {
            // Go-only vectors (env-array-shaped: no-PATH-entry / duplicate-PATH) have no
            // Rust runtime equivalent — the Rust Command env is a map. Skip them.
            if v["go_only"].as_bool().unwrap_or(false) {
                continue;
            }
            let extras: Vec<String> = v["extra_dirs"]
                .as_array()
                .unwrap()
                .iter()
                .map(|d| d.as_str().unwrap().to_string())
                .collect();
            let got = augment_path(v["path"].as_str().unwrap(), &extras);
            assert_eq!(
                got,
                v["expected_path"].as_str().unwrap(),
                "augment {}",
                v["name"]
            );
        }
    }
}
