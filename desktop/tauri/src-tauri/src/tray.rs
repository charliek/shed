//! The system tray / menu-bar (B1a foundation + B1 menu + B1b mac popover).
//!
//! Both platforms get a native menu (Open Dashboard / Approvals / Preferences /
//! Quit). On **Linux** that menu IS the tray surface — Tauri emits no tray
//! left-click events or icon geometry there (`tauri-2.11/src/tray/mod.rs`: "Linux:
//! Unsupported"), so a rich anchored popover is impossible; a click opens the menu.
//! On **macOS** the menu moves to **right-click** and a **left-click opens the rich
//! popover** (B1b) — a second, opaque webview mirroring the Swift `MenuBarContentView`,
//! anchored at the tray via `tauri-plugin-positioner`. Building the tray is best-effort
//! (a headless / no-SNI host may have nowhere to show it), so a failure logs and the
//! app keeps running — the dashboard window is always reachable.

use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::TrayIconBuilder;
#[cfg(target_os = "macos")]
use tauri::tray::{MouseButton, MouseButtonState, TrayIconEvent};
use tauri::{AppHandle, Emitter, Manager};

/// The tray icon id (so later milestones can fetch + update it — e.g. the count badge).
pub const TRAY_ID: &str = "shed-tray";

/// The macOS menu-bar popover window label (a 2nd webview; created in `lib.rs::setup`
/// on macOS only). Its snapshot is keyed under this label, read by `tray.dump`.
pub const POPOVER_ID: &str = "popover";

/// The popover is a fixed WIDTH; its height content-sizes — the webview measures its
/// rendered content and reports it to `resize_popover` (clamped to these bounds), so
/// the window hugs its content like the Swift `NSPopover` (no dead space). Built at
/// MAX height so a silently-ignored `set_size` (a non-resizable-window regression)
/// leaves the window tall and is caught by `tray.dump`'s reported height.
#[cfg(target_os = "macos")]
pub const POPOVER_WIDTH: f64 = 320.0;
#[cfg(target_os = "macos")]
pub const POPOVER_MIN_HEIGHT: f64 = 120.0;
#[cfg(target_os = "macos")]
pub const POPOVER_MAX_HEIGHT: f64 = 640.0;

/// Menu item ids — kept in sync with [`menu_item_ids`] so `tray.dump` can assert
/// the menu over IPC without reaching into the native menu.
const ID_OPEN: &str = "open";
const ID_APPROVALS: &str = "approvals";
const ID_PREFS: &str = "preferences";
const ID_QUIT: &str = "quit";

/// The tray menu's actionable item ids, in order — the drivable view of the menu.
pub fn menu_item_ids() -> Vec<&'static str> {
    vec![ID_OPEN, ID_APPROVALS, ID_PREFS, ID_QUIT]
}

/// Build + install the tray. Best-effort: returns the build error to the caller,
/// which logs and continues (the window stays reachable regardless).
pub fn build(app: &AppHandle) -> tauri::Result<()> {
    let open = MenuItem::with_id(app, ID_OPEN, "Open Dashboard", true, None::<&str>)?;
    let approvals = MenuItem::with_id(app, ID_APPROVALS, "Approvals", true, None::<&str>)?;
    let prefs = MenuItem::with_id(app, ID_PREFS, "Preferences…", true, None::<&str>)?;
    let sep = PredefinedMenuItem::separator(app)?;
    let quit = MenuItem::with_id(app, ID_QUIT, "Quit Shed Desktop", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&open, &approvals, &prefs, &sep, &quit])?;

    let mut builder = TrayIconBuilder::with_id(TRAY_ID)
        .tooltip("Shed Desktop")
        .menu(&menu)
        .on_menu_event(|app, event| match event.id.as_ref() {
            ID_OPEN => show_main(app),
            ID_APPROVALS => open_pane(app, "approvals"),
            ID_PREFS => open_prefs(app),
            ID_QUIT => app.exit(0),
            _ => {}
        });
    // macOS: the menu is right-click; a left-click toggles the rich popover (B1b).
    // On Linux the menu stays the click surface (no popover — no tray click events).
    #[cfg(target_os = "macos")]
    {
        builder = builder
            .show_menu_on_left_click(false)
            .on_tray_icon_event(|tray, event| {
                let app = tray.app_handle();
                // Cache the tray rect for the positioner (from ANY tray event) so
                // `TrayCenter` can anchor the popover.
                tauri_plugin_positioner::on_tray_event(app, &event);
                // `button_state` fires Up AND Down per click, so toggle on ONE edge.
                if let TrayIconEvent::Click {
                    button: MouseButton::Left,
                    button_state: MouseButtonState::Up,
                    ..
                } = event
                {
                    toggle_popover(app);
                }
            });
    }
    // Icon. macOS: a monochrome TEMPLATE glyph (a black-on-transparent shippingbox
    // silhouette) + `icon_as_template(true)`, so the status item auto-tints for the
    // light/dark menu bar and sizes to ~18pt — Swift `NSStatusItem` parity. The @2x
    // (36px) asset renders crisp when tray-icon scales it to 18pt on retina. Falls
    // back to the colored window icon if the embedded template can't decode.
    // Elsewhere (Linux): the colored window icon — a template silhouette would render
    // as a black blob on GTK trays (no template concept there).
    #[cfg(target_os = "macos")]
    match tauri::image::Image::from_bytes(include_bytes!("../icons/tray-template@2x.png")) {
        Ok(icon) => builder = builder.icon(icon).icon_as_template(true),
        Err(e) => {
            eprintln!("shed-desktop-tauri: tray template icon failed to decode ({e}); using app icon");
            if let Some(icon) = app.default_window_icon().cloned() {
                builder = builder.icon(icon);
            }
        }
    }
    #[cfg(not(target_os = "macos"))]
    if let Some(icon) = app.default_window_icon().cloned() {
        builder = builder.icon(icon);
    }
    builder.build(app)?;
    Ok(())
}

