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

use std::time::Duration;

use async_trait::async_trait;
use shed_core::approval::{CredentialResponse, TokenResponse, CAP_CREDENTIAL_GET};
use shed_core::http::ShedError;
use shed_core::token::{
    AuthMode, CredentialRequest, MintedCertificate, MintedCredential, MintedToken, TokenMinter,
};

use crate::host_agent::{HostAgentClient, DEFAULT_TOKEN_TIMEOUT};
use crate::timefmt;

/// Mints control tokens for the shed-core `ControlTokenProvider` by asking the
/// host agent over the UDS. One instance serves every server — the server name
/// is threaded through `mint(server)`.
pub struct HostAgentTokenMinter {
    client: HostAgentClient,
    timeout: Duration,
}

impl HostAgentTokenMinter {
    pub fn new(client: HostAgentClient) -> Self {
        Self {
            client,
            timeout: DEFAULT_TOKEN_TIMEOUT,
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
        map_response(resp, server)
    }

    /// Advertised per CONNECTION, not per build: the answer is whether the agent
    /// on the other end of the socket right now can relay a CSR. A build that
    /// claimed mtls support unconditionally would make the provider generate a
    /// keypair and compose a CSR for every mint against an agent that will drop
    /// the frame.
    fn supports_mtls(&self) -> bool {
        self.client.supports(CAP_CREDENTIAL_GET)
    }

    async fn mint_credential(
        &self,
        server: &str,
        req: &CredentialRequest,
    ) -> Result<MintedCredential, ShedError> {
        let resp = self
            .client
            .request_credential(server, req.csr_base64(), self.timeout)
            .await
            .map_err(|e| ShedError::Transport(e.to_string()))?;
        map_credential_response(resp, server)
    }
}

/// Map a `credential.response` into a `MintedCredential`, or an `Err` for a
/// fail-closed reply. Pure, for the same reason `map_response` is.
///
/// Only the UDS-specific guards live here (an `error` field, a reply naming
/// another server); the credential SHAPE rules are
/// [`credential_from_parts`]'s — see there for why they are shared.
fn map_credential_response(
    resp: CredentialResponse,
    server: &str,
) -> Result<MintedCredential, ShedError> {
    if let Some(err) = resp.error.as_deref().filter(|e| !e.is_empty()) {
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
            expires_at_unix: expiry_unix(resp.expires_at.as_deref()),
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
    pub expires_at_unix: Option<u64>,
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
    // STRICT mode parse, deliberately not `AuthMode::from_wire`: that decoder's
    // unknown-means-token lenience is for BUNDLES (where an old client meeting
    // a future server should degrade to the populated shape), but this is a
    // minter's ANSWER — adopting a credential whose mode this build cannot
    // interpret as if it were a bearer token would fail open. Absent/empty and
    // the legacy "secure" spelling stay token (the compat contract); anything
    // else is refused by name.
    let mode = match parts.auth_mode.as_deref().map(str::trim) {
        None | Some("") | Some("token") | Some("secure") => AuthMode::Token,
        Some("mtls") => AuthMode::Mtls,
        Some(other) => {
            return Err(ShedError::Config(format!(
                "host agent reported unknown auth_mode {other:?} for {server}; \
                 refusing to adopt the credential (upgrade this client?)"
            )));
        }
    };
    match mode {
        AuthMode::Token => {
            minted_token(parts.token, parts.expires_at_unix, server).map(MintedCredential::Token)
        }
        AuthMode::Mtls => {
            if parts.client_cert.is_empty() {
                return Err(ShedError::Config(format!(
                    "host agent reported mtls but returned no certificate for {server}"
                )));
            }
            Ok(MintedCredential::Certificate(MintedCertificate {
                cert_pem: parts.client_cert,
                serial: parts.cert_serial,
                expires_at_unix: parts.expires_at_unix,
            }))
        }
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
            cert_serial: Some("0a0b".into()),
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
}
