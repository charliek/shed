//! Wire DTOs for the shed-server HTTP API.
//!
//! These decoders are defensive: `{"sheds": null}` -> `[]`, omitted optionals,
//! `host` absent on the wire (stamped by the client), lenient `ShedStatus`,
//! `"?"` name sentinels, and timestamps carried VERBATIM as strings (flexible
//! parsing + all display helpers stay in the clients, off the decode path).
//!
//! Rust field names are snake_case and shed-server JSON is snake_case, so no
//! `#[serde(rename)]` is needed; serde also maps a missing `Option` field to
//! `None`, so those need no `default` either. `default` is applied only where it
//! does real work (sentinels, collections, zero-valued scalars/structs).
//!
//! Scope (M1): the shed-server *read* DTOs. `CreateShedRequest` (a request
//! body) lands with the create flow in M4.

use serde::{Deserialize, Deserializer, Serialize};
use serde_json::Value;

use shed_lane::rc::{strip_format_chars, RcActivity, RcKind, RcSession, RcState, TMUX_PREFIX};
use shed_lane::text::{clean_display, non_empty_str, opt_trimmed};

/// Deserialize `T`, mapping an explicit JSON `null` to `T::default()`. serde's
/// `#[serde(default)]` only covers an ABSENT field; shed-server sends `null` for
/// empty collections (`{"sheds": null}`, `df` arrays), which a bare
/// `#[serde(default)] Vec<_>` rejects. Pair them: `#[serde(default,
/// deserialize_with = "null_default")]`.
pub(crate) fn null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

fn unknown_name() -> String {
    "?".to_string()
}

/// A shed's lifecycle status. Lenient: an unrecognized value
/// (`#[serde(other)]`) OR an absent field (`default` on the field) both decode
/// to `Unknown`, so a new server status never breaks decode.
///
/// `Stopping` (`internal/config/types.go` `StatusStopping`) means a stop is in
/// progress; only a backend with an asynchronous lifecycle (remote) sends it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum ShedStatus {
    Running,
    Stopped,
    Starting,
    Stopping,
    Error,
    #[default]
    #[serde(other)]
    Unknown,
}

/// `GET /api/info`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct ServerInfo {
    pub name: String,
    pub version: String,
    pub backend: Option<String>,
    pub ssh_port: Option<i64>,
    pub http_port: Option<i64>,
    /// Server feature tokens (`overview`, `rc-enrich`, `rc-events`,
    /// `rc-proxy`, … — the `FEATURE_*` consts below). Emitted since the
    /// overview work (Go `internal/api/handlers.go` info handler); absent/null
    /// on older servers → `[]`. `/api/info` is the unauthenticated bootstrap
    /// call, making this the reliable capability signal (vs probing for 404s).
    #[serde(default, deserialize_with = "null_default")]
    pub features: Vec<String>,
}

impl ServerInfo {
    /// Whether `feature` is advertised (endpoint discovery, replacing probing).
    pub fn has_feature(&self, feature: &str) -> bool {
        self.features.iter().any(|f| f == feature)
    }
}

/// A shed. `host` is absent from shed-server JSON (the client stamps it after
/// decode); it defaults to "" here to mirror `Shed.init(from:)`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct Shed {
    #[serde(default)]
    pub host: String,
    pub name: String,
    #[serde(default)]
    pub status: ShedStatus,
    /// Why the shed is in `status` when that is not the plain case — a code
    /// such as `capacity`, `run_limit`, `external` or `no_pin` (only the remote
    /// backend sends one). Carried verbatim: the clients show it beside the
    /// status, as the CLI's `stopped (capacity)`. Absent or `null` → `None`.
    /// Decoded tolerantly, as the overview path decodes it: a reason that is
    /// not a string (a number, an object, a list) is `None`, never a failed
    /// decode of the shed — or of the whole `/api/sheds` list, which would
    /// drop every shed on that server.
    #[serde(default, deserialize_with = "lenient")]
    pub status_reason: Option<String>,
    pub backend: Option<String>,
    pub repo: Option<String>,
    pub image: Option<String>,
    pub image_digest: Option<String>,
    pub local_dir: Option<String>,
    pub ip_address: Option<String>,
    pub cpus: Option<i64>,
    pub memory_mb: Option<i64>,
    // Carried verbatim — never parsed/normalized here (the clients own flexible
    // timestamp parsing for display).
    pub created_at: Option<String>,
    pub started_at: Option<String>,
    #[serde(default, deserialize_with = "null_default")]
    pub active_namespaces: Vec<String>,
}

/// The `{"sheds": [...] | null}` wrapper for `GET /api/sheds`. `null` and an
/// omitted key both decode to `[]`.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct ShedList {
    #[serde(default, deserialize_with = "null_default")]
    pub sheds: Vec<Shed>,
}

/// One installed image (`GET /api/images`). Lenient for pre-v0.6.1 servers that
/// omit alias/is_default; absent `name` -> `"?"`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct ShedImage {
    #[serde(default = "unknown_name")]
    pub name: String,
    pub docker_ref: Option<String>,
    pub alias: Option<String>,
    #[serde(default)]
    pub is_default: bool,
    #[serde(default)]
    pub cached: bool,
    #[serde(default)]
    pub in_use: bool,
    pub digest: Option<String>,
    pub source: Option<String>,
    #[serde(default)]
    pub size_bytes: i64,
}

/// The `{"images": [...] | null}` wrapper for `GET /api/images`.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct ImageList {
    #[serde(default, deserialize_with = "null_default")]
    pub images: Vec<ShedImage>,
}

/// A logical/physical byte pair. Missing halves -> 0.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct DiskSize {
    #[serde(default)]
    pub logical_bytes: i64,
    #[serde(default)]
    pub physical_bytes: i64,
}

/// One image/shed/orphan disk entry. Absent `name` -> `"?"`, absent size -> 0/0.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct DiskEntry {
    #[serde(default = "unknown_name")]
    pub name: String,
    pub docker_ref: Option<String>,
    #[serde(default)]
    pub size: DiskSize,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct DiskTotals {
    #[serde(default)]
    pub images: DiskSize,
    #[serde(default)]
    pub sheds: DiskSize,
    #[serde(default)]
    pub snapshots: DiskSize,
    #[serde(default)]
    pub orphans: DiskSize,
    #[serde(default)]
    pub all: DiskSize,
}

/// `GET /api/system/df`. Arrays default to `[]` (null/omitted), totals to zero.
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct SystemDiskUsage {
    pub server_name: Option<String>,
    pub backend: Option<String>,
    #[serde(default, deserialize_with = "null_default")]
    pub images: Vec<DiskEntry>,
    #[serde(default, deserialize_with = "null_default")]
    pub sheds: Vec<DiskEntry>,
    #[serde(default, deserialize_with = "null_default")]
    pub orphans: Vec<DiskEntry>,
    #[serde(default)]
    pub totals: DiskTotals,
}

/// One egress profile fragment. Mirrors shed-server's `config.EgressProfile`
/// (all-lowercase single-word keys). `Serialize` so shed-app's per-host egress
/// rows serialize straight to the clients (like `SystemDiskUsage`).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct EgressProfile {
    pub mode: Option<String>,
    pub allow: Option<Vec<String>>,
    pub deny: Option<Vec<String>>,
    pub rule: Option<String>,
}

/// One entry of `GET /api/egress/profiles`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct EgressProfileInfo {
    pub name: String,
    pub source: String,
    pub profile: EgressProfile,
}

/// One decision on `GET /api/egress/stream` (shed-server's `egress.AuditRecord`): a
/// connection a shed's egress proxy allowed or denied. Absent fields are empty; unknown
/// ones are ignored. Undecoded beyond strings and the port: `ts` and `shed_created_at`
/// are the server's RFC 3339 text, and every string is the server's, not yet cleaned for
/// display.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize, Serialize)]
#[serde(default)]
pub struct EgressDecision {
    pub ts: String,
    pub shed: String,
    /// The shed incarnation the decision came from: a shed deleted and created again
    /// under its name is another one. Empty from a server older than the field.
    pub shed_created_at: String,
    pub host: String,
    pub port: i64,
    pub resolved_ip: String,
    pub protocol: String,
    /// `allow` or `deny`.
    pub verdict: String,
    pub reason: String,
}

/// Body for `POST /api/sheds`. Only non-null fields are sent (`None` optionals
/// are omitted). `repo`/`local_dir` are mutually exclusive.
#[derive(Debug, Clone, Default, Serialize)]
pub struct CreateShedRequest {
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub repo: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub local_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub backend: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpus: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub memory_mb: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub no_provision: Option<bool>,
}

// ---- sessions read-plane (GET /api/sheds/{name}/sessions) ------------------

