//! Control-token FSM — ported from Swift's `ControlTokenProvider` actor, with
//! the mobile client's FSM extensions ported from
//! `shed-mobile/lib/control/control_token_provider.dart` (itself a faithful
//! port of the orchestrator's controlToken.ts).
//!
//! Caches a shed-server CONTROL token, refreshing it near expiry or on demand
//! (`invalidate*`, called on a 401). The mint primitive is a foreign
//! `TokenMinter` (the host agent, in Swift; a mock in tests) — this crate owns
//! only the cache/refresh/single-flight logic, so it stays pure.
//!
//! FSM extensions (all builder-opted, defaults preserve the pre-existing
//! desktop behavior — plan 001 §3.4):
//!   * a tunable refresh window ([`ControlTokenProvider::with_refresh_window`])
//!   * deterministic per-name refresh jitter ([`name_jitter`],
//!     [`ControlTokenProvider::with_name_jitter`])
//!   * a mint-failure cooldown ([`ControlTokenProvider::with_mint_cooldown`])
//!   * a persisted seed token ([`ControlTokenProvider::with_seed`])
//!   * an injectable clock ([`ControlTokenProvider::with_now`])
//!
//! Two unconditional semantics ported as strict improvements (§3.4):
//!   * keep-valid-on-proactive-failure — a failed refresh mint inside the
//!     refresh window returns the still-valid cached token instead of erroring
//!     (`control_token_provider.dart:82-92`)
//!   * the stale-401 guard — [`ControlTokenProvider::invalidate_if_current`]
//!     ignores a 401 for a token already rotated past
//!     (`control_token_provider.dart:99-105`)
//!
//! Fail-closed contract (mirrors the SDK/CLI, guarded by the tests here since
//! test mode drops the token path so e2e can't reach it): a mint failure with
//! no still-valid cached token yields an error and the client then sends NO
//! token — never a static downgrade.

use std::sync::{Arc, LazyLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use regex::Regex;
use serde_json::Value;
use tokio::sync::Mutex;

use crate::http::ShedError;
use crate::models::dart_trim;

/// A minted control token plus its optional expiry (unix seconds). `None` expiry
/// → only an explicit `invalidate*()` forces a refresh (mirrors `MintedToken`).
/// Swift parses the host agent's ISO-8601 expiry to epoch before handing it over;
/// the SSH-bundle path parses RFC3339 in this module instead
/// ([`parse_token_bundle`]), and always yields `Some` — a bundle without a
/// parseable expiry is rejected, never treated as non-expiring (plan 001 §3.5).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MintedToken {
    pub token: String,
    pub expires_at_unix: Option<u64>,
}

/// The mint primitive: request a fresh CONTROL token for `server`. Implemented
/// by the foreign host-agent bridge (Swift) or a test mock. A failure (Err) is
/// fail-closed — the provider surfaces it and the caller sends no token.
#[async_trait::async_trait]
pub trait TokenMinter: Send + Sync {
    async fn mint(&self, server: &str) -> Result<MintedToken, ShedError>;
}

/// The default 2h refresh window mirrors the SDK/CLI: refresh this long before
/// expiry so routine requests rarely race a 401. (Mobile passes its own 2h5m
/// via [`ControlTokenProvider::with_refresh_window`] — per-client parity, not
/// forced convergence.)
const REFRESH_WINDOW: Duration = Duration::from_secs(2 * 60 * 60);

/// The PERSISTED mint-failure message, served on later cooldown-blocked
/// calls. Deliberately FIXED/redacted: a [`TokenMinter`] error's Display text
/// can embed transport detail or even token material, which must never be
/// stored and replayed to later callers. The immediate caller of the failing
/// mint still receives the minter's real error (matching the pre-existing
/// propagation and Dart's rethrow, `control_token_provider.dart:149`).
const MINT_FAILED_REDACTED: &str = "control token mint failed";

/// Deterministic per-name jitter in `[0, max_ms)` — stable across restarts, no
/// RNG. Ported exactly from mobile's `nameJitter`
/// (`control_token_provider.dart:175-181`), which mirrors controlToken.ts (a
/// 32-bit signed hash like JS `|0`): iterate the name's UTF-16 CODE UNITS
/// (Dart `codeUnitAt` parity — a non-BMP char such as an emoji contributes its
/// surrogate PAIR, two units), fold `h = h*31 + cu` wrapped to 32-bit signed
/// at every step, then take `unsigned_abs()` (`i32::MIN`-safe where Dart's
/// 64-bit `abs()` is trivially safe and a Rust `abs()` would overflow) modulo
/// `max(max_ms, 1)`. `max_ms == 0` therefore always yields 0 (jitter off).
pub fn name_jitter(name: &str, max_ms: u64) -> u64 {
    let mut h: i32 = 0;
    for cu in name.encode_utf16() {
        // Dart computes `h * 31 + cu` in 64-bit then `.toSigned(32)`; a
        // stepwise 32-bit wrapping mul+add is identical (truncation mod 2^32
        // distributes over * and +).
        h = h.wrapping_mul(31).wrapping_add(i32::from(cu));
    }
    u64::from(h.unsigned_abs()) % max_ms.max(1)
}

// ---------------------------------------------------------------------------
// SSH token-bundle parsing (plan 001 §3.5) — fail-closed ports of mobile's
// `token_bundle.dart` (itself a port of the orchestrator's controlToken.ts).
//
// Sibling, NOT a duplicate: `shed-broker/src/bootstrap.rs:decode_bundle` also
// decodes a `_bootstrap` bundle, but with Go-parity semantics — expiry is
// OPTIONAL there (absent/zero → non-expiring `None`). These parsers are the
// MOBILE-parity view: expiry is REQUIRED and a bundle without one is rejected.
// The two must not be merged (§3.5); shed-core also must never depend on
// shed-broker (the dependency direction is broker → core).
// ---------------------------------------------------------------------------

/// Fail-closed rejection of an SSH `_bootstrap` token bundle. Mirrors the
/// mobile error codes (`app_error.dart`): `SHED_AUTH_EXPIRED`,
/// `SHED_TLS_PIN_MISMATCH`, `SHED_TLS_PIN_MISSING`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum TokenBundleError {
    /// Bad JSON, non-`control` scope, empty token, or a missing / unparseable
    /// / pre-epoch `expires_at` (mobile `SHED_AUTH_EXPIRED`).
    #[error("control token bundle rejected: expired or malformed (SHED_AUTH_EXPIRED)")]
    AuthExpired,
    /// The bundle's TLS fingerprint differs from the already-configured pin —
    /// a trust-model change we refuse to make silently
    /// (mobile `SHED_TLS_PIN_MISMATCH`).
    #[error("control token bundle TLS fingerprint does not match the configured pin (SHED_TLS_PIN_MISMATCH)")]
    PinMismatch,
    /// The bundle omits a well-formed TLS fingerprint where one is REQUIRED
    /// (the add-server flow, [`parse_control_bundle`]; mobile
    /// `SHED_TLS_PIN_MISSING`).
    #[error("control token bundle omits a valid tls_cert_fingerprint (SHED_TLS_PIN_MISSING)")]
    PinMissing,
}

/// The full `_bootstrap control` bundle (token + TLS pin + https port), used by
/// the add-server flow, which bootstraps the TLS pin from this SSH-delivered
/// value (the SSH channel is host-key-pinned). Stricter than
/// [`parse_token_bundle`]: the fingerprint and a positive `https_port` are
/// REQUIRED. Port of `token_bundle.dart:66-78`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ControlBundle {
    pub token: String,
    pub expires_at_unix: u64,
    /// Canonical `sha256:<lowercase hex>` leaf-cert pin (`fingerprint.dart:5-8`).
    pub tls_cert_fingerprint: String,
    pub https_port: u16,
}

/// Canonical TLS pin form: `sha256:` + lowercase hex of SHA-256(DER leaf) —
/// mobile's `kTlsFingerprintRe` (`fingerprint.dart:8`). The SSH host-key pin
/// uses a different format (`SHA256:<base64>`); never cross-compare the two.
static RE_TLS_FINGERPRINT: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^sha256:[0-9a-f]{64}$").unwrap());

