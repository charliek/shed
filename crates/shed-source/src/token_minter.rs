//! A [`shed_core::token::TokenMinter`] over this crate's control-token provider
//! ([`crate::controltoken::ControlTokenProvider`]) (plan 039 §3.7, commit 2): what
//! `shed-app`'s `EmbeddedTokenMinter` does over the broker today, here so that an
//! embedder without the broker (plan 039 PR B) can hand shed-core's `Backend` a control
//! minter.
//!
//! Two different types share the name `ControlTokenProvider`: shed-core's (the FSM in
//! front of a [`TokenMinter`], which generates the keypair and sends the CSR) and this
//! crate's (which runs the SSH bootstrap). [`SourceTokenMinter`] sits between them: it
//! relays shed-core's CSR verbatim and maps what the server issued onto shed-core's
//! credential, refusing every shape that is not exactly one complete credential
//! ([`credential_from_parts`]), so a bad mint sends nothing rather than a downgrade.

use std::sync::Arc;

use shed_core::http::ShedError;
use shed_core::token::{
    limits, AuthMode, CredentialRequest, MintedCertificate, MintedCredential, MintedToken,
    TokenMinter,
};

use crate::controltoken::{
    ControlCredentialMinter, ControlTokenMinter, ControlTokenProvider, MintedControlCredential,
};
use crate::time::parse_rfc3339_to_unix;

/// shed-core's [`TokenMinter`] over the control-token provider's two seams.
pub struct SourceTokenMinter {
    tokens: Arc<dyn ControlTokenMinter>,
    credentials: Arc<dyn ControlCredentialMinter>,
}

impl SourceTokenMinter {
    /// Both seams from one provider (production).
    pub fn new(provider: Arc<ControlTokenProvider>) -> Self {
        Self::from_parts(
            provider.clone() as Arc<dyn ControlTokenMinter>,
            provider as Arc<dyn ControlCredentialMinter>,
        )
    }

    /// The two seams taken separately (tests drive either against a stand-in).
    pub fn from_parts(
        tokens: Arc<dyn ControlTokenMinter>,
        credentials: Arc<dyn ControlCredentialMinter>,
    ) -> Self {
        SourceTokenMinter {
            tokens,
            credentials,
        }
    }
}

#[async_trait::async_trait]
impl TokenMinter for SourceTokenMinter {
    /// Static: the credential path is compiled in, so the provider always sends a CSR.
    fn supports_mtls(&self) -> bool {
        true
    }

    /// Relays shed-core's CSR (the provider holds the private half; nothing here makes
    /// a second keypair) and maps the issued credential.
    async fn mint_credential(
        &self,
        server: &str,
        req: &CredentialRequest,
    ) -> Result<MintedCredential, ShedError> {
        let minted = self
            .credentials
            .mint_control_credential(server, req.csr_base64().unwrap_or_default())
            .await
            .map_err(ShedError::Config)?;
        credential_from_parts(minted, server)
    }

    async fn mint(&self, server: &str) -> Result<MintedToken, ShedError> {
        let minted = self
            .tokens
            .mint_control(server)
            .await
            .map_err(ShedError::Config)?;
        // An absent or unparseable expiry is none: the provider caches until invalidated.
        let expiry = minted
            .expires_at
            .as_deref()
            .and_then(|e| parse_rfc3339_to_unix(e).ok().flatten())
            .map(|s| s.max(0) as u64);
        minted_token(minted.token, expiry, server)
    }
}

