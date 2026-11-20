//! The credential-shape rules for a minted CONTROL credential: what the embedded
//! broker's mint must look like before the shed-core provider adopts it. A
//! fail-closed answer (no credential, an ambiguous one, an unparseable expiry, an
//! oversized field) surfaces as an `Err`, so the FSM sends NOTHING — never a
//! static downgrade (F6).
//!
//! Broker-gated (`lib.rs`): the embedded bridge is the only caller.

use shed_core::http::ShedError;
use shed_core::token::{limits, AuthMode, MintedCertificate, MintedCredential, MintedToken};

use crate::timefmt;

/// A minted credential's payload, normalized off the embedded broker's
/// `MintedControlCredential` (`broker_bridge.rs`). Absent fields arrive as empty
/// strings.
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

/// The credential-shape rules, in one place.
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
    //    reaching the arm logic.
    if let Some(field) = over_cap_field(&parts) {
        return Err(ShedError::Config(format!(
            "the broker returned an oversized {field} for {server}; refusing"
        )));
    }
    // 5. A populated expiry must PARSE. Silently dropping it would disable the
    //    proactive refresh and leave the credential to die mid-request; an
    //    ABSENT one genuinely means "the minter reported none" and is fine.
    let expires_at_unix = match parsed_expiry(parts.expires_at.as_deref()) {
        Ok(e) => e,
        Err(raw) => {
            return Err(ShedError::Config(format!(
                "the broker returned an unparseable expiry {raw:?} for {server}; refusing"
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
                "the broker reported unknown auth mode {other:?} for {server}; \
                 refusing to fall back to a bearer token — upgrade shed-desktop"
            )));
        }
    };
    // 6. The selected arm's payload must be complete AND exclusive. A token arm
    //    carrying certificate fields (or vice versa) is a protocol violation —
    //    not an empty success, and not something to resolve by preferring one
    //    field over the other.
    match mode {
        AuthMode::Token => {
            if !parts.client_cert.is_empty() || !parts.cert_serial.is_empty() {
                return Err(ShedError::Config(format!(
                    "the broker returned a token AND certificate fields for {server}; \
                     refusing an ambiguous credential"
                )));
            }
            minted_token(parts.token, expires_at_unix, server).map(MintedCredential::Token)
        }
        AuthMode::Mtls => {
            if parts.client_cert.is_empty() {
                return Err(ShedError::Config(format!(
                    "the broker reported auth mode mtls but returned no certificate for {server}"
                )));
            }
            if !parts.token.is_empty() {
                return Err(ShedError::Config(format!(
                    "the broker returned a certificate AND a token for {server}; \
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
/// parse. An empty string counts as absent.
fn parsed_expiry(raw: Option<&str>) -> Result<Option<u64>, &str> {
    let Some(raw) = raw.filter(|r| !r.is_empty()) else {
        return Ok(None);
    };
    match timefmt::parse_unix(raw) {
        Some(s) => Ok(Some(s.max(0) as u64)),
        None => Err(raw),
    }
}

/// The empty-token guard, shared by every token-shaped mint (the token-only
/// mint and the token arm above): a blank token is NEVER a valid mint, so the
/// FSM must send nothing rather than a valid-looking downgrade (F6).
pub(crate) fn minted_token(
    token: String,
    expires_at_unix: Option<u64>,
    server: &str,
) -> Result<MintedToken, ShedError> {
    if token.is_empty() {
        return Err(ShedError::Config(format!(
            "the broker returned no token for {server}"
        )));
    }
    Ok(MintedToken {
        token,
        expires_at_unix,
    })
}

/// A wire expiry string as unix seconds, for the token-only mint. An
/// absent/unparseable expiry -> `None` -> the provider caches and only re-mints
/// on `invalidate()` (matches `token.rs`); shed-app owns timestamp parsing.
pub(crate) fn expiry_unix(raw: Option<&str>) -> Option<u64> {
    raw.and_then(timefmt::parse_unix).map(|s| s.max(0) as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parts(auth_mode: Option<&str>, token: &str, cert: &str, serial: &str) -> CredentialParts {
        CredentialParts {
            auth_mode: auth_mode.map(String::from),
            token: token.into(),
            client_cert: cert.into(),
            cert_serial: serial.into(),
            expires_at: None,
        }
    }

    fn refusal(p: CredentialParts) -> String {
        match credential_from_parts(p, "prod") {
            Err(ShedError::Config(m)) => m,
            other => panic!("expected a Config refusal, got {other:?}"),
        }
    }

    // The arms the bridge's own tests (`broker_bridge.rs`) do not reach through
    // `MintedControlCredential`, which always names a mode.
    #[test]
    fn absent_empty_and_legacy_modes_are_token() {
        for mode in [None, Some(""), Some("token"), Some("secure")] {
            match credential_from_parts(parts(mode, "tok", "", ""), "prod").unwrap() {
                MintedCredential::Token(t) => assert_eq!(t.token, "tok", "{mode:?}"),
                other => panic!("{mode:?}: expected a token, got {other:?}"),
            }
        }
    }

    #[test]
    fn an_unknown_mode_is_refused_exactly_as_spelled() {
        for mode in ["quantum", "MTLS", " mtls "] {
            let m = refusal(parts(Some(mode), "", "PEM", ""));
            assert!(m.contains("unknown auth mode"), "{mode:?}: {m}");
        }
    }

    #[test]
    fn ambiguous_answers_are_refused() {
        let m = refusal(parts(Some("mtls"), "tok", "PEM", ""));
        assert!(m.contains("ambiguous credential"), "{m}");
        // A serial alone is a certificate field too.
        let m = refusal(parts(Some("token"), "tok", "", "0a"));
        assert!(m.contains("ambiguous credential"), "{m}");
    }

    #[test]
    fn an_mtls_answer_without_a_serial_is_a_certificate() {
        match credential_from_parts(parts(Some("mtls"), "", "PEM", ""), "prod").unwrap() {
            MintedCredential::Certificate(c) => {
                assert_eq!(c.cert_pem, "PEM");
                assert_eq!(c.serial, "");
                assert_eq!(c.expires_at_unix, None);
            }
            other => panic!("expected a certificate, got {other:?}"),
        }
    }

    #[test]
    fn an_unparseable_expiry_refuses_the_certificate_arm_too() {
        let mut p = parts(Some("mtls"), "", "PEM", "0a");
        p.expires_at = Some("next tuesday".into());
        assert!(refusal(p).contains("unparseable expiry"));
    }

    #[test]
    fn each_oversized_field_is_named() {
        let cases = [
            (
                parts(
                    Some("token"),
                    &"t".repeat(limits::MAX_TOKEN_BYTES + 1),
                    "",
                    "",
                ),
                "oversized token",
            ),
            (
                parts(
                    Some("mtls"),
                    "",
                    &"c".repeat(limits::MAX_CLIENT_CERT_BYTES + 1),
                    "",
                ),
                "oversized certificate for",
            ),
            (
                parts(
                    Some("mtls"),
                    "",
                    "PEM",
                    &"s".repeat(limits::MAX_CERT_SERIAL_BYTES + 1),
                ),
                "oversized certificate serial",
            ),
        ];
        for (p, want) in cases {
            let m = refusal(p);
            assert!(m.contains(want), "{want}: {m}");
        }
        // At the cap is fine.
        let at_cap = parts(Some("token"), &"t".repeat(limits::MAX_TOKEN_BYTES), "", "");
        assert!(credential_from_parts(at_cap, "prod").is_ok());
    }
}
