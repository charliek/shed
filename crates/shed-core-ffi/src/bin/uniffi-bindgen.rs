// The uniffi-bindgen CLI, built from this crate so `cargo run --bin
// uniffi-bindgen -- generate --library <staticlib> --language swift` emits the
// Swift bindings from the same uniffi version the crate is compiled against.
fn main() {
    uniffi::uniffi_bindgen_main()
}
