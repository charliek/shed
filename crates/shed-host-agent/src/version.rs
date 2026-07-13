//! Build/version identity for the daemon. The Go-vs-Rust differential harness
//! masks the version value (§1 of the wire catalog), so the value itself is
//! never wire-compared — this is the analog of the Go `version.FullInfo()`.
//!
//! Version source: the release pipeline builds this binary with
//! `builder: rust` and sets `SHED_HOST_AGENT_VERSION={{ .Version }}` (the
//! goreleaser tag version, already `v`-stripped; a snapshot pseudo-version in
//! `--snapshot`). We read it at compile time via `option_env!` so the shipped
//! `version` reports the release version rather than `CARGO_PKG_VERSION` — which
//! tracks the *desktop* selector (`crates/Cargo.toml [workspace.package]`) and
//! is NOT bumped on a go-only release. Local dev (var unset) falls back to
//! `CARGO_PKG_VERSION`.

/// Choose the version string: the release-injected value when present, else the
/// crate's compiled `CARGO_PKG_VERSION`. Pure so both branches are unit-testable
/// in a single build (a compiled `option_env!` fixes only one branch per build).
fn pick_version<'a>(injected: Option<&'a str>, fallback: &'a str) -> &'a str {
    injected.filter(|v| !v.is_empty()).unwrap_or(fallback)
}

/// `full_info` returns the daemon's version line, printed by the `version`
/// subcommand and carried in the LiveStatus `version` field.
pub fn full_info() -> String {
    let version = pick_version(option_env!("SHED_HOST_AGENT_VERSION"), env!("CARGO_PKG_VERSION"));
    format!("shed-host-agent {version}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn full_info_is_nonempty() {
        assert!(full_info().starts_with("shed-host-agent "));
        assert!(full_info().len() > "shed-host-agent ".len());
    }

    #[test]
    fn pick_version_prefers_injected() {
        assert_eq!(pick_version(Some("0.8.0"), "0.7.10"), "0.8.0");
        assert_eq!(pick_version(Some("0.0.0-snapshot-abc"), "0.7.10"), "0.0.0-snapshot-abc");
    }

    #[test]
    fn pick_version_falls_back_when_absent_or_empty() {
        assert_eq!(pick_version(None, "0.7.10"), "0.7.10");
        // An empty injected var (e.g. `SHED_HOST_AGENT_VERSION=`) must not
        // produce a `shed-host-agent ` line with a blank version.
        assert_eq!(pick_version(Some(""), "0.7.10"), "0.7.10");
    }
}
