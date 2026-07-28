//! UniFFI bridge over `shed-core` → Swift. Kept thin so Phase 3's GTK app can
//! link `shed-core` directly without paying for UniFFI.
//!
//! The wire DTOs live in the pure `shed-core` crate; the UniFFI records here
//! mirror them (with `From` conversions), so `shed-core` needs no uniffi
//! dependency. The Swift adapter maps these records to the app's Swift `Models`;
//! the M2 golden-JSON parity gate guards the two representations against drift.

use std::sync::Arc;

use shed_core::http::{Client, ShedError as CoreError};
use shed_core::models;
use shed_core::token as core_token;

uniffi::setup_scaffolding!();

// ---- M0 FFI canary (async method + foreign async callback + cancellation) ----

/// M0 canary: an async export routed through the shared tokio runtime.
#[uniffi::export(async_runtime = "tokio")]
pub async fn ping(echo: String) -> String {
    shed_core::ping(echo).await
}

/// A Swift→Rust async callback, mirroring the shape of the real TokenMinter
/// (Rust owns the token FSM in M3; the host-agent mint stays foreign).
#[uniffi::export(with_foreign)]
#[async_trait::async_trait]
pub trait MinterProbe: Send + Sync {
    async fn mint(&self, server: String) -> String;
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn mint_via(minter: Arc<dyn MinterProbe>, server: String) -> String {
    minter.mint(server).await
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn slow_echo(echo: String, delay_ms: u64) -> String {
    tokio::time::sleep(std::time::Duration::from_millis(delay_ms)).await;
    format!("slow: {echo}")
}

// ---- ShedCore: the shed-server read client exposed to Swift (M2) ----

/// FFI error mirroring `shed-core`'s `ShedError` (and Swift's `ShedClientError`).
#[derive(Debug, thiserror::Error, uniffi::Error)]
pub enum ShedError {
    #[error("shed-server returned HTTP {status}")]
    BadStatus { status: u16 },
    #[error("transport error: {message}")]
    Transport { message: String },
    #[error("decode error: {message}")]
    Decode { message: String },
    #[error("create failed: {message}")]
    Create { message: String },
    #[error("{message}")]
    Config { message: String },
}

impl From<CoreError> for ShedError {
    fn from(e: CoreError) -> Self {
        match e {
            CoreError::BadStatus(status) => ShedError::BadStatus { status },
            CoreError::Transport(message) => ShedError::Transport { message },
            CoreError::Decode(message) => ShedError::Decode { message },
            CoreError::Create(message) => ShedError::Create { message },
            CoreError::Config(message) => ShedError::Config { message },
        }
    }
}

#[derive(uniffi::Enum, Clone)]
pub enum ShedStatus {
    Running,
    Stopped,
    Starting,
    Error,
    Unknown,
}

impl From<models::ShedStatus> for ShedStatus {
    fn from(s: models::ShedStatus) -> Self {
        match s {
            models::ShedStatus::Running => ShedStatus::Running,
            models::ShedStatus::Stopped => ShedStatus::Stopped,
            models::ShedStatus::Starting => ShedStatus::Starting,
            models::ShedStatus::Error => ShedStatus::Error,
            models::ShedStatus::Unknown => ShedStatus::Unknown,
        }
    }
}

#[derive(uniffi::Record)]
pub struct ServerInfo {
    pub name: String,
    pub version: String,
    pub backend: Option<String>,
    pub ssh_port: Option<i64>,
    pub http_port: Option<i64>,
}

impl From<models::ServerInfo> for ServerInfo {
    fn from(v: models::ServerInfo) -> Self {
        Self {
            name: v.name,
            version: v.version,
            backend: v.backend,
            ssh_port: v.ssh_port,
            http_port: v.http_port,
        }
    }
}

#[derive(uniffi::Record, Clone)]
pub struct Shed {
    pub host: String,
    pub name: String,
    pub status: ShedStatus,
    pub backend: Option<String>,
    pub repo: Option<String>,
    pub image: Option<String>,
    pub image_digest: Option<String>,
    pub local_dir: Option<String>,
    pub ip_address: Option<String>,
    pub cpus: Option<i64>,
    pub memory_mb: Option<i64>,
    pub created_at: Option<String>,
    pub started_at: Option<String>,
    pub active_namespaces: Vec<String>,
}

impl From<models::Shed> for Shed {
    fn from(v: models::Shed) -> Self {
        Self {
            host: v.host,
            name: v.name,
            status: v.status.into(),
            backend: v.backend,
            repo: v.repo,
            image: v.image,
            image_digest: v.image_digest,
            local_dir: v.local_dir,
            ip_address: v.ip_address,
            cpus: v.cpus,
            memory_mb: v.memory_mb,
            created_at: v.created_at,
            started_at: v.started_at,
            active_namespaces: v.active_namespaces,
        }
    }
}

#[derive(uniffi::Record)]
pub struct ShedImage {
    pub name: String,
    pub docker_ref: Option<String>,
    pub alias: Option<String>,
    pub is_default: bool,
    pub cached: bool,
    pub in_use: bool,
    pub digest: Option<String>,
    pub source: Option<String>,
    pub size_bytes: i64,
}

impl From<models::ShedImage> for ShedImage {
    fn from(v: models::ShedImage) -> Self {
        Self {
            name: v.name,
            docker_ref: v.docker_ref,
            alias: v.alias,
            is_default: v.is_default,
            cached: v.cached,
            in_use: v.in_use,
            digest: v.digest,
            source: v.source,
            size_bytes: v.size_bytes,
        }
    }
}

#[derive(uniffi::Record)]
pub struct DiskSize {
    pub logical_bytes: i64,
    pub physical_bytes: i64,
}

impl From<models::DiskSize> for DiskSize {
    fn from(v: models::DiskSize) -> Self {
        Self {
            logical_bytes: v.logical_bytes,
            physical_bytes: v.physical_bytes,
        }
    }
}

#[derive(uniffi::Record)]
pub struct DiskEntry {
    pub name: String,
    pub docker_ref: Option<String>,
    pub size: DiskSize,
}

impl From<models::DiskEntry> for DiskEntry {
    fn from(v: models::DiskEntry) -> Self {
        Self {
            name: v.name,
            docker_ref: v.docker_ref,
            size: v.size.into(),
        }
    }
}

#[derive(uniffi::Record)]
pub struct DiskTotals {
    pub images: DiskSize,
    pub sheds: DiskSize,
    pub snapshots: DiskSize,
    pub orphans: DiskSize,
    pub all: DiskSize,
}

impl From<models::DiskTotals> for DiskTotals {
    fn from(v: models::DiskTotals) -> Self {
        Self {
            images: v.images.into(),
            sheds: v.sheds.into(),
            snapshots: v.snapshots.into(),
            orphans: v.orphans.into(),
            all: v.all.into(),
        }
    }
}

#[derive(uniffi::Record)]
pub struct SystemDiskUsage {
    pub server_name: Option<String>,
    pub backend: Option<String>,
    pub images: Vec<DiskEntry>,
    pub sheds: Vec<DiskEntry>,
    pub orphans: Vec<DiskEntry>,
    pub totals: DiskTotals,
}

impl From<models::SystemDiskUsage> for SystemDiskUsage {
    fn from(v: models::SystemDiskUsage) -> Self {
        Self {
            server_name: v.server_name,
            backend: v.backend,
            images: v.images.into_iter().map(Into::into).collect(),
            sheds: v.sheds.into_iter().map(Into::into).collect(),
            orphans: v.orphans.into_iter().map(Into::into).collect(),
            totals: v.totals.into(),
        }
    }
}

#[derive(uniffi::Record)]
pub struct EgressProfile {
    pub mode: Option<String>,
    pub allow: Option<Vec<String>>,
    pub deny: Option<Vec<String>>,
    pub rule: Option<String>,
}

impl From<models::EgressProfile> for EgressProfile {
    fn from(v: models::EgressProfile) -> Self {
        Self {
            mode: v.mode,
            allow: v.allow,
            deny: v.deny,
            rule: v.rule,
        }
    }
}

#[derive(uniffi::Record)]
pub struct EgressProfileInfo {
    pub name: String,
    pub source: String,
    pub profile: EgressProfile,
}

impl From<models::EgressProfileInfo> for EgressProfileInfo {
    fn from(v: models::EgressProfileInfo) -> Self {
        Self {
            name: v.name,
            source: v.source,
            profile: v.profile.into(),
        }
    }
}

/// The credential shape a shed-server issues (mirrors `shed_core::token::AuthMode`).
/// `Token` is a bearer token; `Mtls` is a client certificate bound to a key this
/// process generated and never exports.
#[derive(uniffi::Enum, Clone, Copy, Debug, PartialEq, Eq)]
pub enum AuthMode {
    Token,
    Mtls,
}

impl From<core_token::AuthMode> for AuthMode {
    fn from(m: core_token::AuthMode) -> Self {
        match m {
            core_token::AuthMode::Token => AuthMode::Token,
            core_token::AuthMode::Mtls => AuthMode::Mtls,
        }
    }
}

/// The foreign (Swift) control-credential mint primitive. shed-core's
/// `ControlTokenProvider` FSM caches/refreshes around this; a throw is
/// fail-closed (the client then sends no credential — never a static downgrade).
///
/// Implementations relay to the host agent over its UDS. They never see, hold,
/// or return a private key: the keypair is generated INSIDE the Rust provider,
/// and only the base64 CSR (public material) is handed out (plan 002 §7 P3).
#[uniffi::export(with_foreign)]
#[async_trait::async_trait]
pub trait TokenMinter: Send + Sync {
    /// Legacy token-only mint (the host agent's `token.get`). Still the path an
    /// agent too old for `credential.get` takes.
    async fn mint(&self, server: String) -> Result<MintedToken, ShedError>;

    /// Can this minter carry a CSR to the server and a certificate back — i.e.
    /// does the agent on the other end of the socket RIGHT NOW advertise
    /// `credential.get`? A capability advertisement, not a preference: the
    /// server still chooses what to issue.
    ///
    /// Answering `false` costs nothing but mtls support; answering `true`
    /// against an agent that cannot relay a CSR makes every mint fail, so the
    /// answer must be per-connection, not per-build. Must not block.
    fn supports_mtls(&self) -> bool;

    /// Mint whatever credential the server issues. `csrBase64` is present only
    /// when `supportsMtls()` said `true` — relay it verbatim as the
    /// `csr=<base64>` argument of the bootstrap request; never regenerate it.
    ///
    /// Returns the tagged answer (`token` / `certificate` / `failed`). The
    /// implementation maps the agent's `auth_mode` STRICTLY: absent, empty,
    /// `"token"` and the legacy `"secure"` spelling are the token arm, `"mtls"`
    /// is the certificate arm, and any other value must be reported as `failed`
    /// — never downgraded to a token. Throwing is equally fail-closed; the
    /// `failed` arm exists so a refusal the agent explained in words reaches the
    /// user as those words.
    async fn mint_credential(
        &self,
        server: String,
        csr_base64: Option<String>,
    ) -> Result<MintedCredential, ShedError>;
}

/// A minted control token + optional expiry as unix seconds (Swift parses the
/// host agent's ISO-8601 expiry to epoch before returning it, keeping timestamp
/// parsing on the Swift side).
#[derive(uniffi::Record, Clone)]
pub struct MintedToken {
    pub token: String,
    pub expires_at_unix: Option<u64>,
}

/// A client certificate issued for the CSR the Rust provider submitted.
///
/// The matching private key is deliberately absent and always will be: it stays
/// inside `ControlTokenProvider`, so nothing on this boundary can present the
/// certificate without the core (plan 002 §7 P3).
#[derive(uniffi::Record, Clone)]
pub struct MintedCertificate {
    /// PEM leaf, exactly as the bundle's `client_cert` delivered it.
    pub cert_pem: String,
    /// Lower-case hex serial. Opaque to the client — logs and rotation proofs.
    pub serial: String,
    /// Unix seconds, or `nil` when the agent reported no expiry.
    pub expires_at_unix: Option<u64>,
}

/// The tagged answer to `mintCredential` — the SERVER's choice of credential
/// shape, plus an explicit failure arm.
#[derive(uniffi::Enum, Clone)]
pub enum MintedCredential {
    /// `auth_mode` token (or absent/legacy `"secure"`). An empty token is
    /// rejected by the core, never adopted.
    Token { token: MintedToken },
    /// `auth_mode: mtls`. An empty `certPem` is rejected by the core — "mtls but
    /// no certificate" is a protocol violation, not an empty success.
    Certificate { certificate: MintedCertificate },
    /// The agent (or the implementation's own validation) refused. `message` is
    /// surfaced to the caller verbatim; the core adopts nothing.
    Failed { message: String },
}

/// Adapts the foreign `TokenMinter` to shed-core's pure `TokenMinter` trait.
struct ForeignMinterBridge(Arc<dyn TokenMinter>);

#[async_trait::async_trait]
impl core_token::TokenMinter for ForeignMinterBridge {
    async fn mint(&self, server: &str) -> Result<core_token::MintedToken, CoreError> {
        match self.0.mint(server.to_string()).await {
            Ok(m) => Ok(core_token::MintedToken {
                token: m.token,
                expires_at_unix: m.expires_at_unix,
            }),
            Err(e) => Err(core_error_from_ffi(e)),
        }
    }

    fn supports_mtls(&self) -> bool {
        self.0.supports_mtls()
    }

    async fn mint_credential(
        &self,
        server: &str,
        req: &core_token::CredentialRequest,
    ) -> Result<core_token::MintedCredential, CoreError> {
        let answer = self
            .0
            .mint_credential(server.to_string(), req.csr_base64().map(str::to_string))
            .await
            .map_err(core_error_from_ffi)?;
        credential_from_answer(answer, server)
    }
}

/// The payload rules for a foreign mint answer, in one place — the FFI's
/// counterpart to shed-app's `credential_from_parts` (which this crate cannot
/// reuse: shed-core-ffi depends on shed-core only, and must not grow a
/// dependency on the app layer).
///
/// The mode itself needs no parsing here — the arm IS the mode, which is the
/// point of a tagged result — so what is left to enforce is that the arm's
/// payload can actually authenticate. An empty token or an empty certificate is
/// an error, never a usable credential (F6: the FSM must send nothing rather
/// than a valid-looking downgrade), and every populated field is within its
/// [`core_token::limits`] cap.
///
/// # What this boundary CAN and CANNOT check (read before calling it a parity site)
///
/// The tagged enum erases the wire frame, so several of the shared §7 P9
/// `credential_response.json` vectors are STRUCTURALLY UNREACHABLE here and
/// have no counterpart below — they are refused earlier, by whichever mapper
/// owns the frame (Swift's `validatedCredential(for:)`, shed-app's
/// `map_credential_response`):
///
///   * the raw `auth_mode` string — unknown/uppercase/padded modes, the legacy
///     `secure` spelling, an absent mode: the ARM is the mode, so there is no
///     string left to misinterpret;
///   * `server` mismatch — the reply is not carried across this boundary, only
///     the answer for the server the core asked about;
///   * an unparseable `expires_at` — expiry crosses as parsed unix seconds,
///     and `None` here means "the minter reported none";
///   * ambiguity (a token arm carrying certificate fields, or the reverse) —
///     the arms are exclusive by construction.
///
/// What DOES survive the erasure is field CONTENT, so that is what is enforced:
/// emptiness and the size caps. Both are checked independently of any caller,
/// because a second UniFFI minter (or a regression in Swift's pre-validation)
/// must not be able to hand the core a 4097-byte token this side would adopt.
fn credential_from_answer(
    answer: MintedCredential,
    server: &str,
) -> Result<core_token::MintedCredential, CoreError> {
    match answer {
        MintedCredential::Token { token } => {
            if token.token.is_empty() {
                return Err(CoreError::Config(format!(
                    "host agent returned no token for {server}"
                )));
            }
            if token.token.len() > core_token::limits::MAX_TOKEN_BYTES {
                return Err(oversized("token", server));
            }
            Ok(core_token::MintedCredential::Token(
                core_token::MintedToken {
                    token: token.token,
                    expires_at_unix: token.expires_at_unix,
                },
            ))
        }
        MintedCredential::Certificate { certificate } => {
            if certificate.cert_pem.is_empty() {
                return Err(CoreError::Config(format!(
                    "host agent reported mtls but returned no certificate for {server}"
                )));
            }
            if certificate.cert_pem.len() > core_token::limits::MAX_CLIENT_CERT_BYTES {
                return Err(oversized("certificate", server));
            }
            if certificate.serial.len() > core_token::limits::MAX_CERT_SERIAL_BYTES {
                return Err(oversized("certificate serial", server));
            }
            Ok(core_token::MintedCredential::Certificate(
                core_token::MintedCertificate {
                    cert_pem: certificate.cert_pem,
                    serial: certificate.serial,
                    expires_at_unix: certificate.expires_at_unix,
                },
            ))
        }
        MintedCredential::Failed { message } => Err(CoreError::Config(if message.is_empty() {
            format!("the control-credential mint for {server} failed (no reason given)")
        } else if message.len() > core_token::limits::MAX_ERROR_BYTES {
            // Same shape as shed-app's: the refusal names the oversize rather
            // than echoing (or truncating) a megabyte of attacker-chosen text
            // into every log line and error surface downstream.
            format!(
                "host agent returned an oversized error message for {server} ({} bytes); refusing",
                message.len()
            )
        } else {
            message
        })),
    }
}

/// The refusal wording for an over-cap field, byte-for-byte shed-app's, so the
/// shared §7 P9 vectors get the same sentence whichever mapper refused.
fn oversized(field: &str, server: &str) -> CoreError {
    CoreError::Config(format!(
        "host agent returned an oversized {field} for {server}; refusing"
    ))
}

/// What the provider adopted, delivered to [`CredentialObserver`].
///
/// Carries enough to drive UI state and (on clients whose store is the
/// sanctioned home for it) a persisted entry — and nothing that could
/// authenticate on its own: in `Mtls` mode there is no certificate, no serial,
/// and above all no private key (plan 002 §7 P1/P3).
#[derive(uniffi::Record, Clone)]
pub struct CredentialAdopted {
    /// The server name the provider was constructed for.
    pub server: String,
    /// The shape just adopted.
    pub mode: AuthMode,
    /// Unix seconds; `nil` when the minter reported no expiry.
    pub expires_at_unix: Option<u64>,
    /// The bearer token — set in `Token` mode ONLY, always `nil` in `Mtls`.
    /// The desktop persists NOTHING from this (`~/.shed/config.yaml` is
    /// CLI-owned); it is here for clients whose own store is the token's home.
    pub token: Option<String>,
}

impl From<&core_token::CredentialAdopted> for CredentialAdopted {
    fn from(e: &core_token::CredentialAdopted) -> Self {
        Self {
            server: e.server.clone(),
            mode: e.mode.into(),
            expires_at_unix: e.expires_at_unix,
            token: e.token.clone(),
        }
    }
}

/// Fire-and-forget notifications about the provider's credential adoptions
/// (plan 002 §7 P1). Optional: a client that misses every event pays one
/// re-learn on next launch and nothing more.
///
/// # Delivery contract (read before implementing)
///
/// Callbacks arrive on the core's own dispatcher THREAD, in adoption order,
/// with no provider lock held — so a handler cannot stall a mint, and may call
/// back into the core without deadlocking. They are therefore ASYNCHRONOUS with
/// respect to the request that triggered the mint: code that must observe an
/// event waits for it rather than reading state right after the call returns.
///
/// **Handlers must RETURN.** Blocking forever (a semaphore, a synchronous
/// network call, a main-thread hop that can't complete) parks that one
/// dispatcher thread and every LATER event behind it. Hop to your own queue and
/// return.
///
/// **Hold the core WEAKLY.** The core's dispatcher thread strongly retains this
/// observer for the provider's lifetime; an observer that strongly retains its
/// `ShedCore` back closes a cycle nothing can free (core → provider →
/// dispatcher → observer → core). In practice a per-server core lives for the
/// app's lifetime anyway, but an implementation that rebuilds cores must use a
/// weak back-reference.
#[uniffi::export(with_foreign)]
pub trait CredentialObserver: Send + Sync {
    /// A mint succeeded and the core adopted `event`'s credential. Fires on
    /// EVERY successful mint, including a rotation that changed nothing but the
    /// credential's value.
    fn credential_adopted(&self, event: CredentialAdopted);

    /// The derived transition event: the adopted shape differs from the one
    /// last announced, in either direction. Always delivered immediately after
    /// the `credentialAdopted` for the same mint, so a consumer handling both
    /// sees the adoption first. Silent for a same-shape re-mint (a rotation
    /// after a 401 is not a mode flip).
    fn mode_changed(&self, server: String, mode: AuthMode);
}

/// Adapts the foreign `CredentialObserver` to shed-core's pure trait.
struct ForeignObserverBridge(Arc<dyn CredentialObserver>);

impl core_token::CredentialObserver for ForeignObserverBridge {
    fn on_credential_adopted(&self, event: &core_token::CredentialAdopted) {
        self.0.credential_adopted(event.into());
    }

    fn on_mode_changed(&self, server: &str, mode: core_token::AuthMode) {
        self.0.mode_changed(server.to_string(), mode.into());
    }
}

fn core_error_from_ffi(e: ShedError) -> CoreError {
    match e {
        ShedError::BadStatus { status } => CoreError::BadStatus(status),
        ShedError::Transport { message } => CoreError::Transport(message),
        ShedError::Decode { message } => CoreError::Decode(message),
        ShedError::Create { message } => CoreError::Create(message),
        ShedError::Config { message } => CoreError::Config(message),
    }
}

// ---- create (SSE) — a pull-based store the Swift side polls (M4) ----

use std::sync::OnceLock;

/// The state of an in-flight create (maps to Swift's CreateState wire strings).
#[derive(uniffi::Enum, Clone, PartialEq)]
pub enum CreateState {
    Progress,
    Complete,
    Error,
}

impl From<shed_core::create::CreateState> for CreateState {
    fn from(s: shed_core::create::CreateState) -> Self {
        match s {
            shed_core::create::CreateState::Progress => CreateState::Progress,
            shed_core::create::CreateState::Complete => CreateState::Complete,
            shed_core::create::CreateState::Error => CreateState::Error,
        }
    }
}

/// A snapshot of an in-flight create, polled by Swift via `create_status`.
#[derive(uniffi::Record, Clone)]
pub struct CreateProgress {
    pub id: String,
    pub state: CreateState,
    pub messages: Vec<String>,
    pub shed: Option<Shed>,
    pub error: Option<String>,
}

impl From<shed_core::create::CreateProgress> for CreateProgress {
    fn from(p: shed_core::create::CreateProgress) -> Self {
        Self {
            id: p.id,
            state: p.state.into(),
            messages: p.messages,
            shed: p.shed.map(Into::into),
            error: p.error,
        }
    }
}

/// Body for POST /api/sheds (FFI mirror of shed_core::models::CreateShedRequest).
#[derive(uniffi::Record)]
pub struct CreateShedRequest {
    pub name: String,
    pub repo: Option<String>,
    pub local_dir: Option<String>,
    pub image: Option<String>,
    pub backend: Option<String>,
    pub cpus: Option<i64>,
    pub memory_mb: Option<i64>,
    pub no_provision: Option<bool>,
}

impl From<CreateShedRequest> for shed_core::models::CreateShedRequest {
    fn from(v: CreateShedRequest) -> Self {
        Self {
            name: v.name,
            repo: v.repo,
            local_dir: v.local_dir,
            image: v.image,
            backend: v.backend,
            cpus: v.cpus,
            memory_mb: v.memory_mb,
            no_provision: v.no_provision,
        }
    }
}

/// Process-wide create store. Host-less by contract: `create_status(id)` carries
/// no host, so every per-host `ShedCore` shares this one store. The orchestration
/// itself lives in pure `shed-core` (the GTK app makes its own per-App instance).
fn create_store() -> &'static shed_core::create::CreateStore {
    static STORE: OnceLock<shed_core::create::CreateStore> = OnceLock::new();
    STORE.get_or_init(shed_core::create::CreateStore::new)
}

/// A read client for one shed-server host. The base URL is injected by the app
/// (the core is env-agnostic); `server_name` is stamped onto listed sheds.
#[derive(uniffi::Object)]
pub struct ShedCore {
    client: Client,
}

#[uniffi::export(async_runtime = "tokio")]
impl ShedCore {
    /// `minter` installs the control-credential FSM (without one the static
    /// `token` is used as-is); `observer` receives that FSM's adoption events.
    /// An observer passed WITHOUT a minter is inert rather than an error — such
    /// a client never mints, so there is nothing to observe.
    #[uniffi::constructor]
    pub fn new(
        base_url: String,
        server_name: String,
        token: String,
        pin: Option<String>,
        minter: Option<Arc<dyn TokenMinter>>,
        observer: Option<Arc<dyn CredentialObserver>>,
    ) -> Result<Arc<Self>, ShedError> {
        let client = match minter {
            // Provider-backed: build the provider here so the observer can be
            // attached before it mints. `with_provider` takes no static token —
            // a provider-backed client never reads one (see `Client::build`).
            Some(m) => {
                let bridge = Arc::new(ForeignMinterBridge(m)) as Arc<dyn core_token::TokenMinter>;
                let mut provider =
                    core_token::ControlTokenProvider::new(server_name.clone(), bridge);
                if let Some(o) = observer {
                    provider = provider.with_observer(Arc::new(ForeignObserverBridge(o)));
                }
                Client::with_provider(base_url, server_name, pin, Arc::new(provider))?
            }
            None => Client::new(base_url, server_name, token, pin, None)?,
        };
        Ok(Arc::new(Self { client }))
    }

