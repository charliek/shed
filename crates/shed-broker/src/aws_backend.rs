//! The AWS credential backend — a faithful Rust port of
//! `cmd/shed-host-agent/aws_backend.go`.
//!
//! Two vending modes (selected per shed by [`crate::config::AwsConfig::resolve`]):
//!
//! * **assume-role** — `sts:AssumeRole` from the configured source profile into the
//!   resolved role, cached per `server/shed` until near expiry. The STS call goes
//!   through the [`AssumeRoler`] seam so unit tests inject fakes; the real
//!   [`SdkAssumeRoler`] builds its SDK client **lazily on first use**, so a
//!   passthrough-only config never loads the AssumeRole credential chain.
//! * **passthrough** — vends the source profile's existing session credentials
//!   directly, re-reading the shared credentials file on every request (no cache) so
//!   a fresh `aws sso login` is picked up immediately. This path is **hand-rolled**
//!   (a tiny INI reader + the expiry-hint scan) — no SDK — which keeps the only
//!   differentially-tested path SDK-free and makes Go's "profile present but no keys"
//!   branch naturally reachable.
//!
//! **Divergence from Go, documented:** Go's `config.LoadDefaultConfig` returns an
//! error (surfaced as `loading AWS config for profile "<p>": <err>`); the Rust
//! `aws_config::from_env()...load()` is **infallible** (credential resolution is
//! deferred to first use), so a bad source profile surfaces instead as an
//! `sts:AssumeRole failed for <arn>: <err>` from the `send()`. The lazy-once client
//! build is preserved (the behavioural property the `passthrough_only_never_builds_client`
//! test pins); there is simply no load-time error to latch.
//!
//! **Wiring:** this module is reached through the aws-credentials bus handler
//! (`bus.rs`) + `main.rs`, which construct the backend via [`new_sts_backend`] and
//! dispatch `get_credentials`/`status` to it.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;

use crate::bus::BusLog;
use crate::config::{
    parse_go_duration_nanos, user_home_dir, AwsConfig, ResolvedAws, AWS_MODE_PASSTHROUGH,
};

/// A cached (or freshly vended) set of temporary AWS credentials. For passthrough
/// mode `expiration` may be `None` when the source profile carries no expiry hint
/// (the guest then discovers expiry on a 403). `expiration` is unix seconds; `None`
/// mirrors Go's zero `time.Time`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CachedCreds {
    pub access_key_id: String,
    pub secret_access_key: String,
    pub session_token: String,
    pub expiration: Option<i64>,
}

/// The host-side AWS credential operations (mirror Go's `AWSBackend` interface).
/// Async + `&self` so the bus handler can hold it as `Arc<dyn AwsBackend>`.
/// [`Self::status`] NEVER errors — a missing/unreadable credentials file degrades
/// to a `None` expiry rather than an error the status path can't return.
#[async_trait]
pub trait AwsBackend: Send + Sync {
    /// Return temporary credentials for `shed` on `server`.
    async fn get_credentials(&self, server: &str, shed: &str) -> Result<CachedCreds, String>;

    /// Return the role label and cache/expiry instant for `shed` on `server`.
    fn status(&self, server: &str, shed: &str) -> (String, Option<i64>);
}

/// The inputs to one `sts:AssumeRole` call (mirror `sts.AssumeRoleInput`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AssumeRoleParams {
    pub role_arn: String,
    pub session_name: String,
    pub duration_seconds: i32,
}

/// The credentials one `sts:AssumeRole` returns (mirror `sts.Credentials`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AssumedCreds {
    pub access_key_id: String,
    pub secret_access_key: String,
    pub session_token: String,
    pub expiration: Option<i64>,
}

/// The STS seam: performs one `AssumeRole`. A trait so unit tests inject fakes
/// without a live STS endpoint (the assume-role path has no live differential —
/// it is owned by unit tests + goldens). `Ok(Some(creds))` = credentials returned;
/// **`Ok(None)`** = the call succeeded but the SDK's optional credential fields were
/// absent → the backend maps this to `sts:AssumeRole returned nil credentials for
/// <arn>` (never a panic); `Err(msg)` = the call itself failed (the backend wraps it
/// as `sts:AssumeRole failed for <arn>: <msg>`).
#[async_trait]
pub trait AssumeRoler: Send + Sync {
    async fn assume_role(&self, input: AssumeRoleParams) -> Result<Option<AssumedCreds>, String>;
}

/// `parse_duration_or` parses a Go-`time.ParseDuration` string, falling back to
/// `default` (with a warning) when empty, unparseable, or non-positive. A faithful
/// port of `watcher.go:parseDurationOr`, reusing the crate's Go-duration parser.
/// In Go this helper lives in `watcher.go` (discovery) — when the discovery port
/// lands, move it to a shared util module rather than importing it from here.
pub fn parse_duration_or(raw: &str, default: Duration, label: &str, log: &dyn BusLog) -> Duration {
    match parse_go_duration_nanos(raw) {
        Some(nanos) if nanos > 0 => Duration::from_nanos(nanos as u64),
        _ => {
            log.warn(&format!(
                "invalid duration, using default: field={label} value={raw:?} default={default:?}"
            ));
            default
        }
    }
}

/// The `server/shed` cache + session key (mirror `config.go:serverShedKey`): keying
/// by both keeps identical shed names on different servers isolated.
fn server_shed_key(server: &str, shed: &str) -> String {
    format!("{server}/{shed}")
}

/// The concrete AWS backend (mirror Go's `stsBackend`). The STS client itself lives
/// behind the [`AssumeRoler`] seam and is built lazily there, so a passthrough-only
/// config never loads the AssumeRole credential chain.
pub struct StsBackend {
    cfg: AwsConfig,
    refresh_before: Duration,
    session_dur: Duration,
    assume_roler: Arc<dyn AssumeRoler>,
    log: Arc<dyn BusLog>,
    cache: Mutex<HashMap<String, CachedCreds>>,
    /// Injectable wall clock (unix seconds) for the cache-window logic + session name.
    now_unix: Box<dyn Fn() -> i64 + Send + Sync>,
}