/// One tmux session row from `GET /api/sheds/{name}/sessions` (and
/// `GET /api/sessions`). Wire shape: Go `config.Session`
/// (`internal/config/types.go:182-197`). Serde-derive like the other read
/// DTOs — strict field types with defensive defaults where the Go side is
/// `omitempty` (and on `created_at`, which Go always emits but we keep
/// `Option` per crate convention: timestamps are carried VERBATIM as strings,
/// never parsed on the decode path).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Session {
    /// The tmux session name (`rc-<slug>` for RC sessions).
    pub name: String,
    /// The owning shed. Not `omitempty` in Go, but defaulted here so a
    /// malformed row degrades a field instead of the whole page.
    #[serde(default)]
    pub shed_name: String,
    /// Stamped only by the CLI's cross-server aggregation; the HTTP handlers
    /// leave it empty (`omitempty` → absent).
    pub server_name: Option<String>,
    /// RFC3339, verbatim. Go marshals `time.Time` unconditionally (never
    /// `omitempty`), so it is always present on the wire — `Option` is
    /// defensiveness, not an expected absence.
    pub created_at: Option<String>,
    #[serde(default)]
    pub attached: bool,
    /// `omitempty` → absent when 0.
    #[serde(default)]
    pub window_count: i64,
    /// RC Session Convention metadata for `rc-*` rows, populated by the
    /// server's enrichment leg (`internal/api/rcenrich.go`). `None` for plain
    /// tmux sessions and for rc rows on a shed whose enrichment degraded (a
    /// `warnings` entry is added in that case).
    pub rc: Option<SessionRC>,
}

/// The RC display subset inside a [`Session`] row. Wire shape: Go
/// `config.SessionRC` (`internal/config/types.go:199-215`) — every field but
/// `managed` is `omitempty`. `kind`/`state`/`activity` stay raw wire strings
/// here (this is the read-plane row, not the enriched [`shed_lane::rc::RcSession`]
/// model — the overview adapter owns that mapping).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct SessionRC {
    pub kind: Option<String>,
    pub state: Option<String>,
    /// The only non-`omitempty` Go field; still defaulted (defensive per crate
    /// style). `false` for legacy/unmanaged rc-* sessions.
    #[serde(default)]
    pub managed: bool,
    pub display_name: Option<String>,
    pub url: Option<String>,
    pub created_by: Option<String>,
    /// Live-activity dimension (`working|needs_input|idle|unknown`), absent
    /// when no hub is running / the kind is unsupported / a blocking state
    /// suppresses it.
    pub activity: Option<String>,
    /// RFC3339, verbatim; absent with `activity`.
    pub activity_at: Option<String>,
    /// Hub-sanitized ≤200-rune preview. NOTE: sanitized for ANSI/C0C1 only —
    /// a renderer should still pass it through
    /// [`shed_lane::rc::strip_format_chars`] before display (the enriched paths
    /// do this at decode; this raw row does not).
    pub last_message: Option<String>,
}

/// The `GET /api/sheds/{name}/sessions` envelope (Go
/// `config.SessionsResponse`, `internal/config/types.go:287-291`).
/// `warnings` carries per-shed RC-enrichment degradations (`omitempty`);
/// null/absent lists decode to `[]`.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct SessionsResponse {
    #[serde(default, deserialize_with = "null_default")]
    pub sessions: Vec<Session>,
    #[serde(default, deserialize_with = "null_default")]
    pub warnings: Vec<String>,
}

// ---- GET /api/overview single-call host snapshot ---------------------------

/// Server feature token for the `GET /api/overview` endpoint itself. The
/// canonical token list is the Go server's `serverFeatures`
/// (`internal/api/overview.go:17-34`); a client gates endpoint use on these
/// (via [`OverviewServer::has_feature`]) instead of probing for 404s.
pub const FEATURE_OVERVIEW: &str = "overview";
/// Keep only the string elements of a maybe-list; a non-list/absent value
/// degrades to `[]` (Dart's `if (raw is List) for (f in raw) if (f is String)`).
fn string_list(v: Option<&Value>) -> Vec<String> {
    match v.and_then(Value::as_array) {
        Some(items) => items
            .iter()
            .filter_map(Value::as_str)
            .map(str::to_string)
            .collect(),
        None => Vec::new(),
    }
}

/// Dart `_asInt` / `_int` (`shed_dtos.dart:424`, `rc_capabilities.dart:146`):
/// an integer as-is, another number truncated, anything else `0`.
fn int_or_zero(v: Option<&Value>) -> i64 {
    v.and_then(|v| v.as_i64().or_else(|| v.as_f64().map(|f| f as i64)))
        .unwrap_or(0)
}

/// Read a maybe-string VERBATIM — no trim, absent/non-string → `""` (Dart's
/// `as String? ?? ''` reads, `rc_models.dart:249-259`).
fn raw_str(v: Option<&Value>) -> &str {
    v.and_then(Value::as_str).unwrap_or("")
}

/// Field-tolerant decode of a synthesized flat session object (the overview
/// adapter's merge of the derived slug/tmux/outer-created_at with the `rc`
/// block). Ported field-for-field from mobile's `RcSession.fromJson`
/// (`rc_models.dart:245-273`) — deliberately NOT the strict [`crate::rc::RcSessionDto`]
/// serde path (that is the `shed-ext-rc` stdout contract): a server-enriched
/// row must decode per-field so a future/unknown enum value or a missing field
/// degrades that FIELD, never vanishes the session (forward-compat: an old
/// client still renders a session a newer server enriches). Tolerances:
/// missing/unknown `kind` → preserved raw via [`RcKind::Other`] (`""` when
/// absent); missing/unknown `state` → `Starting` ([`RcState::from_wire`]);
/// `managed` → `false` unless exactly boolean `true`; every optional string is
/// trimmed-or-`None` — including `workdir`, which stays `None` when
/// absent/blank (`rc_models.dart:261`; NO `DEFAULT_WORKDIR` injection — that
/// fallback belongs to the `from_dto` stdout path only); `activity`
/// absent/blank → `None`, unrecognized → `Unknown`; `last_message` gets the
/// full `_cleanDisplay` treatment (trim → Cf-strip → trim → empty→`None`).
fn rc_session_from_flat(j: &serde_json::Map<String, Value>, shed_name: &str) -> RcSession {
    // slug/tmux_session are synthesized by the caller (or overridden by the rc
    // block); Dart reads them as plain strings, no trim (`rc_models.dart:249-253`).
    let slug = raw_str(j.get("slug")).to_string();
    let display_name =
        opt_trimmed(j.get("display_name")).unwrap_or_else(|| format!("{shed_name}/{slug}")); // `<shed>/<slug>` fallback
    RcSession {
        // host is absent on the wire — stamped by the client after decode
        // (same convention as `Shed.host`).
        host: String::new(),
        shed: shed_name.to_string(),
        tmux_session: raw_str(j.get("tmux_session")).to_string(),
        display_name,
        workdir: opt_trimmed(j.get("workdir")),
        // kind/state take the raw wire string (no trim — Dart passes the value
        // straight to fromWire, rc_models.dart:258-259); absent → "".
        kind: RcKind::from_wire(raw_str(j.get("kind"))),
        state: RcState::from_wire(raw_str(j.get("state"))),
        // Contract v2, carried verbatim: absent/blank stays `None`, so an old
        // server's silence never masquerades as a producer-asserted lane (the
        // `lane_or_tui()` accessor owns the "tui" fallback).
        lane: opt_trimmed(j.get("lane")),
        url: opt_trimmed(j.get("url")),
        rc_id: opt_trimmed(j.get("id")), // id → rc_id, as in `from_dto`
        created_by: opt_trimmed(j.get("created_by")),
        created_at: opt_trimmed(j.get("created_at")),
        target_label: opt_trimmed(j.get("target_label")),
        // Absent/blank → None ("no activity dimension at all"); an unrecognized
        // token → Unknown (rc_models.dart:140-146).
        activity: non_empty_str(j.get("activity")).map(RcActivity::from_wire),
        activity_at: opt_trimmed(j.get("activity_at")),
        // Guest-controlled preview text: strip Unicode format characters (bidi
        // overrides like U+202E) the hub's ANSI/C0C1 sanitizer does not cover
        // (`rc_models.dart:269-271`).
        last_message: clean_display(j.get("last_message")),
        // Not surfaced on the overview flat wire (the server projection carries no
        // pending approvals); the DTO/from_dto path is where a hub-listed snapshot
        // arrives. Absent here, always.
        pending_approvals: None,
        managed: j.get("managed").and_then(Value::as_bool).unwrap_or(false),
        slug,
    }
}