/// Validate an SSH bootstrap bundle (one JSON line) into a [`MintedToken`],
/// failing closed. Rule-for-rule port of `token_bundle.dart:29-60`
/// (`parseTokenBundle`):
///   * bad JSON, a non-object, non-`control` scope, an empty/whitespace
///     token, or a missing/unparseable `expires_at` →
///     [`TokenBundleError::AuthExpired`]
///   * a minted `tls_cert_fingerprint` that is malformed or differs from an
///     already-configured `expected_pin` → [`TokenBundleError::PinMismatch`]
///     (a trust-model change we refuse to make silently)
///
/// When `expected_pin` is `Some`, a present bundle fingerprint must match; an
/// ABSENT one (or a non-string value) is tolerated — the bundle still arrived
/// over a host-key-pinned SSH channel (`token_bundle.dart:25-28,45-52`). The
/// add-server flow additionally requires the pin — that is
/// [`parse_control_bundle`]. `expected_pin` must already be in canonical
/// lowercase `sha256:<hex>` form (mobile stores it canonicalized).
///
/// The result's `expires_at_unix` is always `Some` — elsewhere in this module
/// `None` means non-expiring, but the bundle parse REJECTS an absent expiry
/// instead (plan 001 §3.5). One deliberate divergence from Dart: mobile keeps
/// the parsed expiry as a signed `DateTime`, while `MintedToken` carries
/// `u64` unix seconds — a PRE-EPOCH (negative) expiry therefore maps to
/// [`TokenBundleError::AuthExpired`] here. Justified fail-closed: a control
/// token that expired before 1970 is already expired / nonsensical, and no
/// live mint emits one.
pub fn parse_token_bundle(
    stdout: &str,
    expected_pin: Option<&str>,
) -> Result<MintedToken, TokenBundleError> {
    let raw: Value = serde_json::from_str(stdout).map_err(|_| TokenBundleError::AuthExpired)?;
    let obj = raw.as_object().ok_or(TokenBundleError::AuthExpired)?;

    if obj.get("scope").and_then(Value::as_str) != Some("control") {
        return Err(TokenBundleError::AuthExpired);
    }

    // Dart-set trim ([`dart_trim`]): Dart's `String.trim()` strips U+FEFF
    // (BOM) where Rust's `str::trim` does not — a BOM-only token must fail
    // closed here exactly as it does on mobile (`token_bundle.dart:42-43`).
    let token = dart_trim(
        obj.get("token")
            .and_then(Value::as_str)
            .ok_or(TokenBundleError::AuthExpired)?,
    );
    if token.is_empty() {
        return Err(TokenBundleError::AuthExpired);
    }

    // Conditional/tolerant pin check (`token_bundle.dart:45-52`): only when a
    // pin is configured AND the bundle carries a string fingerprint.
    if let Some(pin) = expected_pin {
        if let Some(minted_fp) = obj.get("tls_cert_fingerprint").and_then(Value::as_str) {
            let minted = dart_trim(minted_fp).to_lowercase();
            if !RE_TLS_FINGERPRINT.is_match(&minted) || minted != pin {
                return Err(TokenBundleError::PinMismatch);
            }
        }
    }

    let expires_at_unix = require_expiry_unix(obj)?;

    Ok(MintedToken {
        token: token.to_string(),
        expires_at_unix: Some(expires_at_unix),
    })
}

/// Parse the full `_bootstrap control` bundle for the add-server flow. Rule-
/// for-rule port of `token_bundle.dart:80-118` (`parseControlBundle`) —
/// STRICTER than [`parse_token_bundle`]:
///   * JSON / object / `scope == "control"` / non-blank token / required
///     parseable `expires_at` → [`TokenBundleError::AuthExpired`] on failure
///   * `tls_cert_fingerprint` REQUIRED: not a string, or malformed after
///     trim+lowercase → [`TokenBundleError::PinMissing`]; well-formed but
///     different from a `Some` `expected_pin` →
///     [`TokenBundleError::PinMismatch`]
///   * `https_port` REQUIRED: not an integer or outside `1..=65535` →
///     [`TokenBundleError::AuthExpired`]
///
/// Same pre-epoch-expiry divergence as [`parse_token_bundle`] (see its doc).
pub fn parse_control_bundle(
    stdout: &str,
    expected_pin: Option<&str>,
) -> Result<ControlBundle, TokenBundleError> {
    let raw: Value = serde_json::from_str(stdout).map_err(|_| TokenBundleError::AuthExpired)?;
    let obj = raw.as_object().ok_or(TokenBundleError::AuthExpired)?;

    if obj.get("scope").and_then(Value::as_str) != Some("control") {
        return Err(TokenBundleError::AuthExpired);
    }

    // Dart-set trim, as in [`parse_token_bundle`] (BOM parity,
    // `token_bundle.dart:90-93`).
    let token = dart_trim(
        obj.get("token")
            .and_then(Value::as_str)
            .ok_or(TokenBundleError::AuthExpired)?,
    );
    if token.is_empty() {
        return Err(TokenBundleError::AuthExpired);
    }

    let fp = dart_trim(
        obj.get("tls_cert_fingerprint")
            .and_then(Value::as_str)
            .ok_or(TokenBundleError::PinMissing)?,
    )
    .to_lowercase();
    if !RE_TLS_FINGERPRINT.is_match(&fp) {
        return Err(TokenBundleError::PinMissing);
    }
    if let Some(pin) = expected_pin {
        if fp != pin {
            return Err(TokenBundleError::PinMismatch);
        }
    }

    // Dart `portRaw is! int` (`token_bundle.dart:101-104`): a JSON float is
    // rejected — `Value::as_i64` is `None` for non-integer numbers.
    let port = obj
        .get("https_port")
        .and_then(Value::as_i64)
        .ok_or(TokenBundleError::AuthExpired)?;
    let https_port = u16::try_from(port).map_err(|_| TokenBundleError::AuthExpired)?;
    if https_port == 0 {
        return Err(TokenBundleError::AuthExpired);
    }

    let expires_at_unix = require_expiry_unix(obj)?;

    Ok(ControlBundle {
        token: token.to_string(),
        expires_at_unix,
        tls_cert_fingerprint: fp,
        https_port,
    })
}

/// The shared REQUIRED-expiry tail of both bundle parses
/// (`token_bundle.dart:54-57,107-110`): `expires_at` must be a string that
/// parses as RFC3339; anything else — including the pre-epoch u64 divergence
/// documented on [`parse_token_bundle`] — is [`TokenBundleError::AuthExpired`].
fn require_expiry_unix(obj: &serde_json::Map<String, Value>) -> Result<u64, TokenBundleError> {
    let raw = obj
        .get("expires_at")
        .and_then(Value::as_str)
        .ok_or(TokenBundleError::AuthExpired)?;
    let secs = parse_rfc3339_to_unix(raw).map_err(|()| TokenBundleError::AuthExpired)?;
    u64::try_from(secs).map_err(|_| TokenBundleError::AuthExpired)
}

/// Parse a strict UTC RFC3339 timestamp
/// (`YYYY-MM-DDTHH:MM:SS[.frac](Z|±HH:MM)`) to unix seconds, sub-second
/// digits validated then truncated.
///
/// Std-only (shed-core carries no time dependency): the same Howard Hinnant
/// days-from-civil mechanism as `shed-broker/src/status.rs:
/// parse_rfc3339_to_unix` — PORTED, not imported (the dependency direction is
/// broker → core), and with one semantic difference: the broker collapses the
/// Go zero time `0001-01-01T00:00:00Z` to `None` (Go `IsZero()` parity),
/// while this parser has no such case — it returns the plain instant
/// (-62135596800), matching Dart's `DateTime.tryParse`, which parses the Go
/// zero time successfully (`token_bundle.dart:56`). The bundle parses above
/// then reject any pre-epoch value at the u64 conversion.
///
/// Calendar validation is STRICTER than both siblings — deliberate for a
/// fail-closed auth parser (a live mint never emits an impossible date):
/// impossible dates (`2023-02-29`, day 32, month 13) are REJECTED rather
/// than silently normalized through the civil-date math (the broker parser)
/// or by `DateTime.tryParse` (Dart). The one permissive carve-out is
/// `second == 60`, kept because real RFC3339 timestamps carry leap seconds.
fn parse_rfc3339_to_unix(s: &str) -> Result<i64, ()> {
    let (date, time_and_zone) = s.split_once('T').ok_or(())?;
    let (y, mo, d) = {
        let mut it = date.split('-');
        let y: i64 = parse_fixed(it.next().ok_or(())?, 4)?;
        let mo: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let d: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (y, mo, d)
    };
    // Split the zone suffix off the time, then split off fractional seconds.
    let (time_part, offset_secs) = split_zone(time_and_zone)?;
    let (hms, frac) = match time_part.split_once('.') {
        Some((h, f)) => (h, Some(f)),
        None => (time_part, None),
    };
    if let Some(f) = frac {
        // Sub-second digits are validated then dropped (truncation).
        if f.is_empty() || !f.bytes().all(|b| b.is_ascii_digit()) {
            return Err(());
        }
    }
    let (h, mi, se) = {
        let mut it = hms.split(':');
        let h: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let mi: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let se: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (h, mi, se)
    };
    // Strict calendar validation (see the doc comment): real month/day only.
    // se == 60 admits a leap second (RFC3339 allows it).
    if !(1..=12).contains(&mo) || d < 1 || d > days_in_month(y, mo) || h > 23 || mi > 59 || se > 60
    {
        return Err(());
    }
    let days = days_from_civil(y, mo, d);
    Ok(days * 86_400 + i64::from(h) * 3_600 + i64::from(mi) * 60 + i64::from(se) - offset_secs)
}

