// MenuBarContentView.swift — the menu-bar popover content (SwiftUI hosted
// inside an NSPopover managed by the app, so it's force-openable and
// screenshot-able). M0 shows running sheds + quick actions; the approval
// section lands in M3.

import AppKit
import ShedKit
import SwiftUI

public struct MenuBarContentView: View {
    @ObservedObject var state: AppState
    let onOpenDashboard: () -> Void
    let onOpenPreferences: () -> Void
    let onCheckForUpdates: () -> Void
    let onQuit: () -> Void

    public init(
        state: AppState,
        onOpenDashboard: @escaping () -> Void,
        onOpenPreferences: @escaping () -> Void,
        onCheckForUpdates: @escaping () -> Void = {},
        onQuit: @escaping () -> Void
    ) {
        self.state = state
        self.onOpenDashboard = onOpenDashboard
        self.onOpenPreferences = onOpenPreferences
        self.onCheckForUpdates = onCheckForUpdates
        self.onQuit = onQuit
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text("shed desktop").font(.system(size: 13, weight: .medium))
                Spacer()
                Text(state.hostAgentConnected ? "● host agent" : "○ host agent")
                    .font(.system(size: 11))
                    .foregroundStyle(state.hostAgentConnected ? Color.green : Color.secondary)
            }
            .padding(.horizontal, 14).padding(.vertical, 10)
            Divider()

            if !state.approvals.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    Text("\(state.approvals.count) pending approval\(state.approvals.count == 1 ? "" : "s")")
                        .font(.system(size: 11, weight: .medium)).foregroundStyle(.red)
                    ForEach(state.approvals.prefix(3)) { item in
                        let req = item.request
                        HStack(spacing: 8) {
                            NamespaceIcon(req.namespace).scaleEffect(0.8)
                            VStack(alignment: .leading, spacing: 1) {
                                Text("\(req.namespace) \(req.op)").font(.system(size: 12, weight: .medium))
                                Text(req.qualifiedShed).font(.system(size: 11)).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Button { state.onApprovalDecide?(req, item.approveChoice) } label: {
                                Image(systemName: item.gate.isBiometric ? "touchid" : "checkmark")
                            }
                            .buttonStyle(.borderless).foregroundStyle(.green)
                            Button { state.onApprovalDecide?(req, item.denyChoice) } label: { Image(systemName: "xmark") }
                                .buttonStyle(.borderless).foregroundStyle(.red)
                        }
                    }
                }
                .padding(.horizontal, 14).padding(.vertical, 8)
                .background(Color.red.opacity(0.08))
                Divider()
            }

            VStack(alignment: .leading, spacing: 0) {
                menuHeader("Sheds", trailing: "\(state.runningCount) running")
                ForEach(state.sheds.filter { $0.status == .running }.prefix(6)) { shed in
                    HStack(spacing: 8) {
                        StatusDot(.green)
                        Text("\(shed.host)/\(shed.name)").font(.system(size: 12))
                        Spacer()
                    }
                    .padding(.leading, 30).padding(.trailing, 14).padding(.vertical, 5)
                }
                if state.runningCount == 0 {
                    Text("no running sheds")
                        .font(.system(size: 12)).foregroundStyle(.tertiary)
                        .padding(.leading, 30).padding(.vertical, 5)
                }
            }
            .padding(.vertical, 6)

            Divider()
            MenuActionRow("Open dashboard", systemImage: "macwindow", action: onOpenDashboard)
            MenuActionRow("Preferences…", systemImage: "gearshape", action: onOpenPreferences)
            MenuActionRow("Check for Updates…", systemImage: "arrow.down.circle", action: onCheckForUpdates)
            MenuActionRow("Quit", systemImage: "power", action: onQuit)
                .padding(.bottom, 6)
        }
        .frame(width: 300)
        // The menu panel is background-less; paint an opaque neutral surface so
        // the dropdown stays solid and readable.
        .background(Color(nsColor: .windowBackgroundColor))
    }

    private func menuHeader(_ title: String, trailing: String) -> some View {
        HStack {
            Text(title).font(.system(size: 13, weight: .medium))
            Spacer()
            Text(trailing).font(.system(size: 11)).foregroundStyle(.tertiary)
        }
        .padding(.horizontal, 14).padding(.vertical, 4)
    }
}

private struct MenuActionRow: View {
    let title: String
    let systemImage: String
    let action: () -> Void
    @State private var hovering = false

    init(_ title: String, systemImage: String, action: @escaping () -> Void) {
        self.title = title
        self.systemImage = systemImage
        self.action = action
    }

    // The native menu-highlight blue (matches Docker / Tailscale / cmux) with
    // its paired text color. controlAccentColor stays vivid even though the
    // panel isn't key (selectedContentBackgroundColor would fall back to gray).
    private var highlight: Color { Color(nsColor: .controlAccentColor) }
    private var highlightFg: Color { Color(nsColor: .alternateSelectedControlTextColor) }

    var body: some View {
        Button(action: action) {
            HStack(spacing: 10) {
                Image(systemName: systemImage).frame(width: 18)
                    .foregroundStyle(hovering ? highlightFg : Theme.textSecondary)
                Text(title).font(.system(size: 13))
                    .foregroundStyle(hovering ? highlightFg : Theme.text)
                Spacer()
            }
            .padding(.horizontal, 8).padding(.vertical, 6)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 6).fill(hovering ? highlight : Color.clear))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .padding(.horizontal, 6)
        .onHover { hovering = $0 }
    }
}
