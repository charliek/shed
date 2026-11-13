package main

import (
	"fmt"
	"io"
	"time"

	"github.com/charliek/shed/internal/config"
)

// A remote server answers start and stop before the work is done: the shed
// comes back "starting" or "stopping" and settles later. The CLI polls until
// it settles, so `shed start` prints "Started" only once the shed runs.
// Firecracker and VZ answer with the final status, so nothing here polls for
// them.

var (
	// settleTimeout bounds the wait: a stop in flight (about 50 s on GCE)
	// then a start (about 40 s), with slack.
	settleTimeout = 180 * time.Second
	// settlePollInterval is the GetShed cadence while waiting.
	settlePollInterval = 2 * time.Second
)

// shedGetter is the slice of APIClient the wait needs.
type shedGetter interface {
	GetShed(name string) (*config.Shed, error)
}

// inTransition reports whether status is one a remote shed passes through.
func inTransition(status string) bool {
	return status == config.StatusStarting || status == config.StatusStopping
}

// waitSettled polls GetShed while shed is starting or stopping, writing a
// line to progress each time the status changes, and returns the settled
// shed. A shed already settled is returned as is, with no request.
func waitSettled(c shedGetter, shed *config.Shed, progress io.Writer) (*config.Shed, error) {
	if shed == nil || !inTransition(shed.Status) {
		return shed, nil
	}
	deadline := time.Now().Add(settleTimeout)
	last := ""
	for inTransition(shed.Status) {
		if shed.Status != last {
			fmt.Fprintf(progress, "Waiting for shed %s (%s)...\n", shed.Name, shed.Status)
			last = shed.Status
		}
		if time.Now().After(deadline) {
			return shed, fmt.Errorf("shed %s is still %s after %s; check `shed list -v` later", shed.Name, shed.Status, settleTimeout)
		}
		time.Sleep(settlePollInterval)
		next, err := c.GetShed(shed.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get shed status: %w", err)
		}
		shed = next
	}
	return shed, nil
}

// statusWithReason renders "stopped (capacity)" when the shed has a reason.
func statusWithReason(s config.Shed) string {
	if s.StatusReason == "" {
		return s.Status
	}
	return s.Status + " (" + s.StatusReason + ")"
}

// requireSettledStatus returns an error naming the shed's status and reason
// when it settled anywhere but want.
func requireSettledStatus(shed *config.Shed, want string) error {
	if shed.Status == want {
		return nil
	}
	return fmt.Errorf("shed %s is %s, not %s", shed.Name, statusWithReason(*shed), want)
}

// remoteCreateRefusal refuses, before anything is sent, the create flags a
// remote server cannot honour (it would answer 422 FEATURE_UNSUPPORTED).
// --egress off and --mounts none ask for nothing and pass.
func remoteCreateRefusal(localDir string, addDirs []string, fromSnapshot string, egress []string, mounts *[]string) error {
	flag := ""
	switch {
	case localDir != "":
		flag = "--local-dir"
	case len(addDirs) > 0:
		flag = "--add-dir"
	case fromSnapshot != "":
		flag = "--from-snapshot"
	case asksForEgress(egress):
		flag = "--egress"
	case mounts != nil && len(*mounts) > 0:
		flag = "--mounts"
	default:
		return nil
	}
	return fmt.Errorf("%s is not supported on a remote server (cloud sheds have no host directories, snapshots, egress profiles or server mounts)", flag)
}

// asksForEgress reports whether an egress selection asks for any profile:
// nothing and exactly ["off"] ask for none.
func asksForEgress(egress []string) bool {
	return len(egress) > 1 || (len(egress) == 1 && egress[0] != "off")
}
