// AgentsView.swift — the remote-control agents pane (M2).

import ShedKit
import SwiftUI

struct AgentsView: View {
    @ObservedObject var state: AppState

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 12) {
                Text("Remote-control agents").font(.system(size: 26, weight: .bold)).foregroundStyle(Theme.text)
                Spacer()
                Button { state.onRcRefresh?() } label: {
                    Image(systemName: "arrow.clockwise").foregroundStyle(Theme.textSecondary)
                }
                .buttonStyle(.plain).help("Refresh")
                Button { state.showLaunchSheet = true } label: {
                    Label("New session", systemImage: "plus")
                        .font(.system(size: 13, weight: .medium))
                        .foregroundStyle(Theme.accent)
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 20).padding(.top, 18).padding(.bottom, 12)

            if state.rcSessions.isEmpty {
                empty
            } else {
                ScrollView {
                    VStack(spacing: 10) {
                        ForEach(state.rcSessions) { session in
                            AgentRow(session: session, state: state)
                        }
                    }
                    .padding(.horizontal, 20).padding(.bottom, 16)
                }
            }
        }
    }

    private var empty: some View {
        VStack(spacing: 8) {
            Spacer()
            Image(systemName: "wand.and.stars").font(.system(size: 26)).foregroundStyle(Theme.textMuted)
            Text("No remote-control sessions. Launch one into a running shed.")
                .font(.system(size: 12)).foregroundStyle(Theme.textSecondary)
                .multilineTextAlignment(.center).frame(maxWidth: 320)
            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

struct AgentRow: View {
    let session: RcSession
    @ObservedObject var state: AppState
    @State private var confirmingKill = false

    var body: some View {
        HStack(spacing: 12) {
            StatePill(session.state)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text("\(session.shed)/\(session.displayName)").font(.system(size: 14, weight: .semibold)).foregroundStyle(Theme.text)
                    Badge(session.kind.rawValue)
                    if !session.managed { Badge("legacy", tone: .legacy) }
                }
                Text(metaLine).font(.system(size: 12)).foregroundStyle(Theme.textSecondary)
                    .lineLimit(1).help(session.createdBy ?? "")
            }
            Spacer()
            if needsFix {
                Text(fixHint).font(.system(size: 11)).foregroundStyle(Theme.attention).lineLimit(1)
            }
            if let url = session.url, session.state == .ready {
                Button { state.onOpenURL?(url) } label: {
                    Label("Open in Claude", systemImage: "arrow.up.right.square")
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(Theme.accent)
                        .padding(.horizontal, 10).padding(.vertical, 6)
                        .background(Theme.accentSubtle, in: RoundedRectangle(cornerRadius: 8))
                        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Theme.accentBorder, lineWidth: 0.5))
                }
                .buttonStyle(.plain)
            }
            // Attach a terminal to the session's tmux. Always available (except
            // when dead) — it's also the way to act on the needs-trust/auth hint.
            if session.state != .dead {
                IntentButton("terminal", "Open console", Theme.accent) { state.onRcAttach?(session) }
            }
            // A managed session kills outright; an unmanaged (legacy/foreign)
            // one confirms first, per the convention's destructive-action rule.
            IntentButton("trash", "Kill session", Theme.danger) {
                if session.managed { state.onRcKill?(session) } else { confirmingKill = true }
            }
        }
        .padding(.horizontal, 14).padding(.vertical, 12)
        .cardSurface()
        .confirmationDialog(
            "Kill \(session.tmuxSession)? This rc-* session is legacy/unmanaged (not created by a v1 tool).",
            isPresented: $confirmingKill
        ) {
            Button("Kill", role: .destructive) { state.onRcKill?(session) }
        }
    }

    private var metaLine: String {
        var parts = ["tmux \(session.tmuxSession)", session.workdir]
        if session.managed, let prov = provenance { parts.append(prov) }
        return parts.joined(separator: " · ")
    }

    /// "made by <tool> <age>" for a managed session, where <tool> is the token
    /// before the final `/` of SHED_RC_CREATED_BY (capped so a foreign value
    /// can't crowd out the rest). Age is omitted when createdAt is absent or
    /// unparseable.
    private var provenance: String? {
        guard let by = session.createdBy, !by.isEmpty else { return nil }
        let token = by.split(separator: "/").dropLast().joined(separator: "/")
        let tool = String((token.isEmpty ? by : token).prefix(40))
        if let at = session.createdAt, let date = DateFormatting.parseFlexibleTimestamp(at) {
            return "made by \(tool) \(DateFormatting.shortRelative(date))"
        }
        return "made by \(tool)"
    }

    private var needsFix: Bool {
        session.state == .needsTrust || session.state == .needsAuth
    }

    private var fixHint: String {
        switch session.state {
        case .needsTrust: return "attach + trust the folder, then relaunch"
        case .needsAuth: return "attach + claude auth login, then relaunch"
        default: return ""
        }
    }
}

struct StatePill: View {
    let state: RcState
    init(_ state: RcState) { self.state = state }
    var body: some View {
        let color = Theme.rcStateColor(state)
        Text(state.rawValue)
            .font(.system(size: 11, weight: .medium))
            .frame(width: 84)
            .padding(.vertical, 4)
            .background(color.opacity(0.18))
            .foregroundStyle(color)
            .clipShape(RoundedRectangle(cornerRadius: 6))
    }
}
