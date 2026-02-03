package docker

import (
	"testing"

	"github.com/charliek/shed/internal/backend"
)

func TestDockerBackendImplementsInterface(t *testing.T) {
	// Compile-time check (also in backend.go, but explicit test is good)
	var _ backend.Backend = (*DockerBackend)(nil)
}
