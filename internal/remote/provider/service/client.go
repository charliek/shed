package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/remote/provider"
)

// The HTTP client: provider.Provider over the compute contract. Every call
// goes to the service the server is enrolled with, through the directory
// runtime's client (issuer only, no redirect), with a compute:manage token
// the runtime mints ahead of use. Every reply is classified by the status
// matrix (classify.go) and validated (validate.go); an error body's message
// is never read, so it is never logged, stored or shown.

// Session is what the client takes from the directory runtime: what one
// call needs (directory.Runtime.ComputeAccess), and a way to drop a token
// a 401 refused.
type Session interface {
	ComputeAccess(ctx context.Context) (directory.ComputeAccess, error)
	ForgetComputeToken(bearer string)
}

// Defaults of the client.
const (
	// DefaultPollFirst is the operation poll interval for its first
	// DefaultPollSwitch polls; DefaultPollAfter is the interval after.
	DefaultPollFirst  = 2 * time.Second
	DefaultPollAfter  = 5 * time.Second
	DefaultPollSwitch = 5
	// maxPollFailures is how many polls in a row may fail as unavailable
	// before Wait gives up.
	maxPollFailures = 6
)

// Config configures a Client.
type Config struct {
	// Session gives each call its token, the issuer, the server's enrolled
	// name and the HTTP client. Required.
	Session Session
	// TestDrivers lets an access host be loopback, link-local, unspecified
	// or multicast (remote.allow_test_drivers).
	TestDrivers bool
	// PollFirst, PollAfter and PollSwitch override the operation polling:
	// PollFirst for the first PollSwitch polls, then PollAfter.
	PollFirst, PollAfter time.Duration
	PollSwitch           int
	// CallDeadline overrides the 12 s a call may take; tests shorten it.
	CallDeadline time.Duration
}

// Client is the compute service's provider.Provider.
type Client struct {
	cfg Config
}

var _ provider.Provider = (*Client)(nil)

// New returns a client.
func New(cfg Config) (*Client, error) {
	if cfg.Session == nil {
		return nil, errors.New("service: no session")
	}
	if cfg.PollFirst <= 0 {
		cfg.PollFirst = DefaultPollFirst
	}
	if cfg.PollAfter <= 0 {
		cfg.PollAfter = DefaultPollAfter
	}
	if cfg.PollSwitch <= 0 {
		cfg.PollSwitch = DefaultPollSwitch
	}
	if cfg.CallDeadline <= 0 {
		cfg.CallDeadline = CallDeadline
	}
	return &Client{cfg: cfg}, nil
}

// Name implements provider.Provider.
func (c *Client) Name() string { return provider.NameService }

// call is one call of the contract.
type call struct {
	kind   Call
	method string
	path   string
	body   any
	shedID string
}

