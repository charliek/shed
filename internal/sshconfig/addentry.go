package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// AddOutcome says what AddEntryIfAbsent did, so a caller can print
// "wrote Host <alias> to ~/.ssh/config" only when it actually wrote one.
type AddOutcome int

const (
	// AddedEntry: the entry was appended to the managed block, which was
	// created if it did not exist.
	AddedEntry AddOutcome = iota
	// AlreadyManaged: a Host entry inside the managed block already carries
	// the alias. Nothing was written.
	AlreadyManaged
	// AlreadyUserDefined: a Host entry OUTSIDE the managed block carries the
	// alias. Nothing was written — a hand-written entry wins.
	AlreadyUserDefined
)

func (o AddOutcome) String() string {
	switch o {
	case AddedEntry:
		return "added"
	case AlreadyManaged:
		return "already managed"
	case AlreadyUserDefined:
		return "already defined by the user"
	default:
		return fmt.Sprintf("AddOutcome(%d)", int(o))
	}
}

// AliasDeclared reports whether any Host line in the config at path names
// alias — the read-only half of AddEntryIfAbsent's "is this alias already
// claimed" question, for a caller that needs the answer without writing.
//
// Managed or hand-written makes no difference here, exactly as it makes none
// there: what is being asked is whether `ssh <alias>` already resolves to
// something on this machine. A missing file is not an error — it is a file
// with no aliases in it — so the only error this returns is a real read
// failure.
//
// No lock is taken. This is a hint, not a read-modify-write: a caller acting
// on it goes on to call AddEntryIfAbsent, which locks and re-checks under the
// lock, so a config that changed in between costs nothing.
func AliasDeclared(path, alias string) (bool, error) {
	content, _, err := readConfig(path)
	if err != nil {
		return false, err
	}
	return hostAliasDeclared(content, alias), nil
}

// AddEntryIfAbsent adds ONE Host entry to the managed block, and only if no
// Host entry anywhere in the file already claims its alias.
//
// **This is not ComputeDiff + Write, and it must not become them.** That pair
// is a WHOLE-BLOCK writer: it takes the complete desired entry set, and
// anything not in it is a removal. Handing it a single entry — which is what a
// caller that wants "just make sure shed-foo resolves" naturally has — deletes
// every other managed entry in the file. `shed ssh-config install` is the only
// caller that legitimately knows the whole set; everything else belongs here.
//
// Three guarantees, in order of how badly each would be missed:
//
//   - **An existing alias is never touched.** Managed or hand-written, the
//     file is left byte-identical and the outcome says which it was. A user
//     who wrote their own `Host shed-foo` with a ProxyJump and a different
//     IdentityFile meant it, and silently replacing it is the worst thing
//     this function could do.
//   - **Every other managed entry survives.** The block is edited in place —
//     one entry spliced in before the end marker — rather than regenerated,
//     so the other entries keep their exact bytes and the block keeps its own
//     "Last updated" header.
//   - **Everything outside the block is byte-for-byte preserved.** Comments,
//     `Host *` defaults, Include lines, ordering, trailing whitespace.
//
// Safe to call when the file, the managed block, or the whole `~/.ssh`
// directory does not exist yet.
func AddEntryIfAbsent(path string, entry Entry) (AddOutcome, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return AddedEntry, fmt.Errorf("failed to create %s: %w", dir, err)
	}

	// The lock spans the read AND the write: this is a read-modify-write on a
	// file two `shed` processes can reach at the same time (two terminals
	// attaching to two different sheds is the ordinary case, not the exotic
	// one), and without it the second reader's write erases the first's entry.
	// The atomic rename below is what keeps a CRASH from leaving a truncated
	// config; it does nothing at all about two well-behaved writers racing,
	// which is why both are here.
	//
	// **What this does not cover, stated plainly:** `runSSHConfigInstall` —
	// the whole-block writer behind `shed ssh-config install` — takes no lock,
	// so a concurrent run of that command can still lose this entry. That race
	// predates this function and is not made worse by it; closing it means
	// teaching that path the same lock, which is a change to shipped behaviour
	// and belongs in its own commit.
	unlock, err := lockConfig(path)
	if err != nil {
		return AddedEntry, err
	}
	defer unlock()

	content, mode, err := readConfig(path)
	if err != nil {
		return AddedEntry, err
	}

	// The managed block first, because it is the specific answer: an alias
	// found there is one shed wrote, and saying so is more useful than "it
	// exists somewhere".
	parsed := Parse(content)
	if parsed.FindEntry(entry.Name) != nil {
		return AlreadyManaged, nil
	}
	if hostAliasDeclared(content, entry.Name) {
		return AlreadyUserDefined, nil
	}

	// Checked only now that we know we are actually going to write: a
	// read-only config that already declares the alias is a no-op, not an
	// error, and refusing it would be gratuitous.
	if err := ensureWritable(path); err != nil {
		return AddedEntry, err
	}

	updated := spliceEntry(content, entry)
	if err := writeAtomic(path, updated, mode); err != nil {
		return AddedEntry, err
	}
	return AddedEntry, nil
}

