//! Plan 038 D1 crate-isolation contract.
//!
//! Enforces the workspace dependency boundaries via `cargo metadata` and
//! per-package `cargo tree` over every target (each crate resolved alone, so
//! no sibling member unifies features into it). Every rule holds under both the
//! crate's default features and `--all-features`:
//! - `shed-lane` is a pure leaf (no `shed-core`, `shed-roost`, `shed-app`,
//!   `roost-ipc`, `reqwest`, `chrono`, or UI toolkits).
//! - `shed-core` is pure shed-server protocol (no `roost-ipc`, `shed-roost`,
//!   `shed-app`, `shed-craze`, or UI toolkits).
//! - `shed-craze` depends on `shed-lane` only (no `shed-core`, `shed-roost`,
//!   `shed-app`, `roost-ipc`, `reqwest`, `chrono`, or UI toolkits).
//! - `shed-app` has no `roost-ipc`, `shed-roost`, `shed-lane`
//!   (direct), `shed-craze`, `saphyr-parser`, or UI toolkits.
//! - `shed-desktop-core`: toolkit-free desktop runtime — no `tauri`, `wry`,
//!   `tao`, `webkit2gtk`, `iced`, `saphyr-parser`, and no direct `roost-ipc`
//!   (normal, build, or dev).
//! - `shedctl` has no `roost-ipc` or `shed-roost` (direct or transitive).
//! - `shed-roost` is the SOLE direct `roost-ipc` consumer in `crates/` (across
//!   normal, build, and dev dependencies).
//! - No crate in `crates/` depends on `tauri`, `wry`, `tao`,
//!   `webkit2gtk`, or `iced`.
//! - `shed-provision` reaches no workspace crate; `shed-provision-providers`
//!   reaches only `shed-provision`; neither reaches `saphyr-parser`,
//!   `tokio` or `reqwest` (plan 039 §3.7). `shed-core`, `shed-lane`,
//!   `shed-roost`, `shed-craze` and `shed-app` reach
//!   neither, so shed-mobile's build is unchanged. All of these hold across
//!   normal, build and dev edges.
//! - `shed-source` (plan 039 §3.7) reaches no workspace crate but `shed-core`,
//!   `shed-lane` and `shed-provision`, and never `saphyr-parser`; `shed-core`, `shed-lane`,
//!   `shed-roost`, `shed-craze` and `shed-app` do not reach
//!   it, so shed-mobile's build is unchanged. All across normal, build and dev
//!   edges.

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

/// Every crate name in `pkg`'s resolved dependency graph for the given `-e`
/// edge kinds (`normal,build` or `normal,build,dev`), for EVERY target and
/// for both `pkg`'s default features and `--all-features`: a rule here is
/// unconditional, so a non-default feature or a target-specific dependency
/// that reaches a forbidden crate must fail it too.
fn resolved_deps(pkg: &str, edges: &str) -> HashSet<String> {
    let mut deps = cargo_tree(pkg, edges, false);
    deps.extend(cargo_tree(pkg, edges, true));
    deps
}