// do sends one call and classifies its reply: once, again after a 401 with a
// new token, and again after a 429 while the deadline allows. The whole of
// it is bounded by the call deadline. It returns the decoded answer of an
// ok reply, or the error the reply means.
func (c *Client) do(ctx context.Context, k call) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CallDeadline)
	defer cancel()
	var raw []byte
	if k.body != nil {
		b, err := json.Marshal(k.body)
		if err != nil {
			return nil, provider.Errorf(provider.ErrInvalid, string(k.kind), "the request cannot be encoded")
		}
		if len(b) > MaxRequest {
			return nil, provider.Errorf(provider.ErrInvalid, string(k.kind), "the request is %d bytes, over %d", len(b), MaxRequest)
		}
		raw = b
	}
	reauthed := false
	for {
		acc, err := c.cfg.Session.ComputeAccess(ctx)
		if err != nil {
			return nil, c.accessError(ctx, k, err)
		}
		vctx := Context{ShedID: k.shedID, Server: acc.Server, TestDrivers: c.cfg.TestDrivers}
		rep, written := c.send(ctx, acc, k, raw)
		if rep.Err != nil && !written {
			// The request was never written (the connection was refused,
			// the host not found, the TLS handshake failed, the deadline
			// came first): the service cannot have acted on it. Any attempt
			// before this one was answered (a 401 or a 429), so the call as
			// a whole changed nothing.
			return nil, &provider.Error{Kind: provider.ErrUnavailable, Op: string(k.kind),
				Detail: "the request was never delivered: " + deliveryFailure(rep.Err), Err: provider.ErrNotDelivered}
		}
		o := Classify(k.kind, rep, vctx)
		switch o.Result {
		case ResultOK:
			return o.Value, nil
		case ResultReauth:
			c.cfg.Session.ForgetComputeToken(acc.Bearer)
			if reauthed {
				// Both attempts were refused at the token, before any
				// judgement: the service did not act on the call.
				return nil, &provider.Error{Kind: provider.ErrUnavailable, Op: string(k.kind), Detail: "a second 401", Err: provider.ErrNotActed}
			}
			reauthed = true
			continue
		case ResultRetry:
			// A 429 is a refusal before any judgement: past the deadline
			// the call is unavailable and was not acted on, and its
			// Retry-After goes with it for the caller to honour.
			limited := &provider.Error{Kind: provider.ErrUnavailable, Op: string(k.kind), Code: "RATE_LIMITED",
				Detail: "rate limited past the call's deadline", Err: provider.ErrNotActed, RetryAfter: o.RetryAfter}
			deadline, _ := ctx.Deadline()
			if time.Until(deadline) <= o.RetryAfter {
				return nil, limited
			}
			select {
			case <-time.After(o.RetryAfter):
			case <-ctx.Done():
				return nil, limited
			}
			continue
		case ResultRefused:
			return nil, c.fail(k, o.Kind, o.Code, fmt.Sprintf("refused (HTTP %d)", rep.Status))
		case ResultUnknown:
			return nil, c.fail(k, o.Kind, "", "no reply it can read: the outcome is unknown; send it again with the same request id")
		}
		return nil, c.fail(k, o.Kind, "", c.why(rep))
	}
}

// why says, for a log, what a failed reply was: its status, or that none
// arrived whole. Nothing of the body.
func (c *Client) why(r Reply) string {
	if r.Err != nil {
		return "no reply arrived whole"
	}
	return fmt.Sprintf("HTTP %d", r.Status)
}

// accessError is the error of a call that got no token: the scope refused
// is denied; anything else is unavailable (the service could not be asked).
// The call itself was never sent (provider.ErrNotDelivered).
func (c *Client) accessError(ctx context.Context, k call, err error) error {
	notSent := func(detail string) error {
		return &provider.Error{Kind: provider.ErrUnavailable, Op: string(k.kind), Detail: detail, Err: provider.ErrNotDelivered}
	}
	switch {
	case errors.Is(err, directory.ErrComputeDenied):
		return c.fail(k, provider.ErrDenied, "COMPUTE_DENIED", "the account service refuses this server the compute scope")
	case errors.Is(err, directory.ErrComputeNotEnrolled):
		return notSent("the server is not enrolled with the account service")
	case ctx.Err() != nil:
		return notSent("no compute token within the call's deadline")
	}
	return notSent("no compute token")
}

// deliveryFailure names, for a log, why a request was never written: never
// the URL or anything the service sent.
func deliveryFailure(err error) string {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "the call's deadline came first"
	case errors.As(err, &dns):
		return "the service's host was not found"
	case errors.As(err, &op) && op.Op == "dial":
		return "the connection was refused or could not be made"
	}
	return "the connection could not be made"
}

func (c *Client) fail(k call, kind error, code, detail string) error {
	return &provider.Error{Kind: kind, Op: string(k.kind), Code: code, Detail: detail}
}

// send makes one attempt and reads its reply to the status's limit.
// written says whether the request was written to a connection (in whole or
// in part): a failure before that is one the service never saw.
func (c *Client) send(ctx context.Context, acc directory.ComputeAccess, k call, body []byte) (rep Reply, written bool) {
	rep = Reply{Bearer: acc.Bearer, Assertion: acc.Assertion}
	var rd io.Reader // a nil interface, not a nil *bytes.Reader, when there is no body
	if body != nil {
		rd = bytes.NewReader(body)
	}
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		// Called once the request has been written, or its write failed
		// part way: either way the service may have read it.
		WroteHeaders: func() { wrote.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) },
	})
	defer func() { written = wrote.Load() }()
	req, err := directory.NewRequest(ctx, k.method, acc.Issuer+k.path, rd)
	if err != nil {
		rep.Err = err
		return rep, false
	}
	req.Header.Set("Authorization", "Bearer "+acc.Bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := acc.HTTP.Do(req)
	if err != nil {
		rep.Err = err
		return rep, false
	}
	defer resp.Body.Close()
	rep.Status = resp.StatusCode
	rep.Header = resp.Header
	b, err := ReadBody(resp.Body, BodyLimit(k.kind, resp.StatusCode))
	switch {
	case errors.Is(err, ErrTooLarge):
		rep.Body, rep.TooLarge = b, true
	case err != nil:
		rep.Err = err
	default:
		rep.Body = b
	}
	return rep, true
}

