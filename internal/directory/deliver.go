package directory

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/delivery"
)

// The hosted source's client (the provision contract, version 2, section
// 3b): a separate delivery token (scope deliveries:request), cached apart
// from the pull's under the same rules, and one POST /v1/deliveries per
// delivery, on a second HTTP client that waits for the reply as long as the
// caller's context lets it (the request's deadline). It reuses the
// enrolment, its key and the issuer-only rules of the pull.
//
// Every reply is classified into a delivery.HostedStatus and dropped: the
// service's error codes are compared and never kept, its messages, error
// descriptions, redirect targets and unparseable bodies are never read into
// anything that is logged, recorded or returned. Only a 200's body (the
// answer, judged by the caller) leaves this file.

const (
	// deliveriesScope is the scope a delivery token asks for and its reply
	// must carry.
	deliveriesScope = "deliveries:request"
	// MaxDeliveryRequest is the largest request document the call sends.
	MaxDeliveryRequest = 64 << 10
	// deliveryConnectTimeout bounds the connect of a delivery's requests:
	// a service whose port is closed or unreachable is source_offline
	// within it, whatever the deadline.
	deliveryConnectTimeout = 5 * time.Second
	// NotDeliveringFor is how long a refusal of deliveries (invalid_scope
	// on the token request, INSUFFICIENT_SCOPE on the call) is remembered:
	// a directory user's delivery is then not applicable with no request.
	NotDeliveringFor = 5 * time.Minute
)

// deliveryFlight is one delivery token request, shared by every delivery
// that needs a token while it runs.
type deliveryFlight struct {
	done          chan struct{}
	tok, asserted string
	status        delivery.HostedStatus
	ok            bool
	notDelivering bool
	// abandoned: the request ended because its caller's context did; a
	// waiter whose own context lives asks again.
	abandoned bool
}

// Deliver makes one hosted delivery: a delivery token (cached, or one token
// request shared with any other delivery that needs one now), then the call,
// once, and once more after a 401 with a new token. It returns how it ended,
// the 200's body for HostedAnswer, and notDelivering when the service
// refused this server deliveries (invalid_scope, INSUFFICIENT_SCOPE). ctx
// bounds all of it; its end aborts the request in flight.
func (c *Client) Deliver(ctx context.Context, request []byte) (status delivery.HostedStatus, body []byte, notDelivering bool) {
	if len(request) > MaxDeliveryRequest {
		return delivery.HostedRefused, nil, false
	}
	tok, asserted, st, ok, nd := c.deliveryToken(ctx)
	if !ok {
		return st, nil, nd
	}
	sent := []string{tok, asserted}
	st, body, again, nd := c.callDeliveries(ctx, tok, sent, request)
	if !again {
		return st, body, nd
	}
	// A 401: the token is forgotten, a new one asked for, the call sent
	// once more.
	c.forgetDelivery(tok)
	tok, asserted, st, ok, nd = c.deliveryToken(ctx)
	if !ok {
		return st, nil, nd
	}
	sent = append(sent, tok, asserted)
	st, body, again, nd = c.callDeliveries(ctx, tok, sent, request)
	if again {
		c.forgetDelivery(tok)
		return delivery.HostedRefused, nil, false
	}
	return st, body, nd
}

// deliveryToken returns the delivery token in memory while it is usable,
// and otherwise the result of one token request: this caller's, or the one
// in flight, which it waits for. ok is false with the token request's
// classification.
func (c *Client) deliveryToken(ctx context.Context) (tok, asserted string, st delivery.HostedStatus, ok, notDelivering bool) {
	for {
		c.mu.Lock()
		if c.dtoken != "" && wallClock(c.now).Before(c.dtokenUntil) {
			tok, asserted = c.dtoken, c.dassertion
			c.mu.Unlock()
			return tok, asserted, delivery.HostedAnswer, true, false
		}
		f := c.dflight
		lead := f == nil
		if lead {
			f = &deliveryFlight{done: make(chan struct{})}
			c.dflight = f
		}
		c.mu.Unlock()
		if lead {
			c.requestDeliveryToken(ctx, f)
			c.mu.Lock()
			c.dflight = nil
			c.mu.Unlock()
			close(f.done)
			return f.tok, f.asserted, f.status, f.ok, f.notDelivering
		}
		select {
		case <-f.done:
			if f.abandoned && ctx.Err() == nil {
				continue
			}
			return f.tok, f.asserted, f.status, f.ok, f.notDelivering
		case <-ctx.Done():
			return "", "", delivery.HostedOffline, false, false
		}
	}
}

