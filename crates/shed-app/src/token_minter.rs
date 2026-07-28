//! The foreign side of the control-credential FSM: a
//! [`shed_core::token::TokenMinter`] that obtains a CONTROL credential via the
//! host agent's UDS. The shed-core `ControlTokenProvider` caches/refreshes
//! around this and invalidates on a refusal; a fail-closed reply (its `error`
//! set, or no credential) surfaces as an `Err`, so the FSM sends NOTHING — never
//! a static downgrade (F6). Ports Swift's `HostAgentTokenMinter` +
//! `ControlTokenProvider.hostAgent`.
//!
//! Two messages, chosen per request, not per build:
//!
//!   * `credential.get` when the agent advertises it — mode-agnostic, and the
//!     only one that can carry a certificate. The CSR travels; the private key
//!     it pairs with stays in this process, which is why the provider generates
//!     the keypair and hands only `CredentialRequest::csr_base64` down here.
//!   * `token.get` otherwise — an agent too old to know the newer message. That
//!     is a real, supported pairing (the two components ship separately), and it
//!     keeps working against a token-mode server. Against an mtls server it
//!     cannot work, and the agent says so in words.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use shed_core::approval::{CredentialResponse, TokenResponse};
use shed_core::http::ShedError;
use shed_core::token::{
    AuthMode, CredentialRequest, MintedCertificate, MintedCredential, MintedToken, TokenMinter,
};

use crate::auth_modes::AuthModeRegistry;
use crate::host_agent::{
    AgentCapabilityState, HostAgentClient, HostAgentClientError, DEFAULT_TOKEN_TIMEOUT,
};
use crate::timefmt;

/// Size caps on the credential exchange, in both directions — re-exported from
/// [`shed_core::token::limits`], which owns them.
///
/// They live in the core because the OTHER Rust credential mapper
/// (`shed-core-ffi`'s `credential_from_answer`, which serves any foreign
/// UniFFI minter) must enforce the same numbers and cannot depend on this
/// crate. Swift's `HostAgentCredentialLimits` is the third site and is pinned
/// to the same numbers by the shared §7 P9 fixtures' `limits` block.
pub use shed_core::token::limits;

/// How long an mtls-expecting mint waits for the agent's `hello_ack` before
/// giving up on learning its capability. Bounded and short: the ack is the
/// agent's first frame, and a refusal here is retried by the very next request.
const CAPABILITY_WAIT: Duration = Duration::from_secs(2);

/// Mints control credentials for the shed-core `ControlTokenProvider` by asking
/// the host agent over the UDS. One instance serves every server — the server
/// name is threaded through `mint(server)` / `mint_credential(server, …)`.
pub struct HostAgentTokenMinter {
    client: HostAgentClient,
    timeout: Duration,
    capability_wait: Duration,
    /// What is known about each server's credential shape: the config seed, what
    /// the core has adopted, and what THIS minter last saw (recorded
    /// synchronously below). `None` for a build with no registry — a token-only
    /// deployment, which is every pre-mtls caller.
    modes: Option<Arc<AuthModeRegistry>>,
}

impl HostAgentTokenMinter {
    pub fn new(client: HostAgentClient) -> Self {
        Self {
            client,
            timeout: DEFAULT_TOKEN_TIMEOUT,
            capability_wait: CAPABILITY_WAIT,
            modes: None,
        }
    }

    /// Attach the per-server mode registry that drives the §7 P5 capability
    /// gate (and that this minter writes its own observations into).
    pub fn with_modes(mut self, modes: Arc<AuthModeRegistry>) -> Self {
        self.modes = Some(modes);
        self
    }

    /// Shorten the pre-ack capability wait (tests only — production takes
    /// [`CAPABILITY_WAIT`], which is sized for a real agent's first frame).
    #[cfg(test)]
    fn with_capability_wait(mut self, wait: Duration) -> Self {
        self.capability_wait = wait;
        self
    }