func machinePath(shedID string) string {
	return "/v1/compute/machines/" + url.PathEscape(shedID)
}

// --- the calls ---

// Catalog implements provider.Provider.
func (c *Client) Catalog(ctx context.Context) (provider.Catalog, error) {
	v, err := c.do(ctx, call{kind: CallCatalog, method: http.MethodGet, path: "/v1/compute/catalog"})
	if err != nil {
		return provider.Catalog{}, err
	}
	cat := v.(*Catalog)
	out := provider.Catalog{
		DefaultSize:  cat.DefaultSize,
		DefaultImage: cat.Images.Default,
		Aliases:      append([]string(nil), cat.Images.Aliases...),
		AccessKinds:  append([]string(nil), cat.AccessKinds...),
		Policy: provider.Policy{
			PersonInitialLease: time.Duration(cat.Policy.Person.InitialLeaseS) * time.Second,
			PersonMaxAge:       time.Duration(cat.Policy.Person.MaxAgeS) * time.Second,
			PersonExtend:       cat.Policy.Person.ExtendAllowed,
			PresetsH:           append([]int(nil), cat.Policy.Person.PresetsH...),
			ClientMaxLease:     time.Duration(cat.Policy.Client.MaxLeaseS) * time.Second,
			Warn:               time.Duration(cat.Policy.WarnS) * time.Second,
		},
		Limits:          provider.Limits{PersonMax: cat.Limits.PersonMax, OrgMax: cat.Limits.OrgMax},
		IngressWritable: cat.Ingress.Writable,
		OwnerRequired:   true,
	}
	for _, s := range cat.Sizes {
		out.Sizes = append(out.Sizes, provider.Size{Name: s.Name, CPUs: s.CPUs, MemoryMB: s.MemoryMB, Arch: s.Arch, DiskGBDefault: s.DiskGBDefault})
	}
	return out, nil
}

// Create implements provider.Provider.
func (c *Client) Create(ctx context.Context, spec provider.Spec) (provider.Pending, provider.Instance, error) {
	if err := spec.Validate(); err != nil {
		return provider.Pending{}, provider.Instance{}, err
	}
	if _, err := checkRequestID(CallCreate, spec.RequestID); err != nil { // Validate refused an empty one
		return provider.Pending{}, provider.Instance{}, err
	}
	body := CreateRequest{
		RequestID:      spec.RequestID,
		Name:           spec.Name,
		User:           spec.User,
		Client:         spec.Client,
		Size:           spec.Size,
		Image:          spec.Image,
		DiskGB:         spec.DiskGB,
		Labels:         spec.Labels,
		AuthorizedKeys: spec.AuthorizedKeys,
		Spec:           spec.Spec,
		AccessKinds:    spec.AccessKinds,
		LeaseS:         spec.LeaseS,
	}
	if body.Labels == nil {
		body.Labels = map[string]string{}
	}
	v, err := c.do(ctx, call{kind: CallCreate, method: http.MethodPut, path: machinePath(spec.ShedID), body: body, shedID: spec.ShedID})
	if err != nil {
		return provider.Pending{}, provider.Instance{}, err
	}
	a := v.(*Accepted)
	ref := provider.Ref(spec.ShedID)
	pend := provider.Pending{Ref: ref, Operation: a.Operation, Want: provider.StateRunning}
	return pend, toInstance(a.Machine), nil
}

