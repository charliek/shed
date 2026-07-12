fn main() {
    tauri_build::build();

    // The macOS sparkle-updater crate (C2) links Sparkle.framework, whose install
    // name is `@rpath/Sparkle.framework/...`, as a LOAD-TIME dependency. The bundled
    // `.app` resolves it via Tauri's `@executable_path/../Frameworks` rpath (the
    // bundler copies the framework there). But `cargo test` / `cargo run` and the
    // raw harness binary (`target/debug/shed-desktop-tauri`) are UNbundled, so dyld
    // would abort at startup unable to find the framework. Add an rpath to the
    // framework staged beside this manifest (`make sparkle-framework`) for DEBUG
    // builds only — the release DMG stays free of this dev-machine path and relies
    // solely on its bundled `@executable_path/../Frameworks` copy.
    let target_os = std::env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    let profile = std::env::var("PROFILE").unwrap_or_default();
    if target_os == "macos" && profile == "debug" {
        let manifest = std::env::var("CARGO_MANIFEST_DIR").unwrap();
        println!("cargo:rustc-link-arg=-Wl,-rpath,{manifest}");
    }
}
