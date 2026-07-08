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
                SheetField("Kind", help: kindCopy.toggleHelp) {
                    Picker("", selection: $kind) {
                        ForEach(RcKind.creatable, id: \.self) { k in
                            Text(k.rawValue).tag(k)
                        }
                    }
                    .pickerStyle(.segmented)
                    .labelsHidden()
                }
                if kind.acceptsTypedInput {
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
                                   disabled: target.isEmpty, action: launch)
            }
            .padding(16)
        }
        .modalCard()
        .onAppear { if target.isEmpty { target = runningSheds.first?.id ?? "" } }
    }

    private var selectedShed: Shed? {
        runningSheds.first(where: { $0.id == target })
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
    /// the kind is switched once. Only the creatable kinds (claude-rc, shell) reach
    /// the sheet, so `default` is the claude-rc case.
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
        let name = displayName.trimmingCharacters(in: .whitespaces)
        let promptRaw = initialPrompt.trimmingCharacters(in: .whitespacesAndNewlines)
        let prompt = (kind.acceptsTypedInput && !promptRaw.isEmpty) ? promptRaw : nil
        state.onRcLaunch?(RcLaunchInput(
            host: shed.host, shed: shed.name, kind: kind,
            displayName: name.isEmpty ? nil : name, initialPrompt: prompt))
        close()
    }
}
