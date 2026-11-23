//! Typed shed target identifier (`roost:<server>/<shed>`).
//!
//! Every host, row, craze operation, and lane operation is addressed by a
//! [`ShedId`]: the configured server name and the shed name.
//!
//! **Parsing a valid [`ShedId`] NEVER authorizes SSH by itself.** Every
//! operation in [`crate::ShedHosts`] checks that the shed is currently
//! server-listed in `listed_sheds` before building a transport or spawning SSH.

use std::fmt;
use std::str::FromStr;

use serde::{Deserialize, Deserializer, Serialize, Serializer};

/// The wire prefix for every shed target token (`roost:<server>/<shed>`).
pub const SHED_PREFIX: &str = "roost:";

/// A shed addressed by its configured server and shed name
/// (`roost:<server>/<shed>`).
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct ShedId {
    server: String,
    shed: String,
}

impl ShedId {
    /// Construct a [`ShedId`] from a server name and shed name, validating both.
    pub fn new(server: impl Into<String>, shed: impl Into<String>) -> Result<Self, String> {
        let server = server.into();
        let shed = shed.into();
        validate_server(&server)?;
        validate_shed(&shed)?;
        Ok(Self { server, shed })
    }

    /// Parse a shed target token (`roost:<server>/<shed>`).
    ///
    /// Rejects empty input, a missing or extra `/`, control characters,
    /// leading/trailing whitespace on the server alias, and any whitespace in
    /// the shed name. Internal spaces in server aliases (e.g. `Dev Lab`,
    /// accepted by `shed server add --name`) are preserved.
    pub fn parse(raw: &str) -> Result<Self, String> {
        let trimmed = raw.trim();
        if trimmed.is_empty() {
            return Err("an empty shed target names nothing (want roost:<server>/<shed>)".into());
        }
        let Some(rest) = trimmed.strip_prefix(SHED_PREFIX) else {
            return Err(format!(
                "{trimmed:?} is not a shed target (want roost:<server>/<shed>)"
            ));
        };
        let Some((server, shed)) = rest.split_once('/') else {
            return Err(format!(
                "{trimmed:?} names no server/shed pair (want roost:<server>/<shed>)"
            ));
        };
        if server.is_empty() || shed.is_empty() || shed.contains('/') {
            return Err(format!(
                "{trimmed:?} is not a valid shed target (want roost:<server>/<shed>)"
            ));
        }
        Self::new(server, shed)
    }

    /// The configured shed server name.
    pub fn server(&self) -> &str {
        &self.server
    }

    /// The shed name on [`Self::server`].
    pub fn shed(&self) -> &str {
        &self.shed
    }

    /// The canonical wire target string: `"roost:<server>/<shed>"`.
    pub fn as_target(&self) -> String {
        self.to_string()
    }
}

fn validate_server(value: &str) -> Result<(), String> {
    if value.is_empty()
        || value.trim() != value
        || value.contains('/')
        || value
            .chars()
            .any(|c| c.is_control() || (c.is_whitespace() && c != ' '))
    {
        return Err(format!(
            "invalid server name {value:?} (want non-empty name without '/', control chars, or surrounding whitespace)"
        ));
    }
    Ok(())
}

fn validate_shed(value: &str) -> Result<(), String> {
    if value.is_empty()
        || value.contains('/')
        || value.chars().any(|c| c.is_whitespace() || c.is_control())
    {
        return Err(format!(
            "invalid shed name {value:?} (want non-empty name without '/' or whitespace)"
        ));
    }
    Ok(())
}

impl fmt::Display for ShedId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{SHED_PREFIX}{}/{}", self.server, self.shed)
    }
}

impl FromStr for ShedId {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        Self::parse(s)
    }
}

impl Serialize for ShedId {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(&self.as_target())
    }
}

impl<'de> Deserialize<'de> for ShedId {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let s = String::deserialize(deserializer)?;
        ShedId::parse(&s).map_err(serde::de::Error::custom)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_and_formats_shed_targets() {
        let id = ShedId::parse("roost:mini3/dev").expect("valid shed target");
        assert_eq!(id.server(), "mini3");
        assert_eq!(id.shed(), "dev");
        assert_eq!(id.as_target(), "roost:mini3/dev");
        assert_eq!(id.to_string(), "roost:mini3/dev");

        // Surrounding whitespace is trimmed on parse.
        assert_eq!(
            ShedId::parse("  roost:srv-1/my_shed  ").unwrap(),
            id_of("srv-1", "my_shed")
        );

        // Server aliases with internal spaces (e.g. `shed server add --name "Dev Lab"`)
        // are preserved cleanly.
        let spaced = ShedId::parse("roost:Dev Lab/dev").expect("server alias with space");
        assert_eq!(spaced.server(), "Dev Lab");
        assert_eq!(spaced.shed(), "dev");
        assert_eq!(spaced.as_target(), "roost:Dev Lab/dev");
        assert_eq!(ShedId::new("Dev Lab", "dev").unwrap(), spaced);

        // Serde round-trip as plain string.
        let json = serde_json::to_value(&id).unwrap();
        assert_eq!(json, serde_json::json!("roost:mini3/dev"));
        let back: ShedId = serde_json::from_value(json).unwrap();
        assert_eq!(back, id);
    }

    #[test]
    fn rejects_machines_bare_names_and_malformed_tokens() {
        for bad in [
            "",
            "   ",
            "localhost",
            "mini3",
            "machine:mini3",
            "machine:localhost",
            "roost:",
            "roost:mini3",
            "roost:mini3/",
            "roost:/dev",
            "roost:mini3/a/b",
            "roost: mini3/dev",
            "roost:mini3 /dev",
            "roost:mini\t3/dev",
            "roost:mini\n3/dev",
            "roost:mini3/d ev",
        ] {
            assert!(
                ShedId::parse(bad).is_err(),
                "{bad:?} must be rejected by ShedId::parse"
            );
            assert!(
                serde_json::from_value::<ShedId>(serde_json::json!(bad)).is_err(),
                "{bad:?} must be rejected by ShedId Deserialize"
            );
        }
    }

    fn id_of(server: &str, shed: &str) -> ShedId {
        ShedId::new(server, shed).unwrap()
    }
}