/// Update the menu-bar status-item title to the running-shed count (`" N"`, empty
/// when zero) — Swift `updateStatusItemTitle` parity (`AppModel.swift`). Driven from
/// `ui_report` when the dashboard (`main`) reports its shed list, so it tracks live
/// like the Swift status item. macOS-only (Linux trays carry no title). A
/// process-global cached count skips the native `set_title` on identical re-renders
/// (`ui_report` fires every dashboard render — e.g. the Approvals pane's 1s tick).
#[cfg(target_os = "macos")]
pub fn update_running_count(app: &AppHandle, running: usize) {
    use std::sync::atomic::{AtomicIsize, Ordering};
    static LAST: AtomicIsize = AtomicIsize::new(-1);
    if LAST.swap(running as isize, Ordering::Relaxed) == running as isize {
        return;
    }
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        let title = if running > 0 { format!(" {running}") } else { String::new() };
        let _ = tray.set_title(Some(title));
    }
}
#[cfg(not(target_os = "macos"))]
pub fn update_running_count(_app: &AppHandle, _running: usize) {}

/// Show + focus the main dashboard window via the single `present_main_window` path
/// (which also flips macOS back to `Regular` in production — a visible dashboard
/// gets a Dock icon; guarded off under the harness).
pub fn show_main(app: &AppHandle) {
    crate::ipc::present_main_window(app);
}

/// Raise the dashboard on `pane` — show the window, then emit the same `navigate`
/// event the webview's bridge listens for. The window is hidden, not closed, so the
/// listener is live; showing it first makes the emit land.
fn open_pane(app: &AppHandle, pane: &str) {
    show_main(app);
    let _ = app.emit("navigate", serde_json::json!({ "pane": pane }));
}

/// Raise the dashboard + open the Preferences modal (the `ui.show_preferences` path).
fn open_prefs(app: &AppHandle) {
    show_main(app);
    let _ = app.emit("show-preferences", serde_json::json!({}));
}

// -- B1b popover show/hide (shared by the mac tray-icon click AND the hermetic
//    `tray.show`/`tray.toggle`/`tray.hide` IPC drive ops — one Rust path) -------

/// Show the popover, anchored at the tray. `TrayCenter` needs a real tray click to
/// have cached the rect (via `on_tray_event`); a hermetic `tray.show` with no prior
/// click falls back to the top-right so the window still appears. Emits
/// `popover-refresh` so the popover re-fetches on open. macOS-only (a no-op elsewhere
/// — there's no popover window).
pub fn show_popover(app: &AppHandle) {
    #[cfg(target_os = "macos")]
    if let Some(win) = app.get_webview_window(POPOVER_ID) {
        use tauri_plugin_positioner::{Position, WindowExt};
        if win.move_window_constrained(Position::TrayCenter).is_err() {
            let _ = win.move_window(Position::TopRight);
        }
        let _ = win.show();
        let _ = win.set_focus();
        let _ = app.emit_to(POPOVER_ID, "popover-refresh", ());
    }
    #[cfg(not(target_os = "macos"))]
    let _ = app;
}

/// Hide the popover (a no-op if it's already hidden / not present).
pub fn hide_popover(app: &AppHandle) {
    #[cfg(target_os = "macos")]
    if let Some(win) = app.get_webview_window(POPOVER_ID) {
        let _ = win.hide();
    }
    #[cfg(not(target_os = "macos"))]
    let _ = app;
}

/// Toggle the popover — hide it if visible, else show + anchor it.
pub fn toggle_popover(app: &AppHandle) {
    #[cfg(target_os = "macos")]
    {
        if let Some(win) = app.get_webview_window(POPOVER_ID) {
            if win.is_visible().unwrap_or(false) {
                let _ = win.hide();
                return;
            }
        }
        show_popover(app);
    }
    #[cfg(not(target_os = "macos"))]
    let _ = app;
}

/// The popover footer's "Open dashboard": the Sheds-pane menu action + dismiss the
/// popover.
pub fn open_dashboard(app: &AppHandle) {
    open_pane(app, "sheds");
    hide_popover(app);
}

/// The popover footer's "Preferences…": the Preferences menu action + dismiss the
/// popover.
pub fn open_preferences(app: &AppHandle) {
    open_prefs(app);
    hide_popover(app);
}
