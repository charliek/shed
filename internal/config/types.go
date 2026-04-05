// Package config provides configuration types and loading for shed.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Sentinel errors for shed and session operations.
var (
	// ErrShedNotFoundSentinel is returned when a shed does not exist.
	ErrShedNotFoundSentinel = errors.New("shed not found")

	// ErrShedAlreadyExistsSentinel is returned when creating a shed that already exists.
	ErrShedAlreadyExistsSentinel = errors.New("shed already exists")

	// ErrShedAlreadyRunningSentinel is returned when starting a shed that is already running.
	ErrShedAlreadyRunningSentinel = errors.New("shed is already running")

	// ErrSessionNotFoundSentinel is returned when a tmux session does not exist.
	ErrSessionNotFoundSentinel = errors.New("session not found")

	// ErrTmuxNotAvailableSentinel is returned when tmux is not installed in the container.
	ErrTmuxNotAvailableSentinel = errors.New("tmux is not available in this container")

	// ErrShedNotRunningSentinel is returned when an operation requires a running shed.
	ErrShedNotRunningSentinel = errors.New("shed is not running")

	// ErrUnknownImageSentinel is returned when a requested image variant does not exist.
	ErrUnknownImageSentinel = errors.New("unknown image")

	// ErrImageNotFoundSentinel is returned when a cached image does not exist.
	ErrImageNotFoundSentinel = errors.New("image not found")

	// ErrImageInUseSentinel is returned when trying to delete an image referenced by config.
	ErrImageInUseSentinel = errors.New("image is referenced by config")

	// ErrNotSupportedSentinel is returned when an operation is not supported by a backend.
	ErrNotSupportedSentinel = errors.New("not supported by this backend")
)

// shedNameRegex validates shed names: lowercase alphanumeric and hyphens, starting with a letter.
var shedNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$`)

// sessionNameRegex validates session names: alphanumeric, underscores, and hyphens.
var sessionNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

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
	Name        string                         `json:"name" yaml:"name"`
	Status      string                         `json:"status" yaml:"status"`
	CreatedAt   time.Time                      `json:"created_at" yaml:"created_at"`
	Repo        string                         `json:"repo,omitempty" yaml:"repo,omitempty"`
	ContainerID string                         `json:"container_id" yaml:"container_id"`
	Backend     string                         `json:"backend,omitempty" yaml:"backend,omitempty"`
	IPAddress   string                         `json:"ip_address,omitempty" yaml:"ip_address,omitempty"`
	CPUs        int                            `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	MemoryMB    int                            `json:"memory_mb,omitempty" yaml:"memory_mb,omitempty"`
	PID         int                            `json:"pid,omitempty" yaml:"pid,omitempty"`
	RootfsPath  string                         `json:"rootfs_path,omitempty" yaml:"rootfs_path,omitempty"`
	LocalDir    string                         `json:"local_dir,omitempty" yaml:"local_dir,omitempty"`
	Image       string                         `json:"image,omitempty" yaml:"image,omitempty"`
	LastHealthy *time.Time                     `json:"last_healthy,omitempty" yaml:"last_healthy,omitempty"` // last heartbeat from agent (VM backends only)
	StartedAt   *time.Time                     `json:"started_at,omitempty" yaml:"started_at,omitempty"`     // agent boot time from heartbeat (VM backends only)
	Extensions  map[string]ExtensionHealthInfo `json:"extensions,omitempty" yaml:"extensions,omitempty"`     // per-extension health (VM backends only)
}

// ExtensionHealthInfo is the API-facing extension health for a shed.
type ExtensionHealthInfo struct {
	Guest string `json:"guest"` // "running", "stopped", "failed"
	Host  string `json:"host"`  // "connected", "unreachable", "unknown"
}

// Shed status constants.
const (
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusError    = "error"
)