    /// Does `server` issue certificates, as far as config / the core's observer /
    /// this minter's own last mint know?
    fn expects_mtls(&self, server: &str) -> bool {
        self.modes.as_ref().is_some_and(|m| m.expects_mtls(server))
    }

    fn record_mode(&self, server: &str, mode: AuthMode) {
        if let Some(m) = &self.modes {
            // The SYNCHRONOUS write, deliberately: it outranks the core's later
            // `CredentialObserver` callback for the same mint, so a delayed
            // callback from an older mint cannot walk this back
            // (`auth_modes.rs`'s ordering rule).
            m.record(server, mode);
        }
    }

    /// The user-facing sentence for a transport/capability failure: "upgrade
    /// shed-host-agent" when the live agent genuinely cannot do this, "still
    /// connecting" when the answer is simply not in yet.
    fn capability_message(err: &HostAgentClientError, server: &str) -> String {
        match err {
            HostAgentClientError::Unsupported(_) => format!(
                "shed-host-agent is too old to obtain a client certificate for {server}, \
                 which requires mtls; upgrade shed-host-agent"
            ),
            HostAgentClientError::CapabilityLost
            | HostAgentClientError::NotConnected
            | HostAgentClientError::Disconnected => format!(
                "connecting to shed-host-agent; {server} requires mtls and the agent has not \
                 announced its capabilities yet"
            ),
            other => format!("{other} while obtaining a credential for {server}"),
        }
    }
}

#[async_trait]
impl TokenMinter for HostAgentTokenMinter {
    async fn mint(&self, server: &str) -> Result<MintedToken, ShedError> {
        // A transport failure (not connected / timed out / dropped) is fail-closed
        // — the provider propagates the Err and the client sends no token.
        let resp = self
            .client
            .request_token(server, self.timeout)
            .await
            .map_err(|e| ShedError::Transport(e.to_string()))?;
        let minted = map_response(resp, server)?;
        self.record_mode(server, AuthMode::Token);
        Ok(minted)
    }

    /// Advertised per CONNECTION, not per build: the answer is whether the agent
    /// on the other end of the socket right now can relay a CSR. A build that
    /// claimed mtls support unconditionally would make the provider generate a
    /// keypair and compose a CSR for every mint against an agent that will drop
    /// the frame.
    ///
    /// The third state is what plan 002 §7 P5 added. Before the first
    /// `hello_ack` this connection has learned NOTHING, and answering "no" would
    /// send a `token.get` to a server that only issues certificates — which the
    /// agent answers with its "upgrade the app" error, an error the user cannot
    /// act on because nothing is actually out of date. So pre-ack the answer is
    /// the CONFIG's (plus whatever has been learned): claim the capability for a
    /// server believed to be mtls, and let [`Self::mint_credential`] wait for the
    /// ack before it commits to a message.
    ///
    /// A cached read, never I/O — this runs under the provider's mutex.
    fn supports_mtls(&self) -> bool {
        match self.client.credential_capability() {
            AgentCapabilityState::Supported => true,
            AgentCapabilityState::Unsupported => false,
            AgentCapabilityState::Unknown => {
                self.modes.as_ref().is_some_and(|m| m.any_expects_mtls())
            }
        }
    }