/// Days in `month` of Gregorian `year` (proleptic leap rules:
/// `y%4==0 && (y%100!=0 || y%400==0)`). Callers guarantee `1 <= month <= 12`.
fn days_in_month(year: i64, month: u32) -> u32 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        _ => {
            if year % 4 == 0 && (year % 100 != 0 || year % 400 == 0) {
                29
            } else {
                28
            }
        }
    }
}

/// Days from the civil date to the unix epoch (Howard Hinnant's
/// `days_from_civil`; the same math as `shed-broker/src/status.rs:171-179`).
fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400; // [0, 399]
    let mp = i64::from(if m > 2 { m - 3 } else { m + 9 }); // [0, 11]
    let doy = (153 * mp + 2) / 5 + i64::from(d) - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146_097 + doe - 719_468
}

/// Split an RFC3339 zone suffix (`Z` or `±HH:MM`) off the end, returning the
/// remaining time text and the offset in seconds to SUBTRACT to reach UTC.
fn split_zone(time_and_zone: &str) -> Result<(&str, i64), ()> {
    if let Some(rest) = time_and_zone.strip_suffix('Z') {
        return Ok((rest, 0));
    }
    // The last '+' or '-' after index 0 starts the offset.
    let bytes = time_and_zone.as_bytes();
    let mut idx = None;
    for (i, &b) in bytes.iter().enumerate() {
        if (b == b'+' || b == b'-') && i > 0 {
            idx = Some(i);
        }
    }
    let i = idx.ok_or(())?;
    let (time_part, off) = time_and_zone.split_at(i);
    // `+05:00` is 5h AHEAD of UTC → subtract 5h to reach UTC; `-05:00` adds.
    let sign = if off.as_bytes()[0] == b'+' { 1 } else { -1 };
    let off = &off[1..];
    let (oh, om) = off.split_once(':').ok_or(())?;
    let oh = i64::from(parse_fixed::<u32>(oh, 2)?);
    let om = i64::from(parse_fixed::<u32>(om, 2)?);
    if oh > 23 || om > 59 {
        return Err(());
    }
    Ok((time_part, sign * (oh * 3_600 + om * 60)))
}

/// Parse an exactly-`width`-digit unsigned field (RFC3339 fields are
/// fixed-width).
fn parse_fixed<T: std::str::FromStr>(s: &str, width: usize) -> Result<T, ()> {
    if s.len() != width || !s.bytes().all(|b| b.is_ascii_digit()) {
        return Err(());
    }
    s.parse().map_err(|_| ())
}

/// The provider's mutable state, all behind one lock so the mint stays
/// single-flight (mirrors the Dart fields, `control_token_provider.dart:48-54`;
/// the Dart `_inflight` future dance is subsumed by holding the lock across
/// the mint).
#[derive(Default)]
struct State {
    cached: Option<MintedToken>,
    /// A 401 was observed for the cached token (`invalidate*`) — the next
    /// `token()` must mint and may never return the rejected token
    /// (`control_token_provider.dart:72-79`).
    must_mint: bool,
    /// Unix second before which no mint attempt is made (set by a mint
    /// failure; `control_token_provider.dart:52,147`).
    cooldown_until: u64,
    /// Presence of a recorded mint failure (`_lastError`,
    /// `control_token_provider.dart:53,148`), surfaced when a later call must
    /// error without attempting a mint (cooldown). Always the fixed
    /// [`MINT_FAILED_REDACTED`] text, never the minter's Display output — see
    /// that const's doc.
    last_error: Option<String>,
}

impl State {
    /// The cached token was rejected by a 401: drop it and force the next
    /// `token()` to mint — it may never return the rejected token
    /// (`control_token_provider.dart:72-79`). The shared tail of both
    /// `invalidate*` variants.
    fn reject_cached(&mut self) {
        self.cached = None;
        self.must_mint = true;
    }
}

/// Caches a control token, refreshing when missing or within the refresh window
/// of expiry. Concurrent `token()` callers serialize on the mint (single-flight:
/// a late caller re-checks the cache under the lock and returns the fresh token
/// rather than minting again).
///
/// Transport-identity binding is deliberately NOT ported from the Dart
/// provider (plan 001 §3.4): a shed-core `Client` + provider pair is immutable
/// per transport identity — the app layer constructs a NEW `Client` when the
/// host/port/pin changes, which deletes the identity-race class instead of
/// managing it. Recorded as a Phase B invariant.
///
/// Cancellation caveat (accepted limitation — C6 adversarial review #3, plan
/// §9): because the lock is held ACROSS the mint, cancelling a `token()`
/// future from outside (e.g. `Client::rc_events` bounds bearer resolution
/// with its connect timeout) can drop the very future that holds the lock
/// mid-mint. The lock frees on drop, but a remote mint side effect (host
/// agent request, SSH mint) may still complete out-of-band, so the caller's
/// immediate retry can start a SECOND concurrent mint. For this crate's
/// minters a rare double-mint costs one extra round trip plus one abandoned
/// short-TTL token — never a lockout (cooldown state mutates only when a mint
/// COMPLETES as a failure) and never a wrong cached token (the cancelled
/// mint's result is dropped, not cached). The upgrade, if it ever matters, is
/// provider-owned in-flight mint state joined by waiters off the lock —
/// shed-broker's watch-based single-flight (`shed-broker/src/minter.rs`,
/// module docs) is the in-repo pattern.
pub struct ControlTokenProvider {
    server: String,
    minter: Arc<dyn TokenMinter>,
    /// The clock, unix SECONDS (an `Arc<dyn Fn>` so tests can drive advancing
    /// time — a bare `fn` pointer can't capture per-test mutable state).
    now_unix: Arc<dyn Fn() -> u64 + Send + Sync>,
    refresh_window: Duration,
    /// Deterministic per-name refresh jitter, derived ONCE from the server
    /// name by [`Self::with_name_jitter`]. Subtracted from the refresh
    /// threshold, so refresh happens EARLIER — a fleet of providers with the
    /// same expiry de-synchronizes instead of thundering the minter together.
    jitter: Duration,
    /// Cooldown started by a mint failure; while it runs, `token()` makes no
    /// mint attempt (returns the cached still-valid token, or the last error).
    mint_cooldown: Duration,
    state: Mutex<State>,
}

impl ControlTokenProvider {
    pub fn new(server: String, minter: Arc<dyn TokenMinter>) -> Self {
        Self {
            server,
            minter,
            now_unix: Arc::new(default_now_unix),
            refresh_window: REFRESH_WINDOW,
            jitter: Duration::ZERO,
            mint_cooldown: Duration::ZERO,
            state: Mutex::new(State::default()),
        }
    }

    /// Replace the clock (unix seconds). Public, not `#[cfg(test)]`: the seam
    /// must be reachable from shed-app tests and the Phase B bridge (the same
    /// public-builder convention as `SseParser::with_max_event_bytes`).
    pub fn with_now(mut self, now: impl Fn() -> u64 + Send + Sync + 'static) -> Self {
        self.now_unix = Arc::new(now);
        self
    }

    /// Refresh this long before expiry (default [`REFRESH_WINDOW`], 2h —
    /// unchanged desktop behavior; mobile passes 2h5m).
    pub fn with_refresh_window(mut self, window: Duration) -> Self {
        self.refresh_window = window;
        self
    }

    /// Enable deterministic refresh jitter in `[0, max)`: the value is derived
    /// ONCE from the server name via [`name_jitter`] (Dart parity —
    /// `control_token_provider.dart:39`, `nameJitter(name, jitterMs)`) and
    /// SUBTRACTED from the refresh threshold, so this provider refreshes up to
    /// `max` earlier than an un-jittered one. Default off (zero jitter).
    ///
    /// [`name_jitter`] works in the Dart algorithm's millisecond domain for
    /// cross-language stability; the provider's clock is unix seconds, so the
    /// derived jitter truncates to whole seconds here (sub-second precision is
    /// noise against mobile's 5-minute max).
    pub fn with_name_jitter(mut self, max: Duration) -> Self {
        self.jitter = Duration::from_millis(name_jitter(&self.server, max.as_millis() as u64));
        self
    }

