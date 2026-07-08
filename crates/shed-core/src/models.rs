//! Wire DTOs ported from shed-desktop's `Models.swift`.
//!
//! These decoders reproduce the defensive semantics pinned by
//! `ModelDecodingTests` exactly: `{"sheds": null}` -> `[]`, omitted optionals,
//! `host` absent on the wire (stamped by the client), lenient `ShedStatus`,
//! `"?"` name sentinels, and timestamps carried VERBATIM as strings (flexible
//! parsing + all display helpers stay in Swift, off the decode path).
//!
//! Rust field names are snake_case and shed-server JSON is snake_case, so no
//! `#[serde(rename)]` is needed; serde also maps a missing `Option` field to
//! `None`, so those need no `default` either. `default` is applied only where it
//! does real work (sentinels, collections, zero-valued scalars/structs).
//!
//! Scope (M1): the shed-server *read* DTOs. `CreateShedRequest` (a request
//! body) lands with the create flow in M4.

use serde::{Deserialize, Deserializer, Serialize};

/// Deserialize `T`, mapping an explicit JSON `null` to `T::default()`. serde's
/// `#[serde(default)]` only covers an ABSENT field; shed-server sends `null` for
/// empty collections (`{"sheds": null}`, `df` arrays), which a bare
/// `#[serde(default)] Vec<_>` rejects. Pair them: `#[serde(default,
/// deserialize_with = "null_default")]`.
pub(crate) fn null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

fn unknown_name() -> String {
    "?".to_string()
}

/// A shed's lifecycle status. Lenient like the Swift enum: an unrecognized value
/// (`#[serde(other)]`) OR an absent field (`default` on the field) both decode
/// to `Unknown`, so a new server status never breaks decode.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum ShedStatus {
    Running,
    Stopped,
    Starting,
    Error,
    #[default]
    #[serde(other)]
    Unknown,
}

/// `GET /api/info`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct ServerInfo {
    pub name: String,
    pub version: String,
    pub backend: Option<String>,
    pub ssh_port: Option<i64>,
    pub http_port: Option<i64>,
}

/// A shed. `host` is absent from shed-server JSON (the client stamps it after
/// decode); it defaults to "" here to mirror `Shed.init(from:)`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct Shed {
    #[serde(default)]
    pub host: String,
    pub name: String,
    #[serde(default)]
    pub status: ShedStatus,
    pub backend: Option<String>,
    pub repo: Option<String>,
    pub image: Option<String>,
    pub image_digest: Option<String>,
    pub local_dir: Option<String>,
    pub ip_address: Option<String>,
    pub cpus: Option<i64>,
    pub memory_mb: Option<i64>,
    // Carried verbatim — never parsed/normalized here (Swift owns flexible
    // timestamp parsing for display).
    pub created_at: Option<String>,
    pub started_at: Option<String>,
    #[serde(default, deserialize_with = "null_default")]
    pub active_namespaces: Vec<String>,
}

/// The `{"sheds": [...] | null}` wrapper for `GET /api/sheds`. `null` and an
/// omitted key both decode to `[]`.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct ShedList {
    #[serde(default, deserialize_with = "null_default")]
    pub sheds: Vec<Shed>,
}

/// One installed image (`GET /api/images`). Lenient for pre-v0.6.1 servers that
/// omit alias/is_default; absent `name` -> `"?"`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct ShedImage {
    #[serde(default = "unknown_name")]
    pub name: String,
    pub docker_ref: Option<String>,
    pub alias: Option<String>,
    #[serde(default)]
    pub is_default: bool,
    #[serde(default)]
    pub cached: bool,
    #[serde(default)]
    pub in_use: bool,
    pub digest: Option<String>,
    pub source: Option<String>,
    #[serde(default)]
    pub size_bytes: i64,
}

/// The `{"images": [...] | null}` wrapper for `GET /api/images`.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct ImageList {
    #[serde(default, deserialize_with = "null_default")]
    pub images: Vec<ShedImage>,
}

/// A logical/physical byte pair. Missing halves -> 0.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct DiskSize {
    #[serde(default)]
    pub logical_bytes: i64,
    #[serde(default)]
    pub physical_bytes: i64,
}

