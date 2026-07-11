//! Build/version identity for the daemon. The Go-vs-Rust differential harness
//! masks the version value (§1 of the wire catalog), so slice 0 just emits a
//! nonempty, stable line — the analog of the Go `version.FullInfo()`.

/// `full_info` returns the daemon's version line, printed by the `version`
/// subcommand and carried in the LiveStatus `version` field.
pub fn full_info() -> String {
    format!("shed-host-agent {}", env!("CARGO_PKG_VERSION"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn full_info_is_nonempty() {
        assert!(full_info().starts_with("shed-host-agent "));
        assert!(full_info().len() > "shed-host-agent ".len());
    }
}