// Session represents a tmux session within a shed container.
type Session struct {
	Name        string    `json:"name"`
	ShedName    string    `json:"shed_name"`
	ServerName  string    `json:"server_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Attached    bool      `json:"attached"`
	WindowCount int       `json:"window_count,omitempty"`
}

// Session constants.
const (
	// DefaultSessionName is the name used when no session is specified.
	DefaultSessionName = "default"

	// MaxSessionNameLength is the maximum allowed length for a session name.
	MaxSessionNameLength = 63
)

// ValidateSessionName validates that a session name is valid.
// Names must be alphanumeric with underscores and hyphens allowed,
// must start with an alphanumeric character, and must be at most 63 characters.
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("session name cannot be empty")
	}

	if len(name) > MaxSessionNameLength {
		return fmt.Errorf("session name cannot exceed %d characters", MaxSessionNameLength)
	}

	if !sessionNameRegex.MatchString(name) {
		return fmt.Errorf("session name must be alphanumeric with underscores and hyphens (not at start), starting with alphanumeric")
	}

	return nil
}

// ServerInfo is returned by GET /api/info.
type ServerInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	SSHPort  int    `json:"ssh_port"`
	HTTPPort int    `json:"http_port"`
	Backend  string `json:"backend"`
}

// SSHHostKeyResponse is returned by GET /api/ssh-host-key.
type SSHHostKeyResponse struct {
	HostKey string `json:"host_key"`
}

// ShedsResponse is returned by GET /api/sheds.
type ShedsResponse struct {
	Sheds []Shed `json:"sheds"`
}

// SessionsResponse is returned by GET /api/sheds/{name}/sessions and GET /api/sessions.
type SessionsResponse struct {
	Sessions []Session `json:"sessions"`
	Warnings []string  `json:"warnings,omitempty"`
}

// ImageInfo describes an available image variant.
type ImageInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	DockerRef string `json:"docker_ref,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Source    string `json:"source"` // "config" or "discovered"
	Cached    bool   `json:"cached"`
}

// ImagesResponse is returned by GET /api/images.
type ImagesResponse struct {
	Images []ImageInfo `json:"images"`
}

// PruneImagesResponse is returned by POST /api/images/prune.
type PruneImagesResponse struct {
	Deleted []ImageInfo `json:"deleted"`
}

// CreateShedRequest is the request body for POST /api/sheds.
type CreateShedRequest struct {
	Name        string `json:"name"`
	Repo        string `json:"repo,omitempty"`
	Image       string `json:"image,omitempty"`
	NoProvision bool   `json:"no_provision,omitempty"`

	// Backend specifies which backend to use ("firecracker" or "vz").
	// If empty, uses the server's configured backend.
	Backend string `json:"backend,omitempty"`

	// CPUs specifies the number of vCPUs (firecracker/vz only)
	CPUs int `json:"cpus,omitempty"`

	// MemoryMB specifies the memory in MB (firecracker/vz only)
	MemoryMB int `json:"memory_mb,omitempty"`

	// LocalDir mounts a host directory as the workspace instead of creating
	// a volume. Mutually exclusive with Repo.
	LocalDir string `json:"local_dir,omitempty"`
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
	ErrInvalidRepoURL     = "INVALID_REPO_URL"
	ErrCloneFailed        = "CLONE_FAILED"
	ErrBackendNotEnabled  = "BACKEND_NOT_ENABLED"
	ErrUnknownImage       = "UNKNOWN_IMAGE"
	ErrImageNotFound      = "IMAGE_NOT_FOUND"
	ErrImageInUse         = "IMAGE_IN_USE"
	ErrBackendError       = "BACKEND_ERROR"
	ErrInternalError      = "INTERNAL_ERROR"
	ErrSessionNotFound    = "SESSION_NOT_FOUND"
	ErrInvalidSessionName = "INVALID_SESSION_NAME"
	ErrTmuxNotAvailable   = "TMUX_NOT_AVAILABLE"
	ErrInvalidLocalDir    = "INVALID_LOCAL_DIR"
)

// Backend type constants for Shed.Backend field.
const (
	BackendFirecracker = "firecracker"
	BackendVZ          = "vz"
	BackendDetect      = "detect"
)

// WorkspacePath is the path where the workspace volume is mounted in containers.
const WorkspacePath = "/workspace"

// VirtioFSMountTag is the mount tag used for VirtioFS shared directories.
const VirtioFSMountTag = "workspace"

// CredentialMountTag returns the VirtioFS mount tag for a credential share.
// Tags use the format "cred-{name}" to avoid collisions with the workspace tag.
func CredentialMountTag(name string) string {
	return "cred-" + name
}
