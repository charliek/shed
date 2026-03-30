package vmutil

import (
	"log"
	"os"

	"github.com/charliek/shed/internal/config"
)

// ClassifyCredentials splits credentials into directory-based and file-based.
// Directory credentials are suitable for live mounts (VirtioFS, 9P), while
// file credentials are transferred via tar. Symlinks are followed (os.Stat).
// Missing sources are logged and skipped.
func ClassifyCredentials(credentials map[string]config.MountConfig) (dirCreds, fileCreds map[string]config.MountConfig) {
	dirCreds = make(map[string]config.MountConfig)
	fileCreds = make(map[string]config.MountConfig)

	for name, mount := range credentials {
		info, err := os.Stat(mount.Source)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("Credential %q source does not exist, skipping: %s", name, mount.Source)
			} else {
				log.Printf("Warning: failed to stat credential %q source %s: %v", name, mount.Source, err)
			}
			continue
		}

		if info.IsDir() {
			dirCreds[name] = mount
		} else {
			fileCreds[name] = mount
		}
	}

	return dirCreds, fileCreds
}

// HasWritableCredentials reports whether any credentials in the map are writable.
func HasWritableCredentials(creds map[string]config.MountConfig) bool {
	for _, mount := range creds {
		if !mount.ReadOnly {
			return true
		}
	}
	return false
}
