//go:build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/backend/orchestrator"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/vmutil"
)

// Delivery on a VZ shed: its request, its hooks, and `shed
// provision` (orchestrator.ProvisionShed over this backend).

// requestShed is meta's shed as a delivery request names it.
func requestShed(meta *Metadata) delivery.RequestShed {
	return config.DeliveryRequestShed(meta.Name, meta.ID, delivery.BackendVZ, meta.Repo, localDirOf(meta.ProjectMounts, meta.LandingDir), meta.LandingDir)
}

// localDirOf is the --local-dir host directory of a shed's mounts: the one
// mounted at the landing directory ("" for none).
func localDirOf(mounts []config.MountConfig, landing string) string {
	for _, m := range mounts {
		if m.Target == landing {
			return m.Source
		}
	}
	return ""
}

// cloneEnvFor is the egress proxy environment a clone into meta's shed runs
// with, rebuilt from its persisted assignment, as ConfigureEgressProxy hands
// it to the create's clone. Nil when egress is off for the shed (or on this
// server).
func (c *Client) cloneEnvFor(meta *Metadata) []string {
	if c.egressMgr == nil || meta.EgressPort == 0 {
		return nil
	}
	gateway, subnet := c.egressGatewaySubnet()
	return vmutil.EgressCloneEnv(gateway, subnet, meta.EgressPort, meta.EgressToken)
}

// provisionerFor runs meta's hooks in its landing directory.
func (c *Client) provisionerFor(meta *Metadata) *vmutil.Provisioner {
	p := vmutil.NewProvisioner(c.newAgentClient(meta.Name), meta.Name)
	p.SetWorkDir(meta.LandingDir)
	p.SetAddDirs(config.ProjectAddDirTargets(meta.ProjectMounts, meta.LandingDir))
	p.SetOutput(os.Stdout, os.Stderr)
	return p
}

// BeginProvision admits `shed provision` on a running shed
// (backend.Provisioner): the shed's provisioning slot is held from here
// until the run's Release.
func (c *Client) BeginProvision(ctx context.Context, name string) (backend.ProvisionRun, error) {
	return orchestrator.BeginProvision(ctx, c, c.deliverySvc, &c.provisioning, name)
}

// ProvisionShed is BeginProvision, Run and Release in one call.
func (c *Client) ProvisionShed(ctx context.Context, name string) (*config.Shed, error) {
	return backend.Provision(ctx, c, name)
}

// LockShed takes the shed's lifecycle lock (orchestrator.LocalBackend).
func (c *Client) LockShed(name string) func() { return c.acquireCreateLock(name) }

// Halting reports whether a stop or a delete of the shed is waiting for its
// lock (orchestrator.LocalBackend).
func (c *Client) Halting(name string) bool { return c.halting.Raised(name) }

// LoadShed reads the shed for a provision, under its lifecycle lock (held by
// the caller): checked against ctx, its status reconciled with its VMM. With
// mintID a shed with no permanent id gets one, saved durably.
func (c *Client) LoadShed(ctx context.Context, name string, mintID bool) (*orchestrator.LocalShed, error) {
	shed, err := c.getShedLocked(ctx, name)
	if err != nil {
		return nil, err
	}
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}
	if err := checkIdentity(ctx, name, meta); err != nil {
		return nil, err
	}
	if meta.ID == "" && mintID {
		id, err := config.NewShedID()
		if err != nil {
			return nil, err
		}
		meta.ID = id
		if err := meta.SaveDurable(c.cfg.InstanceDir); err != nil {
			return nil, fmt.Errorf("saving the shed's new id: %w", err)
		}
		shed.ID = id
	}
	agent := c.newAgentClient(name)
	cloneEnv := c.cloneEnvFor(meta)
	return &orchestrator.LocalShed{
		Shed:     shed,
		ID:       meta.ID,
		Record:   meta.Delivery,
		ResetGen: meta.ResetGen,
		Request:  requestShed(meta),
		Origin:   meta.UserOrigin,
		Repo:     meta.Repo,
		CloneEnv: cloneEnv,
		Writer:   agent,
		Clone: func(ctx context.Context, withheld bool) delivery.Step {
			return vmutil.CloneStep(ctx, agent, meta.Repo, func(ctx context.Context) error {
				return vmutil.CloneRepo(ctx, agent, c.serverCfg, meta.Repo, withheld, cloneEnv)
			})
		},
		Hooks: c.provisionerFor(meta).HooksStep,
		Save: func(rec *delivery.Record) error {
			meta.Delivery = rec
			return meta.SaveDurable(c.cfg.InstanceDir)
		},
	}, nil
}
