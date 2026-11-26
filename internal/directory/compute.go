package directory

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// The compute contract's token (tests/compute-contract/v1, section
// "Conventions"): a separate token request that names compute:manage, cached
// apart from the pull's and the delivery's under the same rules, and minted
// ahead of use, so a create or a poll does not wait for a token request. The
// compute calls themselves are made by the remote backend's service provider
// (internal/remote/provider/service), over ComputeAccess.

const (
	// computeScope is the scope a compute token asks for and its reply
	// must carry.
	computeScope = "compute:manage"
	// computeMintAhead is how long before a compute token stops being
	// reused that a new one is asked for, in the background, while the one
	// held is still handed out.
	computeMintAhead = 2 * time.Minute
	// computeTokenTimeout bounds one compute token request.
	computeTokenTimeout = 12 * time.Second
)

// ErrComputeDenied is ComputeAccess's error when the service refuses this
// server compute at the token: the scope is not granted (400 invalid_scope),
// or the server's credentials are refused (invalid_client).
var ErrComputeDenied = errors.New("the account service does not let this server manage compute")

// ErrComputeNotEnrolled is ComputeAccess's error when the server holds no
// usable enrolment: nothing was sent.
var ErrComputeNotEnrolled = errors.New("this server is not enrolled with the account service")

// errComputeUnavailable is a token request that got no usable answer: no
// reply, a 429 or 5xx, or a 200 that is not a token for the scope.
var errComputeUnavailable = errors.New("the account service did not answer the compute token request")

// ComputeAccess is what one compute call needs: a compute:manage bearer and
// the client assertion it was obtained with (a reply that echoes either is
// refused), the server's enrolled name, the issuer the calls go to, and the
// HTTP client that only reaches the issuer and never follows a redirect.
type ComputeAccess struct {
	Bearer    string
	Assertion string
	Server    string
	Issuer    string
	HTTP      *http.Client
}

// computeFlight is one compute token request, shared by every caller that
// needs a token while it runs.
type computeFlight struct {
	done          chan struct{}
	tok, asserted string
	err           error
}

// ComputeAccess reads the enrolment as a pull does and returns what a
// compute call needs, with the compute token of that enrolment's client:
// the one held while it is usable (a new one is asked for in the
// background once it is within computeMintAhead of its end), otherwise the
// result of one token request shared with every caller that needs one now.
func (r *Runtime) ComputeAccess(ctx context.Context) (ComputeAccess, error) {
	d, e, es := r.openEnrolment()
	if d != nil {
		_ = d.Close()
	}
	if es.state != "" {
		return ComputeAccess{}, ErrComputeNotEnrolled
	}
	name := e.Name
	client, err := r.clientFor(e)
	if err != nil {
		clear(e.PrivateKey)
		return ComputeAccess{}, ErrComputeNotEnrolled
	}
	tok, asserted, err := client.computeToken(ctx)
	if err != nil {
		return ComputeAccess{}, err
	}
	return ComputeAccess{Bearer: tok, Assertion: asserted, Server: name, Issuer: client.issuer, HTTP: client.chc}, nil
}

// ForgetComputeToken drops a compute token a 401 refused, so the next call
// asks for a new one.
func (r *Runtime) ForgetComputeToken(tok string) {
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c != nil {
		c.forgetCompute(tok)
	}
}

// computeToken returns the compute token held while it is usable, and
// otherwise the result of one token request, this caller's or the one in
// flight.
func (c *Client) computeToken(ctx context.Context) (tok, asserted string, err error) {
	for {
		c.mu.Lock()
		now := wallClock(c.now)
		if c.ctoken != "" && now.Before(c.ctokenUntil) {
			tok, asserted = c.ctoken, c.cassertion
			if c.cflight == nil && !now.Before(c.ctokenUntil.Add(-computeMintAhead)) {
				// Minted ahead: the token held is still handed out while
				// its successor is asked for.
				f := &computeFlight{done: make(chan struct{})}
				c.cflight = f
				go c.leadCompute(context.Background(), f)
			}
			c.mu.Unlock()
			return tok, asserted, nil
		}
		f := c.cflight
		lead := f == nil
		if lead {
			f = &computeFlight{done: make(chan struct{})}
			c.cflight = f
		}
		c.mu.Unlock()
		if lead {
			// The request runs on its own bound, not the caller's: a caller
			// that gives up does not leave the others without a token.
			go c.leadCompute(context.Background(), f)
		}
		select {
		case <-f.done:
			if f.err != nil {
				return "", "", f.err
			}
			return f.tok, f.asserted, nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
}

// leadCompute runs one token request for f and publishes its result.
func (c *Client) leadCompute(ctx context.Context, f *computeFlight) {
	ctx, cancel := context.WithTimeout(ctx, computeTokenTimeout)
	defer cancel()
	tok, asserted, lifetime, local, err := c.requestComputeToken(ctx)
	c.mu.Lock()
	if err == nil {
		if reuse := lifetime - tokenEarly; reuse > 0 {
			c.ctoken, c.cassertion, c.ctokenUntil = tok, asserted, local.Add(reuse)
		}
	}
	c.cflight = nil
	c.mu.Unlock()
	f.tok, f.asserted, f.err = tok, asserted, err
	close(f.done)
}

// requestComputeToken sends one token request for compute:manage: a 200
// that holds the scope is the token; 400 invalid_scope and invalid_client
// (400 or 401) are ErrComputeDenied; anything else is unavailable. Nothing
// of the reply but the token is kept, and no error holds any of it.
func (c *Client) requestComputeToken(ctx context.Context) (tok, asserted string, lifetime time.Duration, local time.Time, err error) {
	assertion, err := c.assertion()
	if err != nil {
		return "", "", 0, time.Time{}, errComputeUnavailable
	}
	req, err := c.tokenRequest(ctx, computeScope, assertion)
	if err != nil {
		return "", "", 0, time.Time{}, errComputeUnavailable
	}
	resp, err := c.chc.Do(req)
	if err != nil {
		return "", "", 0, time.Time{}, errComputeUnavailable
	}
	defer resp.Body.Close()
	local = wallClock(c.now)
	switch code := resp.StatusCode; code {
	case http.StatusOK:
		if resp.ContentLength > MaxAnswerBody {
			return "", "", 0, time.Time{}, errComputeUnavailable
		}
		b, err := readLimited(resp.Body, MaxAnswerBody)
		if err != nil {
			return "", "", 0, time.Time{}, errComputeUnavailable
		}
		tok, lifetime, err := decodeToken(b, computeScope, true)
		clear(b)
		if err != nil {
			return "", "", 0, time.Time{}, errComputeUnavailable
		}
		return tok, assertion, lifetime, local, nil
	case http.StatusBadRequest, http.StatusUnauthorized:
		errCode, _, oauth := errorBodyShape(deliveryErrorBody(resp))
		if oauth && (errCode == "invalid_client" || (errCode == "invalid_scope" && code == http.StatusBadRequest)) {
			return "", "", 0, time.Time{}, ErrComputeDenied
		}
	}
	return "", "", 0, time.Time{}, errComputeUnavailable
}

// forgetCompute drops tok, if it is the compute token held.
func (c *Client) forgetCompute(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctoken == tok {
		c.ctoken, c.cassertion, c.ctokenUntil = "", "", time.Time{}
	}
}
