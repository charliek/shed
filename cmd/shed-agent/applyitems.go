//go:build linux
// +build linux

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charliek/shed/internal/delivery"
)

// runApplyItems implements "shed-agent apply-items": write one stage of a
// delivery into the shed user's home (and project directory). The input is
// a delivery.WriterInput on stdin; the output is one delivery.WriterLine
// per item on stdout. It runs as the shed user, never as root, and takes no
// argument: nothing secret is ever in its argv or environment.
//
// The exit code is one of delivery.WriterExit*. stderr carries at most a
// rule name or a fixed phrase, never anything from the input.
func runApplyItems(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: shed-agent apply-items < input")
		return delivery.WriterExitRefused
	}
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "apply-items: refusing to run as root")
		return delivery.WriterExitRefused
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		fmt.Fprintln(stderr, "apply-items: cannot resolve the user's home")
		return delivery.WriterExitRefused
	}
	return applyItems(stdin, stdout, stderr, filepath.Clean(u.HomeDir))
}

// applyItems reads, checks and writes one stage relative to home.
func applyItems(stdin io.Reader, stdout, stderr io.Writer, home string) int {
	// A known umask, so no directory or temporary file is ever created
	// unusable (umask 0777 would make Mkdir(0700) a 0000 directory the
	// writer could not enter). Final modes are still set exactly with
	// fchmod. Restored for the tests, which call this in-process.
	defer syscall.Umask(syscall.Umask(0o077))

	body, err := io.ReadAll(io.LimitReader(stdin, delivery.MaxWriterInputBytes+1))
	if err != nil {
		fmt.Fprintln(stderr, "apply-items: cannot read input")
		return delivery.WriterExitRefused
	}
	in, refusal := delivery.ParseWriterInput(body, home)
	if refusal != nil {
		fmt.Fprintf(stderr, "apply-items: input %v\n", refusal)
		return delivery.WriterExitRefused
	}

	w := &stageWriter{home: home, projectDir: in.ProjectDir}
	defer w.close()
	lines, envRemoveFailed := w.apply(in)

	enc := json.NewEncoder(stdout)
	code := delivery.WriterExitOK
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return delivery.WriterExitFailed
		}
		if l.Result != delivery.ResultOK {
			code = delivery.WriterExitFailed
		}
	}
	if envRemoveFailed {
		fmt.Fprintln(stderr, "apply-items: the env file was not removed")
		code = delivery.WriterExitFailed
	}
	return code
}

// applyItemsTestHook, when set (by tests only), is called at two points to
// change the file system under the writer: "descend", after a segment was
// checked and before it is opened; "rename", after the temporary file was
// written and before it is renamed over the target.
var applyItemsTestHook func(point string, dir *os.Root, name string)

// stageWriter writes one stage. Every operation is relative to a directory
// opened as an os.Root, and every path is walked one segment at a time
// without following a symbolic link at any segment (an os.Root alone
// follows links that stay inside it).
type stageWriter struct {
	home       string
	projectDir string
	// dev is the home's file system, once home is opened. A directory on
	// another one (a host directory mounted into the shed) is refused
	// other_filesystem: a delivered file must not land outside the shed,
	// where it would outlive it and be readable from the host. Only
	// directories are compared: on an overlay root a regular file may
	// report its layer's device, not the overlay's (no xino), so an
	// existing target is never judged by its st_dev.
	dev      uint64
	checkDev bool

	homeRoot    *os.Root
	projectRoot *os.Root
	opened      []*os.Root
}

func (w *stageWriter) close() {
	for _, r := range w.opened {
		_ = r.Close()
	}
}

// apply writes the files in order, then (stage 1) the env file, stopping at
// the first failure. It returns a line per item, files first then env items,
// each in input order. envRemoveFailed reports a stage 1 with no env item
// whose env file could not be removed.
func (w *stageWriter) apply(in *delivery.WriterInput) (lines []delivery.WriterLine, envRemoveFailed bool) {
	// The base itself: os.OpenRoot follows a link in the path it is given,
	// so a link at home or any directory above it refuses the whole stage.
	// (The project directory is walked from home a segment at a time.)
	if linkOnPath(w.home) {
		for _, f := range in.Files {
			lines = append(lines, delivery.WriterLine{Name: f.Name, Result: delivery.ResultSymlink})
		}
		for _, e := range in.Env {
			lines = append(lines, delivery.WriterLine{Name: e.Name, Result: delivery.ResultSymlink})
		}
		return lines, in.Stage == 1 && len(in.Env) == 0
	}
	failed := false
	for _, f := range in.Files {
		if failed {
			lines = append(lines, delivery.WriterLine{Name: f.Name, Result: delivery.ResultNotWritten})
			continue
		}
		res := w.writeFile(f)
		lines = append(lines, delivery.WriterLine{Name: f.Name, Result: res})
		failed = res != delivery.ResultOK
	}
	if in.Stage != 1 {
		return lines, false
	}
	if failed {
		for _, e := range in.Env {
			lines = append(lines, delivery.WriterLine{Name: e.Name, Result: delivery.ResultNotWritten})
		}
		return lines, false
	}
	if len(in.Env) == 0 {
		return lines, w.removeEnvFile() != delivery.ResultOK
	}
	res := w.writeEnvFile(in.Env)
	for _, e := range in.Env {
		lines = append(lines, delivery.WriterLine{Name: e.Name, Result: res})
	}
	return lines, false
}