// spliceEntry returns content with one entry added to the managed block,
// creating the block at the end of the file if there is none.
//
// Splicing, not regenerating. The only bytes that move are the ones between
// the last managed entry and the end marker (normalised to a single blank
// separator line, which is the separator GenerateManagedBlock itself uses);
// every other byte in the file is copied through.
func spliceEntry(content string, entry Entry) string {
	// Markers are located as STANDALONE LINES, never as loose substrings.
	// A substring search treats the marker text appearing inside a comment,
	// or inside a quoted value such as `RemoteCommand "... # shed managed
	// ..."`, as a real block boundary, and splices the generated stanza into
	// the middle of that line — corrupting the config. (sol review finding.)
	endIdx := markerLineIndex(content, EndMarker)
	if beginIdx := markerLineIndex(content, BeginMarker); beginIdx == -1 || endIdx <= beginIdx {
		// No usable managed block. Append a fresh one after whatever is
		// already there, which is therefore untouched.
		nl := dominantNewline(content)
		var sb strings.Builder
		sb.WriteString(content)
		if content != "" {
			if !strings.HasSuffix(content, "\n") {
				sb.WriteString(nl)
			}
			sb.WriteString(nl)
		}
		sb.WriteString(withNewline(GenerateManagedBlock([]Entry{entry}), nl))
		return sb.String()
	}

	nl := dominantNewline(content)
	head := strings.TrimRight(content[:endIdx], "\r\n")
	return head + nl + nl + withNewline(GenerateEntry(entry), nl) + content[endIdx:]
}

// markerLineIndex returns the byte offset of the line that IS marker, or -1.
// "Is", not "contains": the marker has to be the whole line once surrounding
// whitespace is removed, so marker text quoted inside a directive or sitting
// in a comment is not mistaken for a block boundary.
func markerLineIndex(content, marker string) int {
	offset := 0
	for _, line := range strings.SplitAfter(content, "\n") {
		if strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")) == marker {
			return offset
		}
		offset += len(line)
	}
	return -1
}

