// EgressView.swift — the Egress pane. Two view-only sub-sections:
//  • Activity — per-shed allow/deny decisions streamed from shed-server via the
//    host-agent (ns=egress audit events).
//  • Profiles — the egress profiles defined on each server (config baseline +
//    user-managed), fetched read-only from GET /api/egress/profiles.
// Egress policy is configured on the server; the desktop only observes.

import ShedKit
import SwiftUI

struct EgressView: View {
    @ObservedObject var state: AppState
    @State private var tab: EgressTab = .activity

    enum EgressTab: String, CaseIterable { case activity = "Activity", profiles = "Profiles" }

    /// Egress decisions only, newest first (state.activity is already ordered).
    private var entries: [AuditEntry] { state.activity.filter(\.isEgress) }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text("Egress").font(.system(size: 26, weight: .bold)).foregroundStyle(Theme.text)
                Spacer()
                Picker("", selection: $tab) {
                    ForEach(EgressTab.allCases, id: \.self) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .frame(width: 200)
            }
            .padding(.horizontal, 20).padding(.top, 18).padding(.bottom, 4)

            Text(tab == .activity
                ? "Per-shed network egress decisions (allow/deny), newest first. View-only — egress policy is configured on the server."
                : "Egress profiles on each server (config baseline + user-managed). View-only — manage them with `shed egress profile`.")
                .font(.system(size: 12)).foregroundStyle(Theme.textMuted)
                .padding(.horizontal, 20).padding(.bottom, 10)

            switch tab {
            case .activity: activityFeed
            case .profiles: EgressProfilesView(state: state)
            }
        }
    }

    @ViewBuilder private var activityFeed: some View {
        if entries.isEmpty {
            VStack(spacing: 6) {
                Spacer()
                Text("No egress activity yet.")
                    .font(.system(size: 12)).foregroundStyle(Theme.textSecondary)
                Text("Enable egress control on a server and create a shed with --egress to see allow/deny decisions here.")
                    .font(.system(size: 11)).foregroundStyle(Theme.textMuted)
                    .multilineTextAlignment(.center).frame(maxWidth: 360)
                Spacer()
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            ScrollView {
                VStack(spacing: 0) {
                    ForEach(Array(entries.enumerated()), id: \.element.id) { index, entry in
                        if index > 0 { Divider() }
                        ActivityRow(entry: entry)
                    }
                }
                .padding(.horizontal, 14)
                .cardSurface()
                .padding(.horizontal, 20)
                .padding(.bottom, 16)
            }
        }
    }
}