    /// `GET /api/info`.
    pub async fn info(&self) -> Result<ServerInfo, ShedError> {
        Ok(self.client.info().await?.into())
    }

    /// `GET /api/sheds` (host-stamped).
    pub async fn list_sheds(&self) -> Result<Vec<Shed>, ShedError> {
        Ok(self
            .client
            .list_sheds()
            .await?
            .into_iter()
            .map(Into::into)
            .collect())
    }

    /// `GET /api/system/df`.
    pub async fn system_df(&self) -> Result<SystemDiskUsage, ShedError> {
        Ok(self.client.system_df().await?.into())
    }

    /// `GET /api/images`.
    pub async fn list_images(&self) -> Result<Vec<ShedImage>, ShedError> {
        Ok(self
            .client
            .list_images()
            .await?
            .into_iter()
            .map(Into::into)
            .collect())
    }

    /// `GET /api/egress/profiles`.
    pub async fn egress_profiles(&self) -> Result<Vec<EgressProfileInfo>, ShedError> {
        Ok(self
            .client
            .egress_profiles()
            .await?
            .into_iter()
            .map(Into::into)
            .collect())
    }

    /// `POST /api/sheds/{name}/start`.
    pub async fn start(&self, name: String) -> Result<(), ShedError> {
        Ok(self.client.start(&name).await?)
    }

