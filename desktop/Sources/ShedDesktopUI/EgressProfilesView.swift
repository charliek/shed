// EgressProfilesView.swift — read-only Egress → Profiles sub-section.
//
// A concrete list → detail drill-down: the left column lists the egress
// profiles defined on each server (config baseline + user-managed), and the
// right pane shows the selected profile's rules. View-only; profiles are managed
// with `shed egress profile`. This list+detail shape is the template later panes
// (Sheds, System) can follow for their own drill-downs.

import ShedKit
import SwiftUI

struct EgressProfilesView: View {
    @ObservedObject var state: AppState
    @State private var selectedID: String?

    private struct Row: Identifiable, Equatable {
        let host: String
        let info: EgressProfileInfo
        // Include source so the id is unique even if a host ever returns a
        // config and user profile of the same name (the server dedupes today).
        var id: String { host + "/" + info.source + "/" + info.name }
    }

    private var rows: [Row] {
        state.egressProfiles.flatMap { h in h.profiles.map { Row(host: h.host, info: $0) } }
    }
    private var multiHost: Bool { state.egressProfiles.count > 1 }
    private var errorRows: [HostEgressProfiles] { state.egressProfiles.filter { $0.error != nil } }
    private var selectedRow: Row? { rows.first { $0.id == selectedID } }

    var body: some View {
        Group {
            if rows.isEmpty {
                // Surface host failures (egress disabled / unreachable) instead of
                // a misleading "no profiles" when a fetch actually errored.
                if errorRows.isEmpty { emptyState } else { errorState }
            } else {
                HSplitView {
                    list.frame(minWidth: 220, idealWidth: 280, maxWidth: 360)
                    detail.frame(maxWidth: .infinity)
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .onAppear { state.onEgressRefresh?() }
    }

    private var list: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 2) {
                ForEach(rows) { row in
                    Button { selectedID = row.id } label: { listRow(row) }
                        .buttonStyle(.plain)
                }
                ForEach(errorRows) { e in
                    Text("\(e.host): unavailable")
                        .font(.system(size: 10)).foregroundStyle(Theme.textMuted)
                        .padding(.horizontal, 10).padding(.vertical, 4)
                }
            }
            .padding(.horizontal, 12).padding(.bottom, 12)
        }
    }

    private func listRow(_ row: Row) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 6) {
                Text(row.info.name).font(.system(size: 13, weight: .medium)).foregroundStyle(Theme.text)
                sourceBadge(row.info.source)
                Spacer()
            }
            if multiHost {
                Text(row.host).font(.system(size: 10)).foregroundStyle(Theme.textMuted)
            }
            Text(summary(row.info.profile)).font(.system(size: 10)).foregroundStyle(Theme.textMuted).lineLimit(1)
        }
        .padding(.vertical, 6).padding(.horizontal, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(selectedID == row.id ? Theme.accentSubtle : Color.clear)
        .clipShape(RoundedRectangle(cornerRadius: 6))
    }

    @ViewBuilder private var detail: some View {
        if let row = selectedRow {
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    HStack(spacing: 8) {
                        Text(row.info.name).font(.system(size: 18, weight: .bold)).foregroundStyle(Theme.text)
                        sourceBadge(row.info.source)
                    }
                    ruleSection("allow", row.info.profile.allow)
                    ruleSection("deny", row.info.profile.deny)
                    if let mode = row.info.profile.mode, !mode.isEmpty { labeled("mode", mode) }
                    if let rule = row.info.profile.rule, !rule.isEmpty { labeled("rule", rule) }
                    Spacer()
                }
                .padding(20)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        } else {
            VStack {
                Spacer()
                Text("Select a profile").font(.system(size: 12)).foregroundStyle(Theme.textMuted)
                Spacer()
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    @ViewBuilder private func ruleSection(_ label: String, _ items: [String]?) -> some View {
        if let items, !items.isEmpty {
            VStack(alignment: .leading, spacing: 3) {
                Text(label).font(.system(size: 11, weight: .semibold)).foregroundStyle(Theme.textSecondary)
                ForEach(items, id: \.self) { item in
                    Text(item).font(.system(size: 12, design: .monospaced)).foregroundStyle(Theme.text)
                }
            }
        }
    }

    private func labeled(_ label: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(label).font(.system(size: 11, weight: .semibold)).foregroundStyle(Theme.textSecondary)
            Text(value).font(.system(size: 12, design: .monospaced)).foregroundStyle(Theme.text)
        }
    }

    private func sourceBadge(_ source: String) -> some View {
        let isUser = source == "user"
        return Text(source)
            .font(.system(size: 9, weight: .semibold))
            .padding(.horizontal, 5).padding(.vertical, 1)
            .background((isUser ? Theme.accent : Theme.textMuted).opacity(0.18))
            .foregroundStyle(isUser ? Theme.accent : Theme.textSecondary)
            .clipShape(Capsule())
    }

    private var errorState: some View {
        VStack(spacing: 8) {
            Spacer()
            Text("Couldn't load egress profiles.").font(.system(size: 12)).foregroundStyle(Theme.textSecondary)
            ForEach(errorRows) { e in
                Text("\(e.host): \(e.error ?? "unavailable")")
                    .font(.system(size: 11)).foregroundStyle(Theme.textMuted)
                    .multilineTextAlignment(.center).frame(maxWidth: 420)
            }
            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var emptyState: some View {
        VStack(spacing: 6) {
            Spacer()
            Text("No egress profiles.").font(.system(size: 12)).foregroundStyle(Theme.textSecondary)
            Text("Define them in the server config or with `shed egress profile set`. Requires egress enabled on the server.")
                .font(.system(size: 11)).foregroundStyle(Theme.textMuted)
                .multilineTextAlignment(.center).frame(maxWidth: 380)
            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func summary(_ p: EgressProfile) -> String {
        var parts: [String] = []
        if let m = p.mode, !m.isEmpty { parts.append("mode=\(m)") }
        if let a = p.allow, !a.isEmpty { parts.append("allow=\(a.count)") }
        if let d = p.deny, !d.isEmpty { parts.append("deny=\(d.count)") }
        if let r = p.rule, !r.isEmpty { parts.append("rule") }
        return parts.isEmpty ? "(empty)" : parts.joined(separator: " ")
    }
}
