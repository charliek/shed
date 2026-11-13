// Package gce is the provider.Provider for Google Compute Engine. It speaks
// the Compute v1 REST API with net/http and the standard library only: no
// Google client library. The access token comes from a service account key
// (see Credentials).
//
// One Client serves one environment in one project and zone. Every instance
// it creates carries the shed labels (provider.NewSpec), the network tag
// provider.NetworkTag(env), no service account, block-project-ssh-keys and
// enable-guest-attributes, a run limit that stops (never deletes) the VM, and
// a pd-balanced boot disk that is deleted with it.
//
// Besides the Provider methods the Client has the reads the backend needs for
// its housekeeping: boot disks by environment (ListDisks, DeleteDisk) and the
// network, subnetwork and firewall rules for the VPC check.
package gce

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// DefaultBaseURL is the Compute v1 REST root.
const DefaultBaseURL = "https://compute.googleapis.com/compute/v1/"

// Network tiers for the external address.
const (
	TierPremium  = "PREMIUM"
	TierStandard = "STANDARD"
)

// DefaultDiskType is the boot disk type.
const DefaultDiskType = "pd-balanced"

const (
	defaultPollInterval = time.Second
	defaultCallTimeout  = 60 * time.Second
	// waitCallTimeout bounds one zoneOperations.wait request, which the API
	// holds open for up to two minutes.
	waitCallTimeout = 150 * time.Second
	maxReplyBytes   = 16 << 20
	listPageSize    = 500
)

// DefaultBackoff is the wait before each retry of a call that failed for a
// reason worth retrying (a rate limit, a 5xx, a dropped connection).
var DefaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// Config configures a Client.
type Config struct {
	// Project, Zone and Env are required. Env is the environment this
	// client serves (provider.ValidEnv).
	Project string
	Zone    string
	Env     string
	// Network is the VPC every instance joins, by name or as
	// "projects/<p>/global/networks/<n>". Required.
	Network string
	// Subnet is the subnetwork in Zone's region, by name or as
	// "projects/<p>/regions/<r>/subnetworks/<s>". Required for a
	// custom-mode network; empty lets the network pick.
	Subnet string
	// NetworkTier is the external address's tier: TierPremium (the
	// default) or TierStandard.
	NetworkTier string
	// DiskType is the boot disk type. Empty means DefaultDiskType.
	DiskType string
	// Sizes maps machine types back to CPUs, memory and architecture for
	// Instance values. Nil means provider.DefaultSizes().
	Sizes []provider.Size

	// Token supplies access tokens. Required; normally *Credentials.
	Token TokenSource

	// BaseURL is the API root, ending in "/". Empty means DefaultBaseURL.
	BaseURL string
	// HTTPClient makes the API calls. Nil means a client with no overall
	// timeout; each call has its own.
	HTTPClient *http.Client
	// PollInterval is how often Wait reads an instance whose state it is
	// following. Zero means one second.
	PollInterval time.Duration
	// CallTimeout bounds one ordinary API request. Zero means 60 s.
	CallTimeout time.Duration
	// Backoff is the schedule for retrying a failed call. Nil means
	// DefaultBackoff; an empty non-nil slice disables retries.
	Backoff []time.Duration
	// Logf receives one line per retry. Nil means log.Printf.
	Logf func(format string, args ...any)
	// NewRequestID returns the requestId that makes a mutating call safe to
	// retry. Nil means a random UUID.
	NewRequestID func() string
}

// Client is the GCE provider. Create one with New.
type Client struct {
	cfg        Config
	base       string
	region     string
	networkURL string
	subnetURL  string
	hc         *http.Client
	logf       func(string, ...any)
}

var (
	_ provider.Provider      = (*Client)(nil)
	_ provider.HostKeyReader = (*Client)(nil)
	_ provider.ImageResolver = (*Client)(nil)
)

