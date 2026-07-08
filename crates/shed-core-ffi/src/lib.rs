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

/// The foreign (Swift) control-token mint primitive. shed-core's
/// `ControlTokenProvider` FSM caches/refreshes around this; a throw is
/// fail-closed (the client then sends no token — never a static downgrade).
#[uniffi::export(with_foreign)]
#[async_trait::async_trait]
pub trait TokenMinter: Send + Sync {
    async fn mint(&self, server: String) -> Result<MintedToken, ShedError>;
}

/// A minted control token + optional expiry as unix seconds (Swift parses the
/// host agent's ISO-8601 expiry to epoch before returning it, keeping timestamp
/// parsing on the Swift side).
#[derive(uniffi::Record)]
pub struct MintedToken {
    pub token: String,
    pub expires_at_unix: Option<u64>,
}

/// Adapts the foreign `TokenMinter` to shed-core's pure `TokenMinter` trait.
struct ForeignMinterBridge(Arc<dyn TokenMinter>);

#[async_trait::async_trait]
impl shed_core::token::TokenMinter for ForeignMinterBridge {
    async fn mint(&self, server: &str) -> Result<shed_core::token::MintedToken, CoreError> {
        match self.0.mint(server.to_string()).await {
            Ok(m) => Ok(shed_core::token::MintedToken {
                token: m.token,
                expires_at_unix: m.expires_at_unix,
            }),
            Err(e) => Err(core_error_from_ffi(e)),
        }
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
    #[uniffi::constructor]
    pub fn new(
        base_url: String,
        server_name: String,
        token: String,
        pin: Option<String>,
        minter: Option<Arc<dyn TokenMinter>>,
    ) -> Result<Arc<Self>, ShedError> {
        let core_minter = minter
            .map(|m| Arc::new(ForeignMinterBridge(m)) as Arc<dyn shed_core::token::TokenMinter>);
        Ok(Arc::new(Self {
            client: Client::new(base_url, server_name, token, pin, core_minter)?,
        }))
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