// base opens (once) the directory a file's path is relative to.
func (w *stageWriter) base(b delivery.Base) (*os.Root, delivery.ItemResult) {
	if w.homeRoot == nil {
		// Never open a home that is not a directory: os.OpenRoot's open has
		// neither O_DIRECTORY nor O_NONBLOCK, so a FIFO there would block.
		if !isDirNoFollow(w.home) {
			return nil, delivery.ResultNotDirectory
		}
		r, err := os.OpenRoot(w.home)
		if err != nil {
			return nil, resultOf(err)
		}
		w.homeRoot = r
		w.opened = append(w.opened, r)
		fi, err := r.Stat(".")
		if err != nil {
			return nil, resultOf(err)
		}
		w.dev, w.checkDev = devOf(fi), true
	}
	if b == delivery.BaseHome {
		return w.homeRoot, delivery.ResultOK
	}
	if w.projectRoot == nil {
		rel, ok := strings.CutPrefix(w.projectDir, w.home+"/")
		if !ok || rel == "" {
			// ParseWriterInput already refuses this; the writer never
			// trusts that it did.
			return nil, delivery.ResultNotDirectory
		}
		r, res := w.walk(w.homeRoot, strings.Split(rel, "/"), false)
		if res != delivery.ResultOK {
			return nil, res
		}
		w.projectRoot = r
	}
	return w.projectRoot, delivery.ResultOK
}

// walk descends from r through dirs, one segment at a time, refusing a
// link or a non-directory at any of them. With create, a missing directory
// is made 0700; without, a missing directory is not_directory.
func (w *stageWriter) walk(r *os.Root, dirs []string, create bool) (*os.Root, delivery.ItemResult) {
	cur := r
	for _, d := range dirs {
		next, res := w.descend(cur, d, create)
		if res != delivery.ResultOK {
			return nil, res
		}
		cur = next
	}
	return cur, delivery.ResultOK
}

// descend opens the directory name directly inside r as a root of its own.
// The segment is checked with Lstat (a link or a non-directory refuses it),
// opened, and the opened directory is checked to be the one Lstat saw, so a
// segment swapped for a link in between is refused too.
func (w *stageWriter) descend(r *os.Root, name string, create bool) (*os.Root, delivery.ItemResult) {
	created := false
	fi, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, delivery.ResultNotDirectory
		}
		if err := r.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, resultOf(err)
		}
		created = true
		fi, err = r.Lstat(name)
	}
	if err != nil {
		return nil, resultOf(err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, delivery.ResultSymlink
	}
	if !fi.IsDir() {
		return nil, delivery.ResultNotDirectory
	}
	if applyItemsTestHook != nil {
		applyItemsTestHook("descend", r, name)
	}
	sub, err := r.OpenRoot(name)
	if err != nil {
		return nil, resultOf(err)
	}
	w.opened = append(w.opened, sub)
	got, err := sub.Stat(".")
	if err != nil {
		return nil, resultOf(err)
	}
	// A concurrent process running as the shed user can still rename
	// things under the writer between these steps; it gains nothing it
	// does not already have (it can write wherever the writer can), so
	// that is not defended. What is defended is what was already there.
	if !os.SameFile(fi, got) {
		return nil, delivery.ResultSymlink
	}
	if w.checkDev && devOf(got) != w.dev {
		return nil, delivery.ResultOtherFilesystem
	}
	if created {
		// Mkdir's mode is cut by the umask; set it exactly, on the
		// directory just opened.
		d, err := sub.Open(".")
		if err != nil {
			return nil, resultOf(err)
		}
		err = d.Chmod(0o700)
		_ = d.Close()
		if err != nil {
			return nil, resultOf(err)
		}
	}
	return sub, delivery.ResultOK
}

// writeFile writes one file item.
func (w *stageWriter) writeFile(f delivery.WriterFile) delivery.ItemResult {
	b, res := w.base(f.Base)
	if res != delivery.ResultOK {
		return res
	}
	segs := strings.Split(f.Path, "/")
	dir, res := w.walk(b, segs[:len(segs)-1], true)
	if res != delivery.ResultOK {
		return res
	}
	return w.replaceFile(dir, segs[len(segs)-1], f.FileMode(), f.Content)
}

