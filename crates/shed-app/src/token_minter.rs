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
/// The mode comes from the reply's own `auth_mode` rather than from which field
/// happens to be populated: "the server said mtls but sent no certificate" is a
/// protocol violation worth reporting, and inferring the mode from the fields
/// would silently turn it into "token mode with an empty token".
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
    let expires_at_unix = resp
        .expires_at
        .as_deref()
        .and_then(timefmt::parse_unix)
        .map(|s| s.max(0) as u64);
    match AuthMode::from_wire(resp.auth_mode.as_deref()) {
        AuthMode::Token => {
            let token = resp.token.filter(|t| !t.is_empty()).ok_or_else(|| {
                ShedError::Config(format!("host agent returned no token for {server}"))
            })?;
            Ok(MintedCredential::Token(MintedToken {
                token,
                expires_at_unix,
            }))
        }
        AuthMode::Mtls => {
            let cert_pem = resp.client_cert.filter(|c| !c.is_empty()).ok_or_else(|| {
                ShedError::Config(format!(
                    "host agent reported mtls but returned no certificate for {server}"
                ))
            })?;
            Ok(MintedCredential::Certificate(MintedCertificate {
                cert_pem,
                serial: resp.cert_serial.unwrap_or_default(),
                expires_at_unix,
            }))
        }
    }
}

/// Map a `token.response` into a `MintedToken`, or an `Err` for a fail-closed
/// reply. Pure, so the fail-closed mapping is unit-tested without a live agent.
/// Expiry is a wire string parsed to unix seconds here (shed-app owns timestamp
/// parsing); an absent/unparseable expiry -> `None` -> the provider caches and
/// only re-mints on `invalidate()` (matches Swift + `token.rs`).
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
    let token = resp
        .token
        .filter(|t| !t.is_empty())
        .ok_or_else(|| ShedError::Config(format!("host agent returned no token for {server}")))?;
    let expires_at_unix = resp
        .expires_at
        .as_deref()
        .and_then(timefmt::parse_unix)
        .map(|s| s.max(0) as u64);
    Ok(MintedToken {
        token,
        expires_at_unix,
    })
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