// requestDeliveryToken sends one token request for deliveries:request and
// classifies its reply into f (section 3b's token table): a 200 that holds
// the scope is the token (cached until tokenEarly before its end); 400
// invalid_scope is not applicable (the service does not deliver to this
// server); 400 or 401 invalid_client is refused; 429, 5xx and no answer are
// offline; anything else, a 200 without the scope included, is refused.
func (c *Client) requestDeliveryToken(ctx context.Context, f *deliveryFlight) {
	defer func() { f.abandoned = !f.ok && ctx.Err() != nil }()
	f.status = delivery.HostedRefused
	assertion, err := c.assertion()
	if err != nil {
		f.status = delivery.HostedOffline
		return
	}
	req, err := c.tokenRequest(ctx, deliveriesScope, assertion)
	if err != nil {
		return
	}
	resp, err := c.dhc.Do(req)
	if err != nil {
		f.status = delivery.HostedOffline
		return
	}
	defer resp.Body.Close()
	local := wallClock(c.now)
	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		if resp.ContentLength > MaxAnswerBody {
			return
		}
		b, err := readLimited(resp.Body, MaxAnswerBody)
		if err != nil {
			if !errors.Is(err, errTooLarge) {
				f.status = delivery.HostedOffline
			}
			return
		}
		tok, lifetime, err := decodeToken(b, deliveriesScope, true)
		clear(b)
		if err != nil {
			return
		}
		f.tok, f.asserted, f.ok = tok, assertion, true
		if reuse := lifetime - tokenEarly; reuse > 0 {
			c.mu.Lock()
			c.dtoken, c.dassertion, c.dtokenUntil = tok, assertion, local.Add(reuse)
			c.mu.Unlock()
		}
	case code == http.StatusTooManyRequests || code >= 500:
		f.status = delivery.HostedOffline
	case code == http.StatusBadRequest || code == http.StatusUnauthorized:
		errCode, _, oauth := errorBodyShape(deliveryErrorBody(resp))
		if oauth && errCode == "invalid_scope" && code == http.StatusBadRequest {
			f.status, f.notDelivering = delivery.HostedNotApplicable, true
		}
	}
}

// callDeliveries sends the call with tok and classifies its reply (section
// 3b's call table). again is a 401: the caller signs in once more and sends
// it again, once. sent are the credentials this delivery presented, which a
// 200's body must not hold.
func (c *Client) callDeliveries(ctx context.Context, tok string, sent []string, request []byte) (st delivery.HostedStatus, body []byte, again, notDelivering bool) {
	req, err := newRequest(ctx, http.MethodPost, c.issuer+"/v1/deliveries", bytes.NewReader(request))
	if err != nil {
		return delivery.HostedRefused, nil, false, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.dhc.Do(req)
	if err != nil {
		return delivery.HostedOffline, nil, false, false
	}
	defer resp.Body.Close()
	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		if resp.ContentLength > delivery.MaxBodyBytes {
			return delivery.HostedTooLarge, nil, false, false
		}
		b, err := readLimited(resp.Body, delivery.MaxBodyBytes)
		switch {
		case errors.Is(err, errTooLarge):
			return delivery.HostedTooLarge, nil, false, false
		case err != nil:
			return delivery.HostedDisconnected, nil, false, false
		}
		// A body that is not JSON is the answer rules' to refuse (not_json);
		// its bytes were searched for the credentials as they stand.
		if echoed, _ := echoes(b, sent); echoed || echoesDecoded(b, request, sent) {
			clear(b)
			return delivery.HostedEchoed, nil, false, false
		}
		return delivery.HostedAnswer, b, false, false
	case code == http.StatusUnauthorized:
		return delivery.HostedRefused, nil, true, false
	case code >= 300 && code < 400:
		return delivery.HostedRefused, nil, false, false
	case code == http.StatusTooManyRequests || code >= 500:
		return delivery.HostedOffline, nil, false, false
	}
	errCode := v1Code(deliveryErrorBody(resp))
	switch {
	case resp.StatusCode == http.StatusNotFound && errCode == "NO_SOURCE":
		return delivery.HostedNotApplicable, nil, false, false
	case resp.StatusCode == http.StatusGone && (errCode == "DEADLINE_PASSED" || errCode == ""):
		// 410 DEADLINE_PASSED; a body that is not an error object (none,
		// or one that does not parse) counts as the status alone, which is
		// that row. A 410 whose code is another is not listed: refused.
		return delivery.HostedTimeout, nil, false, false
	case resp.StatusCode == http.StatusForbidden && errCode == "INSUFFICIENT_SCOPE":
		// The grant was removed after the token was issued.
		c.forgetDelivery(tok)
		return delivery.HostedNotApplicable, nil, false, true
	}
	return delivery.HostedRefused, nil, false, false
}

// echoesDecoded reports whether body, once judged an answer to request
// (the request this call sent), holds one of sent in what the answer rules
// decode: a file item's content (base64 in the body, so no search of its
// bytes finds it) or an env item's value. A body the rules refuse is
// theirs to refuse, and is reported false here.
func echoesDecoded(body, request []byte, sent []string) bool {
	req, refusal := delivery.ParseRequest(request)
	if refusal != nil {
		return false
	}
	a, refusal := delivery.ParseAnswer(body, req)
	if refusal != nil {
		return false
	}
	for _, it := range a.Items {
		for _, s := range sent {
			if s == "" {
				continue
			}
			if bytes.Contains(it.Content, []byte(s)) || strings.Contains(it.Value, s) {
				return true
			}
		}
	}
	return false
}

