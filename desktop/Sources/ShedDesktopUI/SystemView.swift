// SystemView.swift — per-host server disk usage (M7, GET /api/system/df).

import ShedKit
import SwiftUI

struct SystemView: View {
    @ObservedObject var state: AppState

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text("System").font(.system(size: 26, weight: .bold)).foregroundStyle(Theme.text)
                Spacer()
                Button { state.onSystemRefresh?() } label: {
                    Label("Refresh", systemImage: "arrow.clockwise")
                        .font(.system(size: 13, weight: .medium))
                        .foregroundStyle(Theme.accent)
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 20).padding(.top, 18).padding(.bottom, 4)
            Text("Disk usage per host (images, sheds, snapshots, orphans).")
                .font(.system(size: 12)).foregroundStyle(Theme.textMuted)
                .padding(.horizontal, 20).padding(.bottom, 12)

            if state.systemUsage.isEmpty {
                VStack { Spacer()
                    Text("Refreshing disk usage…").font(.system(size: 12)).foregroundStyle(Theme.textSecondary)
                    Spacer() }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView {
                    VStack(spacing: 10) {
                        ForEach(state.systemUsage) { row in
                            HostUsageCard(row: row)
                        }
                    }
                    .padding(.horizontal, 20).padding(.bottom, 16)
                }
            }
        }
        // Auto-fetch on first open; later refreshes are explicit (the button)
        // so re-navigation doesn't trigger a fetch storm.
        .onAppear { if state.systemUsage.isEmpty { state.onSystemRefresh?() } }
    }
}

private struct HostUsageCard: View {
    let row: HostDiskUsage

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                Image(systemName: "internaldrive").foregroundStyle(Theme.textSecondary)
                Text(row.host).font(.system(size: 14, weight: .semibold)).foregroundStyle(Theme.text)
                if let backend = row.usage?.backend {
                    Badge(backend, tone: .backend(backend))
                }
                Spacer()
                if let t = row.usage?.totals {
                    Text(SystemView.bytes(t.all.physicalBytes)).font(.system(size: 13, weight: .semibold)).foregroundStyle(Theme.text)
                }
            }
            if let error = row.error {
                Text(error).font(.system(size: 11)).foregroundStyle(Theme.attention).lineLimit(2)
            } else if let t = row.usage?.totals {
                HStack(spacing: 16) {
                    metric("Images", t.images.physicalBytes)
                    metric("Sheds", t.sheds.physicalBytes)
                    metric("Snapshots", t.snapshots.physicalBytes)
                    metric("Orphans", t.orphans.physicalBytes)
                }
            }
        }
        .padding(14)
        .cardSurface()
    }

    private func metric(_ label: String, _ bytes: Int64) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Text(label).font(.system(size: 10)).foregroundStyle(Theme.textMuted)
            Text(SystemView.bytes(bytes)).font(.system(size: 12, design: .monospaced)).foregroundStyle(Theme.text)
        }
    }
}

extension SystemView {
    static func bytes(_ n: Int64) -> String {
        ByteCountFormatter.string(fromByteCount: n, countStyle: .file)
    }
}
