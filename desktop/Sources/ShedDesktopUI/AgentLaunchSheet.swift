// AgentLaunchSheet.swift — start a remote-control session inside a shed (M2),
// presented as a centered modal card (see DashboardView's modalOverlay). Mirrors
// shed-remote-agent's "New session" form: Session name, a Kind toggle, and an
// optional kickoff line (an initial prompt for claude-rc, a command for shell).

import ShedKit
import SwiftUI

public struct AgentLaunchSheet: View {
    @ObservedObject var state: AppState

    @State private var target: String = ""  // shed id "host/name"
    @State private var kind: RcKind = .default
    @State private var displayName: String = ""
    @State private var initialPrompt: String = ""

    public init(state: AppState) {
        self.state = state
    }

    private var runningSheds: [Shed] {
        state.sheds.filter { $0.status == .running }
    }

    public var body: some View {
        VStack(spacing: 0) {
            SheetHeader(icon: "wand.and.stars", title: "New session",
                        subtitle: "Start a remote-control session inside a shed.", onClose: close)
            Divider()
            VStack(alignment: .leading, spacing: 16) {
                SheetField("Shed") {
                    SheetDropdown(current: shedDisplay) {
                        ForEach(runningSheds) { s in
                            Button("\(s.host)/\(s.name)") { target = s.id }
                        }
                    }
                }
                SheetField("Session name", hint: "optional") {
                    SheetTextField(placeholder: sessionNamePlaceholder, text: $displayName)
                }
                if availableKinds.isEmpty {
                    SheetField("Kind") {
                        Text("No agent kinds available in this shed.")
                            .foregroundStyle(.secondary)
                    }
                } else {
                    SheetField("Kind", help: kindCopy.toggleHelp) {
                        Picker("", selection: $kind) {
                            ForEach(availableKinds, id: \.self) { k in
                                Text(kindLabel(k)).tag(k)
                            }
                        }
                        .pickerStyle(.segmented)
                        .labelsHidden()
                    }
                }
                if !availableKinds.isEmpty, kind.acceptsTypedInput {
                    SheetField(kindCopy.promptLabel, hint: "optional", help: kindCopy.promptHelp) {
                        SheetTextField(placeholder: kindCopy.promptPlaceholder, text: $initialPrompt)
                    }
                }
            }
            .padding(.horizontal, 20).padding(.vertical, 18)
            Divider()
            HStack {
                SheetCancelButton(action: close)
                Spacer()
                SheetPrimaryButton(title: "Create", icon: "plus",
                                   disabled: target.isEmpty || availableKinds.isEmpty, action: launch)
            }
            .padding(16)
        }
        .modalCard()
        .onAppear {
            if target.isEmpty { target = runningSheds.first?.id ?? "" }
            reconcileKind()
        }
        // When the shed changes, its capabilities may not offer the current kind.
        .onChange(of: target) { _ in reconcileKind() }
        // Capabilities can also change for the SAME shed while the sheet stays open
        // (a background probe/refresh); re-reconcile so a now-unoffered kind can't
        // stay selected. `availableKinds` is derived from `state.rcCapabilities[target]`.
        .onChange(of: availableKinds) { _ in reconcileKind() }
    }

    private var selectedShed: Shed? {
        runningSheds.first(where: { $0.id == target })
    }

    /// The gated kind list for the selected shed: with capabilities, the creatable
    /// kinds whose backing agent is installed. Only ABSENT capabilities (old binary
    /// / not yet probed) fall back to claude+shell; present-but-empty capabilities
    /// yield an EMPTY list (the shed advertises no usable kinds — the sheet shows
    /// the unavailable notice, never inventing claude). Mirrors the Tauri
    /// `offeredKinds`.
    private var availableKinds: [RcKind] {
        guard let caps = state.rcCapabilities[target] else { return [.claudeRc, .shell] }
        return caps.creatableKinds
    }

    /// Keep the selected kind valid for the current shed's offered set (a no-op
    /// when the set is empty — the sheet renders the unavailable notice instead).
    private func reconcileKind() {
        let kinds = availableKinds
        if let first = kinds.first, !kinds.contains(kind) { kind = first }
    }

    private func kindLabel(_ k: RcKind) -> String {
        switch k {
        case .claudeRc, .claudeBroker: return "Claude"
        case .codex: return "Codex"
        case .opencode: return "opencode"
        case .cursor: return "Cursor"
        case .shell: return "Shell"
        case .other(let s): return s
        }
    }

    private var shedDisplay: String {
        selectedShed.map { "\($0.host)/\($0.name)" } ?? "—"
    }

    /// Mirrors the default `<shed>/<slug>` display name; `<slug>` is literal text
    /// (the slug is generated at launch), matching shed-remote-agent's placeholder.
    private var sessionNamePlaceholder: String {
        "\(selectedShed?.name ?? "<shed>")/<slug>"
    }

    /// Per-kind copy for the toggle helper line and the kickoff field, grouped so
    /// the kind is switched once.
    private struct KindCopy {
        let toggleHelp, promptLabel, promptPlaceholder, promptHelp: String
    }

    private var kindCopy: KindCopy {
        switch kind {
        case .shell:
            return KindCopy(
                toggleHelp: "plain bash in the shed workspace",
                promptLabel: "Initial command",
                promptPlaceholder: "e.g. npm install && npm test",
                promptHelp: "Run in the shell once it's ready.")
        case .codex:
            return KindCopy(
                toggleHelp: "OpenAI Codex TUI in the shed",
                promptLabel: "Initial prompt",
                promptPlaceholder: "e.g. find and fix the failing test",
                promptHelp: "Typed into codex once it's ready.")
        case .opencode:
            return KindCopy(
                toggleHelp: "opencode TUI in the shed",
                promptLabel: "Initial prompt",
                promptPlaceholder: "e.g. summarize this repo and suggest next steps",
                promptHelp: "Typed into opencode once it's ready.")
        case .cursor:
            return KindCopy(
                toggleHelp: "cursor-agent TUI in the shed",
                promptLabel: "Initial prompt",
                promptPlaceholder: "e.g. add a test for the parser",
                promptHelp: "Typed into cursor-agent once it's ready.")
        default:
            return KindCopy(
                toggleHelp: "live claude REPL with /rc",
                promptLabel: "Initial prompt",
                promptPlaceholder: "e.g. summarize this repo and suggest next steps",
                promptHelp: "Typed into the REPL once it's ready.")
        }
    }

    private func close() { state.showLaunchSheet = false }

    private func launch() {
        guard let shed = selectedShed else { return }
        // Defensive: never send a kind the selected shed no longer offers (guards
        // the window between a capability shrink and the reconcile above).
        guard availableKinds.contains(kind) else { return }
        let name = displayName.trimmingCharacters(in: .whitespaces)
        let promptRaw = initialPrompt.trimmingCharacters(in: .whitespacesAndNewlines)
        let prompt = (kind.acceptsTypedInput && !promptRaw.isEmpty) ? promptRaw : nil
        state.onRcLaunch?(RcLaunchInput(
            host: shed.host, shed: shed.name, kind: kind,
            displayName: name.isEmpty ? nil : name, initialPrompt: prompt))
        close()
    }
}