    /// `POST /api/sheds/{name}/stop`.
    pub async fn stop(&self, name: String) -> Result<(), ShedError> {
        Ok(self.client.stop(&name).await?)
    }

    /// `POST /api/sheds/{name}/reset`.
    pub async fn reset(&self, name: String) -> Result<(), ShedError> {
        Ok(self.client.reset(&name).await?)
    }

    /// `DELETE /api/sheds/{name}`.
    pub async fn delete(&self, name: String) -> Result<(), ShedError> {
        Ok(self.client.delete(&name).await?)
    }

    /// Start a create: POST /api/sheds streamed in the background; returns an id
    /// whose progress the caller polls via `create_status`. Async so it runs on
    /// the tokio runtime — the store spawns the SSE task on the ambient handle (a
    /// sync FFI method would have no runtime context to spawn on).
    pub async fn create_start(&self, request: CreateShedRequest) -> String {
        create_store().start(
            &tokio::runtime::Handle::current(),
            &self.client,
            request.into(),
        )
    }

    /// Snapshot of an in-flight create (poll until state is complete/error).
    #[allow(clippy::needless_pass_by_value)] // uniffi exports take owned params
    pub fn create_status(&self, id: String) -> Option<CreateProgress> {
        create_store().status(&id).map(Into::into)
    }