/// One image/shed/orphan disk entry. Absent `name` -> `"?"`, absent size -> 0/0.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct DiskEntry {
    #[serde(default = "unknown_name")]
    pub name: String,
    pub docker_ref: Option<String>,
    #[serde(default)]
    pub size: DiskSize,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct DiskTotals {
    #[serde(default)]
    pub images: DiskSize,
    #[serde(default)]
    pub sheds: DiskSize,
    #[serde(default)]
    pub snapshots: DiskSize,
    #[serde(default)]
    pub orphans: DiskSize,
    #[serde(default)]
    pub all: DiskSize,
}

/// `GET /api/system/df`. Arrays default to `[]` (null/omitted), totals to zero.
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize, Serialize)]
pub struct SystemDiskUsage {
    pub server_name: Option<String>,
    pub backend: Option<String>,
    #[serde(default, deserialize_with = "null_default")]
    pub images: Vec<DiskEntry>,
    #[serde(default, deserialize_with = "null_default")]
    pub sheds: Vec<DiskEntry>,
    #[serde(default, deserialize_with = "null_default")]
    pub orphans: Vec<DiskEntry>,
    #[serde(default)]
    pub totals: DiskTotals,
}

/// One egress profile fragment. Mirrors shed-server's `config.EgressProfile`
/// (all-lowercase single-word keys).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct EgressProfile {
    pub mode: Option<String>,
    pub allow: Option<Vec<String>>,
    pub deny: Option<Vec<String>>,
    pub rule: Option<String>,
}

/// One entry of `GET /api/egress/profiles`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct EgressProfileInfo {
    pub name: String,
    pub source: String,
    pub profile: EgressProfile,
}

/// Body for `POST /api/sheds`. Only non-null fields are sent (mirrors Swift's
/// Codable, which omits nil optionals). `repo`/`local_dir` are mutually exclusive.
#[derive(Debug, Clone, Default, Serialize)]
pub struct CreateShedRequest {
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub repo: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub local_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub backend: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpus: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub memory_mb: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub no_provision: Option<bool>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn server_info_full_fixture() {
        let v: ServerInfo =
            serde_json::from_str(include_str!("../../fixtures/server_info.json")).unwrap();
        assert_eq!(v.name, "mini2");
        assert_eq!(v.version, "0.6.2");
        assert_eq!(v.backend.as_deref(), Some("firecracker"));
        assert_eq!(v.ssh_port, Some(2222));
        assert_eq!(v.http_port, Some(8080));
    }