// New checks cfg and returns a Client. It makes no API call.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.Project == "":
		return nil, errors.New("gce: project is required")
	case cfg.Zone == "":
		return nil, errors.New("gce: zone is required")
	case !provider.ValidEnv(cfg.Env):
		return nil, fmt.Errorf("gce: environment %q is not a valid name", cfg.Env)
	case cfg.Network == "":
		return nil, errors.New("gce: network is required")
	case cfg.Token == nil:
		return nil, errors.New("gce: a token source is required")
	}
	region, err := regionOf(cfg.Zone)
	if err != nil {
		return nil, err
	}
	switch cfg.NetworkTier {
	case "":
		cfg.NetworkTier = TierPremium
	case TierPremium, TierStandard:
	default:
		return nil, fmt.Errorf("gce: network tier %q is not %s or %s", cfg.NetworkTier, TierPremium, TierStandard)
	}
	if cfg.DiskType == "" {
		cfg.DiskType = DefaultDiskType
	}
	if cfg.Sizes == nil {
		cfg.Sizes = provider.DefaultSizes()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultCallTimeout
	}
	if cfg.Backoff == nil {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.NewRequestID == nil {
		cfg.NewRequestID = newUUID
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	c := &Client{cfg: cfg, base: base, region: region, hc: hc, logf: logf}
	c.networkURL = qualify(cfg.Network, "projects/"+cfg.Project+"/global/networks/")
	if cfg.Subnet != "" {
		c.subnetURL = qualify(cfg.Subnet, "projects/"+cfg.Project+"/regions/"+region+"/subnetworks/")
	}
	return c, nil
}

// qualify turns a bare resource name into a project path; a value that is
// already a path is kept.
func qualify(nameOrPath, prefix string) string {
	if strings.Contains(nameOrPath, "/") {
		return strings.TrimPrefix(nameOrPath, "/")
	}
	return prefix + nameOrPath
}

// regionOf returns a zone's region: "us-central1-a" is in "us-central1".
func regionOf(zone string) (string, error) {
	i := strings.LastIndex(zone, "-")
	if i <= 0 || i == len(zone)-1 {
		return "", fmt.Errorf("gce: zone %q has no region part", zone)
	}
	return zone[:i], nil
}

// Name implements provider.Provider.
func (c *Client) Name() string { return provider.NameGCE }

// Capabilities implements provider.Provider. GCE stops a VM at its
// maxRunDuration by itself, and an ephemeral address changes at every start.
func (c *Client) Capabilities() provider.Capabilities {
	return provider.Capabilities{EnforcesRunLimit: true, StableAddress: false}
}

// Env returns the environment the client serves.
func (c *Client) Env() string { return c.cfg.Env }

// Zone returns the zone the client creates instances in.
func (c *Client) Zone() string { return c.cfg.Zone }

// projectPath is "projects/<project>/".
func (c *Client) projectPath() string { return "projects/" + c.cfg.Project + "/" }

func (c *Client) zonePath(zone string) string {
	return c.projectPath() + "zones/" + zone + "/"
}

// apiError is a non-2xx reply from the API.
type apiError struct {
	HTTPStatus int
	Message    string
	Status     string
	Reasons    []string
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.HTTPStatus)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if len(e.Reasons) > 0 {
		msg += " (" + strings.Join(e.Reasons, ", ") + ")"
	}
	return msg
}

func (e *apiError) hasReason(reasons ...string) bool {
	for _, r := range e.Reasons {
		for _, want := range reasons {
			if r == want {
				return true
			}
		}
	}
	return false
}

func parseAPIError(status int, body []byte) *apiError {
	e := &apiError{HTTPStatus: status}
	var wire struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &wire) == nil && (wire.Error.Message != "" || len(wire.Error.Errors) > 0) {
		e.Message = wire.Error.Message
		e.Status = wire.Error.Status
		for _, x := range wire.Error.Errors {
			if x.Reason != "" {
				e.Reasons = append(e.Reasons, x.Reason)
			}
			if e.Message == "" {
				e.Message = x.Message
			}
		}
		return e
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	e.Message = text
	return e
}

// rateLimited reports a reply that asks the caller to slow down.
func (e *apiError) rateLimited() bool {
	return e.HTTPStatus == http.StatusTooManyRequests ||
		e.hasReason("rateLimitExceeded", "userRateLimitExceeded", "RATE_LIMIT_EXCEEDED")
}

