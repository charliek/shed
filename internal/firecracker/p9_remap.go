//go:build linux
// +build linux

package firecracker

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/hugelgupf/p9/p9"
)

// remappingAttacher wraps a p9.Attacher to intercept file operations and
// remap UID/GID ownership. This fixes the problem where localfs runs as root
// and ignores UID/GID parameters in Create/Mkdir/Symlink, causing all new
// files to be owned by root on the host.
type remappingAttacher struct {
	inner     p9.Attacher
	root      string
	targetUID int
	targetGID int
}

// remappingFile wraps a p9.File to intercept file-creation and metadata
// operations for UID/GID remapping. The hostPath is tracked independently
// because localfs.Local.path is unexported.
type remappingFile struct {
	p9.File          // embedded -- delegates all unintercepted methods
	hostPath  string // tracked ourselves (localfs.Local.path is unexported)
	targetUID int
	targetGID int
}

// newRemappingAttacher creates a remapping attacher that wraps localfs.
// If targetUID and targetGID are both 0, callers should use localfs.Attacher
// directly instead (passthrough mode).
func newRemappingAttacher(inner p9.Attacher, root string, targetUID, targetGID int) *remappingAttacher {
	return &remappingAttacher{
		inner:     inner,
		root:      root,
		targetUID: targetUID,
		targetGID: targetGID,
	}
}

// Attach implements p9.Attacher.Attach.
func (a *remappingAttacher) Attach() (p9.File, error) {
	f, err := a.inner.Attach()
	if err != nil {
		return nil, err
	}
	return newRemappingFile(f, a.root, a.targetUID, a.targetGID), nil
}

// newRemappingFile wraps an inner p9.File with UID remapping.
func newRemappingFile(inner p9.File, hostPath string, uid, gid int) *remappingFile {
	return &remappingFile{
		File:      inner,
		hostPath:  hostPath,
		targetUID: uid,
		targetGID: gid,
	}
}

// unwrapFile extracts the inner p9.File from a remappingFile.
// If the file is not wrapped, returns it unchanged. This is needed for
// Link, RenameAt, and Renamed which pass File arguments that localfs
// type-asserts to *Local.
func unwrapFile(f p9.File) p9.File {
	if w, ok := f.(*remappingFile); ok {
		return w.File
	}
	return f
}

// lchown calls os.Lchown and logs on error. We use Lchown (not Chown) to
// avoid following symlinks, preventing symlink attacks where a guest creates
// a symlink to a sensitive file and triggers ownership change on the target.
func (f *remappingFile) lchown(path string) {
	if err := os.Lchown(path, f.targetUID, f.targetGID); err != nil {
		log.Printf("Warning: lchown %s to %d:%d failed: %v", path, f.targetUID, f.targetGID, err)
	}
}

// Walk implements p9.File.Walk with hostPath tracking.
func (f *remappingFile) Walk(names []string) ([]p9.QID, p9.File, error) {
	qids, inner, err := f.File.Walk(names)
	if err != nil {
		return qids, nil, err
	}
	if inner == nil {
		return qids, nil, nil
	}

	// Track the host path: empty names = clone (same path),
	// otherwise join the walked names.
	newPath := f.hostPath
	if len(names) > 0 {
		newPath = filepath.Join(append([]string{f.hostPath}, names...)...)
	}
	return qids, newRemappingFile(inner, newPath, f.targetUID, f.targetGID), nil
}

// Create implements p9.File.Create. After delegation, Lchown the new file.
func (f *remappingFile) Create(name string, flags p9.OpenFlags, permissions p9.FileMode, uid p9.UID, gid p9.GID) (p9.File, p9.QID, uint32, error) {
	inner, qid, iounit, err := f.File.Create(name, flags, permissions, uid, gid)
	if err != nil {
		return nil, qid, iounit, err
	}

	childPath := filepath.Join(f.hostPath, name)
	f.lchown(childPath)

	var wrapped p9.File
	if inner != nil {
		wrapped = newRemappingFile(inner, childPath, f.targetUID, f.targetGID)
	}
	return wrapped, qid, iounit, nil
}