/// Build an AWS backend. Validates the duration knobs (warn + default on a bad value)
/// and reports whether AWS is configured at all; the STS client is created on first
/// AssumeRole use inside the [`SdkAssumeRoler`]. Mirror `NewSTSBackend`.
///
/// Unwired in this slice — the handler + `main.rs` construct it in commit 3 (the
/// same "ported, wired by a later slice" posture as `minter.rs::refresh_loop`).
pub fn new_sts_backend(cfg: AwsConfig, log: Arc<dyn BusLog>) -> Result<StsBackend, String> {
    if !cfg.enabled() {
        return Err("no AWS credentials configured (set aws.default_role, aws.mode: passthrough, or a per-server/shed role/mode)".to_string());
    }
    // NewSTSBackend uses time.ParseDuration directly (fall back only on a PARSE error,
    // not on non-positive — that stricter check is parse_duration_or, used by
    // resolve_session_dur). Mirror that split faithfully.
    let refresh_before = parse_ctor_duration(&cfg.cache_refresh_before);
    let session_dur = parse_ctor_duration(&cfg.session_duration);
    if refresh_before.is_none() {
        log.warn(&format!(
            "invalid cache_refresh_before, using default: value={:?} default=5m",
            cfg.cache_refresh_before
        ));
    }
    if session_dur.is_none() {
        log.warn(&format!(
            "invalid session_duration, using default: value={:?} default=1h",
            cfg.session_duration
        ));
    }
    let refresh_before = refresh_before.unwrap_or(Duration::from_secs(300));
    let session_dur = session_dur.unwrap_or(Duration::from_secs(3600));
    let source_profile = cfg.source_profile.clone();
    log.info(&format!(
        "AWS backend initialized: profile={source_profile:?} default_role={:?} session_duration={session_dur:?} cache_refresh_before={refresh_before:?}",
        cfg.default_role,
    ));
    Ok(StsBackend {
        refresh_before,
        session_dur,
        assume_roler: Arc::new(SdkAssumeRoler::new(&source_profile)),
        log,
        cache: Mutex::new(HashMap::new()),
        now_unix: Box::new(crate::status::now_unix),
        cfg,
    })
}

/// Constructor-time duration parse: `Some(dur)` on a valid non-negative value,
/// `None` on a parse error (the caller then warns + uses the default). Unlike
/// [`parse_duration_or`] a zero value is accepted (mirror Go's `time.ParseDuration`
/// in `NewSTSBackend`, which falls back only on `err != nil`); a negative value —
/// which Go would keep — falls back here since a `Duration` cannot be negative.
fn parse_ctor_duration(raw: &str) -> Option<Duration> {
    match parse_go_duration_nanos(raw) {
        Some(nanos) if nanos >= 0 => Some(Duration::from_nanos(nanos as u64)),
        _ => None,
    }
}

impl StsBackend {
    /// Session duration for a resolved policy, falling back to the backend default
    /// when the override is unset or invalid (mirror `resolveSessionDur`).
    fn resolve_session_dur(&self, resolved: &ResolvedAws) -> Duration {
        if resolved.session_duration.is_empty() {
            return self.session_dur;
        }
        parse_duration_or(
            &resolved.session_duration,
            self.session_dur,
            "session_duration",
            self.log.as_ref(),
        )
    }

    /// Vend the source profile's existing session credentials directly, re-reading
    /// the shared files on every request (no cache, no lock). Mirror
    /// `getPassthroughCreds` — but hand-rolled over the INI reader, no SDK.
    fn get_passthrough_creds(&self, server: &str, shed: &str) -> Result<CachedCreds, String> {
        let profile = &self.cfg.source_profile;
        let creds_path = shared_credentials_path();
        let cfg_path = shared_config_path();

        // Go's LoadSharedConfigProfile merges the config + credentials files with the
        // CREDENTIALS file taking precedence for the static-credential keys. We mirror
        // that pragmatically: read the profile from the config file as the base, then
        // overlay the credentials file (credentials wins) for the three keys we need.
        // The expiry hint is read from the credentials file ONLY (Go parity — the SDK
        // never surfaces it, so parse_session_expiry hand-scans that file alone).
        let cfg_section = read_ini_profile(&cfg_path, profile);
        let creds_section = read_ini_profile(&creds_path, profile);

        // A profile present in NEITHER file → Go's LoadSharedConfigProfile errors.
        // Outer wrap is Go-parity; the inner text is OURS (documented divergence — Go
        // wraps the SDK's error text there, asserted as prefix + profile-name).
        if cfg_section.is_none() && creds_section.is_none() {
            return Err(format!(
                "passthrough: loading profile \"{profile}\" from {}: profile not found in the shared credentials or config file",
                creds_path.display()
            ));
        }

        let mut kv = cfg_section.unwrap_or_default();
        if let Some(creds_kv) = creds_section {
            for (k, v) in creds_kv {
                kv.insert(k, v); // credentials file overrides config file
            }
        }
        let access = kv.get("aws_access_key_id").cloned().unwrap_or_default();
        let secret = kv.get("aws_secret_access_key").cloned().unwrap_or_default();
        let token = kv.get("aws_session_token").cloned().unwrap_or_default();

        // Exact Go byte strings (aws_backend.go:200-204).
        if access.is_empty() || secret.is_empty() {
            return Err(format!(
                "passthrough: profile \"{profile}\" in {} has no static credentials; run your SSO/SAML login (e.g. `aws sso login`) to refresh",
                creds_path.display()
            ));
        }
        if token.is_empty() {
            return Err(format!(
                "passthrough: profile \"{profile}\" has no aws_session_token; passthrough expects temporary SSO/SAML session credentials, not long-lived keys"
            ));
        }

        let expiration = parse_session_expiry(&creds_path, profile);
        self.log.info(&format!(
            "vending passthrough credentials: server={server:?} shed={shed:?} profile={profile:?} expires={}",
            expiry_label(expiration)
        ));
        Ok(CachedCreds {
            access_key_id: access,
            secret_access_key: secret,
            session_token: token,
            expiration,
        })
    }
}

