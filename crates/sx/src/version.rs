//! Build/version identity for `sx`. The Go-vs-Rust RC differential harness
//! masks the version value (`tests/rc-parity/normalize.py:mask_version` asserts
//! only a `<prog> <non-blank>` shape and then replaces both tokens), so the
//! value itself is never wire-compared — only the shape is.
//!
//! Version source: from plan 011 `sx` is its own release component, built by
//! goreleaser's `builder: rust` with `SX_VERSION={{ .Version }}` (the tag,
//! already `v`-stripped; a snapshot pseudo-version under `--snapshot`). We read
//! it at compile time via `option_env!` so the shipped `sx version` reports the
//! release version rather than `CARGO_PKG_VERSION` — which tracks the *desktop*
//! selector (`crates/Cargo.toml [workspace.package]`) and is NOT bumped on an
//! sx-only tag. Local dev (var unset) falls back to `CARGO_PKG_VERSION`.
//!
//! This mirrors `crates/shed-host-agent/src/version.rs` deliberately: same
//! `pick_version` shape, same empty-string guard, same test set. Two components,
//! one convention.

/// Choose the version string: the release-injected value when present, else the
/// crate's compiled `CARGO_PKG_VERSION`. Pure so both branches are unit-testable
/// in a single build (a compiled `option_env!` fixes only one branch per build).
fn pick_version<'a>(injected: Option<&'a str>, fallback: &'a str) -> &'a str {
    injected.filter(|v| !v.is_empty()).unwrap_or(fallback)
}

/// The version string printed by `sx version` / `sx rc version`.
pub fn version() -> &'static str {
    pick_version(option_env!("SX_VERSION"), env!("CARGO_PKG_VERSION"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn version_is_nonempty() {
        assert!(!version().is_empty());
    }

    #[test]
    fn pick_version_prefers_injected() {
        assert_eq!(pick_version(Some("0.8.3"), "0.8.1"), "0.8.3");
        assert_eq!(
            pick_version(Some("0.0.0-snapshot-abc"), "0.8.1"),
            "0.0.0-snapshot-abc"
        );
    }

    #[test]
    fn pick_version_falls_back_when_absent_or_empty() {
        assert_eq!(pick_version(None, "0.8.1"), "0.8.1");
        // An empty injected var (e.g. `SX_VERSION=`) must not produce an
        // `sx ` line with a blank version — the parity harness's shape assert
        // would fail on it, and so would a human reading `sx version`.
        assert_eq!(pick_version(Some(""), "0.8.1"), "0.8.1");
    }
}