// dominantNewline reports the line ending the file already uses, so an edit
// to a CRLF config does not leave it with mixed endings. Ties and empty
// files get "\n".
func dominantNewline(content string) string {
	crlf := strings.Count(content, "\r\n")
	lf := strings.Count(content, "\n") - crlf
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

// withNewline rewrites a generated block's LF endings to nl. GenerateEntry
// always emits LF; this is a no-op unless the target file is CRLF.
func withNewline(block, nl string) string {
	if nl == "\n" {
		return block
	}
	return strings.ReplaceAll(block, "\n", nl)
}

// hostLinePattern matches an ssh config `Host` declaration.
//
// Looser than parser.go's `(?m)^Host\s+` on purpose, in both directions that
// matter for "did the user already declare this?":
//
//   - **Case-insensitive.** ssh config keywords are, so `host shed-foo` is a
//     perfectly ordinary entry, and a check that missed it would overwrite a
//     real one.
//   - **Leading whitespace allowed.** ssh accepts an indented `Host` line, and
//     people who indent their config still mean it.
//
// Erring toward "declared" is the safe direction: a false positive leaves the
// file alone and the caller prints nothing, where a false negative appends a
// duplicate alias that shadows what the user wrote.
//   - **`Host=name` as well as `Host name`.** OpenSSH accepts an equals sign
//     (optionally spaced) as the keyword/argument separator, so `Host=shed-foo`
//     is a real declaration that a space-only pattern would miss. (sol review
//     finding: a false negative appends a duplicate alias, and ssh then uses
//     whichever came first — the user's, silently.)
var hostLinePattern = regexp.MustCompile(`(?im)^[ \t]*Host[ \t]*(?:=[ \t]*|[ \t]+)(.*)$`)

// hostAliasDeclared reports whether any Host line in content names alias
// exactly.
//
// EXACT, never by pattern match. A `Host *` block matches every alias as far
// as ssh is concerned, but it is a defaults stanza, not an entry for this
// shed; treating it as one would mean never writing an entry at all on the
// many configs that have one. Negations (`!name`) are likewise not a
// declaration of `name`.
func hostAliasDeclared(content, alias string) bool {
	for _, match := range hostLinePattern.FindAllStringSubmatch(content, -1) {
		// A trailing `#` comment is not part of the pattern list.
		patterns := match[1]
		if hash := strings.Index(patterns, "#"); hash >= 0 {
			patterns = patterns[:hash]
		}
		for _, pattern := range strings.Fields(patterns) {
			// OpenSSH allows a pattern to be double-quoted, which is how a
			// name containing a space is written. The quotes are syntax, not
			// part of the name.
			if strings.Trim(pattern, `"`) == alias {
				return true
			}
		}
	}
	return false
}

// readConfig reads the config file, returning its content and the mode to
// write it back with. A missing file is empty content and 0600.
func readConfig(path string) (content string, mode os.FileMode, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0o600, nil
		}
		return "", 0, fmt.Errorf("failed to read SSH config: %w", err)
	}
	// Keep the mode the user's file already has rather than imposing 0600 on
	// it: this function edits somebody else's file, and silently re-permitting
	// it is a side effect nobody asked for. A file shed CREATES gets 0600.
	mode = 0o600
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	return string(data), mode, nil
}

// writeAtomic writes content to path through a temp file in the same
// directory and a rename, so a crash mid-write cannot leave a truncated
// ~/.ssh/config behind.
//
// The temp file is uniquely named (os.CreateTemp), unlike writer.go's fixed
// `path + ".tmp"`: two writers sharing one temp path is a second race on top
// of the one the lock above closes.
func writeAtomic(path, content string, mode os.FileMode) error {
	// Write through to the SYMLINK TARGET, never over the link.
	//
	// os.ReadFile follows a symlink but os.Rename replaces one, so a naive
	// temp-and-rename on a `~/.ssh/config` symlinked into a dotfiles repo
	// reads the repo's copy and then replaces the LINK with a regular file —
	// the repo copy silently unchanged, the link silently gone. Resolving
	// first means the edit lands where the user actually keeps the file.
	// (sol review finding.) A broken link resolves to nothing and is left to
	// the create path below.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".shed-ssh-config-*")
	if err != nil {
		return fmt.Errorf("failed to create a temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// A no-op once the rename has happened.
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close() //nolint:errcheck // the write error is the one to report
		return fmt.Errorf("failed to write %s: %w", tmpPath, err)
	}
	// CreateTemp makes the file 0600; match the destination's mode explicitly
	// so the rename does not change it.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close() //nolint:errcheck // the chmod error is the one to report
		return fmt.Errorf("failed to chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", tmpPath, path, err)
	}
	return nil
}

// ensureWritable refuses to edit a config the user has deliberately made
// read-only.
//
// A temp-file-plus-rename only needs the DIRECTORY to be writable, so without
// this check a mode-0400 ~/.ssh/config sitting in a writable ~/.ssh is
// silently replaced — the one outcome someone who chmod'd it read-only was
// trying to prevent. (sol review finding.) A missing file is not an error:
// the caller is allowed to create one.
func ensureWritable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if os.IsPermission(err) {
			return fmt.Errorf("refusing to edit read-only SSH config %s: %w", path, err)
		}
		return fmt.Errorf("failed to open %s for writing: %w", path, err)
	}
	return f.Close()
}

// lockConfig takes an exclusive flock on a sidecar lock file and returns the
// release.
//
// The lock is on a SIDECAR, not on the config itself, because the write ends
// in a rename: flock keys on the open file description, so a lock held on the
// old inode says nothing to a process that opened the new one.
func lockConfig(path string) (func(), error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close() //nolint:errcheck // the flock error is the one to report
		return nil, fmt.Errorf("failed to lock %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