    /// Abort a create's stream + drop its state. The Swift stream's onTermination
    /// calls this, since Task.cancel doesn't propagate over the FFI (M0 finding).
    #[allow(clippy::needless_pass_by_value)] // uniffi exports take owned params
    pub fn create_cancel(&self, id: String) {
        create_store().cancel(&id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::time::Duration;

    // ---- the foreign minter contract (plan 002 §7 P2) ----

    /// A stand-in for Swift's `HostAgentTokenMinter`: implements the EXPORTED
    /// trait (the one the generated bindings project into Swift), records what
    /// the core handed it, and answers with a scripted tagged result.
    #[derive(Default)]
    struct FakeForeignMinter {
        supports_mtls: bool,
        answer: Mutex<Option<MintedCredential>>,
        /// `Some(csr)` / `None` per `mint_credential` call, in order.
        seen_csrs: Mutex<Vec<Option<String>>>,
        seen_servers: Mutex<Vec<String>>,
    }

    impl FakeForeignMinter {
        fn new(supports_mtls: bool, answer: MintedCredential) -> Arc<Self> {
            Arc::new(Self {
                supports_mtls,
                answer: Mutex::new(Some(answer)),
                ..Default::default()
            })
        }

        fn token_answer(token: &str) -> MintedCredential {
            MintedCredential::Token {
                token: MintedToken {
                    token: token.into(),
                    expires_at_unix: Some(4_000_000_000),
                },
            }
        }

        fn csrs(&self) -> Vec<Option<String>> {
            self.seen_csrs.lock().unwrap().clone()
        }
    }

    #[async_trait::async_trait]
    impl TokenMinter for FakeForeignMinter {
        async fn mint(&self, _server: String) -> Result<MintedToken, ShedError> {
            Err(ShedError::Config {
                message: "legacy mint() must not be called when mint_credential exists".into(),
            })
        }

        fn supports_mtls(&self) -> bool {
            self.supports_mtls
        }

        async fn mint_credential(
            &self,
            server: String,
            csr_base64: Option<String>,
        ) -> Result<MintedCredential, ShedError> {
            self.seen_servers.lock().unwrap().push(server);
            self.seen_csrs.lock().unwrap().push(csr_base64);
            Ok(self
                .answer
                .lock()
                .unwrap()
                .clone()
                .expect("an answer was scripted"))
        }
    }

    fn bridge(m: Arc<FakeForeignMinter>) -> ForeignMinterBridge {
        ForeignMinterBridge(m as Arc<dyn TokenMinter>)
    }

    async fn mint_through(
        m: &Arc<FakeForeignMinter>,
        req: &core_token::CredentialRequest,
    ) -> Result<core_token::MintedCredential, CoreError> {
        core_token::TokenMinter::mint_credential(&bridge(m.clone()), "mini2", req).await
    }

    #[tokio::test]
    async fn tagged_token_arm_maps_to_a_core_token() {
        let m = FakeForeignMinter::new(false, FakeForeignMinter::token_answer("tok"));
        let got = mint_through(&m, &core_token::CredentialRequest::default())
            .await
            .unwrap();
        match got {
            core_token::MintedCredential::Token(t) => {
                assert_eq!(t.token, "tok");
                assert_eq!(t.expires_at_unix, Some(4_000_000_000));
            }
            other => panic!("expected a token, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn tagged_certificate_arm_maps_to_a_core_certificate() {
        let m = FakeForeignMinter::new(
            true,
            MintedCredential::Certificate {
                certificate: MintedCertificate {
                    cert_pem: "-----BEGIN CERTIFICATE-----\nZZ\n-----END CERTIFICATE-----\n".into(),
                    serial: "0a0b".into(),
                    expires_at_unix: Some(123),
                },
            },
        );
        let got = mint_through(&m, &core_token::CredentialRequest::with_csr("Q1NS"))
            .await
            .unwrap();
        match got {
            core_token::MintedCredential::Certificate(c) => {
                assert!(c.cert_pem.contains("BEGIN CERTIFICATE"));
                assert_eq!(c.serial, "0a0b");
                assert_eq!(c.expires_at_unix, Some(123));
            }
            other => panic!("expected a certificate, got {other:?}"),
        }
        // The CSR the core composed is relayed verbatim — the property that
        // proves the foreign side generated no second keypair.
        assert_eq!(m.csrs(), vec![Some("Q1NS".to_string())]);
    }

    #[tokio::test]
    async fn tagged_failed_arm_is_fail_closed_with_the_agents_words() {
        let m = FakeForeignMinter::new(
            true,
            MintedCredential::Failed {
                message: "upgrade shed-host-agent to enroll a certificate".into(),
            },
        );
        let err = mint_through(&m, &core_token::CredentialRequest::with_csr("Q1NS"))
            .await
            .unwrap_err();
        assert!(
            matches!(&err, CoreError::Config(msg) if msg.contains("upgrade shed-host-agent")),
            "got {err:?}"
        );
    }

    #[tokio::test]
    async fn empty_payloads_are_refused_not_adopted() {
        // An empty token would be a valid-looking downgrade (F6).
        let m = FakeForeignMinter::new(false, FakeForeignMinter::token_answer(""));
        let err = mint_through(&m, &core_token::CredentialRequest::default())
            .await
            .unwrap_err();
        assert!(
            matches!(&err, CoreError::Config(msg) if msg.contains("no token")),
            "got {err:?}"
        );

        // "mtls but no certificate" is a protocol violation, not an empty success.
        let m = FakeForeignMinter::new(
            true,
            MintedCredential::Certificate {
                certificate: MintedCertificate {
                    cert_pem: String::new(),
                    serial: String::new(),
                    expires_at_unix: None,
                },
            },
        );
        let err = mint_through(&m, &core_token::CredentialRequest::with_csr("Q1NS"))
            .await
            .unwrap_err();
        assert!(
            matches!(&err, CoreError::Config(msg) if msg.contains("no certificate")),
            "got {err:?}"
        );

        // A `failed` arm with nothing in it still names the server.
        let m = FakeForeignMinter::new(
            true,
            MintedCredential::Failed {
                message: String::new(),
            },
        );
        let err = mint_through(&m, &core_token::CredentialRequest::with_csr("Q1NS"))
            .await
            .unwrap_err();
        assert!(
            matches!(&err, CoreError::Config(msg) if msg.contains("mini2")),
            "got {err:?}"
        );
    }

    // ---- the shared size caps, enforced independently at THIS boundary ----

    fn credential_fixture() -> serde_json::Value {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(
            "../../tests/host-agent-diff/fixtures/desktop-credential/credential_response.json",
        );
        let raw = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("read fixture {}: {e}", path.display()));
        serde_json::from_str(&raw).expect("fixture is valid JSON")
    }

    /// The caps are shared DATA, not a per-crate constant: this boundary, the
    /// shed-app mapper, and Swift's `HostAgentCredentialLimits` all answer to
    /// the fixture's `limits` block. A cap that drifted in one of the three
    /// would mean one client adopting a credential another refuses.
    #[test]
    fn the_shared_fixture_limits_are_the_constants_this_boundary_enforces() {
        let fx = credential_fixture();
        let limits = &fx["limits"];
        let cap = |k: &str| limits[k].as_u64().expect("cap") as usize;
        assert_eq!(cap("token_bytes"), core_token::limits::MAX_TOKEN_BYTES);
        assert_eq!(
            cap("client_cert_bytes"),
            core_token::limits::MAX_CLIENT_CERT_BYTES
        );
        assert_eq!(
            cap("cert_serial_bytes"),
            core_token::limits::MAX_CERT_SERIAL_BYTES
        );
        assert_eq!(cap("error_bytes"), core_token::limits::MAX_ERROR_BYTES);
        assert_eq!(cap("csr_bytes"), core_token::limits::MAX_CSR_BYTES);
    }

    /// Every §7 P9 oversize vector, driven through the FFI arm mapper as the
    /// ARM a foreign minter would hand us. Swift refuses these before it ever
    /// constructs an arm, which is exactly why this test exists: another
    /// UniFFI minter — or a regression in Swift's pre-validation — must not be
    /// able to smuggle a 4097-byte token past this side.
    #[tokio::test]
    async fn oversized_fixture_vectors_are_refused_at_the_ffi_boundary() {
        let fx = credential_fixture();
        let limits = fx["limits"].clone();
        let mut checked = 0;
        for v in fx["vectors"].as_array().expect("vectors") {
            let Some(field) = v["oversize_field"].as_str() else {
                continue;
            };
            let name = v["name"].as_str().unwrap();
            let cap = limits[format!("{field}_bytes")].as_u64().unwrap() as usize;
            let big = v["oversize_char"].as_str().unwrap().repeat(cap + 1);
            let frame = &v["frame"];
            let answer = match field {
                "token" => MintedCredential::Token {
                    token: MintedToken {
                        token: big,
                        expires_at_unix: None,
                    },
                },
                "client_cert" => MintedCredential::Certificate {
                    certificate: MintedCertificate {
                        cert_pem: big,
                        serial: frame["cert_serial"].as_str().unwrap_or_default().into(),
                        expires_at_unix: None,
                    },
                },
                "cert_serial" => MintedCredential::Certificate {
                    certificate: MintedCertificate {
                        cert_pem: frame["client_cert"]
                            .as_str()
                            .expect("a cert to pair")
                            .into(),
                        serial: big,
                        expires_at_unix: None,
                    },
                },
                "error" => MintedCredential::Failed { message: big },
                other => panic!("{name}: unhandled oversize field {other}"),
            };
            let m = FakeForeignMinter::new(true, answer);
            let err = mint_through(&m, &core_token::CredentialRequest::with_csr("Q1NS"))
                .await
                .unwrap_err();
            let needle = v["expected"]["message_contains"].as_str().unwrap();
            assert!(
                matches!(&err, CoreError::Config(msg) if msg.contains(needle)),
                "{name}: refusal {err:?} lacks {needle:?}"
            );
            checked += 1;
        }
        assert_eq!(checked, 4, "the fixture's four oversize vectors");
    }

    /// The cap is a ceiling, not a fence: a field EXACTLY at it is adopted, so
    /// the guard can't quietly refuse legitimate credentials.
    #[tokio::test]
    async fn a_field_exactly_at_its_cap_is_adopted() {
        let at_cap = "a".repeat(core_token::limits::MAX_TOKEN_BYTES);
        let m = FakeForeignMinter::new(false, FakeForeignMinter::token_answer(&at_cap));
        match mint_through(&m, &core_token::CredentialRequest::default())
            .await
            .unwrap()
        {
            core_token::MintedCredential::Token(t) => assert_eq!(t.token.len(), at_cap.len()),
            other => panic!("expected a token, got {other:?}"),
        }

        let serial = "0".repeat(core_token::limits::MAX_CERT_SERIAL_BYTES);
        let m = FakeForeignMinter::new(
            true,
            MintedCredential::Certificate {
                certificate: MintedCertificate {
                    cert_pem: "-----BEGIN CERTIFICATE-----\nZZ\n-----END CERTIFICATE-----\n".into(),
                    serial: serial.clone(),
                    expires_at_unix: None,
                },
            },
        );
        match mint_through(&m, &core_token::CredentialRequest::with_csr("Q1NS"))
            .await
            .unwrap()
        {
            core_token::MintedCredential::Certificate(c) => assert_eq!(c.serial, serial),
            other => panic!("expected a certificate, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn a_thrown_ffi_error_maps_back_to_the_core_error() {
        struct Thrower;
        #[async_trait::async_trait]
        impl TokenMinter for Thrower {
            async fn mint(&self, _server: String) -> Result<MintedToken, ShedError> {
                unreachable!()
            }
            fn supports_mtls(&self) -> bool {
                true
            }
            async fn mint_credential(
                &self,
                _server: String,
                _csr_base64: Option<String>,
            ) -> Result<MintedCredential, ShedError> {
                Err(ShedError::Transport {
                    message: "host agent not connected".into(),
                })
            }
        }
        let b = ForeignMinterBridge(Arc::new(Thrower) as Arc<dyn TokenMinter>);
        let err = core_token::TokenMinter::mint_credential(
            &b,
            "mini2",
            &core_token::CredentialRequest::default(),
        )
        .await
        .unwrap_err();
        assert!(
            matches!(&err, CoreError::Transport(m) if m.contains("not connected")),
            "got {err:?}"
        );
    }

    /// The capability answer decides whether the core pays for a keypair at all:
    /// a `false` minter must never be handed a CSR, a `true` one always is.
    #[tokio::test]
    async fn supports_mtls_gates_the_csr_through_a_real_provider() {
        for supports_mtls in [false, true] {
            let m = FakeForeignMinter::new(supports_mtls, FakeForeignMinter::token_answer("tok"));
            let provider = core_token::ControlTokenProvider::new(
                "mini2".into(),
                Arc::new(bridge(m.clone())) as Arc<dyn core_token::TokenMinter>,
            );
            assert_eq!(provider.token().await.unwrap(), "tok");
            let csrs = m.csrs();
            assert_eq!(csrs.len(), 1, "one mint");
            match (supports_mtls, &csrs[0]) {
                (true, Some(csr)) => assert!(!csr.is_empty(), "a real CSR travels"),
                (false, None) => {}
                (s, got) => panic!("supports_mtls={s} got csr {got:?}"),
            }
            assert_eq!(*m.seen_servers.lock().unwrap(), vec!["mini2".to_string()]);
        }
    }

    // ---- the observer seam (plan 002 §7 P1) ----

    #[derive(Default)]
    struct FakeForeignObserver {
        adopted: Mutex<Vec<CredentialAdopted>>,
        modes: Mutex<Vec<(String, AuthMode)>>,
    }

    impl FakeForeignObserver {
        /// Events are delivered on the core's dispatcher thread, so a test that
        /// must see one waits for it (the delivery contract's asynchrony).
        async fn wait_for(&self, adoptions: usize, mode_changes: usize) {
            for _ in 0..400 {
                if self.adopted.lock().unwrap().len() >= adoptions
                    && self.modes.lock().unwrap().len() >= mode_changes
                {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
            panic!(
                "waited for {adoptions} adoption(s) + {mode_changes} mode change(s); got {} + {}",
                self.adopted.lock().unwrap().len(),
                self.modes.lock().unwrap().len()
            );
        }
    }

    impl CredentialObserver for FakeForeignObserver {
        fn credential_adopted(&self, event: CredentialAdopted) {
            self.adopted.lock().unwrap().push(event);
        }
        fn mode_changed(&self, server: String, mode: AuthMode) {
            self.modes.lock().unwrap().push((server, mode));
        }
    }

    #[tokio::test]
    async fn an_observer_receives_adoptions_through_the_ffi_layer() {
        let obs = Arc::new(FakeForeignObserver::default());
        let m = FakeForeignMinter::new(false, FakeForeignMinter::token_answer("tok"));
        let provider = core_token::ControlTokenProvider::new(
            "mini2".into(),
            Arc::new(bridge(m)) as Arc<dyn core_token::TokenMinter>,
        )
        .with_observer(Arc::new(ForeignObserverBridge(
            obs.clone() as Arc<dyn CredentialObserver>
        )));

        assert_eq!(provider.token().await.unwrap(), "tok");

        obs.wait_for(1, 1).await;
        let event = obs.adopted.lock().unwrap()[0].clone();
        assert_eq!(event.server, "mini2");
        assert_eq!(event.mode, AuthMode::Token);
        assert_eq!(event.expires_at_unix, Some(4_000_000_000));
        // Token mode: the token IS the credential, so the event carries it.
        assert_eq!(event.token.as_deref(), Some("tok"));
        // The first adoption announces a shape nothing announced before, so it
        // IS a transition (the core's "last ANNOUNCED mode" rule) and arrives
        // right after the adoption on the same dispatcher thread.
        assert_eq!(
            *obs.modes.lock().unwrap(),
            vec![("mini2".to_string(), AuthMode::Token)]
        );
    }

    /// The mtls half of the event mapping. Adopting a real certificate needs a CA
    /// that signs the provider's CSR (shed-core's test-only harness, not reachable
    /// from this crate), so the bridge is driven directly — the mapping is the
    /// part that lives here.
    #[test]
    fn the_observer_bridge_maps_both_events_and_carries_no_mtls_material() {
        let obs = Arc::new(FakeForeignObserver::default());
        let b = ForeignObserverBridge(obs.clone() as Arc<dyn CredentialObserver>);
        core_token::CredentialObserver::on_credential_adopted(
            &b,
            &core_token::CredentialAdopted {
                server: "mini2".into(),
                mode: core_token::AuthMode::Mtls,
                expires_at_unix: Some(99),
                token: None,
            },
        );
        core_token::CredentialObserver::on_mode_changed(&b, "mini2", core_token::AuthMode::Mtls);

        let adopted = obs.adopted.lock().unwrap().clone();
        assert_eq!(adopted.len(), 1);
        assert_eq!(adopted[0].mode, AuthMode::Mtls);
        assert_eq!(adopted[0].expires_at_unix, Some(99));
        assert_eq!(
            adopted[0].token, None,
            "no credential material in mtls mode"
        );
        assert_eq!(
            *obs.modes.lock().unwrap(),
            vec![("mini2".to_string(), AuthMode::Mtls)]
        );
    }

    #[test]
    fn a_failed_mint_adopts_nothing() {
        let obs = Arc::new(FakeForeignObserver::default());
        let m = FakeForeignMinter::new(
            true,
            MintedCredential::Failed {
                message: "refused".into(),
            },
        );
        let provider = core_token::ControlTokenProvider::new(
            "mini2".into(),
            Arc::new(bridge(m)) as Arc<dyn core_token::TokenMinter>,
        )
        .with_observer(Arc::new(ForeignObserverBridge(
            obs.clone() as Arc<dyn CredentialObserver>
        )));
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        assert!(rt.block_on(provider.token()).is_err());
        std::thread::sleep(Duration::from_millis(50));
        assert!(obs.adopted.lock().unwrap().is_empty());
        assert!(obs.modes.lock().unwrap().is_empty());
    }

    #[test]
    fn the_constructor_accepts_a_minter_and_an_observer() {
        let core = ShedCore::new(
            "http://127.0.0.1:1".into(),
            "mini2".into(),
            String::new(),
            None,
            Some(
                FakeForeignMinter::new(false, FakeForeignMinter::token_answer("tok"))
                    as Arc<dyn TokenMinter>,
            ),
            Some(Arc::new(FakeForeignObserver::default()) as Arc<dyn CredentialObserver>),
        );
        assert!(core.is_ok());
    }

    // ---- plan 002 §7 P3: the exported-surface key-containment audit ----

    /// Every credential-carrying exported type, destructured EXHAUSTIVELY. A new
    /// field on any of them fails to compile here, so "we accidentally exported a
    /// key" cannot land quietly — this is the type-level half of the §7 P3 audit
    /// (the source scan below is the half that catches a NEW exported type).
    #[test]
    fn exported_credential_types_have_exactly_the_audited_fields() {
        let MintedToken {
            token: _,
            expires_at_unix: _,
        } = MintedToken {
            token: String::new(),
            expires_at_unix: None,
        };
        let MintedCertificate {
            cert_pem: _,
            serial: _,
            expires_at_unix: _,
        } = MintedCertificate {
            cert_pem: String::new(),
            serial: String::new(),
            expires_at_unix: None,
        };
        let CredentialAdopted {
            server: _,
            mode: _,
            expires_at_unix: _,
            token: _,
        } = CredentialAdopted {
            server: String::new(),
            mode: AuthMode::Token,
            expires_at_unix: None,
            token: None,
        };
        // The tagged result's arms, exhaustively — a fourth arm (or a new field
        // on one) breaks this match.
        let answer = MintedCredential::Failed {
            message: String::new(),
        };
        match answer {
            MintedCredential::Token { token: _ } => {}
            MintedCredential::Certificate { certificate: _ } => {}
            MintedCredential::Failed { message: _ } => {}
        }
    }

    /// No exported item — record, enum, or `#[uniffi::export]`ed trait/impl — may
    /// mention private-key material. The private half of the control-scope keypair
    /// is generated inside `ControlTokenProvider` and never crosses this boundary;
    /// the only sanctioned crossings are the base64 CSR (out) and the public
    /// certificate (back).
    ///
    /// Source-scanned rather than metadata-scanned: uniffi 0.28 exposes no runtime
    /// registry of exported items, and the file IS the surface definition.
    #[test]
    fn no_exported_type_carries_private_key_material() {
        const SRC: &str = include_str!("lib.rs");
        const FORBIDDEN: &[&str] = &[
            "private_key",
            "privatekey",
            "key_pem",
            "key_der",
            "pkcs8",
            "keypair",
            "key_pair",
            "secret",
            "signing_key",
            "seckey",
        ];

        let mut items: Vec<(String, String)> = Vec::new();
        let lines: Vec<&str> = SRC.lines().collect();
        let mut i = 0;
        while i < lines.len() {
            let line = lines[i];
            let exported = (line.starts_with("#[derive(")
                && (line.contains("uniffi::Record") || line.contains("uniffi::Enum")))
                || line.starts_with("#[uniffi::export");
            if !exported {
                i += 1;
                continue;
            }
            // Walk to the item's declaration line, then capture through its
            // closing brace (top-level items close at column 0).
            let mut j = i + 1;
            while j < lines.len() && lines[j].starts_with('#') {
                j += 1;
            }
            let name = lines
                .get(j)
                .and_then(|d| {
                    d.split_whitespace()
                        .skip_while(|w| !matches!(*w, "struct" | "enum" | "trait" | "impl" | "fn"))
                        .nth(1)
                })
                .unwrap_or("<unnamed>")
                .trim_end_matches(['{', ':', '('])
                .to_string();
            let start = j;
            let mut end = start;
            while end < lines.len() && lines[end] != "}" {
                end += 1;
            }
            items.push((name, lines[start..=end.min(lines.len() - 1)].join("\n")));
            i = end + 1;
        }

        // The scanner must actually be seeing the surface — a silently-empty scan
        // would make this test a no-op forever.
        let names: Vec<&str> = items.iter().map(|(n, _)| n.as_str()).collect();
        for expected in [
            "MintedToken",
            "MintedCertificate",
            "MintedCredential",
            "CredentialAdopted",
            "AuthMode",
            "TokenMinter",
            "CredentialObserver",
            "ShedCore",
        ] {
            assert!(
                names.contains(&expected),
                "surface scanner missed {expected}; found {names:?}"
            );
        }
        assert!(items.len() >= 20, "scanned only {} items", items.len());

        for (name, body) in &items {
            let lowered = body.to_lowercase();
            // Doc comments explain what is deliberately absent ("no private
            // key"), so audit the CODE, not the prose.
            let code: String = lowered
                .lines()
                .filter(|l| !l.trim_start().starts_with("//"))
                .collect::<Vec<_>>()
                .join("\n");
            for needle in FORBIDDEN {
                assert!(
                    !code.contains(needle),
                    "exported item {name} mentions {needle:?} — private-key material must not \
                     cross the FFI boundary (plan 002 §7 P3)"
                );
            }
        }
    }
}