/// The credential-shape rules (as `shed-app`'s `credential_shape`): sizes first, a
/// present expiry must parse, the mode is matched exactly (absent, `token` and the
/// legacy `secure` are token; `mtls` is a certificate; anything else is refused), and
/// the chosen arm must be complete and exclusive.
pub fn credential_from_parts(
    m: MintedControlCredential,
    server: &str,
) -> Result<MintedCredential, ShedError> {
    let refuse = |msg: String| Err(ShedError::Config(msg));
    if m.token.len() > limits::MAX_TOKEN_BYTES {
        return refuse(format!(
            "the minter returned an oversized token for {server}; refusing"
        ));
    }
    if m.client_cert.len() > limits::MAX_CLIENT_CERT_BYTES {
        return refuse(format!(
            "the minter returned an oversized certificate for {server}; refusing"
        ));
    }
    if m.cert_serial.len() > limits::MAX_CERT_SERIAL_BYTES {
        return refuse(format!(
            "the minter returned an oversized certificate serial for {server}; refusing"
        ));
    }
    let expires_at_unix = match m.expires_at.as_deref().filter(|e| !e.is_empty()) {
        None => None,
        Some(raw) => match parse_rfc3339_to_unix(raw) {
            Ok(Some(s)) => Some(s.max(0) as u64),
            _ => {
                return refuse(format!(
                    "the minter returned an unparseable expiry {raw:?} for {server}; refusing"
                ))
            }
        },
    };
    let mode = match m.auth_mode.as_str() {
        "" | "token" | "secure" => AuthMode::Token,
        "mtls" => AuthMode::Mtls,
        other => {
            return refuse(format!(
                "the minter reported unknown auth mode {other:?} for {server}; \
                 refusing to fall back to a bearer token"
            ))
        }
    };
    match mode {
        AuthMode::Token => {
            if !m.client_cert.is_empty() || !m.cert_serial.is_empty() {
                return refuse(format!(
                    "the minter returned a token AND certificate fields for {server}; \
                     refusing an ambiguous credential"
                ));
            }
            minted_token(m.token, expires_at_unix, server).map(MintedCredential::Token)
        }
        AuthMode::Mtls => {
            if m.client_cert.is_empty() {
                return refuse(format!(
                    "the minter reported auth mode mtls but returned no certificate for {server}"
                ));
            }
            if !m.token.is_empty() {
                return refuse(format!(
                    "the minter returned a certificate AND a token for {server}; \
                     refusing an ambiguous credential"
                ));
            }
            Ok(MintedCredential::Certificate(MintedCertificate {
                cert_pem: m.client_cert,
                serial: m.cert_serial,
                expires_at_unix,
            }))
        }
    }
}

