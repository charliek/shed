package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/charliek/shed/internal/egress"
)

const egressProxyBinName = "shed-egress-proxy"

// egressProxyBinDir returns the directory of the running shed-server binary,
// where a co-installed shed-egress-proxy is expected (brew/deb ship both).
func egressProxyBinDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// resolveEgressProxyBin locates the shed-egress-proxy binary: first alongside
// the shed-server binary (exeDir), then on $PATH. Returns an error if neither
// has it, so an egress-enabled server fails to start rather than silently
// running without the proxy.
func resolveEgressProxyBin(exeDir string) (string, error) {
	cand := filepath.Join(exeDir, egressProxyBinName)
	if st, err := os.Stat(cand); err == nil && !st.IsDir() {
		return cand, nil
	}
	if p, err := exec.LookPath(egressProxyBinName); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%s not found in %s or on PATH", egressProxyBinName, exeDir)
}

// logEgressAudit is the interim audit sink: it surfaces denials in the server
// log so the feature is observable before the durable audit log + desktop
// stream land. Allows are intentionally not logged (too noisy in audit mode).
func logEgressAudit(rec egress.AuditRecord) {
	if rec.Verdict == "deny" {
		log.Printf("egress[%s] DENY %s:%d/%s (%s)", rec.Shed, rec.Host, rec.Port, rec.Protocol, rec.Reason)
	}
}
