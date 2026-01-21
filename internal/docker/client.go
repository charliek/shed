// Package docker provides a wrapper around the Docker client for managing shed containers.
package docker

import (
	"context"
	"fmt"
	"log"
	"regexp"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"

	"github.com/charliek/shed/internal/config"
)

// envVarNameRegex validates environment variable names.
// Must start with a letter or underscore, followed by letters, digits, or underscores.
var envVarNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Client wraps the Docker client with shed-specific configuration.
type Client struct {
	docker *client.Client
	config *config.ServerConfig
}

// NewClient creates a new Docker client wrapper with the given server configuration.
func NewClient(cfg *config.ServerConfig) (*Client, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	// Verify connection by pinging Docker
	if _, err := dockerClient.Ping(context.Background()); err != nil {
		dockerClient.Close()
		return nil, fmt.Errorf("failed to connect to docker: %w", err)
	}

	return &Client{
		docker: dockerClient,
		config: cfg,
	}, nil
}

// Close closes the Docker client connection.
func (c *Client) Close() error {
	return c.docker.Close()
}

// Docker returns the underlying Docker client for advanced operations.
func (c *Client) Docker() *client.Client {
	return c.docker
}

// Config returns the server configuration.
func (c *Client) Config() *config.ServerConfig {
	return c.config
}

// buildMounts creates mount configurations for credentials from server config.
func (c *Client) buildMounts(shedName string) []mount.Mount {
	mounts := make([]mount.Mount, 0, len(c.config.Credentials)+1)

	// Add workspace volume mount
	mounts = append(mounts, mount.Mount{
		Type:   mount.TypeVolume,
		Source: config.VolumeName(shedName),
		Target: config.WorkspacePath,
	})

	// Add credential mounts from config
	for _, cred := range c.config.Credentials {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   cred.Source,
			Target:   cred.Target,
			ReadOnly: cred.ReadOnly,
		})
	}

	return mounts
}

// buildEnvList creates environment variable list for containers.
// Invalid environment variable names are logged and skipped.
func (c *Client) buildEnvList() []string {
	envList := make([]string, 0, len(c.config.EnvVars))
	for key, value := range c.config.EnvVars {
		if !envVarNameRegex.MatchString(key) {
			log.Printf("Warning: skipping invalid environment variable name %q", key)
			continue
		}
		envList = append(envList, fmt.Sprintf("%s=%s", key, value))
	}
	return envList
}