// action is start, stop or delete: 202 with an operation, or 200 with the
// machine when it is already in that state (or, for delete, gone).
func (c *Client) action(ctx context.Context, kind Call, method, path string, ref provider.Ref, requestID string, want provider.State) (provider.Pending, error) {
	requestID, err := checkRequestID(kind, requestID)
	if err != nil {
		return provider.Pending{}, err
	}
	shedID := string(ref)
	v, err := c.do(ctx, call{kind: kind, method: method, path: path,
		body: ActionRequest{RequestID: requestID}, shedID: shedID})
	if err != nil {
		return provider.Pending{}, err
	}
	a := v.(*Accepted)
	return provider.Pending{Ref: ref, Operation: a.Operation, Want: want}, nil
}

// Start implements provider.Provider.
func (c *Client) Start(ctx context.Context, ref provider.Ref, requestID string) (provider.Pending, error) {
	return c.action(ctx, CallStart, http.MethodPost, machinePath(string(ref))+"/start", ref, requestID, provider.StateRunning)
}

// Stop implements provider.Provider.
func (c *Client) Stop(ctx context.Context, ref provider.Ref, requestID string) (provider.Pending, error) {
	return c.action(ctx, CallStop, http.MethodPost, machinePath(string(ref))+"/stop", ref, requestID, provider.StateStopped)
}

// Destroy implements provider.Provider.
func (c *Client) Destroy(ctx context.Context, ref provider.Ref, requestID string) (provider.Pending, error) {
	return c.action(ctx, CallDelete, http.MethodDelete, machinePath(string(ref)), ref, requestID, provider.StateAbsent)
}

// Wait implements provider.Provider. It polls the operation (every
// PollFirst for its first PollSwitch polls, then every PollAfter) until it
// is done, follows the machine when the operation is unknown or expired
// (404), and then follows the machine until it is settled. A poll that
// fails as unavailable is tried again, up to maxPollFailures in a row.
func (c *Client) Wait(ctx context.Context, p provider.Pending) (inst provider.Instance, err error) {
	defer func() {
		// The work was accepted: a poll that never reached the service or
		// that it did not act on (a 429, a second 401), or whose read the
		// service refused at the token or scope (403), says
		// nothing about it. Its outcome is unknown, not undelivered and not
		// an operation that failed.
		switch {
		case errors.Is(err, provider.ErrNotActed):
			err = &provider.Error{Kind: provider.ErrUnavailable, Op: "wait", Detail: "the service could not be reached, or did not act on the read, to follow the work"}
		case errors.Is(err, provider.ErrDenied):
			err = &provider.Error{Kind: provider.ErrUnavailable, Op: "wait", Detail: "the service refused the read that follows the work (" + provider.CodeOf(err) + ")"}
		}
	}()
	polls, failures := 0, 0
	pause := func() error {
		d := c.cfg.PollFirst
		if polls >= c.cfg.PollSwitch {
			d = c.cfg.PollAfter
		}
		polls++
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return &provider.Error{Kind: provider.ErrUnavailable, Op: "wait", Err: ctx.Err()}
		}
	}
	transient := func(err error) bool {
		if errors.Is(err, provider.ErrUnavailable) && ctx.Err() == nil && failures < maxPollFailures {
			failures++
			return true
		}
		return false
	}
	op := p.Operation
	for op != "" {
		v, err := c.do(ctx, call{kind: CallOperation, method: http.MethodGet, path: "/v1/compute/operations/" + url.PathEscape(op), shedID: string(p.Ref)})
		switch {
		case errors.Is(err, provider.ErrNotFound):
			// Unknown or expired: follow the machine.
			op = ""
			continue
		case err != nil:
			if transient(err) {
				if perr := pause(); perr != nil {
					return provider.Instance{}, perr
				}
				continue
			}
			return provider.Instance{}, err
		}
		failures = 0
		o := v.(*Operation)
		switch {
		case !o.Done:
			if err := pause(); err != nil {
				return provider.Instance{}, err
			}
			continue
		case o.ErrorCode != "":
			kind := OperationKind(o.ErrorCode)
			if p.Want == provider.StateAbsent && kind == provider.ErrNotFound {
				return provider.Instance{}, nil
			}
			return provider.Instance{}, &provider.Error{Kind: kind, Op: "wait", Code: o.ErrorCode, Detail: "the operation failed"}
		case o.Gone:
			if p.Want == provider.StateAbsent {
				return provider.Instance{}, nil
			}
			return provider.Instance{}, &provider.Error{Kind: provider.ErrNotFound, Op: "wait", Code: "NOT_FOUND", Detail: "the machine is gone"}
		}
		inst := toInstance(o.Machine)
		if done, err := p.Reached(inst); done {
			return inst, err
		}
		op = "" // done, and the machine still moving: follow it
	}
	for {
		inst, err := c.Get(ctx, p.Ref)
		switch {
		case errors.Is(err, provider.ErrNotFound):
			if p.Want == provider.StateAbsent {
				return provider.Instance{}, nil
			}
			return provider.Instance{}, err
		case err != nil:
			if transient(err) {
				if perr := pause(); perr != nil {
					return provider.Instance{}, perr
				}
				continue
			}
			return provider.Instance{}, err
		}
		failures = 0
		if done, err := p.Reached(inst); done {
			return inst, err
		}
		if err := pause(); err != nil {
			return provider.Instance{}, err
		}
	}
}

