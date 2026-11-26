package refserver

import (
	"encoding/json"
	"time"
)

// The compute service's hooks for tests in other packages: what an
// exchange's set-up steps and the command's control file do, as calls.

// ComputeStep applies one of the compute contract's set-up steps, by its
// name and value as an exchange's step holds them (compute.allow,
// cloud.settle, clock.advance, …). clock.advance moves the compute
// service's own clock (the directory's is the server's Now) and runs the
// lease sweep.
func (s *Server) ComputeStep(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.applyComputeStep(name, raw, s.compute.advance)
}

// advance moves the compute service's clock by d.
func (c *computeState) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// setSettingsLocked sets what compute.json's settings set (see
// ComputeSettings). c.mu is held.
func (c *computeState) setSettingsLocked(autoSettle time.Duration, hostKey string, access *ComputeAccess) {
	c.autoSettle = autoSettle
	c.hostKeyDefault = hostKey
	if access != nil {
		c.world.Access = *access
	} else {
		c.world.Access = DefaultComputeWorld().Access
	}
}

// ComputeSettings sets what compute.json's settings set: every operation
// settles autoSettle after it starts (zero: only a cloud.settle step settles
// one); hostKey is published for every machine when it first runs, unless a
// guest.publish_key step published another first; access is where running
// machines are reached (nil: the world's, 127.0.0.1 port 22).
func (s *Server) ComputeSettings(autoSettle time.Duration, hostKey string, access *ComputeAccess) {
	c := s.compute
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setSettingsLocked(autoSettle, hostKey, access)
}

// ComputeForceReply makes the service send rep to every request of a call
// (a name of the contract's calls: catalog, create, …) until it is cleared
// with a nil rep.
func (s *Server) ComputeForceReply(call string, rep *ComputeReply) {
	c := s.compute
	c.mu.Lock()
	defer c.mu.Unlock()
	if rep == nil {
		delete(c.replies, call)
		return
	}
	c.replies[call] = *rep
}