#[async_trait]
impl AwsBackend for StsBackend {
    async fn get_credentials(&self, server: &str, shed: &str) -> Result<CachedCreds, String> {
        let resolved = self.cfg.resolve(server, shed);

        if resolved.mode == AWS_MODE_PASSTHROUGH {
            return self.get_passthrough_creds(server, shed);
        }

        let role_arn = resolved.role.clone();
        if role_arn.is_empty() {
            return Err(format!(
                "no role configured for shed \"{shed}\" on server \"{server}\""
            ));
        }

        let cache_key = server_shed_key(server, shed);
        let now = (self.now_unix)();

        // Cache hit iff the entry expires more than refresh_before from now.
        {
            let cache = self.cache.lock().unwrap();
            if let Some(cached) = cache.get(&cache_key) {
                if let Some(exp) = cached.expiration {
                    if exp - now > self.refresh_before.as_secs() as i64 {
                        self.log.debug(&format!(
                            "returning cached credentials: server={server:?} shed={shed:?} expires={exp}"
                        ));
                        return Ok(cached.clone());
                    }
                }
            }
        }

        let duration_seconds = self.resolve_session_dur(&resolved).as_secs() as i32;
        // Session names are keyed by server/shed/now so parallel sheds don't collide.
        let session_name = format!("shed-{server}-{shed}-{now}");

        let params = AssumeRoleParams {
            role_arn: role_arn.clone(),
            session_name: session_name.clone(),
            duration_seconds,
        };
        let assumed = match self.assume_roler.assume_role(params).await {
            Ok(Some(a)) => a,
            // SDK optional credential fields absent → the exact Go nil-creds error.
            Ok(None) => {
                return Err(format!(
                    "sts:AssumeRole returned nil credentials for {role_arn}"
                ))
            }
            Err(e) => return Err(format!("sts:AssumeRole failed for {role_arn}: {e}")),
        };

        let creds = CachedCreds {
            access_key_id: assumed.access_key_id,
            secret_access_key: assumed.secret_access_key,
            session_token: assumed.session_token,
            expiration: assumed.expiration,
        };
        let entry = creds.clone(); // clone outside the lock; the guard holds only the insert
        self.cache.lock().unwrap().insert(cache_key, entry);

        self.log.info(&format!(
            "assumed role: server={server:?} shed={shed:?} role={role_arn:?} session={session_name:?} expires={}",
            expiry_label(creds.expiration)
        ));
        Ok(creds)
    }

    fn status(&self, server: &str, shed: &str) -> (String, Option<i64>) {
        let resolved = self.cfg.resolve(server, shed);

        if resolved.mode == AWS_MODE_PASSTHROUGH {
            let role = format!("passthrough:{}", self.cfg.source_profile);
            // Scan the file directly (no token validation) so a missing/unreadable
            // file degrades to a None expiry rather than an error status can't return.
            let exp = parse_session_expiry(&shared_credentials_path(), &self.cfg.source_profile);
            return (role, exp);
        }

        let cache = self.cache.lock().unwrap();
        let expiration = cache
            .get(&server_shed_key(server, shed))
            .and_then(|cached| cached.expiration);
        (resolved.role, expiration)
    }
}

/// Render an expiration for logs, rendering `None` as `"none"` (mirror `expiryLabel`).
fn expiry_label(exp: Option<i64>) -> String {
    match exp {
        None => "none".to_string(),
        Some(unix) => aws_literal_z(unix),
    }
}

// ---------------------------------------------------------------------------
// Shared-file path resolution (mirror sharedCredentialsPath / sharedConfigPath)
// ---------------------------------------------------------------------------

/// The shared credentials file: `AWS_SHARED_CREDENTIALS_FILE` if set, else
/// `~/.aws/credentials` (Go's `config.DefaultSharedCredentialsFilename`).
fn shared_credentials_path() -> PathBuf {
    match std::env::var_os("AWS_SHARED_CREDENTIALS_FILE") {
        Some(p) if !p.is_empty() => PathBuf::from(p),
        _ => user_home_dir().join(".aws").join("credentials"),
    }
}

/// The shared config file: `AWS_CONFIG_FILE` if set, else `~/.aws/config` (Go's
/// `config.DefaultSharedConfigFilename`).
fn shared_config_path() -> PathBuf {
    match std::env::var_os("AWS_CONFIG_FILE") {
        Some(p) if !p.is_empty() => PathBuf::from(p),
        _ => user_home_dir().join(".aws").join("config"),
    }
}

// ---------------------------------------------------------------------------
// Hand-rolled INI reader + expiry scan (mirror parseSessionExpiry / parseExpiryValue)
// ---------------------------------------------------------------------------

/// Read the first `[profile]` section's `key = value` pairs from an INI-shaped file.
/// Returns `None` if the file is unreadable OR the section header never appears (so
/// the caller can distinguish "profile missing" from "profile present but empty");
/// `Some(map)` (possibly empty) when the section appears. Semantics match Go's
/// shared-file reader for our needs: `[profile <name>]`-prefix tolerance, `#`/`;`
/// comments, whitespace trimmed, **first key in line order wins** (and, across a
/// duplicate section header, the first section's keys win).
fn read_ini_profile(path: &Path, profile: &str) -> Option<HashMap<String, String>> {
    let data = std::fs::read_to_string(path).ok()?;
    let mut found = false;
    let mut in_section = false;
    let mut map: HashMap<String, String> = HashMap::new();
    for raw in data.split('\n') {
        let line = raw.trim();
        if line.is_empty() || line.starts_with('#') || line.starts_with(';') {
            continue;
        }
        if line.starts_with('[') && line.ends_with(']') {
            let name = line[1..line.len() - 1].trim();
            let name = name.strip_prefix("profile ").unwrap_or(name); // config-style header
            in_section = name == profile;
            if in_section {
                found = true;
            }
            continue;
        }
        if !in_section {
            continue;
        }
        if let Some((k, v)) = line.split_once('=') {
            // First key in line order wins (or_insert keeps the earliest).
            map.entry(k.trim().to_string())
                .or_insert_with(|| v.trim().to_string());
        }
    }
    if found {
        Some(map)
    } else {
        None
    }
}

/// Scan the credentials file's `[profile]` section for a session-expiry hint
/// (`aws_session_expiration` or `x_security_token_expires`). Returns `None` when the
/// file, profile, or key is missing or unparseable. Only the credentials file (bare
/// `[name]`) is scanned; hints in `~/.aws/config` are out of scope. A faithful port of
/// `parseSessionExpiry` (aws_backend.go:282-312): **first matching key in line order
/// within the first matching section wins.**
fn parse_session_expiry(creds_path: &Path, profile: &str) -> Option<i64> {
    let data = std::fs::read_to_string(creds_path).ok()?;
    let mut in_section = false;
    for raw in data.split('\n') {
        let line = raw.trim();
        if line.is_empty() || line.starts_with('#') || line.starts_with(';') {
            continue;
        }
        if line.starts_with('[') && line.ends_with(']') {
            let name = line[1..line.len() - 1].trim();
            let name = name.strip_prefix("profile ").unwrap_or(name);
            in_section = name == profile;
            continue;
        }
        if !in_section {
            continue;
        }
        let Some((key, val)) = line.split_once('=') else {
            continue;
        };
        match key.trim() {
            "aws_session_expiration" | "x_security_token_expires" => {
                return parse_expiry_value(val.trim());
            }
            _ => {}
        }
    }
    None
}

