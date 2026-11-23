package config

import (
	"path"
	"strings"
	"time"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/provider"
)

// ShedDelivery is a shed's delivery as the shed document carries it: the
// shed's record (the provision contract's record shape: state, steps, last
// delivery, items) and what the document reads from it, the names of the
// items that have expired and the steps that are owed. It holds no value.
//
// A caller who may not enter the shed (an admin looking at another user's
// shed) gets the state alone: steps, the last delivery, items, targets and
// problems are withheld (backend.Guard), as the repository and the landing
// directory are.
type ShedDelivery struct {
	delivery.Record
	// Expired is the names of the items in the shed whose expires_at has
	// passed. `shed start` warns once for each.
	Expired []string `json:"expired,omitempty"`
	// Owed is the steps that are owed, in the order they run. `shed
	// provision` finishes them.
	Owed []string `json:"owed,omitempty"`
	// Withheld says the rest of the record was withheld from this caller,
	// who may not enter the shed: the state alone is shown, and the caller
	// may not provision it. A part of the shed document only, never of the
	// contract's record.
	Withheld bool `json:"withheld,omitempty"`
}

// NewShedDelivery is rec as the shed document carries it at now. A nil rec
// is a shed with no record (created before delivery existed): {"state":
// "none"}.
func NewShedDelivery(rec *delivery.Record, now time.Time) *ShedDelivery {
	return &ShedDelivery{
		Record:  delivery.Reported(rec),
		Expired: delivery.ExpiredItems(rec, now),
		Owed:    delivery.OwedSteps(rec),
	}
}

// StateOnly is d with everything but its state withheld, and marked so.
func (d *ShedDelivery) StateOnly() *ShedDelivery {
	if d == nil {
		return nil
	}
	return &ShedDelivery{Record: delivery.Record{State: d.State}, Withheld: true}
}

// DeliveryRequestShed is the shed a delivery request names: its name,
// permanent id and backend, and when it has them its repository (the URL
// with any userinfo removed, and the owner/name it parses to; a URL over the
// contract's bound is left out), its
// --local-dir (the host directory's base name) and its project directory.
func DeliveryRequestShed(name, id, backend, repo, localDir, projectDir string) delivery.RequestShed {
	s := delivery.RequestShed{Name: name, ID: id, Backend: backend}
	if url := provider.StripRepoUserinfo(repo); url != "" && len(url) <= delivery.MaxURLBytes {
		s.Repo = &delivery.RequestRepo{URL: url}
		if n := repoOwnerName(url); len(n) <= delivery.MaxRepoNameBytes {
			s.Repo.Name = n
		}
	}
	if localDir != "" {
		s.LocalDir = path.Base(strings.TrimRight(localDir, "/"))
	}
	if projectDir != "" && projectDir != HomePath {
		s.ProjectDir = projectDir
	}
	return s
}

// repoOwnerName is the owner/name a repository URL parses to: its last two
// path segments, the second without ".git", or "" when the URL has not
// exactly two (a repository nested in groups, or no path at all).
func repoOwnerName(url string) string {
	rest := url
	if _, after, ok := strings.Cut(url, "://"); ok {
		_, p, ok := strings.Cut(after, "/")
		if !ok {
			return ""
		}
		rest = p
	} else if i := strings.Index(url, ":"); i >= 0 {
		rest = url[i+1:]
	}
	rest = strings.TrimSuffix(strings.Trim(rest, "/"), ".git")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(rest, "?#@") {
		return ""
	}
	return rest
}
