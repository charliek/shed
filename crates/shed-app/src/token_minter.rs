//! The foreign side of the control-token FSM: a [`shed_core::token::TokenMinter`]
//! that mints a CONTROL token via the host agent's `token.get`. The shed-core
//! `ControlTokenProvider` caches/refreshes around this and invalidates on a 401;
//! a fail-closed reply (its `error` set, or no token) surfaces as an `Err`, so
//! the FSM sends NO token — never a static downgrade (F6). Ports Swift's
//! `HostAgentTokenMinter` + `ControlTokenProvider.hostAgent`.

use std::time::Duration;

use async_trait::async_trait;
use shed_core::approval::TokenResponse;
use shed_core::http::ShedError;
use shed_core::token::{MintedToken, TokenMinter};

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
}