/// Parse an expiry-hint value defensively across the RFC3339 variants SSO/SAML
/// helpers emit, normalizing to unix seconds (UTC). Returns `None` on any failure.
/// Faithful port of `parseExpiryValue` (aws_backend.go:316-329) — the same four Go
/// layouts, tried in order, with REAL offset arithmetic (non-zero offsets are
/// golden-pinned). Surrounding single/double quotes are trimmed first. The layouts:
///
/// 1. RFC3339 — `2006-01-02T15:04:05Z07:00` (`T` sep, `Z` or `±HH:MM`).
/// 2. RFC3339Nano — as 1 with fractional seconds.
/// 3. `2006-01-02T15:04:05Z0700` — `T` sep, `Z` or `±HHMM` numeric offset (NO colon).
/// 4. `2006-01-02 15:04:05Z07:00` — SPACE separator, `Z` or `±HH:MM`.
fn parse_expiry_value(val: &str) -> Option<i64> {
    let val = val.trim().trim_matches(|c| c == '"' || c == '\'');
    // The three attempts cover Go's four layouts: T+colon (RFC3339 & RFC3339Nano),
    // T+no-colon (Z0700), space+colon (Z07:00). Optional fractional seconds are
    // accepted under ALL of them — Go's `time.Parse` accepts a trailing fractional
    // after the seconds field even when the layout omits it (verified empirically),
    // then `Unix()` truncates to whole seconds. Tried in order; first parse wins.
    parse_layout(val, 'T', OffsetColon::Colon)
        .or_else(|| parse_layout(val, 'T', OffsetColon::NoColon))
        .or_else(|| parse_layout(val, ' ', OffsetColon::Colon))
}

/// Whether the numeric zone offset carries a `:` (`±HH:MM`) or not (`±HHMM`). `Z` is
/// accepted under either.
#[derive(Clone, Copy, PartialEq, Eq)]
enum OffsetColon {
    Colon,
    NoColon,
}

/// Parse one exact layout: `<date><sep><time>[.frac]<zone>`, where `sep` is `'T'` or
/// `' '`, `zone` is `Z` or a signed offset (`colon`-styled or not). Optional fractional
/// seconds are validated then dropped (Go truncates to whole seconds on `Unix()`).
/// Returns unix seconds (offset applied) or `None`.
fn parse_layout(val: &str, sep: char, offset: OffsetColon) -> Option<i64> {
    let (date, time_zone) = val.split_once(sep)?;
    // Date: YYYY-MM-DD. Go's `time.Parse` "2006" year is FIXED-WIDTH 4 digits — a
    // 5-digit year is a parse error, not a big year (CodeRabbit review).
    let mut dparts = date.split('-');
    let ystr = dparts.next()?;
    if ystr.len() != 4 || !ystr.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let y: i64 = ystr.parse().ok()?;
    let mo: u32 = parse_2(dparts.next()?)?;
    let d: u32 = parse_2(dparts.next()?)?;
    if dparts.next().is_some() {
        return None;
    }

    // Split the zone suffix off the time.
    let (time_part, offset_secs) = split_zone(time_zone, offset)?;
    // Optional fractional seconds.
    let (hms, frac) = match time_part.split_once('.') {
        Some((h, f)) => (h, Some(f)),
        None => (time_part, None),
    };
    if let Some(f) = frac {
        if f.is_empty() || !f.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
    }
    let mut tparts = hms.split(':');
    let h: u32 = parse_2(tparts.next()?)?;
    let mi: u32 = parse_2(tparts.next()?)?;
    let se: u32 = parse_2(tparts.next()?)?;
    if tparts.next().is_some() {
        return None;
    }
    // Real calendar validation — Go's `time.Parse` rejects Feb 30 / non-leap Feb 29
    // ("day out of range") and has no leap-second support (`:60` errors), where the
    // Hinnant `days_from_civil` math would silently normalize (CodeRabbit review).
    if !(1..=12).contains(&mo) || d < 1 || d > days_in_month(y, mo) || h > 23 || mi > 59 || se > 59
    {
        return None;
    }
    let days = crate::status::days_from_civil(y, mo, d);
    Some(days * 86_400 + (h as i64) * 3_600 + (mi as i64) * 60 + (se as i64) - offset_secs)
}

/// Days in a month, Gregorian, with leap-year February (mirrors what Go's
/// `time.Parse` accepts). `mo` is validated 1..=12 by the caller.
fn days_in_month(y: i64, mo: u32) -> u32 {
    match mo {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        _ => {
            if (y % 4 == 0 && y % 100 != 0) || y % 400 == 0 {
                29
            } else {
                28
            }
        }
    }
}

/// Split an RFC3339 zone suffix off the time, returning `(time_text, secs_to_subtract)`
/// where the offset is the value to SUBTRACT to reach UTC (`+05:00` → +18000, since a
/// +5h local clock is 5h ahead of UTC). `Z` → 0. A missing zone → `None` (every Go
/// expiry layout requires a zone). `offset` selects the colon style of a `±` offset.
fn split_zone(time_zone: &str, offset: OffsetColon) -> Option<(&str, i64)> {
    if let Some(rest) = time_zone.strip_suffix('Z') {
        return Some((rest, 0));
    }
    let bytes = time_zone.as_bytes();
    let mut idx = None;
    for (i, &b) in bytes.iter().enumerate() {
        if (b == b'+' || b == b'-') && i > 0 {
            idx = Some(i);
        }
    }
    let i = idx?;
    let (time_part, off) = time_zone.split_at(i);
    let sign = if off.as_bytes()[0] == b'+' { 1 } else { -1 };
    let off = &off[1..];
    let (oh, om) = match offset {
        OffsetColon::Colon => {
            let (oh, om) = off.split_once(':')?;
            (parse_2(oh)? as i64, parse_2(om)? as i64)
        }
        OffsetColon::NoColon => {
            if off.len() != 4 || !off.bytes().all(|b| b.is_ascii_digit()) {
                return None;
            }
            (parse_2(&off[..2])? as i64, parse_2(&off[2..])? as i64)
        }
    };
    if oh > 23 || om > 59 {
        return None;
    }
    Some((time_part, sign * (oh * 3_600 + om * 60)))
}

