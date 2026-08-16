package rc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// claudeConfigPath returns ${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json. An empty
// CLAUDE_CONFIG_DIR is treated as unset (→ $HOME), matching claude. Returns "" if
// neither is set (the caller treats preseed as a no-op).
func claudeConfigPath(getenv func(string) string) string {
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = getenv("HOME")
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ".claude.json")
}

// fullscreenUpsellFloor is a "seen count" high enough that claude stops showing the
// fullscreen-renderer upsell dialog.
const fullscreenUpsellFloor = 999

// jsonNumberInt returns v as an int when it is a JSON number (config is decoded with
// UseNumber so integers round-trip exactly); 0 otherwise.
func jsonNumberInt(v any) int {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

// PreseedClaudeConfig prepares claude's config so a fresh session reaches `ready`
// unattended: it marks workdir trusted (no first-run workspace-trust prompt) and
// clears the first-run onboarding gate (no theme picker). It does NOT log in —
// authentication is provisioned separately. Best-effort: it never fails a create
// (the send-keys accept-trust fallback covers any failure), so it returns an error
// only for optional diagnostics. Invariants (mirroring the sh/jq reference):
// merge — never clobber (unknown OAuth/MCP keys preserved); a malformed existing file
// is left untouched; atomic write (temp in the same dir, then rename); a file lock
// serializes concurrent creates; the existing file mode is preserved (0600 on create).
func PreseedClaudeConfig(workdir string, getenv func(string) string) error {
	path := claudeConfigPath(getenv)
	if path == "" {
		return errors.New("no CLAUDE_CONFIG_DIR or HOME; skipping trust preseed")
	}

	// Ensure the config directory exists (the sh reference does `mkdir -p`); a
	// CLAUDE_CONFIG_DIR pointing at a not-yet-created dir would otherwise no-op.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	unlock, err := lockSibling(path)
	if err != nil {
		return fmt.Errorf("locking trust file: %w", err)
	}
	defer unlock()

	config, mode, err := readJSONObject(path)
	if err != nil {
		return err
	}

	// Merge: set only projects[workdir].hasTrustDialogAccepted, preserving every
	// other key (round-tripped through map[string]any, never a typed struct).
	projects, _ := config["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	proj, _ := projects[workdir].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
	}
	proj["hasTrustDialogAccepted"] = true
	projects[workdir] = proj
	config["projects"] = projects

	// Clear the first-run onboarding gate so a fresh shed's claude reaches the
	// session instead of blocking on the theme picker. hasCompletedOnboarding is set
	// idempotently; theme only when absent (never clobber a user's choice).
	config["hasCompletedOnboarding"] = true
	if _, ok := config["theme"]; !ok {
		config["theme"] = "dark"
	}

	// Suppress first-run interstitials that can pop a modal over an unattended
	// session: the fullscreen-renderer upsell (a "seen count" — raise it past the
	// threshold, never lower an existing value) and the auto-mode entry warning.
	if jsonNumberInt(config["fullscreenUpsellSeenCount"]) < fullscreenUpsellFloor {
		config["fullscreenUpsellSeenCount"] = fullscreenUpsellFloor
	}
	if _, ok := config["hasSeenAutoModeEntryWarning"]; !ok {
		config["hasSeenAutoModeEntryWarning"] = true
	}

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding trust config: %w", err)
	}
	return atomicWrite(path, ".claude.json.*.tmp", out, mode)
}

// lockSibling takes an exclusive flock on a SIBLING lockfile (path + a fixed suffix),
// never on the target file itself: each holder must read the CURRENT file after acquiring
// the lock, and an atomic rename by another holder would otherwise leave the waiter
// writing through a stale, unlinked inode. Shared by every merge-never-clobber preseed
// (claude's .claude.json, cursor's hooks.json) so they serialize identically. The
// returned func releases the lock and closes the file; it is never nil on success.
func lockSibling(path string) (func(), error) {
	lock, err := os.OpenFile(path+".shed-ext-rc.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

// readJSONObject reads a merge-never-clobber preseed target (claude's .claude.json,
// cursor's hooks.json) and returns its decoded top-level object plus the file mode a
// rewrite must preserve. Shared by both preseeds so the tolerance rules cannot drift:
//
//   - an absent or empty file yields an EMPTY object at mode 0600 (the create case);
//   - an existing file's permission bits are carried through to the rewrite;
//   - the decode uses UseNumber so large integers anywhere in the document (OAuth/MCP
//     keys, timeouts, a version) round-trip exactly instead of through float64;
//   - a literal `null` decodes the map to nil and is re-seeded as an empty object;
//   - anything else malformed is an ERROR, and every caller's contract is to leave the
//     file exactly as it is when one comes back.
func readJSONObject(path string) (map[string]any, fs.FileMode, error) {
	config := map[string]any{}
	mode := fs.FileMode(0o600)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config, mode, nil
		}
		return nil, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	if len(data) == 0 {
		return config, mode, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&config); err != nil {
		return nil, 0, fmt.Errorf("existing %s is not valid JSON; leaving untouched: %w", path, err)
	}
	if config == nil {
		config = map[string]any{}
	}
	return config, mode, nil
}

// atomicWrite writes data to a temp file in path's directory, fsyncs it, and
// renames it over path (atomic on the same filesystem). tmpPattern is the
// os.CreateTemp pattern for the temp file — passed in rather than hardcoded so each
// preseed's leftovers (if a process dies between create and rename) are identifiable
// as its own, and so a directory holding two different preseeded files never sees one
// writer's debris attributed to the other.
func atomicWrite(path, tmpPattern string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp config: %w", err)
	}
	return nil
}