    /// After a mint failure, make no further mint attempt until `cooldown` has
    /// elapsed — a polling caller can't storm a host with doomed mints
    /// (`control_token_provider.dart:107-123`). During the cooldown `token()`
    /// returns the cached still-valid token if one exists, else the recorded
    /// last error. Default `0` = off (every call may attempt a mint).
    pub fn with_mint_cooldown(mut self, cooldown: Duration) -> Self {
        self.mint_cooldown = cooldown;
        self
    }

    /// Pre-populate the cache with a persisted token (mobile's config seed,
    /// `control_token_provider.dart:153-157`) so the first `token()` can skip
    /// the mint. An empty/whitespace token is IGNORED — an unusable credential
    /// is never cached (plan 001 §3.4).
    pub fn with_seed(mut self, seed: MintedToken) -> Self {
        if !seed.token.trim().is_empty() {
            self.state.get_mut().cached = Some(seed);
        }
        self
    }

    /// The current token, minting/refreshing when it is missing or near expiry.
    /// FSM ported from Dart's `get()` (`control_token_provider.dart:57-97`),
    /// minus the identity-binding and legacy-host branches (see the type-level
    /// docs / the test-module note):
    ///   * after an `invalidate*` the mint is FORCED — on failure this errors,
    ///     never returning the rejected token (`:72-79`)
    ///   * a still-valid cached token inside the refresh window mints
    ///     proactively but KEEPS the cached token when that mint fails
    ///     (`:81-92`)
    ///   * missing/expired → mint; a failure here is fail-closed (the caller
    ///     then sends no token) (`:94-96`)
    pub async fn token(&self) -> Result<String, ShedError> {
        // Hold the lock across the mint so concurrent callers serialize: the
        // first mints, the rest re-check here and return its result.
        let mut st = self.state.lock().await;
        let now = (self.now_unix)();

        // Reactive: a prior 401 means the current token is rejected — mint,
        // and do not fall back to it.
        if st.must_mint {
            return match self.mint_locked(&mut st, now).await {
                Ok(minted) => {
                    st.must_mint = false;
                    Ok(minted.token)
                }
                Err(e) => Err(e),
            };
        }

        if let Some(current) = st.cached.clone() {
            if !expired(&current, now) {
                if self.needs_refresh(&current, now) {
                    match self.mint_locked(&mut st, now).await {
                        Ok(minted) => return Ok(minted.token),
                        Err(e) => {
                            // Keep-valid-on-proactive-failure (strict
                            // improvement #1, `control_token_provider.dart:82-92`):
                            // the refresh mint failed — normally fall through and
                            // return the cached token rather than erroring. BUT
                            // `now` was read before awaiting the minter: a slow
                            // mint can finish AFTER the cached token expired, and
                            // returning it then would hand back a dead credential.
                            // Re-read the clock and only keep the token if it is
                            // STILL valid against the fresh now; otherwise fail
                            // closed with the real mint error.
                            let fresh_now = (self.now_unix)();
                            if expired(&current, fresh_now) {
                                return Err(e);
                            }
                        }
                    }
                }
                return Ok(current.token);
            }
        }

        self.mint_locked(&mut st, now).await.map(|m| m.token)
    }

    /// Drop the cached token UNCONDITIONALLY so the next `token()` force-mints.
    /// Kept for back-compat; prefer [`Self::invalidate_if_current`] when the
    /// rejected token is known — this variant can erase a NEWER token when a
    /// stale 401 races a rotation.
    pub async fn invalidate(&self) {
        self.state.lock().await.reject_cached();
    }

    /// Stale-401 guard (strict improvement #2,
    /// `control_token_provider.dart:99-105`): drop the cache ONLY if the
    /// cached token is `observed` (the one the 401 rejected) — a 401 for a
    /// token already rotated past is ignored. Sets the must-mint flag so the
    /// next `token()` force-mints and never returns the rejected token.
    pub async fn invalidate_if_current(&self, observed: &str) {
        let mut st = self.state.lock().await;
        if let Some(c) = &st.cached {
            if c.token != observed {
                return; // stale 401 — the rejected token is already gone
            }
        }
        st.reject_cached();
    }

    /// One mint attempt under the held state lock (the single-flight point),
    /// with the failure cooldown — Dart's `_mint` + `_doMint`
    /// (`control_token_provider.dart:107-151`): inside the cooldown no attempt
    /// is made (the recorded redacted error is returned, state untouched); a
    /// success replaces the cache and clears the last error; a failure starts
    /// the cooldown, records the fixed [`MINT_FAILED_REDACTED`] marker (never
    /// the minter's Display text — it can embed secrets), returns the REAL
    /// error to this immediate caller, and leaves the cache alone (an
    /// expired-but-cached token stays for a later retry; a still-valid one
    /// backs keep-valid). An empty minted token is a failure, not a usable
    /// credential (fail-closed), even if the minter reported success.
    async fn mint_locked(&self, st: &mut State, now: u64) -> Result<MintedToken, ShedError> {
        if now < st.cooldown_until {
            return Err(last_error(st));
        }
        let outcome = match self.minter.mint(&self.server).await {
            Ok(minted) if minted.token.is_empty() => Err(ShedError::Transport(
                "control-token mint returned an empty token".into(),
            )),
            other => other,
        };
        match outcome {
            Ok(minted) => {
                st.cached = Some(minted.clone());
                st.last_error = None;
                Ok(minted)
            }
            Err(e) => {
                // Fresh clock read for the cooldown start (Dart `_doMint`
                // re-reads `_now()`, `:147` — the mint itself took time).
                st.cooldown_until = (self.now_unix)() + self.mint_cooldown.as_secs();
                st.last_error = Some(MINT_FAILED_REDACTED.into());
                Err(e)
            }
        }
    }

    /// Within the refresh window of expiry: `now >= exp - window - jitter`
    /// (`control_token_provider.dart:162-166`; the jitter subtraction makes
    /// refresh earlier). No expiry → never (only `invalidate*` refreshes).
    /// `saturating_sub` matches Dart's signed math: a threshold below zero
    /// means "always refresh".
    fn needs_refresh(&self, t: &MintedToken, now: u64) -> bool {
        match t.expires_at_unix {
            None => false,
            Some(exp) => {
                now >= exp.saturating_sub(self.refresh_window.as_secs() + self.jitter.as_secs())
            }
        }
    }
}

/// Past expiry (`control_token_provider.dart:159-160`). No expiry → never.
fn expired(t: &MintedToken, now: u64) -> bool {
    matches!(t.expires_at_unix, Some(exp) if now >= exp)
}

/// The error for a `token()` call blocked from minting (cooldown): the
/// recorded redacted marker, or a generic fail-closed message (Dart's
/// `_lastError ?? AppError.authExpired()`, `control_token_provider.dart:78,96`
/// — except the persisted text is [`MINT_FAILED_REDACTED`], never the original
/// minter error).
fn last_error(st: &State) -> ShedError {
    ShedError::Transport(
        st.last_error
            .clone()
            .unwrap_or_else(|| "control token expired and mint unavailable".into()),
    )
}

