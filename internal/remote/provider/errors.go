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

	// ErrLabelTaken: ClaimLabel found the label holding another non-empty
	// value. Nothing was changed.
	ErrLabelTaken = errors.New("label already holds another value")

	// ErrUnexpectedState: Wait saw the instance settle in a state other
	// than the one asked for. The instance is returned alongside.
	ErrUnexpectedState = errors.New("instance settled in an unexpected state")
)

// Error kinds of a compute service (tests/compute-contract/v1): what each
// of the contract's codes and failures means to the backend. The
// contract's fixtures/mapping.json names, for each code, one of these or
// one of the kinds above.
var (
	// ErrSpecMismatch: a create named a shed_id the service holds under a
	// different create body. Nothing was changed.
	ErrSpecMismatch = errors.New("the machine was created with another body")

	// ErrComputeLimit: the service's limit on machines per person or per
	// organization is reached (any state counts). A client may retry once
	// a machine is deleted. The API answers 503 COMPUTE_LIMIT_REACHED.
	ErrComputeLimit = errors.New("compute limit reached")

	// ErrPolicy: the service's policy refused the create (a size, an
	// image, a lease, an access kind, the person or the client). Nothing
	// was sent to the cloud, and retrying will not help. The API answers
	// 422 COMPUTE_POLICY_REFUSED.
	ErrPolicy = errors.New("refused by the compute service's policy")

	// ErrLease: the machine's lease or owner does not allow the start or
	// extend (the lease ended or is at its cap, extends are not allowed,
	// or the machine's person is not active). Nothing was changed.
	ErrLease = errors.New("refused by the machine's lease")

	// ErrDeleting: the machine is being deleted; nothing but the delete
	// will finish.
	ErrDeleting = errors.New("the machine is being deleted")

	// ErrIngress: the service will not apply the ingress sources (it does
	// not let servers write ingress, or a source is wider than it allows).
	ErrIngress = errors.New("ingress refused")

	// ErrDenied: the service does not let this server manage compute (the
	// grant is off, the server is disabled, or compute is refused by
	// policy). The API answers 403 COMPUTE_DENIED.
	ErrDenied = errors.New("compute denied for this server")

	// ErrUnavailable: the service could not be reached or did not answer
	// in time, or answered 5xx, 429 past the deadline, or 401 twice. For
	// a call that changes something, the outcome is unknown: the caller
	// retries with the same request_id or reads the machine. The API
	// answers 503 COMPUTE_UNAVAILABLE.
	ErrUnavailable = errors.New("compute service unavailable")

	// ErrInvalidReply: the service answered 2xx with a body the contract
	// refuses (too large, not the call's shape, a machine object that
	// breaks a rule, or one that echoes the bearer token).
	ErrInvalidReply = errors.New("compute service sent an invalid reply")
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
// or ReasonLimit (for the server's limit and the compute service's). It
// returns "" for every other error.
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrQuota):
		return ReasonQuota
	case errors.Is(err, ErrCapacity):
		return ReasonCapacity
	case errors.Is(err, ErrLimit), errors.Is(err, ErrComputeLimit):
		return ReasonLimit
	}
	return ""
}

// IsRefusal reports whether err is one of the refusals a client may retry
// later: quota, zone capacity, the server limit or the compute service's
// limit.
func IsRefusal(err error) bool {
	return Reason(err) != ""
}
