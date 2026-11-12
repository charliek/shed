package vmutil

import (
	"io"
	"log"
	"os"

	"github.com/charliek/shed/internal/config"
)

// nopWriteCloser wraps an io.Writer to implement io.WriteCloser.
type nopWriteCloser struct {
	w io.Writer
}

func (n *nopWriteCloser) Write(p []byte) (int, error) {
	return n.w.Write(p)
}

func (n *nopWriteCloser) Close() error {
	return nil
}

// NopWriteCloser wraps an io.Writer to implement io.WriteCloser with a no-op Close.
func NopWriteCloser(w io.Writer) io.WriteCloser {
	return &nopWriteCloser{w: w}
}

// SelectMounts returns the server mounts (top-level `mounts:`) a shed with the
// stored selection sel gets: the configured mounts sel allows whose source
// directories exist on the host. A nil selection (metadata written before mount
// selection existed) allows every mount, as before; an unknown mode allows
// none. A selected name that is no longer configured is logged and skipped, as
// is a missing or non-directory source. Returns nil when nothing is selected.
//
// Every create and start path (the SSH auto-start included, which goes
// through StartShed) calls this with the shed's stored selection, so the
// selection holds on every boot.
func SelectMounts(serverCfg *config.ServerConfig, sel *config.MountSelection) map[string]config.MountConfig {
	if sel != nil && sel.Mode == config.MountModeSelected {
		for _, name := range sel.Names {
			if serverCfg == nil || serverCfg.Mounts == nil {
				log.Printf("Mount %q is selected but not configured, skipping", name)
				continue
			}
			if _, ok := serverCfg.Mounts[name]; !ok {
				log.Printf("Mount %q is selected but not configured, skipping", name)
			}
		}
	}
	if serverCfg == nil || len(serverCfg.Mounts) == 0 {
		return nil
	}

	var result map[string]config.MountConfig
	for name, mount := range serverCfg.Mounts {
		if !sel.Allows(name) {
			continue
		}
		info, err := os.Stat(mount.Source)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("Mount %q source does not exist, skipping: %s", name, mount.Source)
			} else {
				log.Printf("Warning: failed to stat mount %q source %s: %v", name, mount.Source, err)
			}
			continue
		}
		if !info.IsDir() {
			log.Printf("Warning: mount %q source %s is not a directory, skipping", name, mount.Source)
			continue
		}
		if result == nil {
			result = make(map[string]config.MountConfig, len(serverCfg.Mounts))
		}
		result[name] = mount
	}
	return result
}