/// Overview-tolerant decode of one `sheds[]` row into the shared [`Shed`] —
/// per-field like Dart's `Shed.fromJson` (`shed_dtos.dart:31-41`: missing/blank
/// `name` → `"?"`, missing/invalid `status` → `Unknown`, wrong-typed scalars →
/// `None`), so a malformed shed row never vanishes from the snapshot. The
/// strict serde derive on [`Shed`] stays the `/api/sheds` contract; this
/// hand-decode is the overview path only.
fn overview_shed_record(obj: &serde_json::Map<String, Value>) -> Shed {
    let opt_verbatim =
        |key: &str| -> Option<String> { obj.get(key).and_then(Value::as_str).map(str::to_string) };
    Shed {
        host: opt_verbatim("host").unwrap_or_default(), // absent on the wire; client stamps it
        name: non_empty_str(obj.get("name")).unwrap_or("?").to_string(),
        // Lenient two ways: an unknown status STRING hits the `#[serde(other)]`
        // arm; a non-string/missing status is Unknown here directly.
        status: non_empty_str(obj.get("status"))
            .and_then(|s| serde_json::from_value(Value::String(s.to_string())).ok())
            .unwrap_or_default(),
        // Optional, verbatim, as `backend`: a non-string or `null` → None.
        status_reason: opt_verbatim("status_reason"),
        backend: opt_verbatim("backend"),
        repo: opt_verbatim("repo"),
        image: opt_verbatim("image"),
        image_digest: opt_verbatim("image_digest"),
        local_dir: opt_verbatim("local_dir"),
        ip_address: opt_verbatim("ip_address"),
        cpus: obj.get("cpus").and_then(Value::as_i64),
        memory_mb: obj.get("memory_mb").and_then(Value::as_i64),
        // Timestamps carried verbatim, as on the strict path.
        created_at: opt_verbatim("created_at"),
        started_at: opt_verbatim("started_at"),
        active_namespaces: string_list(obj.get("active_namespaces")),
    }
}

/// Overview-tolerant decode of one [`DiskSize`] (Dart `DiskSize.fromJson`,
/// `shed_dtos.dart:345-348`): non-map/missing halves → 0.
fn overview_disk_size(v: Option<&Value>) -> DiskSize {
    let o = v.and_then(Value::as_object);
    DiskSize {
        logical_bytes: int_or_zero(o.and_then(|o| o.get("logical_bytes"))),
        physical_bytes: int_or_zero(o.and_then(|o| o.get("physical_bytes"))),
    }
}

/// Overview-tolerant decode of a disk-entry list: non-list → `[]`, non-map
/// elements skipped, per-entry defaults matching the strict DTO's
/// (`name` → `"?"`, absent size → zeros).
fn overview_disk_entries(v: Option<&Value>) -> Vec<DiskEntry> {
    v.and_then(Value::as_array)
        .map(|rows| {
            rows.iter()
                .filter_map(Value::as_object)
                .map(|o| DiskEntry {
                    name: non_empty_str(o.get("name")).unwrap_or("?").to_string(),
                    docker_ref: o
                        .get("docker_ref")
                        .and_then(Value::as_str)
                        .map(str::to_string),
                    size: overview_disk_size(o.get("size")),
                })
                .collect()
        })
        .unwrap_or_default()
}

/// Overview-tolerant decode of the `df` block into the shared
/// [`SystemDiskUsage`] — constructed whenever `df` is a map, zero-defaulting
/// every malformed/missing substructure (Dart `SystemDiskUsage.fromJson` +
/// `DiskTotals.fromJson`, `shed_dtos.dart:345-392`), so a bad nested field
/// degrades that field instead of discarding the whole disk block. The strict
/// serde derive stays the `/api/system/df` contract.
fn overview_df(obj: &serde_json::Map<String, Value>) -> SystemDiskUsage {
    let totals = obj.get("totals").and_then(Value::as_object);
    let total = |key: &str| overview_disk_size(totals.and_then(|t| t.get(key)));
    SystemDiskUsage {
        server_name: opt_trimmed(obj.get("server_name")),
        backend: opt_trimmed(obj.get("backend")),
        images: overview_disk_entries(obj.get("images")),
        sheds: overview_disk_entries(obj.get("sheds")),
        orphans: overview_disk_entries(obj.get("orphans")),
        totals: DiskTotals {
            images: total("images"),
            sheds: total("sheds"),
            snapshots: total("snapshots"),
            orphans: total("orphans"),
            all: total("all"),
        },
    }
}

/// The `server` block of `GET /api/overview`: the server's version and the
/// feature-token set (mirrored from `GET /api/info`). A client learns which
/// endpoints/behaviors a server supports from `features` without probing each.
///
/// Ported from mobile's `OverviewServer.fromJson` (`shed_dtos.dart:221-239`):
/// a missing/blank `version` → `""`; a non-list/missing `features` → `[]`,
/// keeping only the string elements.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct OverviewServer {
    pub version: String,
    pub features: Vec<String>,
}

impl OverviewServer {
    /// Whether `feature` is advertised (endpoint discovery, replacing probing).
    pub fn has_feature(&self, feature: &str) -> bool {
        self.features.iter().any(|f| f == feature)
    }

    fn from_value(v: Option<&Value>) -> OverviewServer {
        let obj = v.and_then(Value::as_object);
        OverviewServer {
            version: opt_trimmed(obj.and_then(|o| o.get("version"))).unwrap_or_default(),
            features: string_list(obj.and_then(|o| o.get("features"))),
        }
    }
}

/// One shed in `GET /api/overview`: the full shed record plus the shed's RC
/// sessions (only the rc-enriched tmux rows are surfaced). A stopped shed
/// carries no sessions.
///
/// Ported from mobile's `OverviewShed.fromJson` (`shed_dtos.dart:246-294`);
/// reuses [`RcSession`] from `rc.rs`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OverviewShed {
    pub shed: Shed,
    pub sessions: Vec<RcSession>,
}

impl OverviewShed {
    /// Decode one `sheds[]` element. The [`Shed`] decodes field-tolerantly from
    /// the same outer map via [`overview_shed_record`] (missing name → `"?"`,
    /// per Dart's `Shed.fromJson` defaults) — a malformed shed row degrades,
    /// it never vanishes. Only a non-map element yields `None`.
    fn from_value(v: &Value) -> Option<OverviewShed> {
        let obj = v.as_object()?;
        let shed = overview_shed_record(obj);

        let sessions = obj
            .get("sessions")
            .and_then(Value::as_array)
            .map(|rows| {
                rows.iter()
                    .filter_map(|row| Self::session_from_row(row, &shed.name))
                    .collect()
            })
            .unwrap_or_default();

        Some(OverviewShed { shed, sessions })
    }

    /// Adapt one `sessions[]` row (a tmux session) to an [`RcSession`], or
    /// `None` to skip it. Only rc-enriched tmux rows carry an `rc` block; a
    /// plain tmux session (e.g. "default"), an un-enriched rc-* row on a
    /// degraded shed, or a non-map element has none — skipped, so the sessions
    /// list holds exactly the RC sessions (parity with the old `shed-ext-rc
    /// list` fan-out). Mirrors `shed_dtos.dart:263-269`.
    fn session_from_row(row: &Value, shed_name: &str) -> Option<RcSession> {
        let row = row.as_object()?;
        let rc = row.get("rc").and_then(Value::as_object)?;
        let name = non_empty_str(row.get("name")).unwrap_or("");
        let slug = name.strip_prefix(TMUX_PREFIX).unwrap_or(name);
        // The server's SessionRC is a display subset (no slug/tmux/id);
        // derive slug + tmux from the session name and pull created_at
        // from the OUTER row, then let the rc-block fields override
        // (`shed_dtos.dart:270-282`) and decode FIELD-TOLERANTLY — a
        // future/unknown enum value or missing field degrades that
        // field, it never drops the session (see [`rc_session_from_flat`]).
        let mut flat = serde_json::Map::new();
        flat.insert("slug".to_string(), Value::String(slug.to_string()));
        flat.insert("tmux_session".to_string(), Value::String(name.to_string()));
        if let Some(created_at) = row.get("created_at") {
            flat.insert("created_at".to_string(), created_at.clone());
        }
        for (k, val) in rc {
            flat.insert(k.clone(), val.clone());
        }
        Some(rc_session_from_flat(&flat, shed_name))
    }
}

/// `GET /api/overview` — a single call a client renders a whole host from:
/// server identity + feature set, disk usage, and every shed with its
/// (rc-enriched) sessions and capabilities. Each sub-block degrades
/// independently into `warnings` server-side (`internal/api/overview.go:38-63`),
/// so a `None` `df` or an empty session list is a tolerated partial, not a
/// failure — the decode has NO error path (a wrong-typed/missing block
/// defaults). Ported from mobile's `Overview.fromJson`
/// (`shed_dtos.dart:301-333`); hand-rolled (like [`shed_lane::rc::RcKind`]'s manual
/// impl) because the flatten-plus-filter session rule and the per-block
/// tolerance aren't expressible with derive.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Overview {
    pub server: OverviewServer,
    pub df: Option<SystemDiskUsage>,
    pub sheds: Vec<OverviewShed>,
    pub warnings: Vec<String>,
}

