//! A resolved shed server the source mints for: [`ServerTarget`], pure data.
//!
//! Moved here from `shed-broker`'s `discovery` module (plan 039 §3.7), which
//! re-exports it; the broker's saphyr-backed `~/.shed/config.yaml` reader stays there.

/// A resolved shed server this agent brokers for (mirror `config.go:ServerTarget`).
/// `name` is the identity key (empty in single-server mode); `url` is the broker base
/// URL. `ssh_host`/`ssh_port` are the SSH endpoint used to mint over `_bootstrap`;
/// `ssh_port == 0` means the entry omitted it (the agent can't self-mint).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServerTarget {
    pub name: String,
    pub url: String,
    /// The credentials-scoped bearer token from config (empty when not token-gated). In
    /// secure mode the agent mints this itself; carried for the (deferred) bus provider.
    pub token: String,
    pub tls_fingerprint: String,
    pub ssh_host: String,
    pub ssh_port: u16,
    /// The credential shape the server last issued, as recorded in the shed CLI config
    /// (`"token"`, `"mtls"`, or `""` for an entry written before the field existed).
    /// Mirror `config.go:ServerTarget.AuthMode`.
    ///
    /// It is knowledge, NOT policy. Every enrollment is CSR-first
    /// ([`crate::minter::CredentialMinter`] always sends a `csr=` argument) and the
    /// server's answer decides the mode, so a stale value can never lock the agent into
    /// the wrong one. What it buys is the two places where acting on stale knowledge is
    /// harmless but acting on none is not: whether a persisted certificate is worth
    /// loading at startup, and whether the desktop's `token.get` can be answered with an
    /// explicit "this server issues certificates" error instead of a mint that returns
    /// no token.
    ///
    /// The EMPTY string is preserved verbatim rather than defaulted to `"token"`, so
    /// "never recorded" and "recorded as token" stay distinguishable — the Go mirror
    /// keeps the same distinction and the shared golden fixture pins it.
    pub auth_mode: String,
    /// The named user the shed CLI recorded for this entry (`user`; empty when absent or
    /// null; [`shed_core::config::USER_NOT_A_STRING`] when it is a list or a map).
    /// The credential sources hold their certificates to it: one issued for another named
    /// user, or one that names no user, is refused (plan 037 §3.2.2). Part of the target,
    /// so a changed `user` restarts the server's sources (the supervisor's whole-target
    /// compare).
    pub user: String,
    /// The SSH key the shed CLI recorded for this entry (`identity_file`; empty when absent
    /// or null; [`shed_core::identityfile::IDENTITY_FILE_UNREADABLE`] when it is a list, a
    /// map, or a value not read exactly, and
    /// [`shed_core::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE`] for an entry with a merge
    /// key or a tagged key, each of which every check refuses). The minter runs ssh with it (`-i <path> -o
    /// IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none`), checking the file
    /// first, and a group that does not mint never sends a stored `credentials_token` for
    /// it (plan 037 A8). Part of the target, so a changed `identity_file` restarts the
    /// server's sources.
    pub identity_file: String,
}

impl ServerTarget {
    /// Whether the server is reached over https — the authoritative local signal that
    /// it runs in secure mode (tokens ⟺ TLS ⟺ secure) and that the agent should
    /// self-mint (mirror `config.go:IsSecure`: scheme, not fingerprint presence).
    /// Consumed by the broker supervisor's `should_mint` (`shed_broker::supervisor`).
    pub fn is_secure(&self) -> bool {
        self.url.to_lowercase().starts_with("https://")
    }

    /// Whether the server was last seen issuing client certificates (mirror
    /// `config.go:ServerTarget.IsMTLS` — `strings.EqualFold`, so the comparison is
    /// case-insensitive on both sides).
    ///
    /// Deliberately NOT the signal that decides whether to enroll: see [`Self::auth_mode`].
    pub fn is_mtls(&self) -> bool {
        self.auth_mode.eq_ignore_ascii_case("mtls")
    }
}
