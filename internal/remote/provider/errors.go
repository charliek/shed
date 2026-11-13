package provider

import (
	"errors"
	"fmt"
)

// Error kinds. Test for them with errors.Is; providers return them wrapped in
// an *Error that says which call failed and why.
var (
	// ErrNotFound: the instance (or, for HostKeyReader, the published key)
	// does not exist. Destroy never returns it: there, not found is
	// success.
	ErrNotFound = errors.New("instance not found")

	// ErrExists: Create's instance name is taken by an instance with a
	// different shed_id.
	ErrExists = errors.New("instance name already in use")

	// ErrQuota: the cloud refused because the project is at a quota (CPUs,
	// addresses, disk). Retrying soon will not help; someone must free
	// resources or raise the quota. The API answers 503
	// CLOUD_QUOTA_EXCEEDED.
	ErrQuota = errors.New("cloud quota exceeded")

	// ErrCapacity: the zone has no room for this machine type right now.
	// It is the cloud's shortage, not the project's, and may clear in
	// minutes. The API answers 503 ZONE_CAPACITY_UNAVAILABLE.
	ErrCapacity = errors.New("zone capacity unavailable")

	// ErrLimit: the server's own limit (remote.limits.max_running or
	// max_sheds) is reached. No cloud returns it; the backend's limit
	// counter does, before any provider call. It lives here so the three
	// refusals map to the API in one place. The API answers 503
	// SERVER_LIMIT_REACHED.
	ErrLimit = errors.New("server limit reached")

	// ErrBusy: the instance is in a transition that does not allow this
	// call. Nothing was changed. The caller waits for the transition with
	// Wait(ctx, Settle(ref)) and calls again.
	ErrBusy = errors.New("instance is busy with another operation")

	// ErrForeign: the instance's shed_env label names another environment.
	// Nothing was changed. This is the guard that keeps one server off
	// another server's sheds.
	ErrForeign = errors.New("instance belongs to another environment")

	// ErrInvalid: the request is malformed (a bad spec, a label that
	// cannot be set). Nothing was sent to the cloud.
	ErrInvalid = errors.New("invalid request")

	// ErrUnexpectedState: Wait saw the instance settle in a state other
	// than the one asked for. The instance is returned alongside.
	ErrUnexpectedState = errors.New("instance settled in an unexpected state")
)

// Status reasons for a refusal, as they appear in a shed's status_reason.
const (
	ReasonQuota    = "quota"
	ReasonCapacity = "capacity"
	ReasonLimit    = "limit"
)

// Error is the error a provider returns. Kind is one of the Err values above,
// or nil for a failure that fits none of them (a network error, an
// unexpected reply).
type Error struct {
	// Kind is the class of failure, matched by errors.Is.
	Kind error
	// Op is the provider call that failed: "create", "wait", and so on.
	Op string
	// Detail is the cloud's own words, safe to log and to show an
	// operator. It never contains credentials.
	Detail string
	// Err is the underlying error, if any.
	Err error
}

// Errorf builds an *Error of the given kind for a provider call.
func Errorf(kind error, op, format string, args ...any) *Error {
	return &Error{Kind: kind, Op: op, Detail: fmt.Sprintf(format, args...)}
}

func (e *Error) Error() string {
	msg := "provider"
	if e.Op != "" {
		msg += " " + e.Op
	}
	switch {
	case e.Kind != nil && e.Detail != "":
		msg += ": " + e.Kind.Error() + ": " + e.Detail
	case e.Kind != nil:
		msg += ": " + e.Kind.Error()
	case e.Detail != "":
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes both the kind and the underlying error to errors.Is and
// errors.As.
func (e *Error) Unwrap() []error {
	var out []error
	if e.Kind != nil {
		out = append(out, e.Kind)
	}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

// Reason returns the status_reason for a refusal: ReasonQuota, ReasonCapacity
// or ReasonLimit. It returns "" for every other error.
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrQuota):
		return ReasonQuota
	case errors.Is(err, ErrCapacity):
		return ReasonCapacity
	case errors.Is(err, ErrLimit):
		return ReasonLimit
	}
	return ""
}

// IsRefusal reports whether err is one of the three refusals a client may
// retry later: quota, zone capacity or the server limit.
func IsRefusal(err error) bool {
	return Reason(err) != ""
}
