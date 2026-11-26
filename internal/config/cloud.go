package config

import (
	"fmt"
	"regexp"
	"time"
)

// A cloud shed's facts (plan 041): the remote backend's sheds carry them in
// Shed.Cloud, from the machine the account service answered. Nothing in them
// names a cloud project, a zone, a network or an address, so every caller
// who may see the shed (a service client included) gets them.

// FeatureLease is the /api/info feature token for cloud sheds with a lease:
// every remote shed record carries "cloud" (ShedCloud), POST
// /api/sheds/{name}/extend moves a lease later, GET /api/cloud/usage reports
// the machines held against the account service's limits, and a service
// client's create may carry "lease". internal/api advertises it.
const FeatureLease = "lease"

// DefaultLeaseWarn is how long before a lease's end a person is warned when
// the record names no time (CloudLease.WarnS absent).
const DefaultLeaseWarn = 2 * time.Hour

// ShedCloud is a cloud shed's machine as the account service reported it.
// Every remote record carries one: before the service has answered for the
// machine it holds what the server asked for (the size, the disk, the image
// name, the shed's creation time) and no lease, which is then unknown.
type ShedCloud struct {
	// Provider is a word for display (which cloud, say); absent until the
	// service has answered.
	Provider string    `json:"provider,omitempty"`
	Size     CloudSize `json:"size"`
	DiskGB   int       `json:"disk_gb,omitempty"`
	// Image is the image the machine was made from: what the create's
	// image name resolved to, or the name itself when the service gave no
	// resolution (or has not answered yet).
	Image string `json:"image"`
	// CreatedAt is when the service made the machine; StartedAt and
	// StoppedAt its last start and last stop, absent when not known (or
	// not since the last start, for StoppedAt).
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// Lease is absent while it is unknown: before the service has
	// answered for the machine.
	Lease *CloudLease `json:"lease,omitempty"`
}

// CloudSize is a cloud machine's size.
type CloudSize struct {
	Name     string `json:"name"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
}

// CloudLease is when the account service deletes a cloud shed's machine and
// how far it may be extended.
type CloudLease struct {
	// ExpiresAt is when the machine is deleted, running or stopped.
	ExpiresAt time.Time `json:"expires_at"`
	// MaxExpiresAt is the cap: no extend goes past it.
	MaxExpiresAt time.Time `json:"max_expires_at"`
	// Extendable says an extend by one hour would be accepted now.
	Extendable bool `json:"extendable"`
	// PresetsH are the hours a person is offered to extend by.
	PresetsH []int `json:"presets_h,omitempty"`
	// WarnS is how long before ExpiresAt (seconds) a person is warned: the
	// service's policy. Absent means DefaultLeaseWarn; 0 means no warning.
	WarnS *int64 `json:"warn_s,omitempty"`
}

// Warn is how long before the lease's end a person is warned (0: never).
func (l CloudLease) Warn() time.Duration {
	if l.WarnS != nil {
		return time.Duration(*l.WarnS) * time.Second
	}
	return DefaultLeaseWarn
}

// Left is the time left on the lease at now, never negative.
func (l CloudLease) Left(now time.Time) time.Duration {
	if d := l.ExpiresAt.Sub(now); d > 0 {
		return d
	}
	return 0
}

// Soon reports whether the lease ends within its warning time of now (or
// has ended). A warning time of 0 never warns.
func (l CloudLease) Soon(now time.Time) bool {
	w := l.Warn()
	return w > 0 && l.Left(now) < w
}

// ExtendShedRequest is the body of POST /api/sheds/{name}/extend.
type ExtendShedRequest struct {
	// Hours is how much later the lease ends: 1 to MaxExtendHours.
	Hours int `json:"hours"`
	// RequestID, optional, is a UUID the caller makes for this extend and
	// sends again when it retries it (a reply it did not get): every POST
	// with the same id for the same shed, by the same caller, moves the
	// lease once, for as long as the account service keeps its answer (24
	// hours). Without one each POST is a new extend. The server never
	// sends it to the service as it is: it derives the service's request id
	// from it, the shed's id and the caller, so another shed or caller
	// never shares it.
	RequestID string `json:"request_id,omitempty"`
}

// uuidRegex is a UUID in either case.
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidUUID reports whether s is a UUID (8-4-4-4-12 hex digits).
func ValidUUID(s string) bool { return uuidRegex.MatchString(s) }

// MaxExtendHours bounds one extend (the compute contract's 30 days).
const MaxExtendHours = 720

// CloudCounts is one line of usage: the machines held, how many of them run
// and how many are stopped, and the limit.
type CloudCounts struct {
	Machines int `json:"machines"`
	Running  int `json:"running"`
	Stopped  int `json:"stopped"`
	Max      int `json:"max"`
}

// CloudUsage is GET /api/cloud/usage: the machines the caller (a person)
// holds against the account service's limit, and the organization's. Person
// is null for a caller who is not a person of the service (a local admin, a
// service client) and for a person the service would refuse a machine.
type CloudUsage struct {
	Person *CloudCounts `json:"person"`
	Org    CloudCounts  `json:"org"`
}

// FormatLeft renders a duration as the lease words do: "1d 2h", "1h 20m",
// "20m" (minutes rounded down, never negative).
func FormatLeft(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	minutes := int(d/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// FormatLeaseTime renders a lease time in UTC: "14:30 UTC" on now's day,
// with the date ("2026-11-27 14:30 UTC") on any other.
func FormatLeaseTime(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04") + " UTC"
	}
	return t.Format("2006-01-02 15:04") + " UTC"
}

// LeaseBanner is the one line a person is shown when they enter a cloud
// shed whose lease ends within its warning time (the server, for a session
// with a terminal; the CLI, for a `shed exec` without one), and false for
// any other shed:
//
//	shed: this cloud shed is deleted at 14:30 UTC (in 1h 20m); extend with: shed extend NAME --hours N
//
// with the shed's name for NAME. A lease that cannot be extended (a
// service client's machine, one at its cap) gets the line without its
// advice.
func LeaseBanner(shed *Shed, now time.Time) (string, bool) {
	if shed == nil || shed.Cloud == nil || shed.Cloud.Lease == nil || shed.Cloud.Lease.ExpiresAt.IsZero() {
		return "", false
	}
	l := *shed.Cloud.Lease
	if !l.Soon(now) {
		return "", false
	}
	line := fmt.Sprintf("shed: this cloud shed is deleted at %s (in %s)", FormatLeaseTime(l.ExpiresAt, now), FormatLeft(l.Left(now)))
	if l.Extendable {
		line += "; extend with: shed extend " + shed.Name + " --hours N"
	}
	return line, true
}