    async fn mint_credential(
        &self,
        server: &str,
        req: &CredentialRequest,
    ) -> Result<MintedCredential, ShedError> {
        let wants_mtls = self.expects_mtls(server);
        let mut capability = self.client.credential_capability_snapshot();
        // Only an mtls-expecting mint pays the wait: a token entry learns nothing
        // useful from the ack and keeps every shipped build's immediate token.get.
        if capability.state == AgentCapabilityState::Unknown && wants_mtls {
            capability = self
                .client
                .await_credential_capability(self.capability_wait)
                .await;
        }
        match capability.state {
            AgentCapabilityState::Supported => {
                let csr = req.csr_base64().filter(|c| !c.is_empty());
                if csr.is_none() && wants_mtls {
                    // The capability appeared after `supports_mtls()` was asked,
                    // so the provider generated no keypair. Failing makes the
                    // next attempt carry a CSR; sending a CSR-less
                    // `credential.get` and hoping for a certificate cannot work
                    // (D4 is CSR-only) and would burn an SSH round-trip.
                    return Err(ShedError::Config(format!(
                        "shed-host-agent announced credential.get after this mint began; \
                         retrying with a certificate request for {server}"
                    )));
                }
                let resp = self
                    .client
                    .request_credential(server, csr, capability, self.timeout)
                    .await
                    .map_err(|e| ShedError::Config(Self::capability_message(&e, server)))?;
                let minted = map_credential_response(resp, server)?;
                if matches!(minted, MintedCredential::Certificate(_)) && csr.is_none() {
                    return Err(ShedError::Config(format!(
                        "host agent returned a certificate for {server} from a request that \
                         carried no CSR; refusing (no key exists for it)"
                    )));
                }
                self.record_mode(server, minted.mode());
                Ok(minted)
            }
            AgentCapabilityState::Unsupported if wants_mtls => {
                Err(ShedError::Config(Self::capability_message(
                    &HostAgentClientError::Unsupported("credential.get"),
                    server,
                )))
            }
            AgentCapabilityState::Unknown if wants_mtls => Err(ShedError::Config(
                Self::capability_message(&HostAgentClientError::CapabilityLost, server),
            )),
            // Token-mode servers keep the behavior every shipped build has: send
            // the token.get. `request_token` itself fails fast when there is no
            // live connection.
            _ => self.mint(server).await.map(MintedCredential::Token),
        }
    }
}

/// Map a `credential.response` into a `MintedCredential`, or an `Err` for a
/// fail-closed reply. Pure, for the same reason `map_response` is.
///
/// Only the UDS-specific guards live here (an `error` field, a reply naming
/// another server); the credential SHAPE rules are
/// [`credential_from_parts`]'s — see there for why they are shared.
pub(crate) fn map_credential_response(
    resp: CredentialResponse,
    server: &str,
) -> Result<MintedCredential, ShedError> {
    if let Some(err) = resp.error.as_deref().filter(|e| !e.is_empty()) {
        if err.len() > limits::MAX_ERROR_BYTES {
            return Err(ShedError::Config(format!(
                "host agent returned an oversized error message for {server} ({} bytes); refusing",
                err.len()
            )));
        }
        return Err(ShedError::Config(err.to_string()));
    }
    if !resp.server.is_empty() && resp.server != server {
        return Err(ShedError::Config(format!(
            "host agent returned a credential for unexpected server {}",
            resp.server
        )));
    }
    credential_from_parts(
        CredentialParts {
            auth_mode: resp.auth_mode,
            token: resp.token.unwrap_or_default(),
            client_cert: resp.client_cert.unwrap_or_default(),
            cert_serial: resp.cert_serial.unwrap_or_default(),
            expires_at: resp.expires_at,
        },
        server,
    )
}

/// A minted credential's payload, normalized off whichever transport carried it
/// — the UDS `credential.response` above, or the embedded broker's
/// `MintedControlCredential` (`broker_bridge.rs`). Absent fields arrive as
/// empty strings; `expires_at` is already parsed by [`expiry_unix`].
pub(crate) struct CredentialParts {
    pub auth_mode: Option<String>,
    pub token: String,
    pub client_cert: String,
    pub cert_serial: String,
    /// The RAW wire expiry, deliberately unparsed here: absent/empty means "the
    /// minter reported none", while a POPULATED-but-unparseable value is a
    /// refusal, and collapsing both to `None` before this point would erase the
    /// distinction (see [`credential_from_parts`] rule 5).
    pub expires_at: Option<String>,
}