fn default_now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};

    // Port coverage of mobile's `control_token_provider_test.dart:28-252`
    // (plan 001 §7 AC#4). NOT ported (§3.4 decision, see the type docs): the
    // two identity-binding cases (`drops the cached token when the host
    // transport identity changes`, `does not hand an in-flight mint to a
    // changed identity`) — a shed-core Client+provider pair is immutable per
    // transport identity; the app layer constructs a NEW Client (and provider)
    // on a host/port/pin change, deleting the identity-race class those cases
    // guard. TRANSLATED rather than ported: `returns null for a legacy
    // (non-secure) host` — Rust represents "legacy" at Client construction (a
    // minter-less Client resolves its bearer from the static token or sends
    // none; http.rs `static_token_used_without_provider` +
    // `mint_failure_is_fail_closed_no_downgrade` pin that contract), not as a
    // provider state.

    /// A minter that counts calls and returns `tok-<n>` (or fails — the
    /// switch is runtime-flippable, the Dart tests' `shouldFail` captures; a
    /// FAILED attempt is counted in `n` too). Optional expiry lets a test
    /// force the refresh-window path; a delay lets one force single-flight.
    struct MockMinter {
        calls: AtomicUsize,
        fail: AtomicBool,
        expires_at_unix: Option<u64>,
        delay_ms: u64,
    }

    impl MockMinter {
        fn new(fail: bool, expires_at_unix: Option<u64>, delay_ms: u64) -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
                fail: AtomicBool::new(fail),
                expires_at_unix,
                delay_ms,
            })
        }
        fn ok() -> Arc<Self> {
            Self::new(false, None, 0)
        }
        fn failing() -> Arc<Self> {
            Self::new(true, None, 0)
        }
        /// The Dart FSM tests' minter shape: flippable failure, far-future
        /// expiry.
        fn flaky(fail: bool) -> Arc<Self> {
            Self::new(fail, Some(FAR_FUTURE), 0)
        }
        fn set_fail(&self, v: bool) {
            self.fail.store(v, Ordering::SeqCst);
        }
        fn count(&self) -> usize {
            self.calls.load(Ordering::SeqCst)
        }
    }

    #[async_trait::async_trait]
    impl TokenMinter for MockMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            let n = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
            if self.delay_ms > 0 {
                tokio::time::sleep(Duration::from_millis(self.delay_ms)).await;
            }
            if self.fail.load(Ordering::SeqCst) {
                return Err(ShedError::Transport("mint failed".into()));
            }
            Ok(MintedToken {
                token: format!("tok-{n}"),
                expires_at_unix: self.expires_at_unix,
            })
        }
    }

    /// Dart's `farFuture` (99999999 ms there; unix seconds here — only "far
    /// beyond every test clock" matters).
    const FAR_FUTURE: u64 = 99_999_999;

    fn seed(exp: u64) -> MintedToken {
        MintedToken {
            token: "seed".into(),
            expires_at_unix: Some(exp),
        }
    }

    #[tokio::test]
    async fn caches_a_no_expiry_token() {
        let minter = MockMinter::ok();
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-1"); // cached, no re-mint
        assert_eq!(minter.count(), 1);
    }

    #[tokio::test]
    async fn invalidate_forces_remint() {
        let minter = MockMinter::ok();
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        p.invalidate().await;
        assert_eq!(p.token().await.unwrap(), "tok-2");
        assert_eq!(minter.count(), 2);
    }

    #[tokio::test]
    async fn refreshes_within_expiry_window() {
        // Expiry = now → expired → re-mint each call.
        let now = default_now_unix();
        let minter = MockMinter::new(false, Some(now), 0);
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-2");
        assert_eq!(minter.count(), 2);
    }

    #[tokio::test]
    async fn does_not_refresh_far_from_expiry() {
        let far = default_now_unix() + 10 * 60 * 60; // 10h out, beyond the 2h window
        let minter = MockMinter::new(false, Some(far), 0);
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-1"); // still fresh
        assert_eq!(minter.count(), 1);
    }

    #[tokio::test]
    async fn mint_failure_is_fail_closed() {
        let minter = MockMinter::failing();
        let p = ControlTokenProvider::new("mini2".into(), minter);
        assert!(p.token().await.is_err()); // caller then sends NO token
    }

    #[tokio::test]
    async fn empty_minted_token_is_fail_closed() {
        struct EmptyMinter;
        #[async_trait::async_trait]
        impl TokenMinter for EmptyMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                Ok(MintedToken {
                    token: String::new(),
                    expires_at_unix: None,
                })
            }
        }
        let p = ControlTokenProvider::new("mini2".into(), Arc::new(EmptyMinter));
        assert!(p.token().await.is_err()); // empty token → mint failure, not cached
    }

    // Also the port of Dart's `collapses concurrent mints into one
    // (single-flight)` (`control_token_provider_test.dart:114-134`): no seed →
    // every caller must mint, and the held lock collapses them to one.
    #[tokio::test]
    async fn concurrent_callers_mint_once() {
        // Single-flight: a slow mint + concurrent callers → exactly one mint.
        let minter = MockMinter::new(false, None, 40);
        let p = Arc::new(ControlTokenProvider::new("mini2".into(), minter.clone()));
        let (a, b, c) = tokio::join!(p.token(), p.token(), p.token());
        assert_eq!(a.unwrap(), "tok-1");
        assert_eq!(b.unwrap(), "tok-1");
        assert_eq!(c.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // ---- mobile FSM ports (control_token_provider_test.dart:28-252) ----

    // Dart `uses the config seed without minting when it is fresh` (:47-62).
    #[tokio::test]
    async fn uses_the_seed_without_minting_when_fresh() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1000))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 0);
    }

    // Dart `proactively mints when within the refresh window` (:64-79).
    #[tokio::test]
    async fn proactively_mints_within_the_refresh_window() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 9000)
            .with_refresh_window(Duration::from_secs(5000))
            .with_seed(seed(10_000));
        // 9000 >= 10000 - 5000 → inside the window (not yet expired) → mint.
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // Dart `keeps a still-valid token when a proactive mint fails` (:81-91) —
    // strict improvement #1 over the pre-C6 Rust FSM, which errored here.
    #[tokio::test]
    async fn keeps_a_still_valid_token_when_a_proactive_mint_fails() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 9000)
            .with_refresh_window(Duration::from_secs(5000))
            .with_seed(seed(10_000));
        assert_eq!(p.token().await.unwrap(), "seed"); // kept, not an error
        assert_eq!(minter.count(), 1); // the refresh mint WAS attempted
    }

    // Finding 4 (fail-closed correctness): `now` is captured BEFORE awaiting the
    // minter, so a SLOW proactive mint can finish after the cached token has
    // expired. Keep-valid must re-check the clock and NOT serve a now-expired
    // token. A minter that advances the shared clock while it runs models the
    // slow mint; the two sub-cases are (a) clock crosses expiry → Err, and (b)
    // clock stays before expiry → the still-valid cached token.
    #[tokio::test]
    async fn proactive_mint_failure_fails_closed_when_the_clock_crossed_expiry() {
        // A failing minter that, mid-mint, advances the shared clock to a target
        // (simulating wall-clock elapsing during a slow doomed mint).
        struct SlowFailMinter {
            clock: Arc<AtomicU64>,
            advance_to: u64,
        }
        #[async_trait::async_trait]
        impl TokenMinter for SlowFailMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                self.clock.store(self.advance_to, Ordering::SeqCst);
                Err(ShedError::Transport("mint failed".into()))
            }
        }

        // (a) clock advances 900 -> 1500, past the 1000 expiry → fail closed.
        let clock = Arc::new(AtomicU64::new(900));
        let p = ControlTokenProvider::new(
            "mini3".into(),
            Arc::new(SlowFailMinter {
                clock: clock.clone(),
                advance_to: 1500,
            }),
        )
        .with_now({
            let clock = clock.clone();
            move || clock.load(Ordering::SeqCst)
        })
        .with_refresh_window(Duration::from_secs(5000)) // 900 >= 1000-5000 → in window
        .with_seed(seed(1000));
        // In the refresh window, mint attempted, fails, and by the time it
        // returns the clock reads 1500 >= 1000 → the cached token is now expired.
        assert!(
            p.token().await.is_err(),
            "must not return the now-expired cached token"
        );
        assert_eq!(clock.load(Ordering::SeqCst), 1500);

        // (b) clock stays at 900 (mint fails without wall-clock crossing expiry)
        // → the still-valid cached token is kept.
        let clock = Arc::new(AtomicU64::new(900));
        let p = ControlTokenProvider::new(
            "mini3".into(),
            Arc::new(SlowFailMinter {
                clock: clock.clone(),
                advance_to: 900, // no advance past expiry
            }),
        )
        .with_now({
            let clock = clock.clone();
            move || clock.load(Ordering::SeqCst)
        })
        .with_refresh_window(Duration::from_secs(5000))
        .with_seed(seed(1000));
        assert_eq!(p.token().await.unwrap(), "seed"); // still valid → kept
    }

    // Dart `mints when the seed is expired, throwing if that mint fails`
    // (:93-112). Cooldown 0 (the default) so the immediate retry may mint.
    #[tokio::test]
    async fn expired_seed_mints_and_errs_when_that_mint_fails() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 5000)
            .with_seed(seed(1000)); // expired
        assert!(p.token().await.is_err());
        minter.set_fail(false);
        assert_eq!(p.token().await.unwrap(), "tok-2"); // attempt 1 failed
        assert_eq!(minter.count(), 2);
    }

    // Dart `forces a fresh mint on invalidate (401), never the rejected token`
    // (:136-155).
    #[tokio::test]
    async fn invalidate_if_current_forces_a_fresh_mint_never_the_rejected_token() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        p.invalidate_if_current("seed").await;
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // The must-mint failure half of Dart's `get()` reactive branch (:72-79):
    // after an invalidate, a failed mint is an ERROR — the still-valid (but
    // rejected) seed is never handed back.
    #[tokio::test]
    async fn must_mint_failure_errs_even_with_an_unexpired_cached_token() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed"); // far from expiry: no mint
        assert_eq!(minter.count(), 0);
        p.invalidate_if_current("seed").await;
        assert!(p.token().await.is_err()); // forced mint failed → Err, not "seed"
        assert_eq!(minter.count(), 1);
    }

    // Dart `does not re-mint within the cooldown after a failed mint`
    // (:157-179). Dart drives ms; the Rust clock is unix seconds — same
    // numbers, seconds domain.
    #[tokio::test]
    async fn does_not_remint_within_the_cooldown_after_a_failed_mint() {
        let now = Arc::new(AtomicU64::new(5000));
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(1000)); // expired
        assert!(p.token().await.is_err()); // fails; cooldown until 6000
        assert_eq!(minter.count(), 1);
        now.store(5500, Ordering::SeqCst); // within cooldown → NO mint attempt
        assert!(p.token().await.is_err());
        assert_eq!(minter.count(), 1);
        now.store(6000, Ordering::SeqCst); // cooldown elapsed → mints again
        assert!(p.token().await.is_err());
        assert_eq!(minter.count(), 2);
    }

    // The cooldown's still-valid half (plan §3.4): during a cooldown a cached
    // still-valid token is returned WITHOUT a mint attempt.
    #[tokio::test]
    async fn cooldown_returns_the_cached_still_valid_token_without_minting() {
        let now = Arc::new(AtomicU64::new(9000));
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_refresh_window(Duration::from_secs(5000))
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(10_000));
        // In the refresh window: the mint fails (keep-valid) + starts cooldown.
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 1);
        now.store(9100, Ordering::SeqCst); // still in window, cooldown active
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 1); // no second attempt
    }

    // C6 adversarial review #2b: the PERSISTED last_error (served on later
    // cooldown-blocked calls) is the fixed redacted marker — a minter error
    // embedding secret material must never be stored and replayed. The
    // immediate caller of the failing mint still receives the real error
    // (pre-C6 propagation parity; Dart rethrows too, :149).
    #[tokio::test]
    async fn cooldown_blocked_error_is_redacted_never_the_minter_text() {
        struct SecretErrMinter;
        #[async_trait::async_trait]
        impl TokenMinter for SecretErrMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                Err(ShedError::Transport(
                    "mint blew up: SECRET-TOKEN-MATERIAL".into(),
                ))
            }
        }
        let now = Arc::new(AtomicU64::new(5000));
        let p = ControlTokenProvider::new("mini3".into(), Arc::new(SecretErrMinter))
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(1000)); // expired seed → the mint is forced
        let immediate = p.token().await.unwrap_err().to_string();
        // The immediate failure surfaces the minter's real error.
        assert!(
            immediate.contains("SECRET-TOKEN-MATERIAL"),
            "got: {immediate}"
        );
        // A later cooldown-blocked call serves only the redacted marker.
        now.store(5500, Ordering::SeqCst);
        let blocked = p.token().await.unwrap_err().to_string();
        assert!(
            !blocked.contains("SECRET"),
            "persisted error leaked: {blocked}"
        );
        assert!(
            blocked.contains("control token mint failed"),
            "got: {blocked}"
        );
    }

    // Dart `ignores a stale 401 for a token it has already rotated past`
    // (:181-203) — strict improvement #2.
    #[tokio::test]
    async fn ignores_a_stale_401_for_an_already_rotated_token() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        p.invalidate_if_current("seed").await; // real 401 on the seed
        assert_eq!(p.token().await.unwrap(), "tok-1"); // rotated
        p.invalidate_if_current("seed").await; // stale 401 → ignored
        assert_eq!(p.token().await.unwrap(), "tok-1"); // still cached
        assert_eq!(minter.count(), 1);
    }

    // Plan §3.4: `with_seed` ignores an empty/whitespace token — never caches
    // an unusable credential; the first token() mints as if unseeded.
    #[tokio::test]
    async fn with_seed_ignores_empty_and_whitespace_tokens() {
        for junk in ["", "   \t "] {
            let minter = MockMinter::flaky(false);
            let p = ControlTokenProvider::new("mini3".into(), minter.clone())
                .with_now(|| 0)
                .with_seed(MintedToken {
                    token: junk.into(),
                    expires_at_unix: Some(FAR_FUTURE),
                });
            assert_eq!(p.token().await.unwrap(), "tok-1", "seed {junk:?}");
            assert_eq!(minter.count(), 1);
        }
    }

    // Jitter is SUBTRACTED from the refresh threshold — refresh happens
    // EARLIER. name_jitter("my-server", 300000 ms) = 145668 ms → 145 s in the
    // provider's seconds-domain comparison. exp = 100000, window = 1000 s:
    // threshold without jitter = 99000, with = 98855. At now = 98900 only the
    // jittered provider refreshes.
    #[tokio::test]
    async fn name_jitter_is_subtracted_so_refresh_happens_earlier() {
        let jittered_minter = MockMinter::flaky(false);
        let jittered = ControlTokenProvider::new("my-server".into(), jittered_minter.clone())
            .with_now(|| 98_900)
            .with_refresh_window(Duration::from_secs(1000))
            .with_name_jitter(Duration::from_secs(300))
            .with_seed(seed(100_000));
        assert_eq!(jittered.token().await.unwrap(), "tok-1");
        assert_eq!(jittered_minter.count(), 1);

        let plain_minter = MockMinter::flaky(false);
        let plain = ControlTokenProvider::new("my-server".into(), plain_minter.clone())
            .with_now(|| 98_900)
            .with_refresh_window(Duration::from_secs(1000))
            .with_seed(seed(100_000));
        assert_eq!(plain.token().await.unwrap(), "seed");
        assert_eq!(plain_minter.count(), 0);
    }

    // Cross-language vectors for [`name_jitter`] (plan §3.4/AC#5): expected
    // values derived by running the Dart algorithm
    // (`control_token_provider.dart:175-181`) by hand — per UTF-16 code unit,
    // h = (h*31 + cu).toSigned(32), then h.abs() % max(maxMs, 1).
    #[test]
    fn name_jitter_matches_the_dart_algorithm() {
        // "my-server" (ASCII), code units m=109 y=121 -=45 s=115 e=101 r=114
        // v=118 e=101 r=114:
        //   109 → 3500 → 108545 → 3365010 → 104315411
        //   → 104315411*31+114 = 3233777855   → wraps to -1061189441
        //   → -1061189441*31+118              → wraps to  1462865815
        //   →  1462865815*31+101              → wraps to -1895799890
        //   → -1895799890*31+114              → wraps to  1359745668
        // abs(1359745668) % 300000 = 145668; % 1000 = 668.
        assert_eq!(name_jitter("my-server", 300_000), 145_668);
        assert_eq!(name_jitter("my-server", 1_000), 668);

        // "grüß-server" (non-ASCII, still BMP — ONE code unit each: ü=252
        // ß=223), units g=103 r=114 ü=252 ß=223 -=45 s=115 e=101 r=114 v=118
        // e=101 r=114:
        //   103 → 3307 → 102769 → 3186062 → 98767967
        //   → 98767967*31+115 = 3061807092    → wraps to -1233160204
        //   → -1233160204*31+101              → wraps to   426739441
        //   →   426739441*31+114              → wraps to   344020897
        //   →   344020897*31+118              → wraps to  2074713333
        //   →  2074713333*31+101              → wraps to  -108396016
        //   →  -108396016*31+114              → wraps to   934690914
        // abs(934690914) % 300000 = 190914; % 1000 = 914.
        assert_eq!(name_jitter("grüß-server", 300_000), 190_914);
        assert_eq!(name_jitter("grüß-server", 1_000), 914);

        // "shed-🚀" (emoji = a SURROGATE PAIR, TWO code units — Dart
        // `codeUnitAt` sees the UTF-16 halves of U+1F680: high 0xD83D=55357,
        // low 0xDE80=56960), units s=115 h=104 e=101 d=100 -=45 55357 56960:
        //   115 → 3669 → 113840 → 3529140 → 109403385
        //   → 109403385*31+55357 = 3391560292 → wraps to  -903407004
        //   → -903407004*31+56960             → wraps to  2059210908
        // abs(2059210908) % 300000 = 10908; % 1000 = 908.
        assert_eq!(name_jitter("shed-🚀", 300_000), 10_908);
        assert_eq!(name_jitter("shed-🚀", 1_000), 908);

        // A name whose FINAL hash is negative, exercising unsigned_abs:
        // "my-server-dev" folds to h = -267572916;
        // abs(-267572916) % 300000 = 272916.
        assert_eq!(name_jitter("my-server-dev", 300_000), 272_916);

        // Dart `maxMs < 1 ? 1 : maxMs`: max 0 → % 1 → always 0 (jitter off).
        assert_eq!(name_jitter("my-server", 0), 0);
        assert_eq!(name_jitter("", 300_000), 0); // h stays 0
    }
}

