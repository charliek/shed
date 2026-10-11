package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charliek/shed/internal/egress"
)

const egressProxyBinName = "shed-egress-proxy"

// bundleExeSuffix is where the brew formula installs shed-server on macOS:
// <keg>/libexec/ShedServer.app/Contents/MacOS/shed-server, with
// <keg>/bin/shed-server a symlink to it beside <keg>/bin/shed-egress-proxy
// (plan 028 D4, #304).
var bundleExeSuffix = filepath.Join("libexec", "ShedServer.app", "Contents", "MacOS", "shed-server")

// egressProxyCandidateDirs returns, in order and without duplicates, the
// directories of the running shed-server's own installation where a
// co-installed shed-egress-proxy is expected (brew and the deb ship both):
//  1. the directory of exe (os.Executable). A launch through a symlink
//     reports the symlink's path on macOS, so under brew services this is
//     opt/shed/bin;
//  2. the directory of exe with symlinks resolved;
//  3. <keg>/bin, when the resolved exe is the bundle executable
//     <keg>/libexec/ShedServer.app/Contents/MacOS/shed-server.
func egressProxyCandidateDirs(exe string) []string {
	if exe == "" {
		return nil
	}
	var dirs []string
	add := func(d string) {
		d = filepath.Clean(d)
		for _, seen := range dirs {
			if seen == d {
				return
			}
		}
		dirs = append(dirs, d)
	}
	add(filepath.Dir(exe))
	resolved := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		resolved = r
		add(filepath.Dir(resolved))
	}
	if keg, ok := strings.CutSuffix(resolved, bundleExeSuffix); ok && strings.HasSuffix(keg, string(filepath.Separator)) {
		add(filepath.Join(keg, "bin"))
	}
	return dirs
}

// resolveEgressProxyBin locates the shed-egress-proxy that belongs to the
// running shed-server's installation, and logs a warning when it has to fall
// back to $PATH.
func resolveEgressProxyBin() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("WARNING: egress: cannot locate the shed-server executable (%v); looking for %s on PATH only", err, egressProxyBinName)
		exe = ""
	}
	return resolveEgressProxyBinFor(exe)
}

// resolveEgressProxyBinFor tries egressProxyCandidateDirs(exe) in order, then
// $PATH as a last resort: a proxy found on PATH may belong to another
// installation or version (internal/egress has no version handshake), so
// that choice is logged as a warning. Returns an error if none has it, so an
// egress-enabled server fails to start rather than silently running without
// the proxy.
func resolveEgressProxyBinFor(exe string) (string, error) {
	dirs := egressProxyCandidateDirs(exe)
	for _, d := range dirs {
		cand := filepath.Join(d, egressProxyBinName)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	searched := strings.Join(dirs, ", ")
	if searched == "" {
		searched = "(no installation directory)"
	}
	if p, err := exec.LookPath(egressProxyBinName); err == nil {
		log.Printf("WARNING: egress: %s is not in this shed-server's installation (searched %s); using %s from PATH, which may be a different version", egressProxyBinName, searched, p)
		return p, nil
	}
	return "", fmt.Errorf("%s not found in %s or on PATH", egressProxyBinName, searched)
}

// logEgressAudit is the interim audit sink: it surfaces denials in the server
// log so the feature is observable before the durable audit log + desktop
// stream land. Allows are intentionally not logged (too noisy in audit mode).
func logEgressAudit(rec egress.AuditRecord) {
	if rec.Verdict == "deny" {
		log.Printf("egress[%s] DENY %s:%d/%s (%s)", rec.Shed, rec.Host, rec.Port, rec.Protocol, rec.Reason)
	}
}