/// The credential-shape rules, in ONE place for BOTH minters: the external
/// (UDS) one above and the embedded-broker one. The two paths feed the SAME
/// provider FSM, so a divergence would mean the app behaved differently in
/// embedded mode than in external mode against the same server — sharing the
/// body is what makes "identical rules" mechanical rather than a convention.
///
/// The mode comes from the credential's own `auth_mode` rather than from which
/// field happens to be populated: "the server said mtls but sent no
/// certificate" is a protocol violation worth reporting, and inferring the mode
/// from the fields would silently turn it into "token mode with an empty
/// token". An empty payload for the claimed mode is an error, never a usable
/// credential.
pub(crate) fn credential_from_parts(
    parts: CredentialParts,
    server: &str,
) -> Result<MintedCredential, ShedError> {
    // 4. Size caps first, so an oversized field is named as such rather than
    //    reaching the arm logic (byte-for-byte the Swift order, so the shared
    //    §7 P9 vectors get the same message from both clients).
    if let Some(field) = over_cap_field(&parts) {
        return Err(ShedError::Config(format!(
            "host agent returned an oversized {field} for {server}; refusing"
        )));
    }
    // 5. A populated expiry must PARSE. Silently dropping it would disable the
    //    proactive refresh and leave the credential to die mid-request; an
    //    ABSENT one genuinely means "the minter reported none" and is fine.
    let expires_at_unix = match parsed_expiry(parts.expires_at.as_deref()) {
        Ok(e) => e,
        Err(raw) => {
            return Err(ShedError::Config(format!(
                "host agent returned an unparseable expiry {raw:?} for {server}; refusing"
            )));
        }
    };
    // 3. STRICT mode parse, deliberately not `AuthMode::from_wire`: that
    //    decoder's unknown-means-token lenience is for BUNDLES (where an old
    //    client meeting a future server should degrade to the populated shape),
    //    but this is a minter's ANSWER — adopting a credential whose mode this
    //    build cannot interpret as if it were a bearer token would fail open.
    //    Matched EXACTLY: no trimming, no case folding, mirroring Go
    //    `sdk.Bundle.Mode`'s "case does not fuzzy-match" rule. Absent/empty and
    //    the legacy "secure" spelling stay token (the compat contract).
    let mode = match parts.auth_mode.as_deref() {
        None | Some("") | Some("token") | Some("secure") => AuthMode::Token,
        Some("mtls") => AuthMode::Mtls,
        Some(other) => {
            return Err(ShedError::Config(format!(
                "host agent reported unknown auth mode {other:?} for {server}; \
                 refusing to fall back to a bearer token — upgrade shed-desktop"
            )));
        }
    };
    // 6. The selected arm's payload must be complete AND exclusive. A token arm
    //    carrying certificate fields (or vice versa) is a protocol violation
    //    from an implementation neither agent has — not an empty success, and
    //    not something to resolve by preferring one field over the other.
    match mode {
        AuthMode::Token => {
            if !parts.client_cert.is_empty() || !parts.cert_serial.is_empty() {
                return Err(ShedError::Config(format!(
                    "host agent returned a token AND certificate fields for {server}; \
                     refusing an ambiguous credential"
                )));
            }
            minted_token(parts.token, expires_at_unix, server).map(MintedCredential::Token)
        }
        AuthMode::Mtls => {
            if parts.client_cert.is_empty() {
                return Err(ShedError::Config(format!(
                    "host agent reported auth mode mtls but returned no certificate for {server}"
                )));
            }
            if !parts.token.is_empty() {
                return Err(ShedError::Config(format!(
                    "host agent returned a certificate AND a token for {server}; \
                     refusing an ambiguous credential"
                )));
            }
            Ok(MintedCredential::Certificate(MintedCertificate {
                cert_pem: parts.client_cert,
                serial: parts.cert_serial,
                expires_at_unix,
            }))
        }
    }
}

/// Names the first field over its cap, or `None` when all fit.
fn over_cap_field(parts: &CredentialParts) -> Option<&'static str> {
    if parts.token.len() > limits::MAX_TOKEN_BYTES {
        return Some("token");
    }
    if parts.client_cert.len() > limits::MAX_CLIENT_CERT_BYTES {
        return Some("certificate");
    }
    if parts.cert_serial.len() > limits::MAX_CERT_SERIAL_BYTES {
        return Some("certificate serial");
    }
    None
}