fn cargo_tree(pkg: &str, edges: &str, all_features: bool) -> HashSet<String> {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_string());
    let mut cmd = Command::new(cargo);
    cmd.arg("tree");
    if all_features {
        cmd.arg("--all-features");
    }
    let out = cmd
        .args([
            "--target",
            "all",
            "--manifest-path",
            crates_dir()
                .join("Cargo.toml")
                .to_str()
                .expect("utf-8 manifest path"),
            "-p",
            pkg,
            "-e",
            edges,
            "--prefix",
            "none",
            "--format",
            "{p}",
        ])
        .output()
        .expect("run cargo tree");
    assert!(
        out.status.success(),
        "cargo tree -p {pkg} -e {edges} (all_features={all_features}) failed: {}",
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

fn resolved_normal_deps(pkg: &str) -> HashSet<String> {
    resolved_deps(pkg, "normal,build")
}

const RETIRED_ADAPTER: &str = concat!("shed-", "opencode");
const TOOLKIT_FORBIDDEN: &[&str] = &["tauri", "wry", "tao", "webkit2gtk", "iced", RETIRED_ADAPTER];

#[test]
fn crate_isolation_invariants() {
    let meta = no_deps_metadata();
    let packages = meta["packages"].as_array().expect("packages");

    // 1. No crate in `crates/` may depend on GUI toolkits across normal,
    //    build, OR dev dependencies.
    for pkg in packages {
        let name = pkg["name"].as_str().expect("pkg name");
        for dep in pkg["dependencies"].as_array().expect("dependencies") {
            let dep_name = dep["name"].as_str().unwrap_or_default();
            assert!(
                !TOOLKIT_FORBIDDEN.contains(&dep_name),
                "workspace crate `{name}` declares forbidden dependency `{dep_name}` (kind={:?})",
                dep["kind"].as_str().unwrap_or("normal")
            );
        }
        let deps = resolved_deps(name, "normal,build,dev");
        for forbidden in TOOLKIT_FORBIDDEN {
            assert!(
                !deps.contains(*forbidden),
                "workspace crate `{name}` transitively depends on forbidden `{forbidden}` (normal/build/dev)"
            );
        }
    }

    // 2. `shed-lane`: pure leaf.
    let lane_deps = resolved_normal_deps("shed-lane");
    for forbidden in [
        "shed-core",
        "shed-roost",
        "shed-app",
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
    for forbidden in ["shed-roost", "shed-app", "shed-craze", "roost-ipc"] {
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
        "roost-ipc",
        "reqwest",
        "chrono",
    ] {
        assert!(
            !craze_deps.contains(forbidden),
            "`shed-craze` must not depend on `{forbidden}`"
        );
    }

    // 5. `shed-app`: no roost-ipc, shed-roost, shed-craze,
    //    saphyr-parser, and no direct `shed-lane` dep.
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
        }
    }
    let app_deps = resolved_deps("shed-app", "normal,build");
    for forbidden in ["shed-roost", "shed-craze", "roost-ipc", "saphyr-parser"] {
        assert!(
            !app_deps.contains(forbidden),
            "`shed-app` must not depend on `{forbidden}`"
        );
    }

    // 6. `shed-desktop-core`: toolkit-free, no direct `roost-ipc`
    //    across normal, build, AND dev dependencies.
    let desktop_core_pkg = packages
        .iter()
        .find(|p| p["name"].as_str() == Some("shed-desktop-core"))
        .expect("shed-desktop-core package");
    for dep in desktop_core_pkg["dependencies"]
        .as_array()
        .expect("dependencies")
    {
        let dep_name = dep["name"].as_str().unwrap_or_default();
        assert_ne!(
            dep_name, "roost-ipc",
            "`shed-desktop-core` must NEVER directly depend on `roost-ipc` (normal, build, or dev)"
        );
    }
    let desktop_core_deps = resolved_deps("shed-desktop-core", "normal,build,dev");
    for forbidden in ["tauri", "wry", "tao", "webkit2gtk", "iced", "saphyr-parser"] {
        assert!(
            !desktop_core_deps.contains(forbidden),
            "`shed-desktop-core` must not depend on `{forbidden}` (normal/build/dev)"
        );
    }

    // 7. `shedctl` never pulls in `roost-ipc` or `shed-roost`.
    for pkg in ["shedctl"] {
        let deps = resolved_normal_deps(pkg);
        for forbidden in ["roost-ipc", "shed-roost"] {
            assert!(
                !deps.contains(forbidden),
                "`{pkg}` must not depend on `{forbidden}`"
            );
        }
    }

    // 8. The provision crates (plan 039 §3.7). Workspace crates are taken
    //    from the metadata, so a crate added later is covered without an
    //    edit here.
    let workspace: Vec<&str> = packages
        .iter()
        .map(|p| p["name"].as_str().expect("pkg name"))
        .collect();
    for (pkg, allowed) in [
        ("shed-provision", &[][..]),
        ("shed-provision-providers", &["shed-provision"][..]),
    ] {
        let deps = resolved_deps(pkg, "normal,build,dev");
        for member in &workspace {
            if *member != pkg && !allowed.contains(member) {
                assert!(
                    !deps.contains(*member),
                    "`{pkg}` must reach no workspace crate but {allowed:?}; found `{member}` (normal/build/dev)"
                );
            }
        }
        for forbidden in ["saphyr-parser", "tokio", "reqwest"] {
            assert!(
                !deps.contains(forbidden),
                "`{pkg}` must not depend on `{forbidden}` (normal/build/dev)"
            );
        }
    }
    for pkg in ["shed-core", "shed-lane", "shed-roost", "shed-craze"] {
        let deps = resolved_deps(pkg, "normal,build,dev");
        for forbidden in ["shed-provision", "shed-provision-providers"] {
            assert!(
                !deps.contains(forbidden),
                "`{pkg}` must not depend on `{forbidden}` (normal/build/dev; shed-mobile links it)"
            );
        }
    }
    let app_deps = resolved_deps("shed-app", "normal,build,dev");
    for forbidden in ["shed-provision", "shed-provision-providers"] {
        assert!(
            !app_deps.contains(forbidden),
            "`shed-app` must not depend on `{forbidden}` (normal/build/dev)"
        );
    }

    // 8b. `shed-source` (plan 039 §3.7): the machine identity, credential store,
    //     minter and control-token provider, and the
    //     delivery stream client. Reaches only `shed-core`, `shed-lane` and
    //     `shed-provision` (every feature, `test-support` included,
    //     and every target), never `saphyr-parser`; shed-mobile's crates
    //     do not reach it.
    let deps = resolved_deps("shed-source", "normal,build,dev");
    // `shed-provision` (pure: the contract's documents and the stream's framing) is the
    // delivery stream client's (plan 039 A6 commit 2); it reaches no workspace crate.
    let allowed = ["shed-core", "shed-lane", "shed-provision"];
    for member in &workspace {
        if *member != "shed-source" && !allowed.contains(member) {
            assert!(
                !deps.contains(*member),
                "`shed-source` must reach no workspace crate but {allowed:?}; found `{member}` (normal/build/dev)"
            );
        }
    }
    assert!(
        !deps.contains("saphyr-parser"),
        "`shed-source` must not depend on `saphyr-parser` (normal/build/dev)"
    );
    for pkg in ["shed-core", "shed-lane", "shed-roost", "shed-craze"] {
        assert!(
            !resolved_deps(pkg, "normal,build,dev").contains("shed-source"),
            "`{pkg}` must not depend on `shed-source` (normal/build/dev; shed-mobile links it)"
        );
    }
    assert!(
        !resolved_deps("shed-app", "normal,build,dev").contains("shed-source"),
        "`shed-app` must not depend on `shed-source` (normal/build/dev)"
    );

    // 9. `shed-roost` is the SOLE direct consumer of `roost-ipc` in `crates/`
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
