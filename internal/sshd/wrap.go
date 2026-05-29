package sshd

// wrapCommand maps the raw SSH command string from the client into the
// argv the agent will exec.
//
// Empty raw → nil so the agent's interactive login-shell default fires
// (cmd/shed-agent runs `{/bin/bash, --login}` when opts.Cmd is empty).
// Non-empty raw → `bash -lc <raw>`, preserving every shell metacharacter
// the client wrote and sourcing /etc/profile + /etc/profile.d/*.sh +
// ~/.profile so mise, nvm, rustup, and similar PATH-mutating tools take
// effect for SSH-driven commands.
//
// This matches what Docker, Codespaces, Coder, devcontainers, and every
// other hosted-shell product does, and it is what Zed Remote-SSH,
// VS Code Remote-SSH, JetBrains Gateway, raw `ssh host 'cmd | pipe'`,
// and `rsync` assume.
//
// The CLI side (cmd/shed/console.go shellQuoteArgs) wraps each argv
// element in single quotes before sending, so `shed exec`'s
// argv-literal semantics survive the bash reparse (bash treats
// single-quoted text as literal data — that is the security gate).
// See CLAUDE.md's "shed exec semantics and the SSH command channel"
// for the full contract.
func wrapCommand(raw string) []string {
	if raw == "" {
		return nil
	}
	return []string{"bash", "-lc", raw}
}