// Mkdir implements p9.File.Mkdir. After delegation, Lchown the new directory.
func (f *remappingFile) Mkdir(name string, permissions p9.FileMode, uid p9.UID, gid p9.GID) (p9.QID, error) {
	qid, err := f.File.Mkdir(name, permissions, uid, gid)
	if err != nil {
		return qid, err
	}
	f.lchown(filepath.Join(f.hostPath, name))
	return qid, nil
}

// Symlink implements p9.File.Symlink. After delegation, Lchown the symlink itself.
func (f *remappingFile) Symlink(oldName string, newName string, uid p9.UID, gid p9.GID) (p9.QID, error) {
	qid, err := f.File.Symlink(oldName, newName, uid, gid)
	if err != nil {
		return qid, err
	}
	f.lchown(filepath.Join(f.hostPath, newName))
	return qid, nil
}

// GetAttr implements p9.File.GetAttr with UID/GID remapping.
// Root-owned files (uid 0) are remapped to the target UID/GID so the guest
// sees them as owned by the shed user.
func (f *remappingFile) GetAttr(req p9.AttrMask) (p9.QID, p9.AttrMask, p9.Attr, error) {
	qid, mask, attr, err := f.File.GetAttr(req)
	if err != nil {
		return qid, mask, attr, err
	}
	if attr.UID == 0 {
		attr.UID = p9.UID(f.targetUID)
	}
	if attr.GID == 0 {
		attr.GID = p9.GID(f.targetGID)
	}
	return qid, mask, attr, nil
}

// SetAttr implements p9.File.SetAttr. localfs.SetAttr returns ENOSYS for
// UID/GID changes, so we handle ownership via os.Lchown directly and
// delegate remaining attributes to the inner file.
func (f *remappingFile) SetAttr(valid p9.SetAttrMask, attr p9.SetAttr) error {
	if valid.UID || valid.GID {
		uid := -1
		gid := -1
		if valid.UID {
			uid = int(attr.UID)
		}
		if valid.GID {
			gid = int(attr.GID)
		}
		if err := os.Lchown(f.hostPath, uid, gid); err != nil {
			return err
		}

		// Strip UID/GID from the mask before delegating
		valid.UID = false
		valid.GID = false
	}

	// Delegate remaining attributes (if any) to inner file
	if !valid.Empty() {
		return f.File.SetAttr(valid, attr)
	}
	return nil
}

// Link implements p9.File.Link. Unwrap the target before delegating so
// localfs can type-assert to *Local.
func (f *remappingFile) Link(target p9.File, newName string) error {
	return f.File.Link(unwrapFile(target), newName)
}

// RenameAt implements p9.File.RenameAt. Unwrap newDir before delegating.
// Do NOT update hostPath here -- that's Renamed's job.
func (f *remappingFile) RenameAt(oldName string, newDir p9.File, newName string) error {
	return f.File.RenameAt(oldName, unwrapFile(newDir), newName)
}

// Renamed implements p9.File.Renamed. Unwrap newDir, delegate, and update hostPath.
func (f *remappingFile) Renamed(newDir p9.File, newName string) {
	unwrapped := unwrapFile(newDir)
	f.File.Renamed(unwrapped, newName)

	// Update our tracked hostPath. Extract the parent path from the
	// unwrapped directory if it's a remappingFile, otherwise we can't
	// track it (shouldn't happen in practice).
	if w, ok := newDir.(*remappingFile); ok {
		f.hostPath = filepath.Join(w.hostPath, newName)
	}
}

// verifyRemappingCapability tests that Lchown works on the target directory
// by creating a temp file, chowning it, and removing it. Logs a warning on
// failure but does not block server creation.
func verifyRemappingCapability(hostPath string, targetUID, targetGID int) {
	tmpFile := filepath.Join(hostPath, fmt.Sprintf(".shed-remap-test-%d", os.Getpid()))
	f, err := os.Create(tmpFile)
	if err != nil {
		log.Printf("Warning: remapping capability check failed (create): %v", err)
		return
	}
	f.Close()
	defer os.Remove(tmpFile)

	if err := os.Lchown(tmpFile, targetUID, targetGID); err != nil {
		log.Printf("Warning: remapping capability check failed (lchown to %d:%d): %v", targetUID, targetGID, err)
	}
}