/// The expiry as unix seconds, distinguishing ABSENT from UNPARSEABLE:
/// `Ok(None)` for absent/empty, `Err(raw)` for a populated value that failed to
/// parse. An empty string counts as absent — both agents omitempty it, so it
/// cannot carry meaning.
fn parsed_expiry(raw: Option<&str>) -> Result<Option<u64>, &str> {
    let Some(raw) = raw.filter(|r| !r.is_empty()) else {
        return Ok(None);
    };
    match timefmt::parse_unix(raw) {
        Some(s) => Ok(Some(s.max(0) as u64)),
        None => Err(raw),
    }
}

/// The empty-token guard, shared by every token-shaped mint (both minters, both
/// messages): a blank token is NEVER a valid mint, so the FSM must send nothing
/// rather than a valid-looking downgrade (F6).
pub(crate) fn minted_token(
    token: String,
    expires_at_unix: Option<u64>,
    server: &str,
) -> Result<MintedToken, ShedError> {
    if token.is_empty() {
        return Err(ShedError::Config(format!(
            "host agent returned no token for {server}"
        )));
    }
    Ok(MintedToken {
        token,
        expires_at_unix,
    })
}

/// A wire expiry string as unix seconds. An absent/unparseable expiry -> `None`
/// -> the provider caches and only re-mints on `invalidate()` (matches Swift +
/// `token.rs`); shed-app owns timestamp parsing, so this is the one conversion.
pub(crate) fn expiry_unix(raw: Option<&str>) -> Option<u64> {
    raw.and_then(timefmt::parse_unix).map(|s| s.max(0) as u64)
}