impl Overview {
    /// Tolerant decode of the overview body; never fails (a non-object value
    /// yields the all-default snapshot).
    pub fn from_value(v: &Value) -> Overview {
        let Some(obj) = v.as_object() else {
            return Overview::default();
        };
        Overview {
            server: OverviewServer::from_value(obj.get("server")),
            df: obj.get("df").and_then(Value::as_object).map(overview_df),
            sheds: obj
                .get("sheds")
                .and_then(Value::as_array)
                .map(|rows| rows.iter().filter_map(OverviewShed::from_value).collect())
                .unwrap_or_default(),
            warnings: string_list(obj.get("warnings")),
        }
    }
}

impl<'de> Deserialize<'de> for Overview {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(Overview::from_value(&Value::deserialize(d)?))
    }
}

// ---- credential sources (plan 035 §3.4) ----
//
// `GET /api/sources` and `POST /api/sources/use` answer the same document: which
// machine answers credential requests for the caller's user on this server (the
// binding) and every machine the server has seen for that user. Decoded tolerantly in
// every field, so a later server can add fields or widen a type without breaking an
// older client: an unknown field is ignored, a missing, `null` or wrongly typed field
// takes its default, and a machine entry that is not an object is dropped. Every
// string here is carried VERBATIM; it came from other machines by way of the server,
// so whoever shows one cleans it first ([`display_source_text`]).

/// Decode `T` from whatever value is there; anything that does not fit (a `null`, the
/// wrong type) becomes `T::default()` rather than failing the whole document.
fn lenient<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: serde::de::DeserializeOwned + Default,
{
    Ok(serde_json::from_value(Value::deserialize(d)?).unwrap_or_default())
}

/// A list of `T` decoded entry by entry: an entry that does not decode is dropped, and
/// anything but an array is an empty list.
fn lenient_list<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: Deserializer<'de>,
    T: serde::de::DeserializeOwned,
{
    Ok(match Value::deserialize(d)? {
        Value::Array(items) => items
            .into_iter()
            .filter_map(|v| serde_json::from_value(v).ok())
            .collect(),
        _ => Vec::new(),
    })
}

/// A binding's slot list decoded POSITION FOR POSITION: an entry that does not decode
/// (a `null`, a number, an object of the wrong shape) is kept as an empty
/// [`SourceRef`], which names nothing, rather than dropped, so a later entry can never
/// slide into an earlier slot and be taken for it. Anything but an array is an empty
/// list.
fn lenient_slots<'de, D>(d: D) -> Result<Vec<SourceRef>, D::Error>
where
    D: Deserializer<'de>,
{
    Ok(match Value::deserialize(d)? {
        Value::Array(items) => items
            .into_iter()
            .map(|v| serde_json::from_value(v).unwrap_or_default())
            .collect(),
        _ => Vec::new(),
    })
}

/// The `kind` of a [`SourceRef`] this client understands: one machine.
pub const SOURCE_REF_MACHINE: &str = "machine";

/// One slot of a binding: what answers. In this cut always `{"kind":"machine","id":…}`.
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct SourceRef {
    #[serde(default, deserialize_with = "lenient")]
    pub kind: String,
    #[serde(default, deserialize_with = "lenient")]
    pub id: String,
}

/// Which machine answers for the user: the delivery slot (who receives requests) and the
/// signing slot (who signs). This cut always holds one machine, the same one, in both.
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct SourceBinding {
    /// Slot for slot as the server sent it ([`lenient_slots`]).
    #[serde(default, deserialize_with = "lenient_slots")]
    pub delivery: Vec<SourceRef>,
    /// Slot for slot as the server sent it ([`lenient_slots`]).
    #[serde(default, deserialize_with = "lenient_slots")]
    pub signing: Vec<SourceRef>,
    /// RFC 3339, verbatim.
    #[serde(default, deserialize_with = "lenient")]
    pub bound_at: String,
    #[serde(default, deserialize_with = "lenient")]
    pub bound_by: String,
    #[serde(default, deserialize_with = "lenient")]
    pub generation: u64,
}

/// One machine the server has seen for this user (its broker has tried to subscribe at
/// least once).
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct SourceMachine {
    #[serde(default, deserialize_with = "lenient")]
    pub id: String,
    /// The machine's own display name, which is untrusted text: see [`display_source_text`].
    #[serde(default, deserialize_with = "lenient")]
    pub name: String,
    #[serde(default, deserialize_with = "lenient")]
    pub key_fingerprint: String,
    /// RFC 3339, verbatim.
    #[serde(default, deserialize_with = "lenient")]
    pub first_seen: String,
    /// Whether it holds a live subscription right now.
    #[serde(default, deserialize_with = "lenient")]
    pub connected: bool,
    #[serde(default, deserialize_with = "lenient")]
    pub namespaces: Vec<String>,
}

/// The body of `GET /api/sources` and of a successful `POST /api/sources/use`.
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct SourceDocument {
    #[serde(default, deserialize_with = "lenient")]
    pub user: String,
    /// `None` until a machine has been bound for the user.
    #[serde(default, deserialize_with = "lenient")]
    pub binding: Option<SourceBinding>,
    #[serde(default, deserialize_with = "lenient_list")]
    pub machines: Vec<SourceMachine>,
}

impl SourceDocument {
    /// The id of the machine that answers: the FIRST signing slot, when that slot is a
    /// well-formed machine reference; when it is not (another kind, no id, an entry that
    /// did not decode), there is no source, whatever later slots say. There is no other
    /// notion of "the source" (no per-machine flag).
    pub fn source_machine_id(&self) -> Option<&str> {
        self.binding
            .as_ref()?
            .signing
            .first()
            .filter(|r| r.kind == SOURCE_REF_MACHINE && !r.id.is_empty())
            .map(|r| r.id.as_str())
    }

    /// The machine entry with this id, if the server listed one.
    pub fn machine(&self, id: &str) -> Option<&SourceMachine> {
        self.machines.iter().find(|m| m.id == id)
    }
}

/// The most of one machine name or id that is shown. Names are at most 63 bytes and ids
/// 32, so this only ever cuts something that was not a valid one.
pub const SOURCE_TEXT_MAX_BYTES: usize = 64;

/// Text that came from another machine by way of the server (a machine's name, its id, a
/// refusal's message), made safe to show: control characters and Unicode format
/// characters (bidirectional overrides, zero-widths, …) removed, so it can neither forge
/// a line nor reorder or hide what is around it, then cut to `max_bytes` on a character
/// boundary.
pub fn display_source_text(raw: &str, max_bytes: usize) -> String {
    let mut out = String::new();
    for c in strip_format_chars(raw).chars().filter(|c| !c.is_control()) {
        if out.len() + c.len_utf8() > max_bytes {
            break;
        }
        out.push(c);
    }
    out
}