// Get implements provider.Provider.
func (c *Client) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	shedID := string(ref)
	v, err := c.do(ctx, call{kind: CallGet, method: http.MethodGet, path: machinePath(shedID), shedID: shedID})
	if err != nil {
		return provider.Instance{}, err
	}
	return toInstance(v.(*Machine)), nil
}

// List implements provider.Provider.
func (c *Client) List(ctx context.Context) (provider.Listing, error) {
	v, err := c.do(ctx, call{kind: CallList, method: http.MethodGet, path: "/v1/compute/machines"})
	if err != nil {
		return provider.Listing{}, err
	}
	l := v.(*List)
	out := provider.Listing{Instances: make([]provider.Instance, 0, len(l.Machines))}
	for i := range l.Machines {
		out.Instances = append(out.Instances, toInstance(&l.Machines[i]))
	}
	for _, d := range l.Deleted {
		out.Deleted = append(out.Deleted, provider.Deletion{Ref: provider.Ref(d.ShedID), At: d.At, Cause: d.Cause})
	}
	return out, nil
}

// SetLabels implements provider.Provider.
func (c *Client) SetLabels(ctx context.Context, ref provider.Ref, requestID string, set, expect map[string]*string) (provider.Instance, error) {
	for _, l := range []map[string]*string{set, expect} {
		if err := provider.CheckLabels(l); err != nil {
			return provider.Instance{}, provider.Errorf(provider.ErrInvalid, "labels", "%v", err)
		}
	}
	requestID, err := checkRequestID(CallLabels, requestID)
	if err != nil {
		return provider.Instance{}, err
	}
	if set == nil {
		set = map[string]*string{}
	}
	shedID := string(ref)
	v, err := c.do(ctx, call{kind: CallLabels, method: http.MethodPost, path: machinePath(shedID) + "/labels",
		body: LabelsRequest{RequestID: requestID, Set: set, Expect: expect}, shedID: shedID})
	if err != nil {
		return provider.Instance{}, err
	}
	return toInstance(v.(*Machine)), nil
}

// Extend implements provider.Provider.
func (c *Client) Extend(ctx context.Context, ref provider.Ref, requestID string, by time.Duration) (provider.Instance, error) {
	if !provider.ValidExtend(by) {
		return provider.Instance{}, provider.Errorf(provider.ErrInvalid, "extend", "extend by whole hours, 1 to 720")
	}
	requestID, err := checkRequestID(CallExtend, requestID)
	if err != nil {
		return provider.Instance{}, err
	}
	shedID := string(ref)
	v, err := c.do(ctx, call{kind: CallExtend, method: http.MethodPost, path: machinePath(shedID) + "/extend",
		body: ExtendRequest{RequestID: requestID, ByS: int64(by / time.Second)}, shedID: shedID})
	if err != nil {
		return provider.Instance{}, err
	}
	return toInstance(v.(*Machine)), nil
}

// Usage implements provider.Provider.
func (c *Client) Usage(ctx context.Context, user string) (provider.Usage, error) {
	path := "/v1/compute/usage"
	if user != "" {
		if !provider.ValidHandle(user) {
			return provider.Usage{}, provider.Errorf(provider.ErrInvalid, "usage", "user %q is not a handle", user)
		}
		path += "?user=" + url.QueryEscape(user)
	}
	v, err := c.do(ctx, call{kind: CallUsage, method: http.MethodGet, path: path})
	if err != nil {
		return provider.Usage{}, err
	}
	u := v.(*Usage)
	out := provider.Usage{Org: provider.Counts(u.Org)}
	if u.Person != nil {
		pc := provider.Counts(*u.Person)
		out.Person = &pc
	}
	return out, nil
}

