package gce

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// operation is a zone operation as the API reports it.
type operation struct {
	Name                string `json:"name"`
	Zone                string `json:"zone"`
	OperationType       string `json:"operationType"`
	Status              string `json:"status"` // PENDING, RUNNING, DONE
	TargetLink          string `json:"targetLink"`
	HTTPErrorStatusCode int    `json:"httpErrorStatusCode"`
	HTTPErrorMessage    string `json:"httpErrorMessage"`
	Error               *struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

const opDone = "DONE"

// failure returns the operation's error as a *provider.Error, or nil when it
// succeeded (or has not finished).
func (o *operation) failure(op string) error {
	if o.Status != opDone || o.Error == nil || len(o.Error.Errors) == 0 {
		return nil
	}
	codes := make([]string, 0, len(o.Error.Errors))
	msgs := make([]string, 0, len(o.Error.Errors))
	for _, e := range o.Error.Errors {
		codes = append(codes, e.Code)
		if e.Message != "" {
			msgs = append(msgs, e.Message)
		}
	}
	msg := strings.Join(msgs, "; ")
	kind := classify(o.HTTPErrorStatusCode, codes, msg)
	if kind == errFingerprint {
		return &provider.Error{Op: op, Detail: strings.Join(codes, ",") + ": " + msg, Err: errFingerprint}
	}
	return &provider.Error{Kind: kind, Op: op, Detail: strings.Join(codes, ",") + ": " + msg}
}

// mutate sends a call that starts a zone operation and returns the operation.
// A requestId makes the call safe to retry: the API answers a repeat with the
// first operation. An operation that is already DONE with an error is
// returned as that error, so a refusal on the call and one on the operation
// look alike to the caller.
func (c *Client) mutate(ctx context.Context, op, method, path string, query url.Values, body any) (*operation, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("requestId", c.cfg.NewRequestID())
	var o operation
	if err := c.call(ctx, request{op: op, method: method, path: path, query: q, body: body}, &o); err != nil {
		if op == "create" || op == "start" {
			return nil, asRefusal(op, err)
		}
		return nil, asProviderError(op, err)
	}
	if err := o.failure(op); err != nil {
		return nil, err
	}
	return &o, nil
}

// errOpUnknown: the API no longer knows the operation (or never did).
var errOpUnknown = errors.New("operation unknown")

// waitOperation blocks until the zone operation is DONE and returns it. It
// uses zoneOperations.wait, which holds the request open for up to two
// minutes, and asks again until the operation is done or ctx ends. The
// operation's own failure is not checked here.
func (c *Client) waitOperation(ctx context.Context, op, zone, name string) (*operation, error) {
	path := c.zonePath(zone) + "operations/" + url.PathEscape(name) + "/wait"
	for {
		started := time.Now()
		var o operation
		err := c.call(ctx, request{op: op, method: http.MethodPost, path: path, timeout: waitCallTimeout}, &o)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && ae.HTTPStatus == http.StatusNotFound {
				return nil, errOpUnknown
			}
			return nil, asProviderError(op, err)
		}
		if o.Status == opDone {
			return &o, nil
		}
		// The wait call returned early. Do not spin if it did so at once.
		if elapsed := time.Since(started); elapsed < c.cfg.PollInterval {
			if err := sleep(ctx, c.cfg.PollInterval-elapsed); err != nil {
				return nil, &provider.Error{Op: op, Err: err}
			}
		}
	}
}

// runOperation waits for an operation the client itself started and must see
// finish before it returns (setLabels, setMetadata, setScheduling, a disk
// delete), and returns its failure.
func (c *Client) runOperation(ctx context.Context, op string, o *operation) error {
	if o.Status == opDone {
		return o.failure(op)
	}
	zone := lastSegment(o.Zone)
	if zone == "" {
		zone = c.cfg.Zone
	}
	done, err := c.waitOperation(ctx, op, zone, o.Name)
	if errors.Is(err, errOpUnknown) {
		return provider.Errorf(nil, op, "operation %s vanished before it finished", o.Name)
	}
	if err != nil {
		return err
	}
	return done.failure(op)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// lastSegment returns what follows the last "/" of a resource URL.
func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
