// Platform helpers for registry-direct pulls. shed selects the matching
// platform manifest from an OCI image index based on the backend's
// native platform — VZ requires linux/arm64 (Apple Silicon), Firecracker
// requires linux/amd64.

package vmimage

import (
	"fmt"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// PlatformOCI parses a Docker-style platform string ("linux/arm64") into
// a go-containerregistry v1.Platform. Returns an error for malformed
// input (missing os/arch, extra components, or empty tokens).
func PlatformOCI(s string) (*v1.Platform, error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fmt.Errorf("invalid platform %q (want os/arch[/variant])", s)
	}
	if parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid platform %q (empty os or arch)", s)
	}
	if len(parts) == 3 && parts[2] == "" {
		return nil, fmt.Errorf("invalid platform %q (empty variant)", s)
	}
	p := &v1.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}