// Ingress implements provider.Provider.
func (c *Client) Ingress(ctx context.Context) (provider.Ingress, error) {
	v, err := c.do(ctx, call{kind: CallIngressGet, method: http.MethodGet, path: "/v1/compute/ingress"})
	if err != nil {
		return provider.Ingress{}, err
	}
	return toIngress(v.(*Ingress)), nil
}

// SetIngress implements provider.Provider.
func (c *Client) SetIngress(ctx context.Context, requestID string, sources []netip.Prefix) (provider.Ingress, error) {
	if len(sources) > MaxIngressSources {
		return provider.Ingress{}, provider.Errorf(provider.ErrInvalid, "ingress_put", "%d sources, over %d", len(sources), MaxIngressSources)
	}
	requestID, err := checkRequestID(CallIngressPut, requestID)
	if err != nil {
		return provider.Ingress{}, err
	}
	list := make([]string, 0, len(sources))
	for _, s := range sources {
		if !s.IsValid() || s.Masked() != s {
			return provider.Ingress{}, provider.Errorf(provider.ErrInvalid, "ingress_put", "source %s is not canonical", s)
		}
		list = append(list, s.String())
	}
	v, err := c.do(ctx, call{kind: CallIngressPut, method: http.MethodPut, path: "/v1/compute/ingress",
		body: IngressRequest{RequestID: requestID, Sources: list}})
	if err != nil {
		return provider.Ingress{}, err
	}
	return toIngress(v.(*Ingress)), nil
}

// checkRequestID makes a request id when none is given and refuses one
// outside the grammar.
func checkRequestID(kind Call, id string) (string, error) {
	if id == "" {
		return provider.NewRequestID(), nil
	}
	if !ValidRequestID(id) {
		return "", provider.Errorf(provider.ErrInvalid, string(kind), "request id %q is not a lower-case UUID", id)
	}
	return id, nil
}

// toInstance turns a validated machine into the provider's view.
func toInstance(m *Machine) provider.Instance {
	if m == nil {
		return provider.Instance{}
	}
	inst := provider.Instance{
		Ref:           provider.Ref(m.ShedID),
		Name:          m.Name,
		State:         toState(m.State),
		User:          m.User,
		Client:        m.Client,
		Size:          provider.Size{Name: m.Size.Name, CPUs: m.Size.CPUs, MemoryMB: m.Size.MemoryMB, Arch: m.Size.Arch},
		DiskGB:        m.DiskGB,
		Image:         m.Image.Requested,
		ImageResolved: m.Image.Resolved,
		Provider:      m.Provider,
		CreatedAt:     m.CreatedAt,
		StartedAt:     m.StartedAt,
		StoppedAt:     m.StoppedAt,
		Lease: provider.Lease{
			ExpiresAt:    m.Lease.ExpiresAt,
			MaxExpiresAt: m.Lease.MaxExpiresAt,
			Extendable:   m.Lease.Extendable,
			PresetsH:     append([]int(nil), m.Lease.PresetsH...),
		},
		IgnoredAccess: m.Ignored,
		Labels:        map[string]string{},
		Spec:          m.Spec,
	}
	for k, v := range m.Labels {
		inst.Labels[k] = v
	}
	for _, a := range m.Access {
		inst.Access = append(inst.Access, provider.Access{Kind: provider.AccessSSH, Host: a.Host.String(), Port: a.Port, User: a.User, HostKey: a.HostKey})
	}
	return inst
}

func toState(s string) provider.State {
	switch s {
	case StateCreating, StateStarting:
		return provider.StateStarting
	case StateRunning:
		return provider.StateRunning
	case StateStopping:
		return provider.StateStopping
	case StateStopped:
		return provider.StateStopped
	case StateDeleting:
		return provider.StateDeleting
	}
	return provider.StateUnknown
}

func toIngress(in *Ingress) provider.Ingress {
	return provider.Ingress{Sources: append([]netip.Prefix(nil), in.Sources...), Exclusive: in.Exclusive, AppliedAt: in.AppliedAt}
}
