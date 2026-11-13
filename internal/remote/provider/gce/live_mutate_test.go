package gce

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// TestLiveMutate drives one real instance through its whole life with the
// provider, against the real Compute API: Create, Wait, the guest host key,
// Stop, Start, SetLabels, List, Destroy and a second Destroy. It creates a
// VM that costs money, so it runs only with SHED_GCE_LIVE_MUTATE=1 and the
// variables TestLiveReadOnly needs, plus:
//
//	SHED_GCE_LIVE_IMAGE      image to boot (default family/shed-extensions-amd64)
//	SHED_REMOTE_SSH_KEY_FILE the OpenSSH private key; its ".pub" twin is put
//	                         in shed-link-authorized-keys
//
// The instance is destroyed at the end, and by a cleanup if a step fails.
//
//	set -a; . ~/.config/shed/remote-dev/env; set +a
//	SHED_GCE_LIVE_MUTATE=1 go test ./internal/remote/provider/gce -run TestLiveMutate -v -timeout 20m
func TestLiveMutate(t *testing.T) {
	if os.Getenv("SHED_GCE_LIVE_MUTATE") != "1" {
		t.Skip("set SHED_GCE_LIVE_MUTATE=1 and the dev environment to run; it creates a VM")
	}
	creds, err := LoadCredentials(os.Getenv, "GOOGLE_CREDENTIALS_BASE64", "SHED_GCE_CREDENTIALS_FILE")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{
		Project: os.Getenv("SHED_GCE_PROJECT"),
		Zone:    os.Getenv("SHED_GCE_ZONE"),
		Env:     os.Getenv("SHED_REMOTE_ENV"),
		Network: os.Getenv("SHED_GCE_NETWORK"),
		Subnet:  os.Getenv("SHED_GCE_SUBNET"),
		Token:   creds,
	})
	if err != nil {
		t.Fatal(err)
	}
	pubFile := os.Getenv("SHED_REMOTE_SSH_KEY_FILE") + ".pub"
	pub, err := os.ReadFile(pubFile)
	if err != nil {
		t.Fatalf("read the link public key: %v", err)
	}
	image := os.Getenv("SHED_GCE_LIVE_IMAGE")
	if image == "" {
		image = provider.ImagePrefixFamily + "shed-extensions-amd64"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	exact, err := c.ResolveImage(ctx, image)
	if err != nil {
		t.Fatalf("resolve %s: %v", image, err)
	}
	t.Logf("image %s resolves to %s", image, exact)

	id := provider.Identity{
		Env:  c.Env(),
		ID:   fmt.Sprintf("live%022d", time.Now().UnixNano()%1e18),
		Name: "live-mutate",
	}
	size, err := provider.ResolveSize(provider.DefaultSizes(), provider.SizeStandard, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := provider.NewSpec(id,
		provider.ShedSpec{CreatedAt: time.Now(), Network: "isolated"},
		[]string{strings.TrimSpace(string(pub))},
		size, provider.DefaultDiskGB, exact, 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	since := func() string { return time.Since(t0).Round(100 * time.Millisecond).String() }
	pending, err := c.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref := pending.Ref
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer ccancel()
		p, err := c.Destroy(cctx, ref)
		if err != nil {
			t.Errorf("cleanup destroy %s: %v", ref, err)
			return
		}
		if _, err := c.Wait(cctx, p); err != nil {
			t.Errorf("cleanup wait %s: %v", ref, err)
		}
	})
	t.Logf("[%s] create accepted: %+v", since(), pending)

	again, err := c.Create(ctx, spec)
	if err != nil || again.Ref != ref {
		t.Errorf("second create with the same spec = %+v, %v; want the same ref and no error", again, err)
	}

	inst, err := c.Wait(ctx, pending)
	if err != nil {
		t.Fatalf("wait for create: %v", err)
	}
	t.Logf("[%s] running: address %s internal %s machine %s cpus %d mem %d disk %d image %s maxrun %v planned stop %v",
		since(), inst.Address, inst.InternalAddress, inst.MachineType, inst.CPUs, inst.MemoryMB, inst.DiskGB, inst.Image, inst.MaxRun, inst.PlannedStopAt)
	if inst.Address == "" {
		t.Error("a running instance has no address")
	}
	if inst.MaxRun != 3*time.Hour {
		t.Errorf("MaxRun = %v, want 3h", inst.MaxRun)
	}
	if inst.Metadata[provider.MetadataShedName] != id.Name || inst.Metadata[provider.MetadataShedSpec] == "" {
		t.Errorf("metadata = %v", inst.Metadata)
	}
	if inst.Labels[provider.LabelState] != string(provider.ShedStateCreating) {
		t.Errorf("labels = %v", inst.Labels)
	}

	// The guest attribute: poll no faster than every 6 s (guest-attribute
	// reads are rate limited per VM).
	var hostKey string
	for hostKey == "" {
		k, err := c.GuestHostKey(ctx, ref)
		switch {
		case err == nil:
			hostKey = k
		case errors.Is(err, provider.ErrNotFound):
			if err := sleep(ctx, 6*time.Second); err != nil {
				t.Fatalf("no host key published: %v", err)
			}
		default:
			t.Fatalf("guest host key: %v", err)
		}
	}
	t.Logf("[%s] guest host key %s", since(), hostKey)
	if !strings.HasPrefix(hostKey, "ssh-ed25519 ") {
		t.Errorf("host key %q is not ed25519", hostKey)
	}
	if err := c.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: hostKey}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if got, err := c.Get(ctx, ref); err != nil || got.Metadata[provider.MetadataHostKeyPin] != hostKey {
		t.Errorf("pin read back = %q, %v", got.Metadata[provider.MetadataHostKeyPin], err)
	}

	p, err := c.Stop(ctx, ref)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if inst, err = c.Wait(ctx, p); err != nil {
		t.Fatalf("wait for stop: %v", err)
	}
	t.Logf("[%s] stopped (address %q)", since(), inst.Address)
	if p, err = c.Stop(ctx, ref); err != nil || p.Operation != "" {
		t.Errorf("stop of a stopped instance = %+v, %v; want a no-op", p, err)
	}

	p, err = c.Start(ctx, ref, provider.StartOptions{MaxRun: 2 * time.Hour})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst, err = c.Wait(ctx, p); err != nil {
		t.Fatalf("wait for start: %v", err)
	}
	t.Logf("[%s] running again: address %s maxrun %v planned stop %v", since(), inst.Address, inst.MaxRun, inst.PlannedStopAt)
	if inst.MaxRun != 2*time.Hour {
		t.Errorf("MaxRun after start = %v, want 2h", inst.MaxRun)
	}
	if k, err := c.GuestHostKey(ctx, ref); err != nil || k != hostKey {
		t.Errorf("host key after restart = %q, %v; want unchanged", k, err)
	}

	if err := c.SetLabels(ctx, ref, map[string]string{
		provider.LabelState:  string(provider.ShedStateReady),
		provider.LabelLayout: provider.LayoutLabel(1),
	}); err != nil {
		t.Fatalf("set labels: %v", err)
	}
	insts, err := c.List(ctx, provider.Selector{Env: c.Env()})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, i := range insts {
		if i.Ref == ref {
			found = true
			if i.Labels[provider.LabelState] != string(provider.ShedStateReady) || i.Labels[provider.LabelLayout] != "1" {
				t.Errorf("listed labels = %v", i.Labels)
			}
		}
	}
	if !found {
		t.Errorf("list of env %s does not hold %s", c.Env(), ref)
	}
	t.Logf("[%s] labels set, listed %d instance(s)", since(), len(insts))

	p, err = c.Destroy(ctx, ref)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	p2, err := c.Destroy(ctx, ref)
	if err != nil {
		t.Errorf("second destroy while deleting: %v", err)
	}
	if _, err := c.Wait(ctx, p); err != nil {
		t.Fatalf("wait for destroy: %v", err)
	}
	if _, err := c.Wait(ctx, p2); err != nil {
		t.Errorf("wait for second destroy: %v", err)
	}
	t.Logf("[%s] destroyed", since())
	if _, err := c.Get(ctx, ref); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("get after destroy = %v, want ErrNotFound", err)
	}
	if p, err = c.Destroy(ctx, ref); err != nil || p.Operation != "" {
		t.Errorf("destroy of a missing instance = %+v, %v", p, err)
	}
}
