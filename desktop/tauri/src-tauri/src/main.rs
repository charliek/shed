// Prevents an extra console window on Windows in release; harmless elsewhere.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    shed_desktop_tauri_lib::run();
}
