package rc

import (
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// PlanMaxBytes caps the plan accepted on `create --plan-stdin`. 1 MiB is generous
// for a markdown plan and guards a runaway paste from filling the guest disk.
const PlanMaxBytes = 1 << 20

// planFileMode is the on-disk permission of a written plan (owner read/write only):
// a plan can carry design details the author would not want world-readable.
const planFileMode = 0o600

// planDirMode is the permission of a created plan directory (owner rwx only).
const planDirMode = 0o700

// planDir returns the directory a kind's plan file is written to. claude kinds use
// claude's native plans dir (${CLAUDE_CONFIG_DIR:-$HOME/.claude}/plans) so the
// running `claude` finds the plan where it looks for saved plans; every other kind
// uses $HOME/.shed-plans. Both are HOME-rooted on purpose — a workdir file would
// dirty a --repo clone and, with --local-dir, write through VirtioFS onto a real
// host directory.
//
// CLAUDE_CONFIG_DIR is environment data, not trusted input: a relative value would
// yield a cwd-dependent (non-absolute) plan path and a control-char value would
// inject into the composed kickoff line, so such a value is ignored in favor of the
// $HOME/.claude default (matching how a broken CLAUDE_CONFIG_DIR degrades elsewhere).
func planDir(kind Kind, env Getenv) (string, error) {
	home := env("HOME")
	if IsClaudeKind(kind) {
		base := env("CLAUDE_CONFIG_DIR")
		if base != "" && (!filepath.IsAbs(base) || HasControlChars(base)) {
			base = ""
		}
		if base == "" {
			if home == "" {
				return "", fmt.Errorf("%w: no CLAUDE_CONFIG_DIR or HOME to place the plan", ErrBadArgs)
			}
			base = filepath.Join(home, ".claude")
		}
		return filepath.Join(base, "plans"), nil
	}
	if home == "" {
		return "", fmt.Errorf("%w: HOME unset; cannot place the plan", ErrBadArgs)
	}
	return filepath.Join(home, ".shed-plans"), nil
}

// PlanPath returns the absolute path a slug's plan file is written to for a kind
// (see planDir for the per-kind location rationale). The final path is asserted
// absolute and control-char-free — it is embedded verbatim in the kickoff line, so
// a hostile $HOME must not be able to smuggle bytes into the delivered prompt.
func PlanPath(kind Kind, slug string, env Getenv) (string, error) {
	dir, err := planDir(kind, env)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "plan-"+slug+".md")
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: plan path %q is not absolute (bad HOME?)", ErrBadArgs, path)
	}
	if HasControlChars(path) {
		return "", fmt.Errorf("%w: plan path contains a control character (bad HOME/CLAUDE_CONFIG_DIR?)", ErrBadArgs)
	}
	return path, nil
}

// writePlan writes content to the per-kind plan file and returns its absolute path.
// The directory is created if missing. The 0600 mode is enforced with an explicit
// Chmod — os.WriteFile's mode only applies at create, so a pre-existing looser file
// (e.g. an earlier 0644 copy) would otherwise keep its old permissions.
func writePlan(kind Kind, slug, content string, env Getenv) (string, error) {
	path, err := PlanPath(kind, slug, env)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), planDirMode); err != nil {
		return "", fmt.Errorf("creating plan dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, planFileMode)
	if err != nil {
		return "", fmt.Errorf("writing plan: %w", err)
	}
	if err := f.Chmod(planFileMode); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("setting plan mode: %w", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("writing plan: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing plan: %w", err)
	}
	return path, nil
}

// composePlanKickoff builds the kickoff line typed into a ready session for a plan
// run: a natural-language instruction to read and implement the plan at path,
// optionally led by the caller's framing (framing leads, the plan stays referenced).
func composePlanKickoff(path, framing string) string {
	base := "Read the plan at " + path + " and implement it. Work through it to completion " +
		"autonomously — don't stop to ask for confirmation; make reasonable decisions and " +
		"keep going until the plan is done."
	if framing != "" {
		return framing + "\n\n" + base
	}
	return base
}

// validatePlanInputs checks a plan-delivery request before any side effect: the kind
// must accept a typed kickoff, the plan must be non-empty, valid UTF-8, and within
// PlanMaxBytes, a raw Prompt and a Plan are mutually exclusive (a plan composes its
// own kickoff), and any framing must be free of unsupported control characters. It
// returns the normalized framing to prepend to the composed kickoff.
func validatePlanInputs(kind Kind, plan, prompt, framing string) (string, error) {
	if prompt != "" {
		return "", fmt.Errorf("%w: a plan and a prompt are mutually exclusive", ErrBadArgs)
	}
	if !AcceptsTypedInput(kind) {
		return "", fmt.Errorf("%w: kind %q does not accept a plan", ErrBadArgs, kind)
	}
	if plan == "" {
		return "", fmt.Errorf("%w: plan is empty", ErrBadArgs)
	}
	if len(plan) > PlanMaxBytes {
		return "", fmt.Errorf("%w: plan is %d bytes; the limit is %d", ErrBadArgs, len(plan), PlanMaxBytes)
	}
	if !utf8.ValidString(plan) {
		return "", fmt.Errorf("%w: plan is not valid UTF-8", ErrBadArgs)
	}
	if framing != "" {
		// Invalid UTF-8 must fail before the rune-based control-char scan: an invalid
		// byte reads as RuneError there and would slip past HasUnsafePromptChars.
		if !utf8.ValidString(framing) {
			return "", fmt.Errorf("%w: plan framing is not valid UTF-8", ErrBadArgs)
		}
		framing = NormalizeNewlines(framing)
		if HasUnsafePromptChars(framing) {
			return "", fmt.Errorf("%w: plan framing contains an unsupported control character", ErrBadArgs)
		}
	}
	return framing, nil
}
