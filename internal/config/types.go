// Package config provides configuration types and loading for shed.
package config

import (
	"fmt"
	"regexp"
	"time"
)

// shedNameRegex validates shed names: lowercase alphanumeric and hyphens, starting with a letter.
var shedNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$`)

// MaxShedNameLength is the maximum allowed length for a shed name.
const MaxShedNameLength = 63

// ValidateShedName validates that a shed name is valid.
// Names must be lowercase alphanumeric with hyphens allowed (not at start/end),
// must start with a letter, and must be at most 63 characters.
func ValidateShedName(name string) error {
	if name == "" {
		return fmt.Errorf("shed name cannot be empty")
	}

	if len(name) > MaxShedNameLength {
		return fmt.Errorf("shed name cannot exceed %d characters", MaxShedNameLength)
	}

	if !shedNameRegex.MatchString(name) {
		return fmt.Errorf("shed name must be lowercase alphanumeric with hyphens (not at start/end), starting with a letter")
	}

	return nil
}

// Shed represents a development environment container.
type Shed struct {
	Name        string    `json:"name" yaml:"name"`
	Status      string    `json:"status" yaml:"status"`
	CreatedAt   time.Time `json:"created_at" yaml:"created_at"`
	Repo        string    `json:"repo,omitempty" yaml:"repo,omitempty"`
	ContainerID string    `json:"container_id" yaml:"container_id"`
}

// Shed status constants.
const (
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusError    = "error"
)

// ServerInfo is returned by GET /api/info.
type ServerInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	SSHPort  int    `json:"ssh_port"`
	HTTPPort int    `json:"http_port"`
}

// SSHHostKeyResponse is returned by GET /api/ssh-host-key.
type SSHHostKeyResponse struct {
	HostKey string `json:"host_key"`
}

// ShedsResponse is returned by GET /api/sheds.
type ShedsResponse struct {
	Sheds []Shed `json:"sheds"`
}

// CreateShedRequest is the request body for POST /api/sheds.
type CreateShedRequest struct {
	Name  string `json:"name"`
	Repo  string `json:"repo,omitempty"`
	Image string `json:"image,omitempty"`
}

// APIError represents an error response from the API.
type APIError struct {
	Error APIErrorDetail `json:"error"`
}

// APIErrorDetail contains the error code and message.
type APIErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewAPIError creates a new APIError with the given code and message.
func NewAPIError(code, message string) APIError {
	return APIError{
		Error: APIErrorDetail{
			Code:    code,
			Message: message,
		},
	}
}

// Error codes for API responses.
const (
	ErrShedNotFound       = "SHED_NOT_FOUND"
	ErrShedAlreadyExists  = "SHED_ALREADY_EXISTS"
	ErrShedAlreadyRunning = "SHED_ALREADY_RUNNING"
	ErrShedAlreadyStopped = "SHED_ALREADY_STOPPED"
	ErrInvalidShedName    = "INVALID_SHED_NAME"
	ErrCloneFailed        = "CLONE_FAILED"
	ErrDockerError        = "DOCKER_ERROR"
	ErrInternalError      = "INTERNAL_ERROR"
)

// Docker label keys for shed containers.
const (
	LabelShed        = "shed"
	LabelShedName    = "shed.name"
	LabelShedCreated = "shed.created"
	LabelShedRepo    = "shed.repo"
)

// ContainerPrefix is prepended to shed names for Docker containers.
const ContainerPrefix = "shed-"

// VolumePrefix is prepended to shed names for Docker volumes.
const VolumePrefix = "shed-"

// VolumeSuffix is appended to shed names for Docker volumes.
const VolumeSuffix = "-workspace"

// ContainerName returns the Docker container name for a shed.
func ContainerName(shedName string) string {
	return ContainerPrefix + shedName
}

// VolumeName returns the Docker volume name for a shed.
func VolumeName(shedName string) string {
	return VolumePrefix + shedName + VolumeSuffix
}

// WorkspacePath is the path where the workspace volume is mounted in containers.
const WorkspacePath = "/workspace"