/// Parse an exactly-2-digit unsigned field.
fn parse_2(s: &str) -> Option<u32> {
    if s.len() != 2 || !s.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

// ---------------------------------------------------------------------------
// Timestamp render helpers (shared with the aws-credentials handler, commit 3)
// ---------------------------------------------------------------------------

/// Render a unix instant as `YYYY-MM-DDTHH:MM:SSZ` with a **literal** `Z` — Go's
/// `time.Time.Format("2006-01-02T15:04:05Z")`, where the bare `Z` is a literal, so
/// the UTC clock numbers are emitted with a trailing `Z`. Reuses `rfc3339_utc`'s
/// civil math, which already renders exactly this shape. Used by the handler
/// (commit 3) for `expiration` / `cached_until`.
pub(crate) fn aws_literal_z(unix: i64) -> String {
    crate::status::rfc3339_utc(unix)
}

/// The audit detail for a credential vend: `expires:none` when there is no expiry
/// (passthrough without a hint) rather than a misleading `expires:00:00`, else
/// `expires:HH:MM` in UTC (mirror `awsExpiryDetail`). Used by the handler (commit 3).
pub(crate) fn aws_expiry_detail(exp: Option<i64>) -> String {
    match exp {
        None => "expires:none".to_string(),
        Some(unix) => {
            // HH:MM slice of the UTC render (chars 11..16 of `YYYY-MM-DDTHH:MM:SSZ`).
            let s = aws_literal_z(unix);
            format!("expires:{}", &s[11..16])
        }
    }
}

// ---------------------------------------------------------------------------
// The real STS-backed AssumeRoler (SDK-owned; no live differential coverage)
// ---------------------------------------------------------------------------

/// The production [`AssumeRoler`]: builds an `aws-sdk-sts` client **lazily on first
/// use** from the source profile's credential chain (SSO / credential_process /
/// static), exactly as Go's `LoadDefaultConfig(WithSharedConfigProfile)`. The TLS
/// connector is built explicitly over `rustls-ring` so the SDK reuses the workspace's
/// ring stack (no `aws-lc-sys`). Mirror `stsBackend.stsClient` (the lazy-once build) +
/// the AssumeRole call.
///
/// Unwired in this slice (constructed by `new_sts_backend`, which is itself wired in
/// commit 3) — see the module-level `allow(dead_code)`.
pub struct SdkAssumeRoler {
    source_profile: String,
    client: tokio::sync::OnceCell<aws_sdk_sts::Client>,
}

impl SdkAssumeRoler {
    pub fn new(source_profile: &str) -> Self {
        Self {
            source_profile: source_profile.to_string(),
            client: tokio::sync::OnceCell::new(),
        }
    }

    /// Build (once) and return the STS client. Only the assume-role path calls this,
    /// so a passthrough-only agent never loads the AssumeRole credential chain.
    async fn client(&self) -> &aws_sdk_sts::Client {
        self.client
            .get_or_init(|| async {
                // Ring-backed rustls HTTPS connector (reuses the workspace's ring —
                // no aws-lc-sys). See the Cargo.toml AWS block.
                let http = aws_smithy_http_client::Builder::new()
                    .tls_provider(aws_smithy_http_client::tls::Provider::Rustls(
                        aws_smithy_http_client::tls::rustls_provider::CryptoMode::Ring,
                    ))
                    .build_https();
                let sdk_cfg = aws_config::from_env()
                    .profile_name(&self.source_profile)
                    .http_client(http)
                    .load()
                    .await;
                aws_sdk_sts::Client::new(&sdk_cfg)
            })
            .await
    }

    /// Test hook mirroring Go's `b.client != nil` check: whether the lazy client has
    /// been built (proves a passthrough-only path never constructs it).
    #[cfg(test)]
    fn client_initialized(&self) -> bool {
        self.client.get().is_some()
    }
}

#[async_trait]
impl AssumeRoler for SdkAssumeRoler {
    async fn assume_role(&self, input: AssumeRoleParams) -> Result<Option<AssumedCreds>, String> {
        let client = self.client().await;
        let out = client
            .assume_role()
            .role_arn(input.role_arn)
            .role_session_name(input.session_name)
            .duration_seconds(input.duration_seconds)
            .send()
            .await
            .map_err(|e| {
                // Render the full SdkError chain (Go's %w), not just the terse Display.
                format!(
                    "{}",
                    aws_smithy_types::error::display::DisplayErrorContext(&e)
                )
            })?;
        // The SDK's optional credential fields → Ok(None) so the backend emits the
        // exact `sts:AssumeRole returned nil credentials for <arn>` (never a panic).
        Ok(out.credentials().map(|c| AssumedCreds {
            access_key_id: c.access_key_id().to_string(),
            secret_access_key: c.secret_access_key().to_string(),
            session_token: c.session_token().to_string(),
            expiration: Some(c.expiration().secs()),
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    // ---- test doubles -----------------------------------------------------------

    /// A `BusLog` that discards everything (the backend's logging is not asserted).
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

    /// An `AssumeRoler` that panics if called — proves the assume-role seam is never
    /// reached (cache hit / passthrough).
    struct PanicRoler;
    #[async_trait]
    impl AssumeRoler for PanicRoler {
        async fn assume_role(
            &self,
            _input: AssumeRoleParams,
        ) -> Result<Option<AssumedCreds>, String> {
            panic!("assume_role must not be called");
        }
    }

    /// A scripted `AssumeRoler`: returns canned outcomes in sequence (repeating the
    /// last), records each session name, and counts calls.
    struct FakeRoler {
        outcomes: Mutex<Vec<Result<Option<AssumedCreds>, String>>>,
        sessions: Mutex<Vec<String>>,
        calls: AtomicUsize,
    }
    impl FakeRoler {
        fn new(outcomes: Vec<Result<Option<AssumedCreds>, String>>) -> Arc<Self> {
            Arc::new(Self {
                outcomes: Mutex::new(outcomes),
                sessions: Mutex::new(Vec::new()),
                calls: AtomicUsize::new(0),
            })
        }
    }
    #[async_trait]
    impl AssumeRoler for FakeRoler {
        async fn assume_role(
            &self,
            input: AssumeRoleParams,
        ) -> Result<Option<AssumedCreds>, String> {
            self.sessions.lock().unwrap().push(input.session_name);
            let i = self.calls.fetch_add(1, Ordering::SeqCst);
            let outcomes = self.outcomes.lock().unwrap();
            outcomes[i.min(outcomes.len() - 1)].clone()
        }
    }

    fn assumed(key: &str, exp: Option<i64>) -> AssumedCreds {
        AssumedCreds {
            access_key_id: format!("{key}_KEY"),
            secret_access_key: format!("{key}_SECRET"),
            session_token: format!("{key}_TOKEN"),
            expiration: exp,
        }
    }

    /// A backend with an injected roler + fixed clock (mirror the Go tests' direct
    /// `stsBackend{...}` struct literals). `refresh_before` defaults to 5m.
    fn backend_fixed(cfg: AwsConfig, roler: Arc<dyn AssumeRoler>, now: i64) -> StsBackend {
        StsBackend {
            cfg,
            refresh_before: Duration::from_secs(300),
            session_dur: Duration::from_secs(3600),
            assume_roler: roler,
            log: silent(),
            cache: Mutex::new(HashMap::new()),
            now_unix: Box::new(move || now),
        }
    }

    /// Clear the two AWS shared-file env vars (paired with `passthrough_env`).
    fn cleanup_env() {
        std::env::remove_var("AWS_SHARED_CREDENTIALS_FILE");
        std::env::remove_var("AWS_CONFIG_FILE");
    }

    /// Drive a future to completion on a fresh current-thread runtime. The
    /// passthrough tests mutate process-global env vars under `env_lock()` and must
    /// hold that guard for the whole operation; running via `block_on` (rather than
    /// `#[tokio::test]` + `.await`) keeps the guard out of any `await` point (the
    /// passthrough path performs no real async work), so it stays a plain `#[test]`.
    fn block_on<F: std::future::Future>(f: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap()
            .block_on(f)
    }

    fn aws_cfg_role(role: &str) -> AwsConfig {
        AwsConfig {
            default_role: role.to_string(),
            source_profile: "default".to_string(),
            session_duration: "1h".to_string(),
            cache_refresh_before: "5m".to_string(),
            ..Default::default()
        }
    }

    // ---- assume-role cache + errors ---------------------------------------------

    #[tokio::test]
    async fn cache_hit_skips_sts_and_is_per_server() {
        // A pre-seeded cache entry within the refresh window returns without ever
        // calling the (panicking) seam. Mirror TestCacheHit.
        let now = 1_000_000_000;
        let b = StsBackend {
            cfg: aws_cfg_role("arn:aws:iam::123:role/test"),
            refresh_before: Duration::from_secs(300),
            session_dur: Duration::from_secs(3600),
            assume_roler: Arc::new(PanicRoler),
            log: silent(),
            cache: Mutex::new(HashMap::new()),
            now_unix: Box::new(|| 1_000_000_000),
        };
        b.cache.lock().unwrap().insert(
            "mini2/my-shed".to_string(),
            CachedCreds {
                access_key_id: "CACHED_KEY".into(),
                secret_access_key: "CACHED_SECRET".into(),
                session_token: "CACHED_TOKEN".into(),
                expiration: Some(now + 30 * 60), // within refresh window
            },
        );
        let creds = b.get_credentials("mini2", "my-shed").await.unwrap();
        assert_eq!(creds.access_key_id, "CACHED_KEY");
        // A different server with the same shed name must NOT share the entry.
        let (_role, until) = b.status("mini3", "my-shed");
        assert_eq!(until, None, "mini3/my-shed must not share mini2's cache");
    }

    #[tokio::test]
    async fn cache_stale_refetches() {
        // The cached entry is inside the refresh window (stale) → the seam is called
        // and its fresh creds returned. Stronger than Go's mock-only TestCacheMiss.
        let now = 1_000_000_000;
        let roler = FakeRoler::new(vec![Ok(Some(assumed("FRESH", Some(now + 3600))))]);
        let mut b = backend_fixed(
            aws_cfg_role("arn:aws:iam::123:role/test"),
            roler.clone(),
            now,
        );
        b.refresh_before = Duration::from_secs(300);
        b.cache.lock().unwrap().insert(
            "mini2/my-shed".to_string(),
            CachedCreds {
                access_key_id: "STALE".into(),
                secret_access_key: "STALE".into(),
                session_token: "STALE".into(),
                expiration: Some(now + 60), // 60s < 300s refresh → stale
            },
        );
        let creds = b.get_credentials("mini2", "my-shed").await.unwrap();
        assert_eq!(creds.access_key_id, "FRESH_KEY");
        assert_eq!(roler.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn no_role_error_exact() {
        let b = backend_fixed(AwsConfig::default(), Arc::new(PanicRoler), 0);
        let err = b
            .get_credentials("mini2", "unknown-shed")
            .await
            .unwrap_err();
        assert_eq!(
            err,
            r#"no role configured for shed "unknown-shed" on server "mini2""#
        );
    }

    #[tokio::test]
    async fn nil_credentials_error() {
        // The seam succeeds but returns no credentials → the exact nil-creds error.
        let roler = FakeRoler::new(vec![Ok(None)]);
        let b = backend_fixed(aws_cfg_role("arn:aws:iam::123:role/x"), roler, 0);
        let err = b.get_credentials("s", "sh").await.unwrap_err();
        assert_eq!(
            err,
            "sts:AssumeRole returned nil credentials for arn:aws:iam::123:role/x"
        );
    }

    #[tokio::test]
    async fn assume_role_failed_error() {
        let roler = FakeRoler::new(vec![Err("AccessDenied: nope".to_string())]);
        let b = backend_fixed(aws_cfg_role("arn:aws:iam::123:role/x"), roler, 0);
        let err = b.get_credentials("s", "sh").await.unwrap_err();
        assert_eq!(
            err,
            "sts:AssumeRole failed for arn:aws:iam::123:role/x: AccessDenied: nope"
        );
    }

    #[tokio::test]
    async fn session_name_shape() {
        let now = 1_700_000_000;
        let roler = FakeRoler::new(vec![Ok(Some(assumed("X", Some(now + 3600))))]);
        let b = backend_fixed(aws_cfg_role("arn:aws:iam::9:role/x"), roler.clone(), now);
        b.get_credentials("mini2", "web").await.unwrap();
        let sessions = roler.sessions.lock().unwrap();
        assert_eq!(sessions[0], format!("shed-mini2-web-{now}"));
    }

    // ---- passthrough -------------------------------------------------------------

    /// Write a credentials file + an empty config file under an isolated temp HOME,
    /// pointing the env vars at them (mirror the Go tests' passthroughEnv). Returns
    /// the guard TempDir (kept alive) + the credentials path.
    fn passthrough_env(content: &str) -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let creds = dir.path().join("credentials");
        std::fs::write(&creds, content).unwrap();
        let cfg = dir.path().join("config");
        std::fs::write(&cfg, "").unwrap();
        std::env::set_var("AWS_SHARED_CREDENTIALS_FILE", &creds);
        std::env::set_var("AWS_CONFIG_FILE", &cfg);
        (dir, creds)
    }

    fn passthrough_cfg(profile: &str) -> AwsConfig {
        AwsConfig {
            source_profile: profile.to_string(),
            mode: AWS_MODE_PASSTHROUGH.to_string(),
            session_duration: "1h".to_string(),
            cache_refresh_before: "5m".to_string(),
            ..Default::default()
        }
    }

    fn passthrough_backend(profile: &str) -> StsBackend {
        backend_fixed(passthrough_cfg(profile), Arc::new(PanicRoler), 0)
    }

    #[test]
    fn passthrough_reads_fixture() {
        let _g = crate::env_lock();
        let (_d, _creds) = passthrough_env(
            "[my-sso]\naws_access_key_id = AKIATEST\naws_secret_access_key = secretXYZ\naws_session_token = tokenXYZ\naws_session_expiration = 2099-01-02T15:04:05Z\n",
        );
        let b = passthrough_backend("my-sso");
        let creds = block_on(b.get_credentials("mini2", "web")).unwrap();
        assert_eq!(creds.access_key_id, "AKIATEST");
        assert_eq!(creds.secret_access_key, "secretXYZ");
        assert_eq!(creds.session_token, "tokenXYZ");
        // 2099-01-02T15:04:05Z as unix seconds.
        assert_eq!(creds.expiration, Some(4071049445));
        // Passthrough must not populate the assume-role cache.
        assert!(b.cache.lock().unwrap().is_empty());
        cleanup_env();
    }

    #[test]
    fn passthrough_reads_via_env_route() {
        // The env-var resolution path (AWS_SHARED_CREDENTIALS_FILE) — Go pins this in
        // its own tests; the differential harness uses the isolated-$HOME route.
        let _g = crate::env_lock();
        let (_d, creds_path) = passthrough_env(
            "[p]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\n",
        );
        assert_eq!(shared_credentials_path(), creds_path);
        let creds = block_on(passthrough_backend("p").get_credentials("s", "sh")).unwrap();
        assert_eq!(creds.access_key_id, "A");
        assert_eq!(creds.expiration, None); // no hint
        cleanup_env();
    }

    #[test]
    fn passthrough_expiry_variants() {
        let _g = crate::env_lock();
        // x_security_token_expires key + non-zero offset (Rust-stronger than Go's Z-only).
        let (_d, _c) = passthrough_env(
            "[p]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\nx_security_token_expires = 2099-06-01T02:00:00+02:00\n",
        );
        let creds = block_on(passthrough_backend("p").get_credentials("s", "sh")).unwrap();
        // +02:00 local == 2099-06-01T00:00:00Z.
        assert_eq!(creds.expiration, Some(4083955200));
        cleanup_env();

        // Missing hint → None.
        let (_d2, _c2) = passthrough_env(
            "[p]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\n",
        );
        let creds = block_on(passthrough_backend("p").get_credentials("s", "sh")).unwrap();
        assert_eq!(creds.expiration, None);
        cleanup_env();
    }

    #[test]
    fn passthrough_relogin_pickup() {
        let _g = crate::env_lock();
        let (dir, creds_path) = passthrough_env(
            "[p]\naws_access_key_id = A1\naws_secret_access_key = S1\naws_session_token = T1\n",
        );
        let b = passthrough_backend("p");
        let first = block_on(b.get_credentials("s", "sh")).unwrap();
        assert_eq!(first.access_key_id, "A1");

        // Simulate `aws sso login` rewriting the file atomically (tmp + rename).
        let tmp = dir.path().join("credentials.tmp");
        std::fs::write(
            &tmp,
            "[p]\naws_access_key_id = A2\naws_secret_access_key = S2\naws_session_token = T2\n",
        )
        .unwrap();
        std::fs::rename(&tmp, &creds_path).unwrap();

        let second = block_on(b.get_credentials("s", "sh")).unwrap();
        assert_eq!(second.access_key_id, "A2");
        cleanup_env();
    }

    #[test]
    fn passthrough_errors() {
        let _g = crate::env_lock();
        // Missing profile → wrap prefix + profile name (inner SDK-text divergence).
        let (_d, _c) = passthrough_env(
            "[other]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\n",
        );
        let err = block_on(passthrough_backend("absent").get_credentials("s", "sh")).unwrap_err();
        assert!(
            err.starts_with(r#"passthrough: loading profile "absent" from "#),
            "got: {err}"
        );
        cleanup_env();

        // No session token → EXACT Go string.
        let (_d2, _c2) = passthrough_env("[p]\naws_access_key_id = A\naws_secret_access_key = S\n");
        let err = block_on(passthrough_backend("p").get_credentials("s", "sh")).unwrap_err();
        assert_eq!(
            err,
            "passthrough: profile \"p\" has no aws_session_token; passthrough expects temporary SSO/SAML session credentials, not long-lived keys"
        );
        cleanup_env();

        // No static credentials → EXACT Go string (path is home-normalized via env).
        let (_d3, creds_path) = passthrough_env("[p]\nregion = us-east-1\n");
        let err = block_on(passthrough_backend("p").get_credentials("s", "sh")).unwrap_err();
        assert_eq!(
            err,
            format!(
                "passthrough: profile \"p\" in {} has no static credentials; run your SSO/SAML login (e.g. `aws sso login`) to refresh",
                creds_path.display()
            )
        );
        cleanup_env();
    }

    #[test]
    fn passthrough_status() {
        let _g = crate::env_lock();
        // With an expiry hint → role + Some(unix).
        let (_d, _c) = passthrough_env(
            "[my-sso]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\naws_session_expiration = 2099-01-02T15:04:05Z\n",
        );
        let (role, until) = passthrough_backend("my-sso").status("mini2", "web");
        assert_eq!(role, "passthrough:my-sso");
        assert_eq!(until, Some(4071049445));
        cleanup_env();

        // No hint → role + None.
        let (_d2, _c2) = passthrough_env(
            "[my-sso]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\n",
        );
        let (role, until) = passthrough_backend("my-sso").status("mini2", "web");
        assert_eq!(role, "passthrough:my-sso");
        assert_eq!(until, None);
        cleanup_env();

        // Missing file must NOT error (status never errors).
        std::env::set_var(
            "AWS_SHARED_CREDENTIALS_FILE",
            std::env::temp_dir().join("shed-aws-nope-xyz"),
        );
        std::env::remove_var("AWS_CONFIG_FILE");
        let (role, until) = passthrough_backend("my-sso").status("mini2", "web");
        assert_eq!(role, "passthrough:my-sso");
        assert_eq!(until, None);
        cleanup_env();
    }

    #[test]
    fn passthrough_only_never_builds_client() {
        let _g = crate::env_lock();
        let (_d, _c) = passthrough_env(
            "[my-sso]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\n",
        );
        // Build the REAL SdkAssumeRoler and prove get_credentials (passthrough) never
        // constructs its lazy STS client — the Rust analog of Go's b.client == nil.
        let sdk = Arc::new(SdkAssumeRoler::new("my-sso"));
        let b = backend_fixed(passthrough_cfg("my-sso"), sdk.clone(), 0);
        block_on(b.get_credentials("mini2", "web")).unwrap();
        assert!(!sdk.client_initialized());
        cleanup_env();
    }

    // ---- parse_session_expiry matrix + parse_expiry_value layouts ---------------

    fn expiry_from(content: &str, profile: &str) -> Option<i64> {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("credentials");
        std::fs::write(&path, content).unwrap();
        parse_session_expiry(&path, profile)
    }

    #[test]
    fn parse_session_expiry_matrix() {
        // (content, profile, wants_some)
        let cases: &[(&str, &str, bool)] = &[
            ("[p]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "p", true),
            ("[p]\nx_security_token_expires = 2099-01-02T15:04:05Z\n", "p", true),
            ("[p]\naws_session_expiration = \"2099-01-02T15:04:05Z\"\n", "p", true),
            ("# header\n[p]\n  aws_session_expiration = 2099-01-02T15:04:05Z  \n", "p", true),
            // duplicate section uses first (second has a bad value).
            ("[p]\naws_session_expiration = 2099-01-02T15:04:05Z\n[p]\naws_session_expiration = bad\n", "p", true),
            ("[profile p]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "p", true),
            ("[a.b]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "a.b", true),
            ("[p]\naws_access_key_id = A\n", "p", false),
            ("[other]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "p", false),
            ("[p]\naws_session_expiration = not-a-time\n", "p", false),
        ];
        for (content, profile, wants_some) in cases {
            let got = expiry_from(content, profile);
            assert_eq!(got.is_some(), *wants_some, "content={content:?}");
        }
    }

    #[test]
    fn parse_session_expiry_key_order_wins() {
        // First matching key in LINE ORDER wins: x_security first (value A), then
        // aws_session (value B) → A's unix.
        let got = expiry_from(
            "[p]\nx_security_token_expires = 2099-01-02T15:04:05Z\naws_session_expiration = 2100-01-01T00:00:00Z\n",
            "p",
        );
        assert_eq!(got, Some(4071049445)); // the 2099 value, not the 2100 one
    }

    #[test]
    fn parse_session_expiry_duplicate_section_first_wins() {
        let got = expiry_from(
            "[p]\naws_session_expiration = 2099-01-02T15:04:05Z\n[p]\naws_session_expiration = 2100-01-01T00:00:00Z\n",
            "p",
        );
        assert_eq!(got, Some(4071049445));
    }

    #[test]
    fn parse_expiry_value_layouts() {
        // Layout 1/2: RFC3339 (Z, +00:00), RFC3339Nano (fractional).
        assert_eq!(parse_expiry_value("2099-01-02T15:04:05Z"), Some(4071049445));
        assert_eq!(
            parse_expiry_value("2099-01-02T15:04:05.123Z"),
            Some(4071049445)
        );
        assert_eq!(
            parse_expiry_value("2099-01-02T15:04:05+00:00"),
            Some(4071049445)
        );
        // Non-zero colon offset: +02:00 → 2h earlier in UTC.
        assert_eq!(
            parse_expiry_value("2099-01-02T17:04:05+02:00"),
            Some(4071049445)
        );
        // Negative offset: -05:00 → 5h later in UTC.
        assert_eq!(
            parse_expiry_value("2099-01-02T10:04:05-05:00"),
            Some(4071049445)
        );
        // Layout 3: numeric offset, NO colon.
        assert_eq!(
            parse_expiry_value("2099-01-02T17:04:05+0200"),
            Some(4071049445)
        );
        assert_eq!(parse_expiry_value("2099-01-02T15:04:05Z"), Some(4071049445));
        // Layout 4: SPACE separator, colon offset.
        assert_eq!(
            parse_expiry_value("2099-01-02 17:04:05+02:00"),
            Some(4071049445)
        );
        assert_eq!(parse_expiry_value("2099-01-02 15:04:05Z"), Some(4071049445));
        // Fractional seconds accepted under EVERY layout (Go's time.Parse leniency),
        // truncated to whole seconds. 15:04:05.5+0200 == 13:04:05Z == 4071042245.
        assert_eq!(
            parse_expiry_value("2099-01-02T15:04:05.5+0200"),
            Some(4071042245)
        );
        assert_eq!(
            parse_expiry_value("2099-01-02 15:04:05.25Z"),
            Some(4071049445)
        );
        // Quotes trimmed.
        assert_eq!(
            parse_expiry_value("\"2099-01-02T15:04:05Z\""),
            Some(4071049445)
        );
        // Failures → None.
        assert_eq!(parse_expiry_value("garbage"), None);
        assert_eq!(parse_expiry_value(""), None);
        assert_eq!(parse_expiry_value("2099-01-02T15:04:05"), None); // no zone
        assert_eq!(parse_expiry_value("2099-13-02T15:04:05Z"), None); // bad month
    }

    // ---- render helpers ----------------------------------------------------------

    #[test]
    fn literal_z_render_utc() {
        assert_eq!(aws_literal_z(0), "1970-01-01T00:00:00Z");
        assert_eq!(aws_literal_z(1_000_000_000), "2001-09-09T01:46:40Z");
        assert_eq!(aws_literal_z(4071049445), "2099-01-02T15:04:05Z");
    }

    #[test]
    fn expiry_detail_render() {
        assert_eq!(aws_expiry_detail(None), "expires:none");
        assert_eq!(aws_expiry_detail(Some(4071049445)), "expires:15:04");
        assert_eq!(aws_expiry_detail(Some(0)), "expires:00:00");
    }

    // ---- constructor -------------------------------------------------------------

    #[test]
    fn new_sts_backend_rejects_unconfigured() {
        assert!(new_sts_backend(AwsConfig::default(), silent()).is_err());
    }

    #[test]
    fn new_sts_backend_accepts_passthrough_only() {
        assert!(new_sts_backend(passthrough_cfg("my-sso"), silent()).is_ok());
    }

    // ---- golden runner (Rust half of aws_expiry.json) ---------------------------

    #[test]
    fn golden_aws_expiry() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/aws_expiry.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");

        for v in fx["expiry_vectors"].as_array().unwrap() {
            let name = v["name"].as_str().unwrap();
            let got = expiry_from(v["ini"].as_str().unwrap(), v["profile"].as_str().unwrap());
            let want = v["expected_unix"].as_i64();
            assert_eq!(got, want, "expiry vector {name:?}");
        }
        for v in fx["literal_z_vectors"].as_array().unwrap() {
            let got = aws_literal_z(v["unix"].as_i64().unwrap());
            assert_eq!(got, v["expected"].as_str().unwrap(), "literal_z {v}");
        }
        for v in fx["expiry_detail_vectors"].as_array().unwrap() {
            let got = aws_expiry_detail(v["unix"].as_i64());
            assert_eq!(got, v["expected"].as_str().unwrap(), "expiry_detail {v}");
        }
    }
}