    #[test]
    fn server_info_minimal() {
        let v: ServerInfo = serde_json::from_str(r#"{"name":"m","version":"1"}"#).unwrap();
        assert_eq!(v.backend, None);
        assert_eq!(v.ssh_port, None);
    }

    #[test]
    fn shed_decodes_real_server_fixture() {
        // No `host`, many optionals omitted; extra fields (container_id, pid) ignored.
        let s: Shed = serde_json::from_str(include_str!("../../fixtures/shed_real.json")).unwrap();
        assert_eq!(s.name, "hello-world");
        assert_eq!(s.status, ShedStatus::Running);
        assert_eq!(s.backend.as_deref(), Some("firecracker"));
        assert_eq!(s.cpus, Some(2));
        assert_eq!(s.memory_mb, Some(4096));
        assert_eq!(s.host, ""); // absent on the wire; client stamps it
        assert_eq!(s.repo, None); // omitted -> None
        assert!(s.active_namespaces.is_empty()); // absent -> []
                                                 // Timestamps carried verbatim, offset preserved.
        assert_eq!(
            s.created_at.as_deref(),
            Some("2026-05-31T13:33:00.884935839-05:00")
        );
    }

    #[test]
    fn shed_digest_only() {
        let s: Shed = serde_json::from_str(
            r#"{"name":"x","status":"running","image_digest":"sha256:abcdef0123456789aa"}"#,
        )
        .unwrap();
        assert_eq!(s.image, None);
        assert_eq!(s.image_digest.as_deref(), Some("sha256:abcdef0123456789aa"));
    }

    #[test]
    fn shed_minimal() {
        let s: Shed = serde_json::from_str(r#"{"name":"x","status":"running"}"#).unwrap();
        assert_eq!(s.name, "x");
        assert_eq!(s.image, None);
        assert_eq!(s.image_digest, None);
    }

    #[test]
    fn shed_status_leniency() {
        // Unknown value -> Unknown.
        let s: Shed = serde_json::from_str(r#"{"name":"x","status":"provisioning"}"#).unwrap();
        assert_eq!(s.status, ShedStatus::Unknown);
        // Absent status -> Unknown.
        let s: Shed = serde_json::from_str(r#"{"name":"x"}"#).unwrap();
        assert_eq!(s.status, ShedStatus::Unknown);
        // Known value.
        let s: Shed = serde_json::from_str(r#"{"name":"x","status":"stopped"}"#).unwrap();
        assert_eq!(s.status, ShedStatus::Stopped);
    }

    #[test]
    fn sheds_null_and_omitted_decode_to_empty() {
        let w: ShedList = serde_json::from_str(r#"{"sheds": null}"#).unwrap();
        assert!(w.sheds.is_empty());
        let w: ShedList = serde_json::from_str(r#"{}"#).unwrap();
        assert!(w.sheds.is_empty());
    }

    #[test]
    fn active_namespaces_null_and_present() {
        let s: Shed = serde_json::from_str(r#"{"name":"x","active_namespaces":null}"#).unwrap();
        assert!(s.active_namespaces.is_empty()); // null -> []
        let s: Shed =
            serde_json::from_str(r#"{"name":"x","active_namespaces":["ssh-agent","aws"]}"#)
                .unwrap();
        assert_eq!(s.active_namespaces, vec!["ssh-agent", "aws"]);
    }

    #[test]
    fn image_enriched_fixture() {
        let img: ShedImage =
            serde_json::from_str(include_str!("../../fixtures/image_enriched.json")).unwrap();
        assert_eq!(img.alias.as_deref(), Some("base"));
        assert!(img.is_default);
        assert!(img.cached);
        assert!(!img.in_use);
        assert_eq!(img.docker_ref.as_deref(), Some("ghcr.io/x/base:v1"));
        assert_eq!(img.size_bytes, 1073741824);
    }

    #[test]
    fn image_lenient_pre_v061() {
        // Older server: no alias / is_default -> defaults, not an error.
        let img: ShedImage =
            serde_json::from_str(r#"{"name":"base","source":"config","cached":true}"#).unwrap();
        assert_eq!(img.alias, None);
        assert!(!img.is_default);
        assert!(img.cached);
        assert_eq!(img.size_bytes, 0);
    }

    #[test]
    fn image_absent_name_sentinel() {
        let img: ShedImage = serde_json::from_str(r#"{"cached":true}"#).unwrap();
        assert_eq!(img.name, "?");
    }

    #[test]
    fn images_null_decodes_to_empty() {
        let w: ImageList = serde_json::from_str(r#"{"images": null}"#).unwrap();
        assert!(w.images.is_empty());
    }

    #[test]
    fn system_df_fixture() {
        let df: SystemDiskUsage =
            serde_json::from_str(include_str!("../../fixtures/system_df.json")).unwrap();
        assert_eq!(df.server_name.as_deref(), Some("mini2"));
        assert_eq!(df.images.len(), 1);
        assert_eq!(df.images[0].name, "full");
        assert_eq!(df.images[0].size.logical_bytes, 1073741824);
        assert_eq!(df.sheds.len(), 1);
        assert!(df.orphans.is_empty());
        assert_eq!(df.totals.all.logical_bytes, 1073743872);
    }

    #[test]
    fn system_df_defaults_and_null_arrays() {
        let df: SystemDiskUsage = serde_json::from_str(r#"{}"#).unwrap();
        assert!(df.images.is_empty());
        assert!(df.sheds.is_empty());
        assert_eq!(df.totals.all.logical_bytes, 0);
        let df: SystemDiskUsage =
            serde_json::from_str(r#"{"images":null,"sheds":null,"orphans":null}"#).unwrap();
        assert!(df.images.is_empty());
    }

    #[test]
    fn disk_entry_absent_name_and_size() {
        let e: DiskEntry = serde_json::from_str(r#"{}"#).unwrap();
        assert_eq!(e.name, "?");
        assert_eq!(e.size, DiskSize::default());
    }

    #[test]
    fn egress_profiles_fixture() {
        let profiles: Vec<EgressProfileInfo> =
            serde_json::from_str(include_str!("../../fixtures/egress_profiles.json")).unwrap();
        assert_eq!(profiles.len(), 2);
        assert_eq!(profiles[0].name, "default");
        assert_eq!(profiles[0].source, "config");
        assert_eq!(profiles[0].profile.mode.as_deref(), Some("audit"));
        assert_eq!(
            profiles[0].profile.allow.as_deref(),
            Some(["*.github.com".to_string()].as_slice())
        );
        assert_eq!(profiles[1].source, "user");
        assert_eq!(profiles[1].profile.mode, None); // omitted
    }
}