/// Map a `token.response` into a `MintedToken`, or an `Err` for a fail-closed
/// reply. Pure, so the fail-closed mapping is unit-tested without a live agent.
fn map_response(resp: TokenResponse, server: &str) -> Result<MintedToken, ShedError> {
    if let Some(err) = resp.error.as_deref().filter(|e| !e.is_empty()) {
        return Err(ShedError::Config(err.to_string()));
    }
    // Empty server is allowed (serde default); a non-empty mismatch is fail-closed.
    if !resp.server.is_empty() && resp.server != server {
        return Err(ShedError::Config(format!(
            "host agent returned token for unexpected server {}",
            resp.server
        )));
    }
    minted_token(
        resp.token.unwrap_or_default(),
        expiry_unix(resp.expires_at.as_deref()),
        server,
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn resp(token: Option<&str>, expires_at: Option<&str>, error: Option<&str>) -> TokenResponse {
        TokenResponse {
            in_reply_to: "q1".into(),
            server: "mini2".into(),
            token: token.map(String::from),
            expires_at: expires_at.map(String::from),
            error: error.map(String::from),
        }
    }

    #[test]
    fn ok_reply_yields_token_and_parsed_expiry() {
        let m = map_response(
            resp(Some("tok"), Some("2026-07-03T00:00:00Z"), None),
            "mini2",
        )
        .unwrap();
        assert_eq!(m.token, "tok");
        // Round-trip through the formatter to confirm the expiry parsed to the
        // correct instant (without hand-computing the epoch).
        let unix = m.expires_at_unix.expect("expiry parsed");
        assert_eq!(timefmt::format_iso8601(unix as i64), "2026-07-03T00:00:00Z");
    }

    #[test]
    fn ok_reply_without_expiry_is_none() {
        let m = map_response(resp(Some("tok"), None, None), "mini2").unwrap();
        assert_eq!(m.token, "tok");
        assert_eq!(m.expires_at_unix, None);
    }

    #[test]
    fn error_reply_is_fail_closed() {
        let e = map_response(resp(None, None, Some("host key mismatch")), "mini2").unwrap_err();
        assert!(matches!(e, ShedError::Config(m) if m.contains("host key mismatch")));
    }

    #[test]
    fn error_reply_wins_even_with_a_token() {
        // Defensive: an `error` set alongside a token is still fail-closed.
        let e = map_response(resp(Some("tok"), None, Some("bad")), "mini2").unwrap_err();
        assert!(matches!(e, ShedError::Config(_)));
    }

    #[test]
    fn missing_or_empty_token_is_fail_closed() {
        assert!(map_response(resp(None, None, None), "mini2").is_err());
        assert!(map_response(resp(Some(""), None, None), "mini2").is_err());
    }

    #[test]
    fn unparseable_expiry_is_none_not_an_error() {
        // A bad expiry doesn't fail the mint — it just means "no known expiry"
        // (the provider then only re-mints on invalidate). The token is still used.
        let m = map_response(resp(Some("tok"), Some("garbage"), None), "mini2").unwrap();
        assert_eq!(m.token, "tok");
        assert_eq!(m.expires_at_unix, None);
    }

    #[test]
    fn wrong_server_is_fail_closed() {
        let e = map_response(resp(Some("tok"), None, None), "other").unwrap_err();
        assert!(matches!(e, ShedError::Config(m) if m.contains("unexpected server")));
    }

    #[test]
    fn empty_server_in_reply_is_allowed() {
        let mut r = resp(Some("tok"), None, None);
        r.server = String::new();
        let m = map_response(r, "mini2").unwrap();
        assert_eq!(m.token, "tok");
    }

    fn cred_resp(
        auth_mode: Option<&str>,
        token: Option<&str>,
        client_cert: Option<&str>,
        error: Option<&str>,
    ) -> CredentialResponse {
        CredentialResponse {
            in_reply_to: "q1".into(),
            server: "mini2".into(),
            auth_mode: auth_mode.map(String::from),
            token: token.map(String::from),
            client_cert: client_cert.map(String::from),
            // A serial belongs to a CERTIFICATE answer. Stamping one on every
            // reply would make each token vector an ambiguous-credential refusal
            // (rule 6), so it rides the certificate exactly as the agents send it.
            cert_serial: client_cert.map(|_| "0a0b".into()),
            expires_at: Some("2026-07-03T00:00:00Z".into()),
            error: error.map(String::from),
        }
    }

    #[test]
    fn credential_reply_maps_both_modes() {
        match map_credential_response(cred_resp(Some("token"), Some("tok"), None, None), "mini2")
            .unwrap()
        {
            MintedCredential::Token(t) => {
                assert_eq!(t.token, "tok");
                assert_eq!(
                    timefmt::format_iso8601(t.expires_at_unix.unwrap() as i64),
                    "2026-07-03T00:00:00Z"
                );
            }
            other => panic!("expected a token, got {other:?}"),
        }
        match map_credential_response(cred_resp(Some("mtls"), None, Some("PEM"), None), "mini2")
            .unwrap()
        {
            MintedCredential::Certificate(c) => {
                assert_eq!(c.cert_pem, "PEM");
                assert_eq!(c.serial, "0a0b");
            }
            other => panic!("expected a certificate, got {other:?}"),
        }
    }

    #[test]
    fn credential_reply_absent_auth_mode_means_token() {
        // Same rule as every other decoder in the tree: an absent auth_mode is a
        // legacy/token answer, never a certificate.
        match map_credential_response(cred_resp(None, Some("tok"), None, None), "mini2").unwrap() {
            MintedCredential::Token(t) => assert_eq!(t.token, "tok"),
            other => panic!("expected a token, got {other:?}"),
        }
    }

    #[test]
    fn credential_reply_fail_closed_cases() {
        // An error wins over any payload.
        assert!(map_credential_response(
            cred_resp(Some("mtls"), None, Some("PEM"), Some("upgrade the app")),
            "mini2"
        )
        .is_err());
        // mtls with no certificate is a protocol violation, not an empty success.
        let e = map_credential_response(cred_resp(Some("mtls"), None, None, None), "mini2")
            .unwrap_err();
        assert!(matches!(e, ShedError::Config(m) if m.contains("no certificate")));
        // token with no token, likewise.
        assert!(
            map_credential_response(cred_resp(Some("token"), None, None, None), "mini2").is_err()
        );
        // A reply for the wrong server never reaches the provider.
        let e = map_credential_response(cred_resp(Some("token"), Some("tok"), None, None), "other")
            .unwrap_err();
        assert!(matches!(e, ShedError::Config(m) if m.contains("unexpected server")));
    }

    // -----------------------------------------------------------------------
    // Plan 002 §7 P9 — the shared `credential.response` vectors.
    //
    // The SAME file the Swift mapper tests and the Go/Rust agent suites read.
    // This is the Rust CLIENT half: the Tauri app's external-broker path runs
    // every reply through `map_credential_response`, and the embedded path runs
    // its own struct through the same `credential_from_parts`, so one fixture
    // gates both modes and pins them against Swift.
    // -----------------------------------------------------------------------

    // -----------------------------------------------------------------------
    // Plan 002 §7 P5 — the PRE-ACK rows of the capability table.
    //
    // A client that was never started stays in the tri-state's `unknown`
    // forever, which is exactly the startup window the rule exists for. The
    // rows that need a live agent (`supported` / `unsupported`) are in
    // `host_agent.rs`, next to the UDS double that produces them.
    // -----------------------------------------------------------------------

    struct FixedClock;
    impl crate::traits::Clock for FixedClock {
        fn now_unix(&self) -> i64 {
            1_700_000_000
        }
    }

    fn disconnected_minter(modes: Arc<AuthModeRegistry>) -> HostAgentTokenMinter {
        let client = HostAgentClient::new("/nonexistent-shed-agent.sock", Arc::new(FixedClock));
        HostAgentTokenMinter::new(client)
            .with_modes(modes)
            .with_capability_wait(Duration::from_millis(50))
    }

    #[tokio::test]
    async fn pre_ack_supports_mtls_follows_what_is_known_about_the_servers() {
        // Nothing known → no keypair is generated for anyone (today's behavior
        // for the entire token-mode fleet).
        let modes = Arc::new(AuthModeRegistry::new());
        assert!(!disconnected_minter(modes.clone()).supports_mtls());
        // One mtls server is enough: `supports_mtls` takes no server, and a
        // false positive costs a CSR a token server ignores, while a false
        // negative costs a token.get a certificate-only server refuses.
        modes.record("prod", AuthMode::Mtls);
        assert!(disconnected_minter(modes).supports_mtls());
    }

    #[tokio::test]
    async fn pre_ack_mtls_mint_reports_connecting_not_upgrade() {
        // The §7 P5 headline: pre-ack, an mtls server must NOT produce a
        // "upgrade shed-host-agent" the user cannot act on, and must NOT fall
        // back to token.get.
        let modes = Arc::new(AuthModeRegistry::new());
        modes.record("prod", AuthMode::Mtls);
        let e = disconnected_minter(modes)
            .mint_credential("prod", &CredentialRequest::with_csr("QUJD"))
            .await
            .unwrap_err();
        let ShedError::Config(m) = e else {
            panic!("expected a Config error");
        };
        assert!(m.contains("connecting to shed-host-agent"), "{m}");
        assert!(!m.contains("upgrade"), "{m}");
    }

    #[tokio::test]
    async fn pre_ack_token_mint_keeps_the_immediate_token_get() {
        // A token entry learns nothing from the ack, so it does NOT pay the
        // wait — it takes the legacy path and fails on the dead socket.
        let modes = Arc::new(AuthModeRegistry::new());
        modes.record("prod", AuthMode::Token);
        let e = disconnected_minter(modes)
            .mint_credential("prod", &CredentialRequest::default())
            .await
            .unwrap_err();
        assert!(
            matches!(&e, ShedError::Transport(m) if m.contains("not connected")),
            "expected the token.get transport failure, got {e:?}"
        );
    }

    fn credential_fixture() -> serde_json::Value {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(
            "../../tests/host-agent-diff/fixtures/desktop-credential/credential_response.json",
        );
        let raw = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("read fixture {}: {e}", path.display()));
        serde_json::from_str(&raw).expect("fixture is valid JSON")
    }

    #[test]
    fn golden_credential_response_arms() {
        let fx = credential_fixture();
        assert_eq!(fx["protocol_version"], 1, "fixture version skew");
        let server = fx["server"].as_str().unwrap();

        // The caps are shared data, not a per-language constant: a client that
        // enforced its own numbers could accept a frame another refuses.
        let limits = &fx["limits"];
        assert_eq!(
            limits["token_bytes"].as_u64().unwrap() as usize,
            limits::MAX_TOKEN_BYTES
        );
        assert_eq!(
            limits["client_cert_bytes"].as_u64().unwrap() as usize,
            limits::MAX_CLIENT_CERT_BYTES
        );
        assert_eq!(
            limits["cert_serial_bytes"].as_u64().unwrap() as usize,
            limits::MAX_CERT_SERIAL_BYTES
        );
        assert_eq!(
            limits["error_bytes"].as_u64().unwrap() as usize,
            limits::MAX_ERROR_BYTES
        );
        assert_eq!(
            limits["csr_bytes"].as_u64().unwrap() as usize,
            limits::MAX_CSR_BYTES
        );

        let vectors = fx["vectors"].as_array().expect("vectors");
        assert!(!vectors.is_empty());
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let mut frame = v["frame"].clone();
            // An oversize vector carries no literal 4 KiB string: build it from
            // the cap so the file stays readable.
            if let Some(field) = v["oversize_field"].as_str() {
                let cap = limits[format!("{field}_bytes")].as_u64().unwrap() as usize;
                let ch = v["oversize_char"].as_str().unwrap();
                frame[field] = serde_json::Value::String(ch.repeat(cap + 1));
            }
            // Through the PRODUCTION decoder, so the serde field mapping is
            // gated too — not just the arm logic.
            let line = serde_json::to_vec(&frame).unwrap();
            let resp = match shed_core::approval::protocol::decode(&line).unwrap() {
                shed_core::approval::protocol::HostAgentInbound::CredentialResponse(r) => r,
                other => panic!("{name}: expected credential.response, got {other:?}"),
            };
            let got = map_credential_response(resp, server);
            let want = &v["expected"];
            match want["arm"].as_str().unwrap() {
                "token" => match got {
                    Ok(MintedCredential::Token(t)) => {
                        assert_eq!(t.token, want["token"].as_str().unwrap(), "{name}: token");
                        assert_eq!(
                            t.expires_at_unix,
                            want["expires_at_unix"].as_u64(),
                            "{name}: expiry"
                        );
                    }
                    other => panic!("{name}: expected a token arm, got {other:?}"),
                },
                "certificate" => match got {
                    Ok(MintedCredential::Certificate(c)) => {
                        assert_eq!(
                            c.cert_pem,
                            want["cert_pem"].as_str().unwrap(),
                            "{name}: pem"
                        );
                        assert_eq!(c.serial, want["serial"].as_str().unwrap(), "{name}: serial");
                        assert_eq!(
                            c.expires_at_unix,
                            want["expires_at_unix"].as_u64(),
                            "{name}: expiry"
                        );
                    }
                    other => panic!("{name}: expected a certificate arm, got {other:?}"),
                },
                "refused" => {
                    let needle = want["message_contains"].as_str().unwrap();
                    match got {
                        Err(ShedError::Config(m)) => {
                            assert!(m.contains(needle), "{name}: refusal {m:?} lacks {needle:?}")
                        }
                        other => panic!(
                            "{name}: expected a refusal containing {needle:?}, got {other:?}"
                        ),
                    }
                }
                other => panic!("{name}: unexpected fixture arm {other:?}"),
            }
        }
    }
}