#[cfg(test)]
mod bundle_tests {
    use super::*;
    use serde_json::json;

    // Port of mobile's `token_bundle_test.dart:29-152` (plan 001 §7 AC#5),
    // plus the RFC3339 parser's own unit tests and the documented pre-epoch
    // u64 divergence.

    /// Dart's `bundle()` helper (`token_bundle_test.dart:17-27`).
    fn bundle(scope: &str, token: &str, fp: Option<&str>, expires_at: Option<&str>) -> String {
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!(scope));
        m.insert("token".into(), json!(token));
        if let Some(fp) = fp {
            m.insert("tls_cert_fingerprint".into(), json!(fp));
        }
        if let Some(e) = expires_at {
            m.insert("expires_at".into(), json!(e));
        }
        Value::Object(m).to_string()
    }

    fn default_bundle() -> String {
        bundle(
            "control",
            "shed_control_abc",
            None,
            Some("2026-06-27T19:09:50.730171-05:00"),
        )
    }

    /// Dart's `cbundle()` helper (`token_bundle_test.dart:98-111`): like its
    /// `includeFp: true` default, the fingerprint is always present — `None`
    /// means the default `sha256:aaa…` pin, not omission (the omission case
    /// builds its map by hand).
    fn cbundle(fp: Option<&str>, port: Value, exp: &str) -> String {
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!("control"));
        m.insert("token".into(), json!("tok"));
        m.insert(
            "tls_cert_fingerprint".into(),
            json!(fp.map_or_else(|| pin('a'), str::to_string)),
        );
        m.insert("https_port".into(), port);
        m.insert("expires_at".into(), json!(exp));
        Value::Object(m).to_string()
    }

    fn pin(c: char) -> String {
        format!("sha256:{}", c.to_string().repeat(64))
    }

    // ---- parseTokenBundle ports (`token_bundle_test.dart:30-95`) ----

    // Dart `accepts a valid control bundle` (:31-35).
    #[test]
    fn token_accepts_a_valid_control_bundle() {
        let t = parse_token_bundle(&default_bundle(), None).unwrap();
        assert_eq!(t.token, "shed_control_abc");
        assert!(t.expires_at_unix.is_some());
    }

    // Dart `accepts a matching minted fingerprint` (:37-41).
    #[test]
    fn token_accepts_a_matching_minted_fingerprint() {
        let p = pin('a');
        let src = bundle(
            "control",
            "shed_control_abc",
            Some(&p),
            Some("2026-06-27T19:09:50.730171-05:00"),
        );
        let t = parse_token_bundle(&src, Some(&p)).unwrap();
        assert_eq!(t.token, "shed_control_abc");
    }

    // Dart `rejects a non-control scope` (:43-50).
    #[test]
    fn token_rejects_a_non_control_scope() {
        let src = bundle(
            "session",
            "shed_control_abc",
            None,
            Some("2026-06-27T19:09:50.730171-05:00"),
        );
        assert_eq!(
            parse_token_bundle(&src, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Dart `rejects a minted fingerprint that differs from the pin (no silent
    // re-pin)` (:52-69).
    #[test]
    fn token_rejects_a_differing_minted_fingerprint() {
        let src = bundle(
            "control",
            "shed_control_abc",
            Some(&pin('b')),
            Some("2026-06-27T19:09:50.730171-05:00"),
        );
        assert_eq!(
            parse_token_bundle(&src, Some(&pin('a'))),
            Err(TokenBundleError::PinMismatch)
        );
    }

    // Dart `rejects unparseable JSON` (:71-76).
    #[test]
    fn token_rejects_unparseable_json() {
        assert_eq!(
            parse_token_bundle("not json", None),
            Err(TokenBundleError::AuthExpired)
        );
        // A parseable non-object is equally rejected (`raw is! Map`, :36).
        assert_eq!(
            parse_token_bundle("\"a string\"", None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Dart `rejects an empty or whitespace-only token` (:78-83).
    #[test]
    fn token_rejects_an_empty_or_whitespace_only_token() {
        let src = bundle("control", "   ", None, Some("2026-06-27T19:09:50Z"));
        assert_eq!(
            parse_token_bundle(&src, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // BOM parity (codex C7 review): Dart's `String.trim()` strips U+FEFF,
    // Rust's `str::trim` does not — [`dart_trim`] closes the gap. A BOM-only
    // token must fail closed in BOTH parsers, exactly as on mobile.
    #[test]
    fn bom_only_token_is_auth_expired_in_both_parsers() {
        let t = bundle("control", "\u{FEFF}", None, Some("2026-06-27T19:09:50Z"));
        assert_eq!(
            parse_token_bundle(&t, None),
            Err(TokenBundleError::AuthExpired)
        );
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!("control"));
        m.insert("token".into(), json!("\u{FEFF}"));
        m.insert("tls_cert_fingerprint".into(), json!(pin('a')));
        m.insert("https_port".into(), json!(8443));
        m.insert("expires_at".into(), json!("2026-06-27T19:09:50Z"));
        assert_eq!(
            parse_control_bundle(&Value::Object(m).to_string(), None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // The BOM half of the fingerprint normalization: a BOM-wrapped but
    // otherwise valid fingerprint matches after [`dart_trim`] in both parsers.
    #[test]
    fn bom_wrapped_fingerprint_matches_after_dart_trim() {
        let wrapped = format!("\u{FEFF}{}\u{FEFF}", pin('a'));
        let t = bundle(
            "control",
            "shed_control_abc",
            Some(&wrapped),
            Some("2026-06-27T19:09:50Z"),
        );
        let minted = parse_token_bundle(&t, Some(&pin('a'))).unwrap();
        assert_eq!(minted.token, "shed_control_abc");

        let c = cbundle(Some(&wrapped), json!(8443), "2026-06-27T19:09:50Z");
        let b = parse_control_bundle(&c, Some(&pin('a'))).unwrap();
        assert_eq!(b.tls_cert_fingerprint, pin('a'));
    }

    // Strict calendar validation surfaces as AuthExpired at the bundle level:
    // an impossible expiry date is a failed parse, never a usable token.
    #[test]
    fn impossible_expiry_date_is_auth_expired() {
        let t = bundle(
            "control",
            "shed_control_abc",
            None,
            Some("2023-02-29T00:00:00Z"),
        );
        assert_eq!(
            parse_token_bundle(&t, None),
            Err(TokenBundleError::AuthExpired)
        );
        let c = cbundle(None, json!(8443), "2023-02-29T00:00:00Z");
        assert_eq!(
            parse_control_bundle(&c, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Dart `rejects a missing or unparseable expiry (never non-expiring)`
    // (:85-94).
    #[test]
    fn token_rejects_a_missing_or_unparseable_expiry() {
        let missing = bundle("control", "shed_control_abc", None, None);
        assert_eq!(
            parse_token_bundle(&missing, None),
            Err(TokenBundleError::AuthExpired)
        );
        let junk = bundle("control", "shed_control_abc", None, Some("soon"));
        assert_eq!(
            parse_token_bundle(&junk, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Load-bearing tolerance (`token_bundle.dart:25-28,45-52`, not a Dart test
    // case): a configured pin with an ABSENT bundle fingerprint is accepted —
    // the bundle arrived over a host-key-pinned SSH channel.
    #[test]
    fn token_tolerates_an_absent_fingerprint_when_a_pin_is_configured() {
        let t = parse_token_bundle(&default_bundle(), Some(&pin('a'))).unwrap();
        assert_eq!(t.token, "shed_control_abc");
    }

    // The regex half of the pin gate (`token_bundle.dart:49`): a present but
    // MALFORMED fingerprint under a configured pin is PinMismatch.
    #[test]
    fn token_rejects_a_malformed_fingerprint_when_a_pin_is_configured() {
        let src = bundle(
            "control",
            "shed_control_abc",
            Some("sha256:nothex"),
            Some("2026-06-27T19:09:50Z"),
        );
        assert_eq!(
            parse_token_bundle(&src, Some(&pin('a'))),
            Err(TokenBundleError::PinMismatch)
        );
    }

    // Exact expiry pin: trimmed token + a known epoch instant.
    #[test]
    fn token_yields_exact_unix_expiry() {
        let src = bundle(
            "control",
            "  shed_control_abc  ",
            None,
            Some("2001-09-09T01:46:40Z"),
        );
        let t = parse_token_bundle(&src, None).unwrap();
        assert_eq!(t.token, "shed_control_abc"); // trimmed (:42)
        assert_eq!(t.expires_at_unix, Some(1_000_000_000));
    }

    // ---- parseControlBundle ports (`token_bundle_test.dart:97-152`) ----

    // Dart `accepts a valid bundle` (:113-118).
    #[test]
    fn control_accepts_a_valid_bundle() {
        let b = parse_control_bundle(&cbundle(None, json!(8443), "2026-06-27T19:09:50Z"), None)
            .unwrap();
        assert_eq!(b.https_port, 8443);
        assert_eq!(b.tls_cert_fingerprint, pin('a'));
        assert_eq!(b.token, "tok");
    }

    // Dart `requires the TLS fingerprint` (:120-127). `cbundle(None, ...)`
    // defaults the fingerprint like Dart's `includeFp: true`; build without it
    // here.
    #[test]
    fn control_requires_the_tls_fingerprint() {
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!("control"));
        m.insert("token".into(), json!("tok"));
        m.insert("https_port".into(), json!(8443));
        m.insert("expires_at".into(), json!("2026-06-27T19:09:50Z"));
        let src = Value::Object(m).to_string();
        assert_eq!(
            parse_control_bundle(&src, None),
            Err(TokenBundleError::PinMissing)
        );
    }

    // A malformed fingerprint is PinMissing in the control variant
    // (`token_bundle.dart:98`), unlike the token variant's PinMismatch.
    #[test]
    fn control_rejects_a_malformed_fingerprint_as_pin_missing() {
        let src = cbundle(Some("sha256:nothex"), json!(8443), "2026-06-27T19:09:50Z");
        assert_eq!(
            parse_control_bundle(&src, None),
            Err(TokenBundleError::PinMissing)
        );
    }

    // Dart `rejects an out-of-range https_port` (:129-138).
    #[test]
    fn control_rejects_an_out_of_range_https_port() {
        for port in [json!(70_000), json!(0)] {
            let src = cbundle(None, port, "2026-06-27T19:09:50Z");
            assert_eq!(
                parse_control_bundle(&src, None),
                Err(TokenBundleError::AuthExpired)
            );
        }
        // Dart `portRaw is! int`: a JSON float / string port is equally out.
        for port in [json!(8443.5), json!("8443")] {
            let src = cbundle(None, port, "2026-06-27T19:09:50Z");
            assert_eq!(
                parse_control_bundle(&src, None),
                Err(TokenBundleError::AuthExpired)
            );
        }
    }

    // Dart `enforces an expected pin` (:140-151).
    #[test]
    fn control_enforces_an_expected_pin() {
        let src = cbundle(None, json!(8443), "2026-06-27T19:09:50Z");
        assert_eq!(
            parse_control_bundle(&src, Some(&pin('b'))),
            Err(TokenBundleError::PinMismatch)
        );
    }

    // Fingerprint normalization (`token_bundle.dart:97,113-115`): uppercase
    // hex is lowercased before the match, and the RESULT carries the
    // normalized form.
    #[test]
    fn control_normalizes_the_fingerprint_to_lowercase() {
        let upper = format!("sha256:{}", "A".repeat(64));
        let src = cbundle(Some(&upper), json!(8443), "2026-06-27T19:09:50Z");
        let b = parse_control_bundle(&src, Some(&pin('a'))).unwrap();
        assert_eq!(b.tls_cert_fingerprint, pin('a'));
    }

    // ---- the documented pre-epoch u64 divergence (plan 001 §3.5) ----

    // Dart's `DateTime.tryParse` parses a pre-epoch expiry (incl. the Go zero
    // time) into a negative instant; `MintedToken.expires_at_unix` is u64, so
    // both parses map it to AuthExpired — a control token that expired before
    // 1970 is already expired.
    #[test]
    fn pre_epoch_expiry_is_auth_expired() {
        for exp in ["0001-01-01T00:00:00Z", "1969-12-31T23:59:59Z"] {
            let t = bundle("control", "shed_control_abc", None, Some(exp));
            assert_eq!(
                parse_token_bundle(&t, None),
                Err(TokenBundleError::AuthExpired),
                "token bundle, expiry {exp}"
            );
            let c = cbundle(None, json!(8443), exp);
            assert_eq!(
                parse_control_bundle(&c, None),
                Err(TokenBundleError::AuthExpired),
                "control bundle, expiry {exp}"
            );
        }
        // The epoch itself is representable and accepted.
        let ok = bundle("control", "t", None, Some("1970-01-01T00:00:00Z"));
        assert_eq!(
            parse_token_bundle(&ok, None).unwrap().expires_at_unix,
            Some(0)
        );
    }

    // ---- the std-only RFC3339 parser ----

    #[test]
    fn rfc3339_known_epochs() {
        assert_eq!(parse_rfc3339_to_unix("1970-01-01T00:00:00Z"), Ok(0));
        // The Unix "billennium": 2001-09-09 01:46:40 UTC.
        assert_eq!(
            parse_rfc3339_to_unix("2001-09-09T01:46:40Z"),
            Ok(1_000_000_000)
        );
        assert_eq!(
            parse_rfc3339_to_unix("2023-11-14T22:13:20Z"),
            Ok(1_700_000_000)
        );
    }

    #[test]
    fn rfc3339_leap_year() {
        // 2024-02-29 exists (leap year) and lands on the known instant.
        assert_eq!(
            parse_rfc3339_to_unix("2024-02-29T00:00:00Z"),
            Ok(1_709_164_800)
        );
        // 2000 is a leap year (divisible by 400); 1900 and 2023 are not —
        // strict calendar validation rejects their Feb 29 (fail-closed;
        // stricter than Dart's normalizing `DateTime.tryParse`).
        assert!(parse_rfc3339_to_unix("2000-02-29T00:00:00Z").is_ok());
        assert_eq!(parse_rfc3339_to_unix("1900-02-29T00:00:00Z"), Err(()));
        assert_eq!(parse_rfc3339_to_unix("2023-02-29T00:00:00Z"), Err(()));
        // 30-day months cap at 30.
        assert_eq!(parse_rfc3339_to_unix("2023-04-31T00:00:00Z"), Err(()));
        assert!(parse_rfc3339_to_unix("2023-04-30T00:00:00Z").is_ok());
    }

    #[test]
    fn rfc3339_timezone_offsets() {
        // +05:00 is 5h ahead of UTC → same instant as midnight UTC.
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T05:00:00+05:00"),
            Ok(1_893_456_000)
        );
        // -05:00 is behind → 5h AFTER the epoch.
        assert_eq!(
            parse_rfc3339_to_unix("1970-01-01T00:00:00-05:00"),
            Ok(18_000)
        );
    }

    #[test]
    fn rfc3339_fractional_seconds_truncate() {
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T00:00:00.500Z"),
            Ok(1_893_456_000)
        );
        // The mobile fixture shape: fraction + offset together.
        assert_eq!(
            parse_rfc3339_to_unix("2026-06-27T19:09:50.730171-05:00"),
            Ok(1_782_605_390)
        );
    }

    #[test]
    fn rfc3339_go_zero_time_parses_pre_epoch() {
        // No zero-time→None collapse here (unlike the broker's parser): the
        // Go zero value parses to its plain pre-epoch instant, Dart
        // `DateTime.tryParse` parity.
        assert_eq!(
            parse_rfc3339_to_unix("0001-01-01T00:00:00Z"),
            Ok(-62_135_596_800)
        );
    }

    #[test]
    fn rfc3339_rejects_malformed_input() {
        for s in [
            "",
            "soon",
            "garbage",
            "2030-01-01",                // date only, no 'T'
            "2030-01-01T00:00:00",       // missing zone
            "2030-13-01T00:00:00Z",      // bad month
            "2023-13-01T00:00:00Z",      // bad month
            "2030-00-01T00:00:00Z",      // month 0
            "2030-01-32T00:00:00Z",      // day 32
            "2023-01-32T00:00:00Z",      // day 32
            "2023-02-29T00:00:00Z",      // impossible date (non-leap Feb 29)
            "2030-01-00T00:00:00Z",      // day 0
            "2030-01-01T24:00:00Z",      // hour 24
            "2030-01-01T25:00:00Z",      // hour 25
            "2030-01-01T00:60:00Z",      // minute 60
            "2030-01-01T00:61:00Z",      // minute 61
            "2030-01-01T00:00:61Z",      // second 61 (60 = leap second is OK)
            "2030-01-01T00:00:00.Z",     // empty fraction
            "2030-01-01T00:00:00.5xZ",   // non-digit fraction
            "2030-1-01T00:00:00Z",       // non-fixed-width month
            "30-01-01T00:00:00Z",        // non-fixed-width year
            "2030-01-01T00:00:00+5:00",  // non-fixed-width offset hour
            "2030-01-01T00:00:00+25:00", // offset hour out of range
        ] {
            assert_eq!(parse_rfc3339_to_unix(s), Err(()), "should reject {s:?}");
        }
        // The leap second itself is admitted (broker-parser parity).
        assert!(parse_rfc3339_to_unix("2030-01-01T00:00:60Z").is_ok());
    }
}
