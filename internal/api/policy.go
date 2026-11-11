package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// clientCreateRefusal applies a service client's create policy to req and
// returns the rule it breaks ("" when it breaks none). It runs before any
// other create validation touches the host (so a refused client learns
// nothing about the server's filesystem) and before the SSE branch. The
// max_sheds rule is separate (reserveClientCreate), because it must count
// creates already in flight.
func (s *Server) clientCreateRefusal(c Caller, req *config.CreateShedRequest) string {
	p := c.Client.Policy
	switch {
	case req.LocalDir != "" || len(req.AddDirs) > 0:
		return "local_dir and add_dirs are not allowed"
	case req.FromSnapshot != "":
		return "from_snapshot is not allowed"
	case req.Image != "" && !slices.Contains(p.Images, req.Image):
		if len(p.Images) == 0 {
			return fmt.Sprintf("image %q is not allowed (only the server default image)", req.Image)
		}
		return fmt.Sprintf("image %q is not allowed (allowed: %s)", req.Image, strings.Join(p.Images, ", "))
	case !strings.HasPrefix(req.Name, p.NamePrefix):
		return fmt.Sprintf("shed name %q must start with %q", req.Name, p.NamePrefix)
	}
	defCPUs, defMem := s.cfg.ActiveDefaultResources()
	cpus, mem := req.CPUs, req.MemoryMB
	if cpus == 0 {
		cpus = defCPUs
	}
	if mem == 0 {
		mem = defMem
	}
	if p.Caps.CPUs > 0 && cpus > p.Caps.CPUs {
		return fmt.Sprintf("cpus %d is over the cap of %d", cpus, p.Caps.CPUs)
	}
	if p.Caps.MemoryMB > 0 && mem > p.Caps.MemoryMB {
		return fmt.Sprintf("memory_mb %d is over the cap of %d", mem, p.Caps.MemoryMB)
	}
	if req.Mounts != nil {
		for _, m := range *req.Mounts {
			if !slices.Contains(p.ServerMounts, m) {
				return fmt.Sprintf("mount %q is not in the client's server_mounts", m)
			}
		}
	}
	if req.Network != "" && req.Network != p.NetworkProfile() {
		return fmt.Sprintf("network %q is not allowed (the client's sheds are %s)", req.Network, p.NetworkProfile())
	}
	return ""
}

// resolveNetworkProfile returns the network profile a create gets. Admin:
// the request's (a known profile, else 400 INVALID_NETWORK_PROFILE), or the
// server's network.default_profile when omitted. Service client: its pinned
// policy.network (a different explicit value was already refused by
// clientCreateRefusal). A non-open profile on a backend that cannot enforce it
// (VZ) is 422 NETWORK_PROFILE_UNSUPPORTED. ok=false means a refusal was
// written.
func (s *Server) resolveNetworkProfile(w http.ResponseWriter, c Caller, req *config.CreateShedRequest) (string, bool) {
	var profile string
	switch {
	case c.IsClient():
		profile = c.Client.Policy.NetworkProfile()
	case req.Network == "":
		profile = s.cfg.NetworkDefaultProfile()
	case config.ValidNetworkProfile(req.Network):
		profile = req.Network
	default:
		writeError(w, http.StatusBadRequest, config.ErrInvalidNetworkProfile,
			fmt.Sprintf("network: %q is not a network profile (use %s or %s)", req.Network, config.NetworkProfileOpen, config.NetworkProfileIsolated))
		return "", false
	}
	if profile != config.NetworkProfileOpen && s.backend.Type() != backend.TypeFirecracker {
		writeError(w, http.StatusUnprocessableEntity, config.ErrNetworkProfileUnsupported,
			fmt.Sprintf("network profile %s is not supported by the %s backend (only %s)", profile, s.backend.Type(), config.NetworkProfileOpen))
		return "", false
	}
	return profile, true
}

// resolveMountSelection turns a create request's mounts field into the
// selection the shed stores. Admin: omitted is every server mount, a list must
// name configured mounts (else 400) and is exactly those ([] = none). Service
// client: omitted is its policy's server_mounts; a list was already checked
// against them by clientCreateRefusal. ok=false means a refusal was written.
func (s *Server) resolveMountSelection(w http.ResponseWriter, c Caller, req *config.CreateShedRequest) (*config.MountSelection, bool) {
	if c.IsClient() {
		if req.Mounts == nil {
			return config.SelectedMounts(c.Client.Policy.ServerMounts), true
		}
		return config.SelectedMounts(*req.Mounts), true
	}
	if req.Mounts == nil {
		return config.AllMounts(), true
	}
	for _, m := range *req.Mounts {
		if _, ok := s.cfg.Mounts[m]; !ok {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, fmt.Sprintf("mounts: %q is not a configured server mount", m))
			return nil, false
		}
	}
	return config.SelectedMounts(*req.Mounts), true
}

// reserveClientCreate counts a service client's sheds against
// policy.max_sheds and reserves a slot for this create of shedName. The count
// is the union, by name, of the sheds the client owns and its creates in
// flight: a backend persists an owned shed's metadata before it boots, so a
// create still in flight is usually listed as owned too and must not count
// twice. On success the caller must call release when the create returns.
// refusal is the policy rule when the client is at its limit; err is a failed
// shed listing.
func (s *Server) reserveClientCreate(ctx context.Context, c Caller, shedName string) (release func(), refusal string, err error) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	sheds, err := s.backend.ListSheds(ctx)
	if err != nil {
		return nil, "", err
	}
	names := map[string]bool{}
	for i := range sheds {
		if sheds[i].Owner == c.Name {
			names[sheds[i].Name] = true
		}
	}
	owned := len(names)
	inFlight := s.clientCreates[c.Name]
	for n := range inFlight {
		names[n] = true
	}
	limit := c.Client.Policy.MaxSheds
	if !names[shedName] && len(names) >= limit {
		return nil, fmt.Sprintf("max_sheds %d reached (%d owned, %d being created)", limit, owned, len(inFlight)), nil
	}
	if s.clientCreates == nil {
		s.clientCreates = map[string]map[string]int{}
	}
	if inFlight == nil {
		inFlight = map[string]int{}
		s.clientCreates[c.Name] = inFlight
	}
	inFlight[shedName]++
	return func() {
		s.clientMu.Lock()
		defer s.clientMu.Unlock()
		inFlight[shedName]--
		if inFlight[shedName] <= 0 {
			delete(inFlight, shedName)
		}
		if len(inFlight) == 0 {
			// No release of this map is outstanding, so a later reservation
			// starts a fresh one.
			delete(s.clientCreates, c.Name)
		}
	}, "", nil
}

// writePolicyViolation writes a 403 POLICY_VIOLATION naming the client and
// the rule.
func writePolicyViolation(w http.ResponseWriter, c Caller, rule string) {
	writeError(w, http.StatusForbidden, config.ErrPolicyViolation, fmt.Sprintf("client %s: %s", c.Name, rule))
}