// deliveryErrorBody reads at most MaxErrorBody of an error reply's body.
func deliveryErrorBody(resp *http.Response) []byte {
	b, _ := readLimited(resp.Body, MaxErrorBody)
	return b
}

// v1Code is a /v1 error body's code, "" for a body that is not one. Only
// the code is read; the message is never kept.
func v1Code(body []byte) string {
	code, _, oauth := errorBodyShape(body)
	if oauth {
		return ""
	}
	return code
}

// forgetDelivery drops tok from memory, if it is the delivery token held.
func (c *Client) forgetDelivery(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dtoken == tok {
		c.dtoken, c.dassertion, c.dtokenUntil = "", "", time.Time{}
	}
}

// notDelivering is the runtime's memory of a refusal of deliveries, keyed
// to the issuer and the enrolment's client id it was had under.
type notDelivering struct {
	issuer, clientID string
	// until is on the elapsed clock (RuntimeOptions.Elapsed).
	until time.Time
}

// deliverMemory guards the memory; it is the Runtime's (one per server).
type deliverMemory struct {
	mu  sync.Mutex
	got notDelivering
	// current is the client id of the enrolment the newest delivery read.
	current string
}

var _ delivery.Hosted = (*Runtime)(nil)

// Deliver is the hosted source's one delivery (delivery.Hosted): it reads
// the enrolment as a pull does, and asks with the client of that enrolment.
// It does not wait for a pull in progress. A server that is not enrolled, or
// whose enrolment cannot be used, is offline (the service cannot be
// reached); one the service has refused deliveries within the last
// NotDeliveringFor is not applicable, with no request.
func (r *Runtime) Deliver(ctx context.Context, request []byte) (delivery.HostedStatus, []byte) {
	d, e, es := r.openEnrolment()
	if d != nil {
		_ = d.Close()
	}
	if es.state != "" {
		r.logf("directory: a delivery was not asked for: this server is %s", es.describe())
		return delivery.HostedOffline, nil
	}
	r.seeEnrolment(es.clientID)
	if r.notDeliveringFor(es.clientID) {
		clear(e.PrivateKey)
		return delivery.HostedNotApplicable, nil
	}
	client, err := r.clientFor(e)
	if err != nil {
		clear(e.PrivateKey)
		r.logf("directory: a delivery was not asked for: the enrolment cannot be used: %v", err)
		return delivery.HostedOffline, nil
	}
	st, body, nd := client.Deliver(ctx, request)
	if nd {
		r.rememberNotDelivering(es.clientID)
	}
	return st, body
}

// NotDelivering reports whether the service refused this server deliveries
// within the last NotDeliveringFor, under the enrolment the server holds now
// (delivery.Hosted): the enrolment is read, and nothing is sent. It never
// changes the memory: only a re-enrolment (seen by the next delivery) and
// `shed users refresh` clear it.
func (r *Runtime) NotDelivering() bool {
	d, e, es := r.openEnrolment()
	if d != nil {
		_ = d.Close()
	}
	if es.state != "" {
		return false
	}
	clear(e.PrivateKey)
	return r.notDeliveringFor(es.clientID)
}

// notDeliveringFor reports, reading only, whether the memory holds for
// clientID under the configured issuer and has not run out.
func (r *Runtime) notDeliveringFor(clientID string) bool {
	r.deliver.mu.Lock()
	defer r.deliver.mu.Unlock()
	m := r.deliver.got
	return !m.until.IsZero() && m.issuer == r.issuer && m.clientID == clientID && r.elapsed().Before(m.until)
}

// seeEnrolment notes clientID as the enrolment the newest delivery read: an
// enrolment other than the one the memory was had under (the server has
// enrolled again) clears the memory.
func (r *Runtime) seeEnrolment(clientID string) {
	r.deliver.mu.Lock()
	defer r.deliver.mu.Unlock()
	r.deliver.current = clientID
	if m := r.deliver.got; !m.until.IsZero() && (m.issuer != r.issuer || m.clientID != clientID) {
		r.deliver.got = notDelivering{}
	}
}

// rememberNotDelivering starts the memory for clientID, when that is still
// the enrolment the newest delivery read: a refusal that lands late, from a
// delivery made under an enrolment the server has since replaced, is
// dropped (it must not replace the current enrolment's memory).
func (r *Runtime) rememberNotDelivering(clientID string) {
	r.deliver.mu.Lock()
	if r.deliver.current != clientID {
		r.deliver.mu.Unlock()
		return
	}
	r.deliver.got = notDelivering{issuer: r.issuer, clientID: clientID, until: r.elapsed().Add(NotDeliveringFor)}
	r.deliver.mu.Unlock()
	r.logf("directory: the account service does not deliver credentials to this server; directory users' deliveries are not applicable, and it is not asked again for %s (or until 'shed users refresh')", NotDeliveringFor)
}

// clearNotDelivering forgets the memory (`shed users refresh`).
func (r *Runtime) clearNotDelivering() {
	r.deliver.mu.Lock()
	r.deliver.got = notDelivering{}
	r.deliver.mu.Unlock()
}
