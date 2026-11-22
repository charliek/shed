//! Plan 038 D1 crate-isolation contract.
//!
//! Enforces the workspace dependency boundaries via `cargo metadata` and
//! per-package `cargo tree` (which evaluates each crate with its own default
//! features, without workspace-wide feature unification from sibling members
//! like `shed-broker-harness`):
//! - `shed-lane` is a pure leaf (no `shed-core`, `shed-roost`, `shed-app`,
//!   `shed-broker`, `roost-ipc`, `reqwest`, `chrono`, or UI toolkits).
//! - `shed-core` is pure shed-server protocol (no `roost-ipc`, `shed-roost`,
//!   `shed-app`, `shed-broker`, `shed-craze`, or UI toolkits).
//! - `shed-craze` depends on `shed-lane` only (no `shed-core`, `shed-roost`,
//!   `shed-app`, `shed-broker`, `roost-ipc`, `reqwest`, `chrono`, or UI toolkits).
//! - `shed-app` (default features) has no `roost-ipc`, `shed-roost`, `shed-lane`
//!   (direct), `shed-craze`, `shed-broker`, `saphyr-parser`, or UI toolkits.
//! - `shed-broker`, `shed-broker-harness`, and `shedctl` have no `roost-ipc` or
//!   `shed-roost` (direct or transitive).
//! - `shed-roost` is the SOLE direct `roost-ipc` consumer in `crates/` (across
//!   normal, build, and dev dependencies).
//! - No crate in `crates/` depends on `shed-opencode`, `tauri`, `wry`, `tao`, or
//!   `webkit2gtk`.

use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::process::Command;

use serde_json::Value;

fn crates_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("..")
}

fn no_deps_metadata() -> Value {
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
        .expect("run cargo metadata --no-deps");
    assert!(
        out.status.success(),
        "cargo metadata failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    serde_json::from_slice(&out.stdout).expect("cargo metadata is JSON")
}

/// Every crate name in `pkg`'s resolved normal+build dependency graph when built
/// with its own default features (no feature unification from sibling members).
fn resolved_normal_deps(pkg: &str) -> HashSet<String> {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_string());
    let out = Command::new(cargo)
        .args([
            "tree",
            "--manifest-path",
            crates_dir()
                .join("Cargo.toml")
                .to_str()
                .expect("utf-8 manifest path"),
            "-p",
            pkg,
            "-e",
            "normal,build",
            "--prefix",
            "none",
            "--format",
            "{p}",
        ])
        .output()
        .expect("run cargo tree");
    assert!(
        out.status.success(),
        "cargo tree -p {pkg} failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    let stdout = String::from_utf8(out.stdout).expect("utf-8 cargo tree output");
    stdout
        .lines()
        .filter_map(|line| line.split_whitespace().next())
        .filter(|name| *name != pkg)
        .map(str::to_string)
        .collect()
}

const TOOLKIT_FORBIDDEN: &[&str] = &["tauri", "wry", "tao", "webkit2gtk", "shed-opencode"];

#[test]
fn crate_isolation_invariants() {
    let meta = no_deps_metadata();
    let packages = meta["packages"].as_array().expect("packages");

    // 1. No crate in `crates/` may depend on GUI toolkits or the deleted `shed-opencode`.
    for pkg in packages {
        let name = pkg["name"].as_str().expect("pkg name");
        let deps = resolved_normal_deps(name);
        for forbidden in TOOLKIT_FORBIDDEN {
            assert!(
                !deps.contains(*forbidden),
                "workspace crate `{name}` transitively depends on forbidden `{forbidden}`"
            );
        }
    }

    // 2. `shed-lane`: pure leaf.
    let lane_deps = resolved_normal_deps("shed-lane");
    for forbidden in [
        "shed-core",
        "shed-roost",
        "shed-app",
        "shed-broker",
        "shed-craze",
        "roost-ipc",
        "reqwest",
        "chrono",
    ] {
        assert!(
            !lane_deps.contains(forbidden),
            "`shed-lane` must stay a pure leaf; found `{forbidden}` in its normal dep graph"
        );
    }

    // 3. `shed-core`: pure shed-server protocol.
    let core_deps = resolved_normal_deps("shed-core");
    for forbidden in [
        "shed-roost",
        "shed-app",
        "shed-broker",
        "shed-craze",
        "roost-ipc",
    ] {
        assert!(
            !core_deps.contains(forbidden),
            "`shed-core` must not depend on `{forbidden}`"
        );
    }

    // 4. `shed-craze`: depends on `shed-lane` only — no shed-core, roost-ipc, reqwest, chrono.
    let craze_deps = resolved_normal_deps("shed-craze");
    for forbidden in [
        "shed-core",
        "shed-roost",
        "shed-app",
        "shed-broker",
        "roost-ipc",
        "reqwest",
        "chrono",
    ] {
        assert!(
            !craze_deps.contains(forbidden),
            "`shed-craze` must not depend on `{forbidden}`"
        );
    }

    // 5. `shed-app` (default features): no roost-ipc, shed-roost, shed-craze,
    //    shed-broker, saphyr-parser, and no direct `shed-lane` dep.
    let app_pkg = packages
        .iter()
        .find(|p| p["name"].as_str() == Some("shed-app"))
        .expect("shed-app package");
    for dep in app_pkg["dependencies"].as_array().expect("dependencies") {
        let kind = dep["kind"].as_str();
        if kind != Some("dev") {
            let dep_name = dep["name"].as_str().unwrap_or_default();
            assert!(
                !["shed-lane", "shed-roost", "shed-craze", "roost-ipc"].contains(&dep_name),
                "`shed-app` must not directly depend on `{dep_name}`"
            );
            if dep_name == "shed-broker" {
                assert_eq!(
                    dep["optional"].as_bool(),
                    Some(true),
                    "`shed-app`'s `shed-broker` dependency must remain optional"
                );
            }
        }
    }
    let app_deps = resolved_normal_deps("shed-app");
    for forbidden in [
        "shed-roost",
        "shed-craze",
        "shed-broker",
        "roost-ipc",
        "saphyr-parser",
    ] {
        assert!(
            !app_deps.contains(forbidden),
            "`shed-app` (default features) must not depend on `{forbidden}`"
        );
    }

    // 6. `shed-broker`, `shed-broker-harness`, and `shedctl` never pull in
    //    `roost-ipc` or `shed-roost`.
    for pkg in ["shed-broker", "shed-broker-harness", "shedctl"] {
        let deps = resolved_normal_deps(pkg);
        for forbidden in ["roost-ipc", "shed-roost"] {
            assert!(
                !deps.contains(forbidden),
                "`{pkg}` must not depend on `{forbidden}`"
            );
        }
    }

    // 7. `shed-roost` is the SOLE direct consumer of `roost-ipc` in `crates/`
    //    (across ALL dependency kinds: normal, build, AND dev).
    let mut roost_ipc_consumers: Vec<&str> = Vec::new();
    for pkg in packages {
        let name = pkg["name"].as_str().expect("pkg name");
        let has_roost_ipc = pkg["dependencies"]
            .as_array()
            .expect("dependencies")
            .iter()
            .any(|d| d["name"].as_str() == Some("roost-ipc"));
        if has_roost_ipc {
            roost_ipc_consumers.push(name);
        }
    }
    roost_ipc_consumers.sort_unstable();
    assert_eq!(
        roost_ipc_consumers,
        vec!["shed-roost"],
        "`shed-roost` must be the SOLE direct consumer of `roost-ipc` in `crates/`"
    );
}
