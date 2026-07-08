// SidebarView.swift — the five panes + the configured-hosts list.

import ShedKit
import SwiftUI

struct SidebarView: View {
    @ObservedObject var state: AppState

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            row(.sheds, "server.rack", "Sheds", count: state.sheds.count)
            row(.approvals, "lock.shield", "Approvals", count: state.approvals.isEmpty ? nil : state.approvals.count, danger: true)
            row(.agents, "wand.and.stars", "Agents", count: state.rcSessions.isEmpty ? nil : state.rcSessions.count)
            row(.activity, "list.bullet.rectangle", "Activity", count: nil)
            row(.egress, "network.badge.shield.half.filled", "Egress", count: nil)
            row(.system, "internaldrive", "System", count: nil)

            Divider().padding(.vertical, 12).padding(.horizontal, 4)

            Text("HOSTS")
                .font(.system(size: 11, weight: .semibold))
                .tracking(0.6)
                .foregroundStyle(Theme.textMuted)
                .padding(.leading, 10)
                .padding(.bottom, 4)
            ForEach(state.hosts, id: \.name) { host in
                HStack(spacing: 8) {
                    Circle()
                        .fill(host.reachable ? Theme.ok : Theme.textMuted)
                        .frame(width: 8, height: 8)
                    Text(host.name)
                        .font(.system(size: 13))
                        .foregroundStyle(Theme.textSecondary)
                        .lineLimit(1)
                    Spacer()
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 5)
                .help(host.lastError ?? "")
            }
            Spacer()
        }
        .padding(.horizontal, 8)
        .padding(.top, 12)
        .padding(.bottom, 10)
        .frame(maxHeight: .infinity, alignment: .top)
        .background(Theme.bgSidebar)
    }

    @ViewBuilder
    private func row(_ pane: DashboardPane, _ icon: String, _ label: String, count: Int?, danger: Bool = false) -> some View {
        let selected = state.pane == pane
        Button {
            state.pane = pane
        } label: {
            HStack(spacing: 9) {
                Image(systemName: icon)
                    .frame(width: 18)
                    .font(.system(size: 14))
                    .foregroundStyle(selected ? Theme.accent : Theme.textMuted)
                Text(label)
                    .font(.system(size: 13, weight: selected ? .medium : .regular))
                    .foregroundStyle(selected ? Theme.accent : Theme.textSecondary)
                Spacer()
                if let count {
                    Text("\(count)")
                        .font(.system(size: 11, weight: .medium))
                        .foregroundStyle(danger ? Theme.danger : Theme.textSecondary)
                        .padding(.horizontal, 7)
                        .padding(.vertical, 1)
                        .background(danger ? Theme.danger.opacity(0.13) : Theme.inset)
                        .clipShape(Capsule())
                }
            }
            .padding(.horizontal, 9)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 8).fill(selected ? Theme.accentSubtle : Color.clear))
            // Make the whole padded row the hit target, not just the text +
            // icon (a Button with a Spacer otherwise only clicks on its content).
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }
}
