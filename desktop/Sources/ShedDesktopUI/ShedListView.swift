// ShedListView.swift — the read-only shed dashboard (M0), grouped by host.

import ShedKit
import SwiftUI

struct ShedListView: View {
    @ObservedObject var state: AppState

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 8) {
                Text("Sheds").font(.system(size: 26, weight: .bold)).foregroundStyle(Theme.text)
                if let err = state.lastError {
                    Label(err, systemImage: "exclamationmark.triangle")
                        .font(.system(size: 11))
                        .foregroundStyle(Theme.attention)
                        .lineLimit(1)
                }
                Spacer()
                Button {
                    state.showCreateSheet = true
                } label: {
                    Label("New shed", systemImage: "plus")
                        .font(.system(size: 13, weight: .medium))
                        .foregroundStyle(Theme.accent)
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 20)
            .padding(.top, 18)
            .padding(.bottom, 12)

            if state.sheds.isEmpty {
                emptyState
            } else {
                ScrollView {
                    VStack(alignment: .leading, spacing: 16) {
                        ForEach(state.shedsByHost(), id: \.host.name) { group in
                            if !group.sheds.isEmpty {
                                hostSection(group.host, group.sheds)
                            }
                        }
                    }
                    .padding(.horizontal, 20)
                    .padding(.bottom, 16)
                }
            }
        }
    }

    private var emptyState: some View {
        VStack(spacing: 8) {
            Spacer()
            Image(systemName: "tray").font(.system(size: 26)).foregroundStyle(Theme.textMuted)
            Text(state.hosts.contains(where: \.reachable)
                 ? "No sheds across the reachable hosts."
                 : "No reachable hosts. Check ~/.shed/config.yaml and that shed-server is running.")
                .font(.system(size: 12))
                .foregroundStyle(Theme.textSecondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 320)
            Button("Reconnect") { state.onReconnect?() }
                .buttonStyle(.plain)
                .font(.system(size: 12, weight: .medium))
                .foregroundStyle(Theme.accent)
                .help("Reload ~/.shed/config.yaml and reconnect to all hosts.")
            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    @ViewBuilder
    private func hostSection(_ host: ShedHost, _ sheds: [Shed]) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(host.name.uppercased())
                .font(.system(size: 11, weight: .semibold))
                .tracking(0.6)
                .foregroundStyle(Theme.textMuted)
            ForEach(sheds) { shed in
                ShedRow(shed: shed, state: state)
            }
        }
    }
}

struct ShedRow: View {
    let shed: Shed
    @ObservedObject var state: AppState
    @State private var confirmingDelete = false
    @State private var confirmingReset = false

    var body: some View {
        HStack(spacing: 12) {
            StatusDot(Theme.statusColor(shed.status))
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(shed.name).font(.system(size: 14, weight: .semibold)).foregroundStyle(Theme.text)
                    if let backend = shed.backend { Badge(backend, tone: .backend(backend)) }
                    if let image = state.imageLabel(for: shed) {
                        Badge(image, tone: .accent, symbol: "square.3.layers.3d")
                    }
                }
                Text(metaLine).font(.system(size: 12)).foregroundStyle(Theme.textSecondary).lineLimit(1)
            }
            Spacer()
            if !shed.activeNamespaces.isEmpty {
                Label(shed.activeNamespaces.joined(separator: " · "), systemImage: "key")
                    .font(.system(size: 11))
                    .foregroundStyle(Theme.textSecondary)
                    .padding(.horizontal, 7).padding(.vertical, 3)
                    .background(Theme.inset)
                    .clipShape(RoundedRectangle(cornerRadius: 6))
            }
            actions
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .cardSurface()
        .opacity(shed.status == .stopped ? 0.7 : 1.0)
        .confirmationDialog("Delete shed \(shed.name)?", isPresented: $confirmingDelete) {
            Button("Delete", role: .destructive) { state.onShedAction?(.delete, shed) }
        }
        .confirmationDialog("Reset shed \(shed.name)? This discards the writable layer.", isPresented: $confirmingReset) {
            Button("Reset", role: .destructive) { state.onShedAction?(.reset, shed) }
        }
    }

    @ViewBuilder
    private var actions: some View {
        HStack(spacing: 6) {
            if shed.status == .running {
                IntentButton("terminal", "Open terminal", Theme.accent) { state.onOpenTerminal?(shed) }
                IntentButton("arrow.clockwise", "Reset", Theme.attention) { confirmingReset = true }
                IntentButton("stop.fill", "Stop", Theme.danger) { state.onShedAction?(.stop, shed) }
            } else if shed.status == .stopped {
                IntentButton("play.fill", "Start", Theme.ok) { state.onShedAction?(.start, shed) }
                IntentButton("trash", "Delete", Theme.danger) { confirmingDelete = true }
            }
        }
    }

    private var metaLine: String {
        if shed.status == .starting { return "starting…" }
        var parts: [String] = []
        if let repo = shed.repo { parts.append(repo) }
        else if let dir = shed.localDir { parts.append(dir) }
        if let cpus = shed.cpus { parts.append("\(cpus) vCPU") }
        if let mem = shed.memoryMB { parts.append("\(mem / 1024) GB") }
        if shed.status == .running, let started = shed.startedAt,
           let date = DateFormatting.parseFlexibleTimestamp(started) {
            parts.append("up \(DateFormatting.shortRelative(date))")
        } else if shed.status == .stopped {
            parts.append("stopped")
        }
        return parts.isEmpty ? shed.status.rawValue : parts.joined(separator: " · ")
    }
}