/// Whether `id` has the machine id form the server accepts: 32 lowercase hex
/// characters, no hyphens.
pub fn valid_machine_id(id: &str) -> bool {
    id.len() == 32 && id.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_lane::rc::LANE_TUI;

    #[test]
    fn opt_trimmed_trims_on_darts_set_including_bom() {
        // Dart's `String.trim` strips U+FEFF (BOM); Rust's `str::trim` does
        // not (FEFF lost White_Space in Unicode 6.3). The shared helpers trim
        // on Dart's set via `dart_trim` — a BOM-padded value trims clean and a
        // BOM-only value is blank (`None`), matching mobile's `_str`.
        let v = Value::String("\u{FEFF} x \u{FEFF}".to_string());
        assert_eq!(opt_trimmed(Some(&v)).as_deref(), Some("x"));
        let bom_only = Value::String("\u{FEFF}".to_string());
        assert_eq!(opt_trimmed(Some(&bom_only)), None);
        assert_eq!(clean_display(Some(&bom_only)), None);
        // Ordinary whitespace behavior is unchanged.
        let ws = Value::String("  hi  ".to_string());
        assert_eq!(opt_trimmed(Some(&ws)).as_deref(), Some("hi"));
    }

    #[test]
    fn server_info_full_fixture() {
        let v: ServerInfo =
            serde_json::from_str(include_str!("../../fixtures/server_info.json")).unwrap();
        assert_eq!(v.name, "mini2");
        assert_eq!(v.version, "0.6.2");
        assert_eq!(v.backend.as_deref(), Some("firecracker"));
        assert_eq!(v.ssh_port, Some(2222));
        assert_eq!(v.http_port, Some(8080));
        assert!(v.features.is_empty()); // pre-overview server: absent → []
    }

    #[test]
    fn server_info_minimal() {
        let v: ServerInfo = serde_json::from_str(r#"{"name":"m","version":"1"}"#).unwrap();
        assert_eq!(v.backend, None);
        assert_eq!(v.ssh_port, None);
    }

    #[test]
    fn server_info_features_present_and_null() {
        let v: ServerInfo = serde_json::from_str(
            r#"{"name":"m","version":"0.8.0",
                "features":["overview","rc-enrich","rc-events","rc-proxy"]}"#,
        )
        .unwrap();
        assert!(v.has_feature(FEATURE_OVERVIEW));
        assert!(v.has_feature(FEATURE_OVERVIEW));
        assert!(!v.has_feature("nope"));
        // Explicit null (not just absent) also decodes to [].
        let v: ServerInfo =
            serde_json::from_str(r#"{"name":"m","version":"1","features":null}"#).unwrap();
        assert!(v.features.is_empty());
        assert!(!v.has_feature(FEATURE_OVERVIEW));
    }

    #[test]
    fn shed_decodes_real_server_fixture() {
        // No `host`, many optionals omitted; extra fields (container_id, pid) ignored.
        let s: Shed = serde_json::from_str(include_str!("../../fixtures/shed_real.json")).unwrap();
        assert_eq!(s.name, "hello-world");
        assert_eq!(s.status, ShedStatus::Running);
        assert_eq!(s.backend.as_deref(), Some("firecracker"));
        assert_eq!(s.cpus, Some(2));
        assert_eq!(s.memory_mb, Some(4096));
        assert_eq!(s.host, ""); // absent on the wire; client stamps it
        assert_eq!(s.repo, None); // omitted -> None
        assert!(s.active_namespaces.is_empty()); // absent -> []
                                                 // Timestamps carried verbatim, offset preserved.
        assert_eq!(
            s.created_at.as_deref(),
            Some("2026-05-31T13:33:00.884935839-05:00")
        );
    }

    #[test]
    fn shed_digest_only() {
        let s: Shed = serde_json::from_str(
            r#"{"name":"x","status":"running","image_digest":"sha256:abcdef0123456789aa"}"#,
        )
        .unwrap();
        assert_eq!(s.image, None);
        assert_eq!(s.image_digest.as_deref(), Some("sha256:abcdef0123456789aa"));
    }

    #[test]
    fn shed_minimal() {
        let s: Shed = serde_json::from_str(r#"{"name":"x","status":"running"}"#).unwrap();
        assert_eq!(s.name, "x");
        assert_eq!(s.image, None);
        assert_eq!(s.image_digest, None);
    }

    #[test]
    fn shed_status_leniency() {
        // Unknown value -> Unknown.
        let s: Shed = serde_json::from_str(r#"{"name":"x","status":"provisioning"}"#).unwrap();
        assert_eq!(s.status, ShedStatus::Unknown);
        // Absent status -> Unknown.
        let s: Shed = serde_json::from_str(r#"{"name":"x"}"#).unwrap();
        assert_eq!(s.status, ShedStatus::Unknown);
        // Known value.
        let s: Shed = serde_json::from_str(r#"{"name":"x","status":"stopped"}"#).unwrap();
        assert_eq!(s.status, ShedStatus::Stopped);
    }

    /// The remote backend's lifecycle (plan 037 §3.14), on the strict
    /// `/api/sheds` path: `stopping` is its own status, `status_reason` is
    /// carried verbatim when present and is `None` when absent or `null`, and
    /// a status no server sends yet still decodes to `Unknown`.
    #[test]
    fn shed_cloud_lifecycle_strict() {
        let s: Shed =
            serde_json::from_str(r#"{"name":"cloudy","status":"stopping","backend":"remote"}"#)
                .unwrap();
        assert_eq!(s.status, ShedStatus::Stopping);
        assert_eq!(s.status_reason, None); // absent
        let s: Shed = serde_json::from_str(
            r#"{"name":"full-zone","status":"stopped","status_reason":"capacity"}"#,
        )
        .unwrap();
        assert_eq!(s.status, ShedStatus::Stopped);
        assert_eq!(s.status_reason.as_deref(), Some("capacity"));
        let s: Shed =
            serde_json::from_str(r#"{"name":"x","status":"starting","status_reason":null}"#)
                .unwrap();
        assert_eq!(s.status, ShedStatus::Starting);
        assert_eq!(s.status_reason, None); // explicit null
        let s: Shed = serde_json::from_str(r#"{"name":"x","status":"rebooting"}"#).unwrap();
        assert_eq!(s.status, ShedStatus::Unknown);
        // What the desktop receives: the lowercase word, and the reason.
        let s: Shed = serde_json::from_str(
            r#"{"name":"cloudy","status":"stopping","status_reason":"run_limit"}"#,
        )
        .unwrap();
        let v = serde_json::to_value(&s).unwrap();
        assert_eq!(v["status"], "stopping");
        assert_eq!(v["status_reason"], "run_limit");
    }

    /// A malformed `status_reason` on the strict `/api/sheds` path degrades to
    /// `None`, as on the overview path: it never fails the shed, and never the
    /// whole list (which would drop every shed on that server as a host
    /// failure).
    #[test]
    fn shed_malformed_status_reason_strict() {
        // The list as a server could send it: the reason is a number.
        let w: ShedList = serde_json::from_str(
            r#"{"sheds":[{"name":"x","status":"running","status_reason":7}]}"#,
        )
        .unwrap();
        assert_eq!(w.sheds.len(), 1);
        assert_eq!(w.sheds[0].name, "x");
        assert_eq!(w.sheds[0].status, ShedStatus::Running);
        assert_eq!(w.sheds[0].status_reason, None);
        // Every non-string shape, on one shed.
        for bad in [
            "7",
            "1.5",
            "true",
            r#"{"code":"capacity"}"#,
            r#"["capacity"]"#,
        ] {
            let s: Shed = serde_json::from_str(&format!(
                r#"{{"name":"x","status":"stopped","status_reason":{bad}}}"#
            ))
            .unwrap_or_else(|e| panic!("status_reason {bad} failed the decode: {e}"));
            assert_eq!(s.status, ShedStatus::Stopped, "{bad}");
            assert_eq!(s.status_reason, None, "{bad}");
        }
        // Two sheds, only one with a bad reason: both decode, the good reason
        // is kept, the bad one is None.
        let w: ShedList = serde_json::from_str(
            r#"{"sheds":[
                {"name":"odd","status":"stopped","status_reason":{"code":"capacity"}},
                {"name":"full-zone","status":"stopped","status_reason":"capacity"}
            ]}"#,
        )
        .unwrap();
        assert_eq!(w.sheds.len(), 2);
        assert_eq!(w.sheds[0].name, "odd");
        assert_eq!(w.sheds[0].status_reason, None);
        assert_eq!(w.sheds[1].name, "full-zone");
        assert_eq!(w.sheds[1].status_reason.as_deref(), Some("capacity"));
    }

    /// The same, on the overview path's hand decoder.
    #[test]
    fn overview_cloud_lifecycle() {
        let o: Overview = serde_json::from_str(
            r#"{"sheds":[
                {"name":"cloudy","status":"stopping","backend":"remote"},
                {"name":"full-zone","status":"stopped","status_reason":"capacity"},
                {"name":"nulled","status":"starting","status_reason":null},
                {"name":"odd","status":"running","status_reason":7},
                {"name":"rebooting","status":"rebooting"}
            ]}"#,
        )
        .unwrap();
        let by = |n: &str| &o.sheds.iter().find(|s| s.shed.name == n).unwrap().shed;
        assert_eq!(by("cloudy").status, ShedStatus::Stopping);
        assert_eq!(by("cloudy").status_reason, None); // absent
        assert_eq!(by("full-zone").status, ShedStatus::Stopped);
        assert_eq!(by("full-zone").status_reason.as_deref(), Some("capacity"));
        assert_eq!(by("nulled").status, ShedStatus::Starting);
        assert_eq!(by("nulled").status_reason, None); // explicit null
        assert_eq!(by("odd").status_reason, None); // wrong type degrades
        assert_eq!(by("rebooting").status, ShedStatus::Unknown);
    }

    #[test]
    fn sheds_null_and_omitted_decode_to_empty() {
        let w: ShedList = serde_json::from_str(r#"{"sheds": null}"#).unwrap();
        assert!(w.sheds.is_empty());
        let w: ShedList = serde_json::from_str(r#"{}"#).unwrap();
        assert!(w.sheds.is_empty());
    }

    #[test]
    fn active_namespaces_null_and_present() {
        let s: Shed = serde_json::from_str(r#"{"name":"x","active_namespaces":null}"#).unwrap();
        assert!(s.active_namespaces.is_empty()); // null -> []
        let s: Shed =
            serde_json::from_str(r#"{"name":"x","active_namespaces":["ssh-agent","aws"]}"#)
                .unwrap();
        assert_eq!(s.active_namespaces, vec!["ssh-agent", "aws"]);
    }

    #[test]
    fn image_enriched_fixture() {
        let img: ShedImage =
            serde_json::from_str(include_str!("../../fixtures/image_enriched.json")).unwrap();
        assert_eq!(img.alias.as_deref(), Some("base"));
        assert!(img.is_default);
        assert!(img.cached);
        assert!(!img.in_use);
        assert_eq!(img.docker_ref.as_deref(), Some("ghcr.io/x/base:v1"));
        assert_eq!(img.size_bytes, 1073741824);
    }

    #[test]
    fn image_lenient_pre_v061() {
        // Older server: no alias / is_default -> defaults, not an error.
        let img: ShedImage =
            serde_json::from_str(r#"{"name":"base","source":"config","cached":true}"#).unwrap();
        assert_eq!(img.alias, None);
        assert!(!img.is_default);
        assert!(img.cached);
        assert_eq!(img.size_bytes, 0);
    }

    #[test]
    fn image_absent_name_sentinel() {
        let img: ShedImage = serde_json::from_str(r#"{"cached":true}"#).unwrap();
        assert_eq!(img.name, "?");
    }

    #[test]
    fn images_null_decodes_to_empty() {
        let w: ImageList = serde_json::from_str(r#"{"images": null}"#).unwrap();
        assert!(w.images.is_empty());
    }

    #[test]
    fn system_df_fixture() {
        let df: SystemDiskUsage =
            serde_json::from_str(include_str!("../../fixtures/system_df.json")).unwrap();
        assert_eq!(df.server_name.as_deref(), Some("mini2"));
        assert_eq!(df.images.len(), 1);
        assert_eq!(df.images[0].name, "full");
        assert_eq!(df.images[0].size.logical_bytes, 1073741824);
        assert_eq!(df.sheds.len(), 1);
        assert!(df.orphans.is_empty());
        assert_eq!(df.totals.all.logical_bytes, 1073743872);
    }

    #[test]
    fn system_df_defaults_and_null_arrays() {
        let df: SystemDiskUsage = serde_json::from_str(r#"{}"#).unwrap();
        assert!(df.images.is_empty());
        assert!(df.sheds.is_empty());
        assert_eq!(df.totals.all.logical_bytes, 0);
        let df: SystemDiskUsage =
            serde_json::from_str(r#"{"images":null,"sheds":null,"orphans":null}"#).unwrap();
        assert!(df.images.is_empty());
    }

    #[test]
    fn disk_entry_absent_name_and_size() {
        let e: DiskEntry = serde_json::from_str(r#"{}"#).unwrap();
        assert_eq!(e.name, "?");
        assert_eq!(e.size, DiskSize::default());
    }

    #[test]
    fn egress_profiles_fixture() {
        let profiles: Vec<EgressProfileInfo> =
            serde_json::from_str(include_str!("../../fixtures/egress_profiles.json")).unwrap();
        assert_eq!(profiles.len(), 2);
        assert_eq!(profiles[0].name, "default");
        assert_eq!(profiles[0].source, "config");
        assert_eq!(profiles[0].profile.mode.as_deref(), Some("audit"));
        assert_eq!(
            profiles[0].profile.allow.as_deref(),
            Some(["*.github.com".to_string()].as_slice())
        );
        assert_eq!(profiles[1].source, "user");
        assert_eq!(profiles[1].profile.mode, None); // omitted
    }

    // ---- sessions read-plane ----

    #[test]
    fn sessions_response_rc_enriched_and_plain_rows() {
        // One plain tmux row (no rc block, omitempty fields absent) + one
        // rc-enriched row carrying the Phase C activity fields.
        let r: SessionsResponse = serde_json::from_str(
            r#"{"sessions":[
                {"name":"default","shed_name":"proj",
                 "created_at":"2026-06-19T18:52:00Z","attached":true},
                {"name":"rc-abc234","shed_name":"proj",
                 "created_at":"2026-06-19T18:53:00Z","attached":false,
                 "window_count":2,
                 "rc":{"kind":"claude-rc","state":"ready","managed":true,
                       "display_name":"proj/abc234",
                       "url":"https://claude.ai/code/session_abc234",
                       "created_by":"shed-mobile/1.0",
                       "activity":"working",
                       "activity_at":"2026-06-19T18:54:12Z",
                       "last_message":"Running the test suite now."}}
            ]}"#,
        )
        .unwrap();
        assert_eq!(r.sessions.len(), 2);
        assert!(r.warnings.is_empty()); // absent → []
        let plain = &r.sessions[0];
        assert_eq!(plain.name, "default");
        assert_eq!(plain.shed_name, "proj");
        assert!(plain.attached);
        assert_eq!(plain.window_count, 0); // omitempty absent → 0
        assert_eq!(plain.server_name, None); // HTTP handlers leave it empty
        assert!(plain.rc.is_none());
        let rc_row = &r.sessions[1];
        assert_eq!(rc_row.window_count, 2);
        assert_eq!(rc_row.created_at.as_deref(), Some("2026-06-19T18:53:00Z"));
        let rc = rc_row.rc.as_ref().unwrap();
        assert_eq!(rc.kind.as_deref(), Some("claude-rc"));
        assert_eq!(rc.state.as_deref(), Some("ready"));
        assert!(rc.managed);
        assert_eq!(rc.display_name.as_deref(), Some("proj/abc234"));
        assert_eq!(rc.activity.as_deref(), Some("working"));
        assert_eq!(rc.activity_at.as_deref(), Some("2026-06-19T18:54:12Z"));
        assert_eq!(
            rc.last_message.as_deref(),
            Some("Running the test suite now.")
        );
    }

    #[test]
    fn sessions_response_null_and_absent_lists() {
        let r: SessionsResponse =
            serde_json::from_str(r#"{"sessions":null,"warnings":null}"#).unwrap();
        assert!(r.sessions.is_empty());
        assert!(r.warnings.is_empty());
        let r: SessionsResponse = serde_json::from_str("{}").unwrap();
        assert!(r.sessions.is_empty());
        // A degraded shed: rc rows WITHOUT an rc block + a warnings entry.
        let r: SessionsResponse = serde_json::from_str(
            r#"{"sessions":[{"name":"rc-abc","shed_name":"proj",
                             "created_at":"2026-01-01T00:00:00Z","attached":false}],
                "warnings":["proj: rc enrichment degraded"]}"#,
        )
        .unwrap();
        assert!(r.sessions[0].rc.is_none());
        assert_eq!(r.warnings, ["proj: rc enrichment degraded"]);
    }

    #[test]
    fn session_rc_sparse_block_defaults() {
        // A legacy/unmanaged rc row: bare block → every optional None,
        // managed false (the only non-omitempty Go field, still defensive).
        let s: Session =
            serde_json::from_str(r#"{"name":"rc-old","shed_name":"p","rc":{}}"#).unwrap();
        let rc = s.rc.as_ref().unwrap();
        assert_eq!(rc.kind, None);
        assert_eq!(rc.state, None);
        assert!(!rc.managed);
        assert_eq!(rc.activity, None);
        assert_eq!(rc.last_message, None);
        assert_eq!(s.created_at, None); // defensive, though Go always emits it
    }

    // ---- overview (golden fixture; assertions ported from mobile's
    // overview_test.dart:38-116 — the fetchOverview HTTP cases land with the
    // Client method) ----

    fn golden_overview() -> Overview {
        serde_json::from_str(include_str!("../../fixtures/overview.json")).unwrap()
    }

    fn proj(o: &Overview) -> &OverviewShed {
        o.sheds.iter().find(|s| s.shed.name == "proj").unwrap()
    }

    #[test]
    fn overview_server_block_version_and_features() {
        let o = golden_overview();
        assert_eq!(o.server.version, "0.8.0");
        assert!(o.server.has_feature(FEATURE_OVERVIEW));
        assert!(!o.server.has_feature("nope"));
    }

    #[test]
    fn overview_activity_flows_through_rc_block_into_session() {
        let o = golden_overview();
        let claude = proj(&o)
            .sessions
            .iter()
            .find(|s| s.slug == "abc234")
            .unwrap();
        assert_eq!(claude.activity, Some(RcActivity::Working));
        assert_eq!(claude.activity_at.as_deref(), Some("2026-06-19T18:54:12Z"));
        assert_eq!(
            claude.last_message.as_deref(),
            Some("Running the test suite now.")
        );
    }

    #[test]
    fn overview_session_carries_lane_and_absent_lane_reads_as_tui() {
        // The enriched row projects `lane` through (server `toSessionRC`); the
        // fixture's second shed session comes from an OLDER guest whose payload
        // has no lane at all — it must read as "tui" without inventing a value.
        let o = golden_overview();
        let p = proj(&o);
        assert_eq!(p.sessions[0].lane.as_deref(), Some(LANE_TUI));
        assert_eq!(p.sessions[0].lane_or_tui(), LANE_TUI);
        assert_eq!(p.sessions[1].lane, None);
        assert_eq!(p.sessions[1].lane_or_tui(), LANE_TUI);
        // A future lane value is carried verbatim, never coerced.
        let o: Overview = serde_json::from_str(
            r#"{"sheds":[{"name":"web","status":"running","sessions":[
                {"name":"rc-s1","rc":{"kind":"codex","state":"ready","lane":"structured"}},
                {"name":"rc-s2","rc":{"kind":"codex","state":"ready","lane":"  "}}]}]}"#,
        )
        .unwrap();
        assert_eq!(o.sheds[0].sessions[0].lane.as_deref(), Some("structured"));
        assert_eq!(o.sheds[0].sessions[0].lane_or_tui(), "structured");
        assert_eq!(o.sheds[0].sessions[1].lane, None); // blank → absent
        assert_eq!(o.sheds[0].sessions[1].lane_or_tui(), LANE_TUI);
    }

    #[test]
    fn overview_df_block_parses() {
        let o = golden_overview();
        let df = o.df.as_ref().unwrap();
        assert_eq!(df.server_name.as_deref(), Some("test-server"));
        assert_eq!(df.backend.as_deref(), Some("vz"));
        assert_eq!(df.totals.all.physical_bytes, 14506430464);
    }

    #[test]
    fn overview_running_shed_carries_only_rc_enriched_sessions() {
        let o = golden_overview();
        let p = proj(&o);
        assert_eq!(p.shed.status, ShedStatus::Running);
        // The plain "default" tmux row (no rc block) is dropped; the two rc
        // rows remain, with slug/tmux derived from the session name.
        let slugs: Vec<&str> = p.sessions.iter().map(|s| s.slug.as_str()).collect();
        assert_eq!(slugs, ["abc234", "cdx777"]);
        let claude = &p.sessions[0];
        assert_eq!(claude.tmux_session, "rc-abc234");
        assert_eq!(claude.kind, RcKind::ClaudeRc);
        assert_eq!(claude.state, RcState::Ready);
        assert!(claude.managed);
        assert!(claude.url.is_some());
        assert_eq!(claude.display_name, "proj/abc234");
        // created_at comes from the OUTER session row (the rc block has none).
        assert_eq!(claude.created_at.as_deref(), Some("2026-06-19T18:53:00Z"));
        // Second row: the codex kind, needs-auth, no url.
        let codex = &p.sessions[1];
        assert_eq!(codex.kind, RcKind::Other("codex".into()));
        assert_eq!(codex.state, RcState::NeedsAuth);
        assert!(codex.url.is_none());
    }

    #[test]
    fn overview_stopped_shed_has_no_sessions() {
        let o = golden_overview();
        let asleep = o.sheds.iter().find(|s| s.shed.name == "asleep").unwrap();
        assert_eq!(asleep.shed.status, ShedStatus::Stopped);
        assert!(asleep.sessions.is_empty());
    }

    #[test]
    fn overview_session_display_name_fallback_and_slug_derivation() {
        // An rc block with no display_name falls back to `<shed>/<slug>`; a
        // session name without the `rc-` prefix keeps the full name as slug.
        let o: Overview = serde_json::from_str(
            r#"{"sheds":[{"name":"web","status":"running","sessions":[
                {"name":"rc-abc","created_at":"2026-01-01T00:00:00Z",
                 "rc":{"kind":"shell","state":"ready","managed":false}},
                {"name":"plain","rc":{"kind":"shell","state":"ready","managed":true}}
            ]}]}"#,
        )
        .unwrap();
        let s = &o.sheds[0].sessions[0];
        assert_eq!(s.slug, "abc");
        assert_eq!(s.tmux_session, "rc-abc");
        assert_eq!(s.display_name, "web/abc");
        assert_eq!(s.created_at.as_deref(), Some("2026-01-01T00:00:00Z"));
        assert_eq!(s.host, ""); // absent on the wire; client stamps it
        assert!(!s.managed);
        let p = &o.sheds[0].sessions[1];
        assert_eq!(p.slug, "plain"); // no rc- prefix to strip
        assert_eq!(p.tmux_session, "plain");
        assert_eq!(p.display_name, "web/plain");
    }

    #[test]
    fn overview_session_decode_is_field_tolerant_forward_compat() {
        // A newer server enriching a session with a future enum value (or a
        // sparse rc block) must NOT vanish the session on this client: each
        // field degrades independently (rc_models.dart:245-273) — unknown
        // state → Starting, missing managed → false, missing kind → the
        // preserved-raw unknown kind — the row is always present.
        let o: Overview = serde_json::from_str(
            r#"{"sheds":[{"name":"web","status":"running","sessions":[
                {"name":"rc-fut1","rc":{"kind":"codex","state":"some-future-state"}},
                {"name":"rc-bare","rc":{}}
            ]}]}"#,
        )
        .unwrap();
        let sessions = &o.sheds[0].sessions;
        assert_eq!(sessions.len(), 2); // nothing dropped
        let fut = &sessions[0];
        assert_eq!(fut.slug, "fut1");
        assert_eq!(fut.kind, RcKind::Other("codex".into()));
        assert_eq!(fut.state, RcState::Starting); // unknown state → transient
        assert!(!fut.managed); // absent → false
        assert_eq!(fut.workdir, None); // absent → None, NO DEFAULT_WORKDIR injection
        let bare = &sessions[1];
        assert_eq!(bare.state, RcState::Starting); // absent state → transient
        assert_eq!(bare.kind, RcKind::Other(String::new())); // absent kind tolerated
        assert!(!bare.kind.is_known());
        assert_eq!(bare.display_name, "web/bare");
    }

    #[test]
    fn overview_session_workdir_stays_none_and_last_message_is_cf_stripped() {
        // workdir: missing/blank → None (rc_models.dart:261) — the
        // DEFAULT_WORKDIR fallback belongs to the from_dto stdout path only.
        // last_message: _cleanDisplay (rc_models.dart:269-271) — trim, strip
        // Unicode-Cf (bidi-override spoofers), trim; Cf-only → None.
        let o: Overview = serde_json::from_str(
            "{\"sheds\":[{\"name\":\"web\",\"status\":\"running\",\"sessions\":[
                {\"name\":\"rc-a\",\"rc\":{\"kind\":\"shell\",\"state\":\"ready\",\"managed\":true,
                 \"workdir\":\"  \",\"last_message\":\"safe\u{202E}txt\"}},
                {\"name\":\"rc-b\",\"rc\":{\"kind\":\"shell\",\"state\":\"ready\",\"managed\":true,
                 \"workdir\":\"/w\",\"last_message\":\"\u{202E}\"}}
            ]}]}",
        )
        .unwrap();
        let a = &o.sheds[0].sessions[0];
        assert_eq!(a.workdir, None); // blank → None
        assert_eq!(a.last_message.as_deref(), Some("safetxt"));
        let b = &o.sheds[0].sessions[1];
        assert_eq!(b.workdir.as_deref(), Some("/w"));
        assert_eq!(b.last_message, None); // Cf-only → None
    }

    #[test]
    fn overview_shed_row_missing_name_defaults_to_question_mark() {
        // A malformed shed row must not vanish: missing/blank name → "?"
        // (Shed.fromJson defaults, shed_dtos.dart:31-41).
        let o: Overview = serde_json::from_str(r#"{"sheds":[{"status":"running"}]}"#).unwrap();
        assert_eq!(o.sheds.len(), 1);
        assert_eq!(o.sheds[0].shed.name, "?");
        assert_eq!(o.sheds[0].shed.status, ShedStatus::Running);
        // Wrong-typed status degrades to Unknown, row still present.
        let o: Overview = serde_json::from_str(r#"{"sheds":[{"name":"x","status":42}]}"#).unwrap();
        assert_eq!(o.sheds[0].shed.name, "x");
        assert_eq!(o.sheds[0].shed.status, ShedStatus::Unknown);
    }

    #[test]
    fn overview_df_with_malformed_totals_zero_defaults() {
        // A malformed field inside df degrades THAT field — never the whole
        // disk block (SystemDiskUsage/DiskTotals.fromJson, shed_dtos.dart:345-392).
        let o: Overview =
            serde_json::from_str(r#"{"df":{"server_name":"srv","totals":null}}"#).unwrap();
        let df = o.df.as_ref().expect("df present despite null totals");
        assert_eq!(df.server_name.as_deref(), Some("srv"));
        assert_eq!(df.totals, DiskTotals::default()); // zero-defaulted
                                                      // Wrong-typed nested pieces zero-default too.
        let o: Overview = serde_json::from_str(
            r#"{"df":{"server_name":"srv","backend":7,
                 "totals":{"all":{"physical_bytes":"x","logical_bytes":9}},
                 "images":"nope"}}"#,
        )
        .unwrap();
        let df = o.df.as_ref().unwrap();
        assert_eq!(df.backend, None);
        assert_eq!(df.totals.all.physical_bytes, 0);
        assert_eq!(df.totals.all.logical_bytes, 9);
        assert!(df.images.is_empty());
    }

    #[test]
    fn overview_wrong_typed_blocks_degrade_to_defaults() {
        // Every sub-block degrades independently — a wrong-typed block is a
        // tolerated partial, never a decode error.
        let o: Overview =
            serde_json::from_str(r#"{"server":42,"df":"nope","sheds":{"a":1},"warnings":"w"}"#)
                .unwrap();
        assert_eq!(o.server, OverviewServer::default());
        assert!(o.df.is_none());
        assert!(o.sheds.is_empty());
        assert!(o.warnings.is_empty());
        // Non-string feature/warning elements are skipped; non-map session
        // rows and rc blocks are skipped too.
        let o: Overview = serde_json::from_str(
            r#"{"server":{"version":"  ","features":["a",1,"b"]},
                "warnings":["w1",2,"w2"],
                "sheds":[{"name":"web","status":"running",
                          "sessions":[42,{"name":"x"},{"name":"y","rc":"nope"}]}]}"#,
        )
        .unwrap();
        assert_eq!(o.server.version, ""); // blank → ""
        assert_eq!(o.server.features, ["a", "b"]);
        assert_eq!(o.warnings, ["w1", "w2"]);
        assert!(o.sheds[0].sessions.is_empty());
    }

    #[test]
    fn overview_null_blocks_degrade_to_defaults() {
        let o: Overview =
            serde_json::from_str(r#"{"server":null,"df":null,"sheds":null,"warnings":null}"#)
                .unwrap();
        assert_eq!(o.server, OverviewServer::default());
        assert!(o.df.is_none());
        assert!(o.sheds.is_empty());
        assert!(o.warnings.is_empty());
        // Empty and non-object bodies also yield the all-default snapshot.
        let o: Overview = serde_json::from_str("{}").unwrap();
        assert_eq!(o, Overview::default());
        let o: Overview = serde_json::from_str("[]").unwrap();
        assert_eq!(o, Overview::default());
    }

    // ---- credential sources (plan 035 §3.4) ----

    const SOURCE_A: &str = "0123456789abcdef0123456789abcdef";
    const SOURCE_B: &str = "fedcba9876543210fedcba9876543210";

    /// The full document the server answers decodes field for field, and the source is
    /// the machine named in the first signing slot.
    #[test]
    fn source_document_decodes_the_full_shape() {
        let doc: SourceDocument = serde_json::from_str(&format!(
            r#"{{"user":"ada",
                "binding":{{"delivery":[{{"kind":"machine","id":"{SOURCE_B}"}}],
                            "signing":[{{"kind":"machine","id":"{SOURCE_B}"}}],
                            "bound_at":"2026-11-16T10:00:00Z","bound_by":"ada","generation":3}},
                "machines":[
                  {{"id":"{SOURCE_A}","name":"laptop","key_fingerprint":"SHA256:aa",
                    "first_seen":"2026-11-15T09:00:00Z","connected":false,"namespaces":[]}},
                  {{"id":"{SOURCE_B}","name":"desk","key_fingerprint":"SHA256:bb",
                    "first_seen":"2026-11-14T09:00:00Z","connected":true,
                    "namespaces":["ssh-agent","docker-credentials"]}}]}}"#
        ))
        .unwrap();
        assert_eq!(doc.user, "ada");
        let b = doc.binding.as_ref().unwrap();
        assert_eq!(b.generation, 3);
        assert_eq!(b.bound_by, "ada");
        assert_eq!(b.bound_at, "2026-11-16T10:00:00Z");
        assert_eq!(b.delivery[0].id, SOURCE_B);
        assert_eq!(doc.source_machine_id(), Some(SOURCE_B));
        let desk = doc.machine(SOURCE_B).unwrap();
        assert_eq!(desk.name, "desk");
        assert!(desk.connected);
        assert_eq!(desk.namespaces, ["ssh-agent", "docker-credentials"]);
        assert_eq!(doc.machine(SOURCE_A).unwrap().key_fingerprint, "SHA256:aa");
    }

    /// A user with no binding yet: `binding: null`, and no source.
    #[test]
    fn source_document_with_no_binding_has_no_source() {
        let doc: SourceDocument =
            serde_json::from_str(r#"{"user":"","binding":null,"machines":[]}"#).unwrap();
        assert_eq!(doc.binding, None);
        assert_eq!(doc.source_machine_id(), None);
        // Missing entirely is the same.
        let doc: SourceDocument = serde_json::from_str("{}").unwrap();
        assert_eq!(doc, SourceDocument::default());
    }

    /// A later server may add fields, drop optional ones or send a type this client did
    /// not expect: none of that fails the document, and one bad machine entry costs only
    /// that entry.
    #[test]
    fn source_document_decodes_tolerantly() {
        let doc: SourceDocument = serde_json::from_str(&format!(
            r#"{{"user":7,"future":{{"x":1}},
                "binding":{{"signing":[{{"kind":"machine","id":"{SOURCE_B}","weight":2}}],
                            "generation":"three","delivery":null,"later":true}},
                "machines":[42,
                  {{"id":"{SOURCE_B}","name":"desk","connected":"yes","namespaces":null,
                    "last_seen":"2026-11-16T10:00:00Z"}}]}}"#
        ))
        .unwrap();
        assert_eq!(doc.user, "");
        let b = doc.binding.as_ref().unwrap();
        assert_eq!(b.generation, 0);
        assert!(b.delivery.is_empty());
        assert_eq!(doc.source_machine_id(), Some(SOURCE_B));
        assert_eq!(doc.machines.len(), 1, "the non-object entry is dropped");
        let m = &doc.machines[0];
        assert_eq!(m.name, "desk");
        assert!(!m.connected, "a wrongly typed flag is false, not an error");
        assert!(m.namespaces.is_empty());
        // A wrongly typed binding is no binding; a wrongly typed list is empty.
        let doc: SourceDocument =
            serde_json::from_str(r#"{"binding":"bound","machines":{"a":1}}"#).unwrap();
        assert_eq!(doc.binding, None);
        assert!(doc.machines.is_empty());
    }

    /// Only a slot that names a machine is the source: another kind (a later cut's) or
    /// an empty id is none.
    #[test]
    fn source_machine_id_needs_a_machine_ref() {
        let doc: SourceDocument =
            serde_json::from_str(r#"{"binding":{"signing":[{"kind":"service","id":"x"}]}}"#)
                .unwrap();
        assert_eq!(doc.source_machine_id(), None);
        let doc: SourceDocument =
            serde_json::from_str(r#"{"binding":{"signing":[{"kind":"machine","id":""}]}}"#)
                .unwrap();
        assert_eq!(doc.source_machine_id(), None);
    }

    /// A malformed slot keeps its position: with `[null, {machine B}]` the first slot
    /// names nothing, so there is no source — B was never `signing[0]`. The same for
    /// any entry that does not decode, and for delivery.
    #[test]
    fn a_malformed_first_slot_is_no_source_not_the_next_one() {
        for bad in [
            "null",
            "42",
            r#""machine""#,
            "[]",
            r#"{"kind":["machine"]}"#,
        ] {
            let doc: SourceDocument = serde_json::from_str(&format!(
                r#"{{"binding":{{"signing":[{bad},{{"kind":"machine","id":"{SOURCE_B}"}}],
                                "delivery":[{bad},{{"kind":"machine","id":"{SOURCE_B}"}}]}}}}"#
            ))
            .unwrap();
            assert_eq!(doc.source_machine_id(), None, "{bad}");
            let b = doc.binding.as_ref().unwrap();
            assert_eq!(b.signing.len(), 2, "{bad}: slots keep their positions");
            assert_eq!(b.signing[0], SourceRef::default(), "{bad}");
            assert_eq!(b.signing[1].id, SOURCE_B, "{bad}");
            assert_eq!(b.delivery.len(), 2, "{bad}");
            assert_eq!(b.delivery[1].id, SOURCE_B, "{bad}");
        }
    }

    /// Text from another machine loses its control and format characters (a bell, a
    /// newline, a bidirectional override, a zero-width space) and is cut on a character
    /// boundary.
    #[test]
    fn display_source_text_cleans_and_bounds() {
        assert_eq!(
            display_source_text("desk\u{202E}\u{7}\n\u{200B}evil", 64),
            "deskevil"
        );
        assert_eq!(display_source_text("héllo wörld", 64), "héllo wörld");
        // é is two bytes: a cut never splits it.
        assert_eq!(display_source_text("aé", 2), "a");
        assert_eq!(display_source_text(&"x".repeat(100), 64).len(), 64);
    }

    #[test]
    fn valid_machine_id_is_32_lowercase_hex() {
        assert!(valid_machine_id(SOURCE_A));
        assert!(!valid_machine_id(&SOURCE_A.to_uppercase()));
        assert!(!valid_machine_id(&SOURCE_A[1..]));
        assert!(!valid_machine_id("0123456789abcdef-0123456789abcde"));
        assert!(!valid_machine_id(""));
    }
}
