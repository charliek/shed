//go:build linux
// +build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/charliek/shed/internal/delivery"
)

// systemEnvMaxLine is the longest line the /etc/environment.d reader takes:
// bufio.Scanner's default token size, which it used before. A longer line
// is skipped; the lines after it still load.
const systemEnvMaxLine = bufio.MaxScanTokenSize

// User environment directory limits (plan 039 §3.4). The loader runs for
// every command the agent starts, so each invocation is bounded whatever
// the directory holds; past a cap the rest is skipped, never an error.
const (
	// userEnvMaxFile is the most read from one file; the rest is ignored,
	// and so is a line the cut falls inside. It is the contract's env file
	// limit, so a whole delivered env file is always read.
	userEnvMaxFile = delivery.MaxEnvFileBytes
	// userEnvMaxLine is the longest line taken; a longer one is skipped.
	userEnvMaxLine = 16 << 10
	// userEnvMaxEntries is how many directory entries are looked at, of
	// any name, read in chunks of userEnvReadChunk.
	userEnvMaxEntries = 4096
	userEnvReadChunk  = 256
	// userEnvMaxFiles is how many .conf files are read: the first in
	// lexical order among the entries looked at.
	userEnvMaxFiles = 64
	// userEnvMaxTotal is the most read across all files.
	userEnvMaxTotal = 1 << 20
	// userEnvMaxEnv is the most the taken lines may add up to; at the
	// first line that would pass it, loading stops.
	userEnvMaxEnv = 512 << 10
)

// environmentDEntries is what the environment directories add to a
// command's environment: systemDir's entries, then (when commands run as
// the shed user) the user's own directory's. A later entry for the same key
// wins when the command starts, so a user value beats a system default; the
// user directory never sets a refused variable.
func environmentDEntries(systemDir string, user *userInfo) []string {
	env := loadEnvironmentD(systemDir)
	if user != nil {
		env = append(env, loadUserEnvironmentD(user.homeDir)...)
	}
	return env
}

// loadEnvironmentD reads all *.conf files in the given directory and returns
// environment variables in KEY=VALUE format. This follows the systemd
// environment.d convention: one variable per line, # comments, blank lines
// ignored. Files are read in alphabetical order so later files can override
// earlier ones (matching systemd behavior).
func loadEnvironmentD(dir string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.conf"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)

	var env []string
	for _, path := range matches {
		vars := parseEnvironmentFile(path)
		env = append(env, vars...)
	}
	return env
}

// parseEnvironmentFile reads a single environment file and returns KEY=VALUE pairs.
func parseEnvironmentFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseEnvLines(f, systemEnvMaxLine, nil)
}

// loadUserEnvironmentD reads the shed user's environment directory,
// home/.config/environment.d, the way the agent reads /etc/environment.d,
// with the care a directory the user writes needs: no symbolic link is
// followed at any segment, only regular files are read (a FIFO never
// blocks the read), at most userEnvMaxFile bytes of each, a line over
// userEnvMaxLine is skipped, and a variable the provision contract refuses
// (PATH, HOME, LD_*, SHED_*, ..., without regard to case) is skipped. A
// value is taken byte for byte after the first '='. Files are read in
// lexical order, within the caps above. A link at home or above it reads
// nothing.
func loadUserEnvironmentD(home string) []string {
	if linkOnPath(home) || !isDirNoFollow(home) {
		return nil
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil
	}
	w := &stageWriter{}
	defer w.close()
	defer root.Close()
	dir, res := w.walk(root, strings.Split(delivery.UserEnvDir, "/"), false)
	if res != delivery.ResultOK {
		return nil
	}
	d, err := dir.Open(".")
	if err != nil {
		return nil
	}
	var names []string
	for seen := 0; seen < userEnvMaxEntries; {
		chunk, err := d.Readdirnames(userEnvReadChunk)
		seen += len(chunk)
		for _, name := range chunk {
			if ok, _ := path.Match("*.conf", name); ok {
				names = append(names, name)
			}
		}
		if err != nil || len(chunk) == 0 {
			break
		}
	}
	_ = d.Close()
	sort.Strings(names)
	if len(names) > userEnvMaxFiles {
		names = names[:userEnvMaxFiles]
	}

	keep := func(key string) bool {
		return !delivery.RefusedVariable(key) && !delivery.RefusedVariable(strings.TrimSpace(key))
	}
	var env []string
	budget, size := userEnvMaxTotal, 0
	for _, name := range names {
		if budget <= 0 {
			break
		}
		data, read, ok := readUserEnvFile(dir, name, min(userEnvMaxFile, budget))
		budget -= read
		if !ok {
			continue
		}
		for _, line := range parseEnvLines(bytes.NewReader(data), userEnvMaxLine, keep) {
			if size+len(line)+1 > userEnvMaxEnv {
				return env
			}
			size += len(line) + 1
			env = append(env, line)
		}
	}
	return env
}

// readUserEnvFile reads up to limit bytes of a regular file name in dir,
// never through a link and never blocking on a FIFO. A file longer than
// limit is cut after its last whole line within it. read is how many bytes
// were read from the file.
func readUserEnvFile(dir *os.Root, name string, limit int) (data []byte, read int, ok bool) {
	fi, err := dir.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, 0, false
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	got, err := f.Stat()
	if err != nil || !got.Mode().IsRegular() || !os.SameFile(fi, got) {
		return nil, 0, false
	}
	data, err = io.ReadAll(io.LimitReader(f, int64(limit)+1))
	read = len(data)
	if err != nil {
		return nil, read, false
	}
	if len(data) > limit {
		data = data[:limit]
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			data = data[:i+1]
		} else {
			data = nil
		}
	}
	return data, read, true
}

// parseEnvLines takes each line of r as KEY=VALUE: the line is trimmed of
// white space at its ends, blank lines and # comments are ignored, and a
// line with no '=' is ignored. A line longer than maxLine bytes is skipped
// and reading goes on with the next. keep, when set, decides by the key
// (the text before the first '=') whether a line is taken.
func parseEnvLines(r io.Reader, maxLine int, keep func(key string) bool) []string {
	br := bufio.NewReaderSize(r, maxLine+1)
	var vars []string
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// No newline within maxLine+1 bytes: the line is too long.
			skipping = true
			continue
		}
		if len(chunk) > 0 && !skipping {
			line := strings.TrimSpace(string(chunk))
			if line != "" && !strings.HasPrefix(line, "#") {
				if key, _, ok := strings.Cut(line, "="); ok && (keep == nil || keep(key)) {
					vars = append(vars, line)
				}
			}
		}
		skipping = false
		if err != nil {
			return vars
		}
	}
}
