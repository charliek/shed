// DashboardView.swift — the main window: header + sidebar + content pane.

import ShedKit
import SwiftUI

public struct DashboardView: View {
    @ObservedObject var state: AppState

    public init(state: AppState) {
        self.state = state
    }

    public var body: some View {
        HStack(spacing: 0) {
            SidebarView(state: state)
                .frame(width: Theme.sidebarWidth)
            Divider()
            VStack(spacing: 0) {
                header
                Divider()
                contentPane
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            }
            .background(Theme.bg)
        }
        .frame(minWidth: 760, minHeight: 480)
        .background(Theme.bg)
        // Both flows are centered modals (not top-edge .sheets) so they match the
        // design and render inside the window (screenshot-able via the IPC ops).
        .modalOverlay(isPresented: state.showCreateSheet,
                      onDismiss: { state.showCreateSheet = false }) {
            CreateShedSheet(state: state)
        }
        .modalOverlay(isPresented: state.showLaunchSheet,
                      onDismiss: { state.showLaunchSheet = false }) {
            AgentLaunchSheet(state: state)
        }
    }

    private var header: some View {
        HStack(spacing: 8) {
            Image(systemName: "shippingbox")
                .foregroundStyle(Theme.textSecondary)
            Text("shed desktop")
                .font(.system(size: 13, weight: .medium))
                .foregroundStyle(Theme.textSecondary)
            Spacer()
            if !state.approvals.isEmpty {
                Label("\(state.approvals.count) pending", systemImage: "shield.lefthalf.filled")
                    .font(.system(size: 12, weight: .medium))
                    .foregroundStyle(Theme.danger)
            }
            HStack(spacing: 6) {
                Circle()
                    .fill(state.hostAgentConnected ? Theme.ok : Theme.textMuted)
                    .frame(width: 7, height: 7)
                Text(state.hostAgentConnected ? "host agent · connected" : "host agent · not connected")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.textSecondary)
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 11)
        .background(Theme.bg)
    }

    @ViewBuilder
    private var contentPane: some View {
        switch state.pane {
        case .sheds:
            ShedListView(state: state)
        case .approvals:
            ApprovalsView(state: state)
        case .agents:
            AgentsView(state: state)
        case .activity:
            ActivityView(state: state)
        case .egress:
            EgressView(state: state)
        case .system:
            SystemView(state: state)
        }
    }
}

struct PlaceholderPane: View {
    let title: String
    let systemImage: String
    let message: String
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(title).font(.system(size: 16, weight: .semibold))
            Spacer()
            VStack(spacing: 10) {
                Image(systemName: systemImage)
                    .font(.system(size: 28))
                    .foregroundStyle(.tertiary)
                Text(message)
                    .font(.system(size: 12))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: 320)
            }
            .frame(maxWidth: .infinity)
            Spacer()
        }
        .padding(18)
    }
}
