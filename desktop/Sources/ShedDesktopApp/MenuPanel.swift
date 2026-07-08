// MenuPanel.swift — a borderless panel for the menu-bar dropdown.
//
// Replaces NSPopover so the dropdown is fully opaque with no arrow (the
// standard menu-bar look — cf. Docker / 1Password / Tailscale). Borderless
// windows don't become key by default; allow it so in-panel controls behave.

import AppKit

final class MenuPanel: NSPanel {
    override var canBecomeKey: Bool { true }
}
