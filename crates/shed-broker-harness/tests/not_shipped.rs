//! The harness is test-only, and these checks keep it so:
//!
//! * no crate of this workspace depends on it, of any kind (a crate that did could
//!   carry it into a shipped binary);
//! * it is a workspace member but not a default member, so a plain `cargo build` or
//!   `cargo test` never turns on shed-app's `broker` feature for the whole workspace;
//! * the Tauri workspace (its own lock, so not in this metadata) does not name it;
//! * no release config, bundle or packaging script names it.
//!
//! The same idea as the Go reference directory server's `TestImportedByNoOtherMain`,
//! for Cargo: the workspace's own metadata, read the way cargo reports it.

use std::path::{Path, PathBuf};
use std::process::Command;

use serde_json::Value;

const HARNESS: &str = "shed-broker-harness";

fn crates_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("..")
}

fn repo_root() -> PathBuf {
    crates_dir().join("..")
}

fn metadata() -> Value {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_string());
    let out = Command::new(cargo)
        .args([
            "metadata",
            "--format-version",
            "1",
            "--no-deps",
            "--manifest-path",
        ])
        .arg(crates_dir().join("Cargo.toml"))
        .output()
        .expect("run cargo metadata");
    assert!(
        out.status.success(),
        "cargo metadata failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    serde_json::from_slice(&out.stdout).expect("cargo metadata is JSON")
}

#[test]
fn no_workspace_crate_depends_on_the_harness_and_it_is_not_a_default_member() {
    let meta = metadata();
    let packages = meta["packages"].as_array().expect("packages");
    // The control: the harness is in the metadata, so the checks below can see it.
    let harness_id = packages
        .iter()
        .find(|p| p["name"] == HARNESS)
        .and_then(|p| p["id"].as_str())
        .expect("the harness is a workspace member");
    assert!(
        packages.len() >= 3,
        "only {} packages listed",
        packages.len()
    );

    for package in packages {
        let name = package["name"].as_str().unwrap_or_default();
        if name == HARNESS {
            continue;
        }
        for dep in package["dependencies"].as_array().expect("dependencies") {
            assert_ne!(
                dep["name"], HARNESS,
                "{name} depends on {HARNESS} ({}); it is test-only and must ride into nothing",
                dep["kind"]
            );
        }
    }

    let defaults = meta["workspace_default_members"]
        .as_array()
        .expect("workspace_default_members (cargo 1.71+)");
    assert!(!defaults.is_empty(), "no default members listed");
    assert!(
        !defaults.iter().any(|id| id == harness_id),
        "{HARNESS} is a default member: a plain cargo build would turn on shed-app's \
         `broker` feature for the whole workspace"
    );
}

/// Every file under `dir` (recursively), or none when it does not exist.
fn files_under(dir: &Path) -> Vec<PathBuf> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir(dir) else {
        return out;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        if path.is_dir() {
            out.extend(files_under(&path));
        } else {
            out.push(path);
        }
    }
    out
}

#[test]
fn no_release_or_bundle_file_names_the_harness() {
    let root = repo_root();
    let mut files = vec![
        root.join("desktop/tauri/src-tauri/Cargo.toml"),
        root.join(".github/workflows/publish-images.yaml"),
    ];
    for file in &files {
        assert!(file.is_file(), "{} is missing", file.display());
    }
    let goreleaser: Vec<PathBuf> = std::fs::read_dir(&root)
        .expect("read the repo root")
        .flatten()
        .map(|e| e.path())
        .filter(|p| {
            p.file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.starts_with(".goreleaser") && n.ends_with(".yaml"))
        })
        .collect();
    assert!(
        !goreleaser.is_empty(),
        "no goreleaser config found at {}",
        root.display()
    );
    files.extend(goreleaser);
    for dir in ["desktop/scripts", "desktop/linux", "packaging"] {
        files.extend(files_under(&root.join(dir)));
    }
    for file in files {
        let Ok(text) = std::fs::read_to_string(&file) else {
            continue; // a binary file names nothing
        };
        assert!(
            !text.contains(HARNESS),
            "{} names {HARNESS}; it ships in no release",
            file.display()
        );
    }
}