// envDir walks to the user environment directory under home.
func (w *stageWriter) envDir(create bool) (*os.Root, delivery.ItemResult) {
	h, res := w.base(delivery.BaseHome)
	if res != delivery.ResultOK {
		return nil, res
	}
	return w.walk(h, strings.Split(delivery.UserEnvDir, "/"), create)
}

// writeEnvFile replaces the env file whole with one KEY=VALUE line per env
// item, in order. Values are written byte for byte: the contract's value
// rule (printable, no edge space) makes them safe as bare lines.
func (w *stageWriter) writeEnvFile(env []delivery.WriterEnv) delivery.ItemResult {
	var b strings.Builder
	for _, e := range env {
		b.WriteString(e.Variable)
		b.WriteByte('=')
		b.WriteString(e.Value)
		b.WriteByte('\n')
	}
	if b.Len() > delivery.MaxEnvFileBytes {
		// More than the shed's loader reads of one file: fail the env items
		// rather than acknowledge values that would be cut off.
		return delivery.ResultIO
	}
	dir, res := w.envDir(true)
	if res != delivery.ResultOK {
		return res
	}
	return w.replaceFile(dir, delivery.EnvFileName, 0o600, []byte(b.String()))
}

// removeEnvFile removes the env file if it is there and is a regular file.
// A missing directory or file is nothing to do; a link (on the way or at
// the file), a directory or anything else is refused and left alone.
func (w *stageWriter) removeEnvFile() delivery.ItemResult {
	h, res := w.base(delivery.BaseHome)
	if res != delivery.ResultOK {
		return res
	}
	dir := h
	for _, d := range strings.Split(delivery.UserEnvDir, "/") {
		if _, err := dir.Lstat(d); errors.Is(err, fs.ErrNotExist) {
			return delivery.ResultOK
		}
		next, res := w.descend(dir, d, false)
		if res != delivery.ResultOK {
			return res
		}
		dir = next
	}
	fi, err := dir.Lstat(delivery.EnvFileName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return delivery.ResultOK
	case err != nil:
		return resultOf(err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return delivery.ResultSymlink
	case fi.IsDir():
		return delivery.ResultIsDirectory
	case !fi.Mode().IsRegular():
		return delivery.ResultIO
	}
	if err := dir.Remove(delivery.EnvFileName); err != nil {
		return resultOf(err)
	}
	return delivery.ResultOK
}

// replaceFile writes content to name in dir with exactly mode: a temporary
// file created exclusively (never through a link) in dir, written and
// synced, then renamed over name. A directory or a link at name is
// refused. The temporary file is removed on any failure.
func (w *stageWriter) replaceFile(dir *os.Root, name string, mode os.FileMode, content []byte) delivery.ItemResult {
	fi, err := dir.Lstat(name)
	switch {
	case err == nil && fi.Mode()&fs.ModeSymlink != 0:
		return delivery.ResultSymlink
	case err == nil && fi.IsDir():
		return delivery.ResultIsDirectory
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return resultOf(err)
	}

	tmp, err := tempName(name)
	if err != nil {
		return delivery.ResultIO
	}
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return resultOf(err)
	}
	done := false
	defer func() {
		if !done {
			_ = dir.Remove(tmp)
		}
	}()
	// The create mode is cut by the umask; set it exactly on the open file.
	err = f.Chmod(mode)
	if err == nil {
		_, err = f.Write(content)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return resultOf(err)
	}
	if applyItemsTestHook != nil {
		applyItemsTestHook("rename", dir, name)
	}
	if err := dir.Rename(tmp, name); err != nil {
		if errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
			return delivery.ResultIsDirectory
		}
		return resultOf(err)
	}
	done = true
	// Make the rename durable; the file is in place either way.
	if d, err := dir.Open("."); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return delivery.ResultOK
}

// tempName is a fresh hidden name beside name, kept within a segment's
// length.
func tempName(name string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return "." + name + ".shed-" + hex.EncodeToString(b[:]) + ".tmp", nil
}

// devOf is the file system (device) a file is on.
func devOf(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev) //nolint:unconvert // Dev is uint32 on some platforms
	}
	return 0
}

// isDirNoFollow reports whether p is a directory, not following a link
// at its last segment (linkOnPath covers the ones above).
func isDirNoFollow(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// linkOnPath reports whether p (absolute) or any directory above it is a
// symbolic link. A path that cannot be examined is left to the open that
// follows, which then fails.
func linkOnPath(p string) bool {
	p = filepath.Clean(p)
	for cur := p; cur != "/" && cur != "."; cur = filepath.Dir(cur) {
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// resultOf maps a file system error to a writer result.
func resultOf(err error) delivery.ItemResult {
	switch {
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EROFS):
		return delivery.ResultPermission
	case errors.Is(err, syscall.ELOOP):
		return delivery.ResultSymlink
	case errors.Is(err, syscall.ENOTDIR):
		return delivery.ResultNotDirectory
	case errors.Is(err, syscall.EISDIR):
		return delivery.ResultIsDirectory
	}
	return delivery.ResultIO
}
