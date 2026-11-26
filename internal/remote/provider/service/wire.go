// Package service is the remote backend's provider for a compute service:
// an account service that holds the cloud credential and makes every cloud
// call, reached over the compute contract (tests/compute-contract/v1).
//
// This file and its siblings hold the contract's wire types, the
// validation of every reply (validate.go) and the classification of every
// status and error code (classify.go), as the contract's fixtures pin
// them. The HTTP client that implements provider.Provider on them comes
// next; nothing here sends a request.
//
// The package imports internal/remote/provider for its error kinds and
// nothing cloud-specific.
package service

import (
	"net/netip"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// Call is one of the contract's calls, as its fixtures name them.
type Call string

// The calls.
const (
	CallCatalog    Call = "catalog"
	CallCreate     Call = "create"
	CallStart      Call = "start"
	CallStop       Call = "stop"
	CallDelete     Call = "delete"
	CallOperation  Call = "operation"
	CallGet        Call = "get"
	CallList       Call = "list"
	CallLabels     Call = "labels"
	CallExtend     Call = "extend"
	CallUsage      Call = "usage"
	CallIngressGet Call = "ingress_get"
	CallIngressPut Call = "ingress_put"
)

// Calls lists every call.
var Calls = []Call{CallCatalog, CallCreate, CallStart, CallStop, CallDelete, CallOperation, CallGet, CallList, CallLabels, CallExtend, CallUsage, CallIngressGet, CallIngressPut}

// Mutation reports whether a call changes anything: its request carries a
// request_id, and a reply that never arrived leaves its outcome unknown.
func (c Call) Mutation() bool {
	switch c {
	case CallCreate, CallStart, CallStop, CallDelete, CallLabels, CallExtend, CallIngressPut:
		return true
	}
	return false
}

// Limits of the contract.
const (
	// MaxRequest is the largest request body.
	MaxRequest = 64 << 10
	// MaxReply is the largest reply body but the list's.
	MaxReply = 64 << 10
	// MaxListReply is the largest machine list.
	MaxListReply = 4 << 20
	// MaxErrorBody is how much of an error body a server reads.
	MaxErrorBody = 4 << 10
	// MaxListMachines is the most machines one list holds.
	MaxListMachines = 500
	// MaxLabels is the most labels a machine holds.
	MaxLabels = provider.MaxLabels
	// MaxSpec is the largest spec, in bytes.
	MaxSpec = provider.MaxSpec
	// ServiceDeadline is how soon a service answers every call;
	// CallDeadline is how long a server waits for one.
	ServiceDeadline = 10 * time.Second
	CallDeadline    = 12 * time.Second
	// MaxRetryAfter caps a 429's Retry-After; DefaultRetryAfter is used
	// when it is absent or malformed.
	MaxRetryAfter     = 30 * time.Second
	DefaultRetryAfter = 5 * time.Second
	// MaxIngressSources is the most sources an ingress holds.
	MaxIngressSources = 64
	// MaxCount bounds every usage count and every limit (person_max,
	// org_max).
	MaxCount = 1000000
	// MaxLeaseS bounds every lease and policy duration, in seconds (one
	// year).
	MaxLeaseS = 31536000
	// LinkUser is the only user an ssh access option names.
	LinkUser = "shed-link"
	// Scope is the token scope every compute call needs.
	Scope = "compute:manage"
)

// The machine states.
const (
	StateCreating = "creating"
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateStopping = "stopping"
	StateDeleting = "deleting"
)

// The deletion causes.
const (
	CauseLeaseEnded = "lease_ended"
	CausePage       = "page"
	CauseExternal   = "external"
	CauseServer     = "server"
)

// The access kinds this version defines.
const AccessSSH = "ssh"

// Catalog is GET /v1/compute/catalog's answer.
type Catalog struct {
	Sizes       []Size        `json:"sizes"`
	DefaultSize string        `json:"default_size"`
	Images      CatalogImages `json:"images"`
	AccessKinds []string      `json:"access_kinds"`
	Policy      Policy        `json:"policy"`
	Limits      Limits        `json:"limits"`
	Ingress     struct {
		Writable bool `json:"writable"`
	} `json:"ingress"`
}

// Size is one size of the catalog.
type Size struct {
	Name          string `json:"name"`
	CPUs          int    `json:"cpus"`
	MemoryMB      int    `json:"memory_mb"`
	Arch          string `json:"arch"`
	DiskGBDefault int    `json:"disk_gb_default"`
}

// CatalogImages names the image default resolves to and the aliases.
type CatalogImages struct {
	Default string   `json:"default"`
	Aliases []string `json:"aliases"`
}

// Policy is the catalog's policy.
type Policy struct {
	Person struct {
		InitialLeaseS int64 `json:"initial_lease_s"`
		MaxAgeS       int64 `json:"max_age_s"`
		ExtendAllowed bool  `json:"extend_allowed"`
		PresetsH      []int `json:"presets_h"`
	} `json:"person"`
	Client struct {
		MaxLeaseS int64 `json:"max_lease_s"`
	} `json:"client"`
	WarnS int64 `json:"warn_s"`
}

// Limits are the catalog's limits.
type Limits struct {
	PersonMax int `json:"person_max"`
	OrgMax    int `json:"org_max"`
}

// CreateRequest is PUT /v1/compute/machines/{shed_id}'s body. Exactly one
// of User and Client is set; LeaseS is set for a client only.
type CreateRequest struct {
	RequestID      string            `json:"request_id"`
	Name           string            `json:"name"`
	User           string            `json:"user,omitempty"`
	Client         string            `json:"client,omitempty"`
	Size           string            `json:"size"`
	Image          string            `json:"image"`
	DiskGB         int               `json:"disk_gb,omitempty"`
	Labels         map[string]string `json:"labels"`
	AuthorizedKeys []string          `json:"authorized_keys"`
	Spec           string            `json:"spec"`
	AccessKinds    []string          `json:"access_kinds"`
	LeaseS         int64             `json:"lease_s,omitempty"`
}

// ActionRequest is the body of start, stop and delete.
type ActionRequest struct {
	RequestID string `json:"request_id"`
}

// LabelsRequest is POST …/labels's body: one atomic write. A nil value in
// Set removes the label; in Expect it means "must be absent".
type LabelsRequest struct {
	RequestID string             `json:"request_id"`
	Set       map[string]*string `json:"set"`
	Expect    map[string]*string `json:"expect,omitempty"`
}

// ExtendRequest is POST …/extend's body.
type ExtendRequest struct {
	RequestID string `json:"request_id"`
	ByS       int64  `json:"by_s"`
}

// IngressRequest is PUT /v1/compute/ingress's body.
type IngressRequest struct {
	RequestID string   `json:"request_id"`
	Sources   []string `json:"sources"`
}

// Machine is the machine object, validated.
type Machine struct {
	ShedID    string
	Name      string
	Server    string
	User      string // "" for a client's machine
	Client    string // "" for a person's machine
	State     string
	Size      MachineSize
	DiskGB    int
	Image     MachineImage
	Provider  string
	CreatedAt time.Time
	StartedAt time.Time // zero: null
	StoppedAt time.Time // zero: null
	Lease     Lease
	// Access holds the options of the kinds this version defines, in the
	// service's order; options of other kinds are left out (Ignored counts
	// them).
	Access  []SSHAccess
	Ignored int
	Labels  map[string]string
	Spec    string
}

// MachineSize is a machine's size.
type MachineSize struct {
	Name     string
	CPUs     int
	MemoryMB int
	Arch     string
}

// MachineImage is what was asked for and what it resolved to.
type MachineImage struct {
	Requested string
	Resolved  string
}

// Lease is a machine's lease.
type Lease struct {
	ExpiresAt    time.Time
	MaxExpiresAt time.Time
	Extendable   bool
	PresetsH     []int
}

// SSHAccess is an ssh access option: direct SSH to Host:Port as User,
// with the attested HostKey.
type SSHAccess struct {
	Host    netip.Addr
	Port    int
	User    string
	HostKey string
}

// Operation is GET /v1/compute/operations/{op}'s answer.
type Operation struct {
	Done bool
	// Machine is the machine when the operation is done and it exists;
	// Gone is true when it is done and the machine no longer exists (a
	// finished delete).
	Machine *Machine
	Gone    bool
	// ErrorCode is the code a failed operation ended with.
	ErrorCode string
}

// List is GET /v1/compute/machines's answer.
type List struct {
	Machines []Machine
	Deleted  []Deletion
}

// Deletion is one recent deletion.
type Deletion struct {
	ShedID string
	At     time.Time
	Cause  string
}

// Counts is one usage line.
type Counts struct {
	Machines int `json:"machines"`
	Running  int `json:"running"`
	Stopped  int `json:"stopped"`
	Max      int `json:"max"`
}

// Usage is GET /v1/compute/usage's answer.
type Usage struct {
	Person *Counts
	Org    Counts
}

// Ingress is the server's ingress, as GET and PUT /v1/compute/ingress
// answer it.
type Ingress struct {
	Sources   []netip.Prefix
	Exclusive bool
	AppliedAt time.Time // zero: null
}

// Accepted is a 202 or 200 to a create, start, stop or delete: the
// operation to follow, the machine when the reply carries one, or Gone for
// a delete's 204.
type Accepted struct {
	Operation string
	Machine   *Machine
	Gone      bool
}
