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

// FilterExistingCredentials returns the subset of credentials in serverCfg whose
// source directories exist on the host. Non-existent sources are logged and skipped.
// Returns nil if serverCfg is nil or has no credentials.
func FilterExistingCredentials(serverCfg *config.ServerConfig) map[string]config.MountConfig {
	if serverCfg == nil || len(serverCfg.Credentials) == 0 {
		return nil
	}

	result := make(map[string]config.MountConfig, len(serverCfg.Credentials))
	for name, mount := range serverCfg.Credentials {
		info, err := os.Stat(mount.Source)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("Credential %q source does not exist, skipping: %s", name, mount.Source)
			} else {
				log.Printf("Warning: failed to stat credential %q source %s: %v", name, mount.Source, err)
			}
			continue
		}
		if !info.IsDir() {
			log.Printf("Warning: credential %q source %s is not a directory, skipping", name, mount.Source)
			continue
		}
		result[name] = mount
	}
	return result
}