// retryable reports whether the same request may succeed if sent again.
func (e *apiError) retryable() bool {
	if e.rateLimited() {
		return true
	}
	if classify(e.HTTPStatus, e.Reasons, e.Message) != nil {
		// Quota, capacity and busy are answers, not hiccups.
		return false
	}
	switch e.HTTPStatus {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// errFingerprint is the kind-less cause of a fingerprint conflict on
// setLabels or setMetadata; the caller reads again and retries.
var errFingerprint = errors.New("fingerprint conflict")

// classify maps a failure's status, reason codes and message to a provider
// error kind, or nil when it is none of them. It is shared by call replies and
// operation errors, which use different spellings of the same causes.
func classify(status int, codes []string, message string) error {
	has := func(subs ...string) bool {
		for _, c := range codes {
			for _, s := range subs {
				if strings.Contains(c, s) {
					return true
				}
			}
		}
		return false
	}
	lower := strings.ToLower(message)
	switch {
	case has("RESOURCE_POOL_EXHAUSTED", "resourcePoolExhausted", "STOCKOUT", "ZONE_RESOURCE_POOL"),
		strings.Contains(lower, "does not have enough resources available"):
		return provider.ErrCapacity
	case has("QUOTA_EXCEEDED", "quotaExceeded", "QUOTA_LIMIT"),
		strings.Contains(lower, "quota '") && strings.Contains(lower, "exceeded"):
		return provider.ErrQuota
	case has("resourceNotReady", "RESOURCE_NOT_READY", "RESOURCE_OPERATION_RATE_EXCEEDED", "resourceInUseByAnotherResource"):
		return provider.ErrBusy
	case status == http.StatusNotFound:
		return provider.ErrNotFound
	case status == http.StatusConflict && has("alreadyExists", "ALREADY_EXISTS", "RESOURCE_ALREADY_EXISTS"):
		return provider.ErrExists
	case status == http.StatusPreconditionFailed || has("conditionNotMet", "CONDITION_NOT_MET"),
		strings.Contains(lower, "fingerprint"):
		return errFingerprint
	}
	return nil
}

// asProviderError wraps a call failure in a *provider.Error of the right
// kind. A rate limit that outlasted the retries has no kind here: on a read
// or a poll it says nothing about the instance, and calling it a quota
// refusal would make Wait report an accepted create or start as refused.
// See asRefusal for the two calls where it is one.
func asProviderError(op string, err error) error {
	if err == nil {
		return nil
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return err
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return &provider.Error{Op: op, Err: err}
	}
	kind := classify(ae.HTTPStatus, ae.Reasons, ae.Message)
	if kind == provider.ErrQuota && ae.rateLimited() {
		// A rate limit whose message reads like a quota is still a rate
		// limit.
		kind = nil
	}
	if kind == errFingerprint {
		return &provider.Error{Op: op, Detail: ae.Error(), Err: errFingerprint}
	}
	return &provider.Error{Kind: kind, Op: op, Detail: ae.Error()}
}

// asRefusal is asProviderError for the calls that ask the cloud to run
// something (insert and start). There a rate limit that outlasted the retries
// is reported as ErrQuota: the request was not accepted, nothing was created
// or started, and the caller should answer "try later".
func asRefusal(op string, err error) error {
	perr := asProviderError(op, err)
	var ae *apiError
	var pe *provider.Error
	if errors.As(err, &ae) && ae.rateLimited() && errors.As(perr, &pe) && pe.Kind == nil && pe.Err == nil {
		pe.Kind = provider.ErrQuota
	}
	return perr
}

// request is one API call.
type request struct {
	op     string // provider call name, for errors
	method string
	path   string // relative to the base URL
	query  url.Values
	body   any
	// timeout overrides the call timeout (operation waits).
	timeout time.Duration
}

// call sends a request, retrying what is worth retrying, and decodes a 2xx
// reply into out. A 401 refreshes the token once. The returned error is an
// *apiError for an API refusal, or the transport error.
func (c *Client) call(ctx context.Context, r request, out any) error {
	var payload []byte
	if r.body != nil {
		var err error
		if payload, err = json.Marshal(r.body); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	refreshed := false
	attempt := 0
	for {
		err := c.once(ctx, r, payload, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ae *apiError
		isAPI := errors.As(err, &ae)
		if isAPI && ae.HTTPStatus == http.StatusUnauthorized && !refreshed {
			refreshed = true
			if inv, ok := c.cfg.Token.(invalidator); ok {
				inv.Invalidate()
			}
			continue
		}
		retry := (isAPI && ae.retryable()) || (!isAPI && !errors.Is(err, errToken))
		if !retry || attempt >= len(c.cfg.Backoff) {
			return err
		}
		wait := c.cfg.Backoff[attempt]
		attempt++
		c.logf("gce: retrying %s %s in %v (attempt %d/%d): %v", r.method, r.path, wait, attempt+1, len(c.cfg.Backoff)+1, err)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// errToken marks a failure to get a token; it is not retried here (the
// token source has its own deadline) and is reported as is.
var errToken = errors.New("no access token")

func (c *Client) once(ctx context.Context, r request, payload []byte, out any) error {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = c.cfg.CallTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tok, err := c.cfg.Token.Token(cctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errToken, err)
	}
	u := c.base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(cctx, r.method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() == nil && cctx.Err() != nil {
			return fmt.Errorf("%s %s: no reply within %v", r.method, r.path, timeout)
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read reply: %w", r.method, r.path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseAPIError(resp.StatusCode, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode reply: %w", r.method, r.path, err)
	}
	return nil
}

// newUUID returns a random RFC 4122 version 4 UUID, the form requestId takes.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