/// A blank token is never a valid mint.
fn minted_token(
    token: String,
    expires_at_unix: Option<u64>,
    server: &str,
) -> Result<MintedToken, ShedError> {
    if token.is_empty() {
        return Err(ShedError::Config(format!(
            "the minter returned no token for {server}"
        )));
    }
    Ok(MintedToken {
        token,
        expires_at_unix,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::controltoken::MintedControlToken;
    use std::sync::Mutex;

    /// Records the CSR it was handed and answers with `answer`.
    struct Creds {
        seen: Mutex<Vec<(String, String)>>,
        answer: Result<MintedControlCredential, String>,
    }

    #[async_trait::async_trait]
    impl ControlCredentialMinter for Creds {
        async fn mint_control_credential(
            &self,
            server: &str,
            csr: &str,
        ) -> Result<MintedControlCredential, String> {
            self.seen
                .lock()
                .unwrap()
                .push((server.to_string(), csr.to_string()));
            self.answer.clone()
        }
    }

    struct Tokens(Result<(String, Option<String>), String>);

    #[async_trait::async_trait]
    impl ControlTokenMinter for Tokens {
        async fn mint_control(&self, _server: &str) -> Result<MintedControlToken, String> {
            self.0
                .clone()
                .map(|(token, expires_at)| MintedControlToken { token, expires_at })
        }
    }

    fn cred(mode: &str, token: &str, cert: &str, serial: &str) -> MintedControlCredential {
        MintedControlCredential {
            auth_mode: mode.into(),
            token: token.into(),
            client_cert: cert.into(),
            cert_serial: serial.into(),
            expires_at: Some("2030-01-01T00:00:00Z".into()),
        }
    }

    fn minter(answer: Result<MintedControlCredential, String>) -> (SourceTokenMinter, Arc<Creds>) {
        let creds = Arc::new(Creds {
            seen: Mutex::new(Vec::new()),
            answer,
        });
        (
            SourceTokenMinter::from_parts(Arc::new(Tokens(Err("unused".into()))), creds.clone()),
            creds,
        )
    }

    #[tokio::test]
    async fn the_csr_is_relayed_verbatim_and_a_certificate_comes_back() {
        let (m, creds) = minter(Ok(cred("mtls", "", "PEM", "0a")));
        assert!(m.supports_mtls());
        let req = CredentialRequest::with_csr("Q1NS");
        match m.mint_credential("prod", &req).await.unwrap() {
            MintedCredential::Certificate(c) => {
                assert_eq!((c.cert_pem.as_str(), c.serial.as_str()), ("PEM", "0a"));
                assert_eq!(c.expires_at_unix, Some(1_893_456_000));
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(
            creds.seen.lock().unwrap().as_slice(),
            &[("prod".to_string(), "Q1NS".to_string())]
        );
    }

    #[tokio::test]
    async fn a_token_answer_is_a_token() {
        let (m, _) = minter(Ok(cred("token", "tok", "", "")));
        let req = CredentialRequest::with_csr("Q1NS");
        match m.mint_credential("prod", &req).await.unwrap() {
            MintedCredential::Token(t) => assert_eq!(t.token, "tok"),
            other => panic!("{other:?}"),
        }
    }

    #[tokio::test]
    async fn every_bad_shape_fails_closed() {
        let cases = [
            (
                Err("ssh: host key mismatch".to_string()),
                "host key mismatch",
            ),
            (Ok(cred("mtls", "", "", "")), "no certificate"),
            (Ok(cred("mtls", "tok", "PEM", "")), "ambiguous"),
            (Ok(cred("token", "tok", "", "0a")), "ambiguous"),
            (Ok(cred("token", "", "", "")), "no token"),
            (Ok(cred("quantum", "", "PEM", "")), "unknown auth mode"),
            (
                Ok(cred(
                    "token",
                    &"t".repeat(limits::MAX_TOKEN_BYTES + 1),
                    "",
                    "",
                )),
                "oversized token",
            ),
            (
                Ok(MintedControlCredential {
                    expires_at: Some("next tuesday".into()),
                    ..cred("mtls", "", "PEM", "")
                }),
                "unparseable expiry",
            ),
        ];
        for (answer, want) in cases {
            let (m, _) = minter(answer);
            let req = CredentialRequest::with_csr("Q1NS");
            match m.mint_credential("prod", &req).await {
                Err(ShedError::Config(msg)) => assert!(msg.contains(want), "{want}: {msg}"),
                other => panic!("{want}: {other:?}"),
            }
        }
    }

    #[tokio::test]
    async fn the_token_only_mint_parses_its_expiry_and_refuses_a_blank() {
        let creds: Arc<dyn ControlCredentialMinter> = Arc::new(Creds {
            seen: Mutex::new(Vec::new()),
            answer: Err("unused".into()),
        });
        let m = SourceTokenMinter::from_parts(
            Arc::new(Tokens(Ok((
                "ctl".into(),
                Some("2030-01-01T00:00:00Z".into()),
            )))),
            creds.clone(),
        );
        let t = m.mint("prod").await.unwrap();
        assert_eq!(
            (t.token.as_str(), t.expires_at_unix),
            ("ctl", Some(1_893_456_000))
        );
        let m = SourceTokenMinter::from_parts(
            Arc::new(Tokens(Ok(("ctl".into(), Some("garbage".into()))))),
            creds.clone(),
        );
        assert_eq!(m.mint("prod").await.unwrap().expires_at_unix, None);
        let m = SourceTokenMinter::from_parts(Arc::new(Tokens(Ok((String::new(), None)))), creds);
        assert!(matches!(m.mint("prod").await, Err(ShedError::Config(_))));
    }

    /// Over the real provider, through this crate's discovery: an entry with no
    /// `ssh_port` refuses before any mint, as shed-core's provider will see it.
    #[tokio::test]
    async fn over_the_provider_an_entry_without_ssh_port_never_mints() {
        use crate::minter::{Minted, Minter, MinterError};
        use crate::target::ServerTarget;
        struct Never;
        #[async_trait::async_trait]
        impl Minter for Never {
            async fn mint(&self, _: &ServerTarget, _: &str) -> Result<Minted, MinterError> {
                panic!("no mint")
            }
        }
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.yaml");
        std::fs::write(
            &path,
            "servers:\n    prod:\n        host: prod.example.test\n        api_url: https://prod.example.test:18443\n        tls_cert_fingerprint: sha256:00\n",
        )
        .unwrap();
        let provider = Arc::new(ControlTokenProvider::new(
            Arc::new(Never),
            path.to_str().unwrap(),
            crate::discovery::resolver(),
        ));
        let m = SourceTokenMinter::new(provider);
        let req = CredentialRequest::with_csr("Q1NS");
        match m.mint_credential("prod", &req).await {
            Err(ShedError::Config(msg)) => assert!(msg.contains("no ssh endpoint"), "{msg}"),
            other => panic!("{other:?}"),
        }
    }
}
