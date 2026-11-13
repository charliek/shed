package gce

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

func TestWaitDestroyOperationNotFound(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	ref := seedMine(f, "gone", "RUNNING")
	f.mu.Lock()
	f.failOps["delete"] = append(f.failOps["delete"], fOpErr{code: "RESOURCE_NOT_FOUND", msg: "The resource was not found", status: 404})
	f.mu.Unlock()
	pend, err := c.Destroy(ctx, ref)
	if err != nil || pend.Operation == "" {
		t.Fatalf("Destroy = %+v, %v", pend, err)
	}
	f.mu.Lock()
	delete(f.instances, mustName(ref)) // the other delete finished
	f.mu.Unlock()
	if inst, err := c.Wait(ctx, pend); err != nil || inst.Ref != "" {
		t.Errorf("Wait = %+v, %v; want the zero instance and no error", inst, err)
	}

	t.Run("a start that ends not found is still an error", func(t *testing.T) {
		ref := seedMine(f, "stopped", "TERMINATED")
		f.mu.Lock()
		f.failOps["start"] = append(f.failOps["start"], fOpErr{code: "RESOURCE_NOT_FOUND", msg: "The resource was not found", status: 404})
		f.mu.Unlock()
		pend, err := c.Start(ctx, ref, provider.StartOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Wait(ctx, pend); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("Wait = %v, want ErrNotFound", err)
		}
	})
}

func TestCreateExistsThenGone(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	spec := goldenSpec(t, "", time.Hour)
	f.failNext("POST", `/instances$`, 409, "alreadyExists", "The resource '"+spec.Name+"' already exists", 1)
	_, err := c.Create(context.Background(), spec)
	if err == nil || errors.Is(err, provider.ErrExists) || errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("Create = %v, want a kind-less error", err)
	}
	// The retry the error asks for works.
	if _, err := c.Create(context.Background(), spec); err != nil {
		t.Errorf("second Create = %v", err)
	}
}

func TestCreateRefusesRepoCredentials(t *testing.T) {
	for _, repo := range []string{"https://token@host/o/r.git", "https://u:p@host/o/r.git", "ssh://git:pw@host/o/r.git"} {
		t.Run(repo, func(t *testing.T) {
			f := newFakeCompute(t)
			c := testClient(t, f)
			spec := goldenSpec(t, "", time.Hour)
			raw, _ := json.Marshal(map[string]any{
				"schema": 1, "created_at": "2026-11-13T01:02:03Z", "repo": repo,
				"image": spec.Image, "size": "standard", "network": "open",
			})
			spec.Metadata[provider.MetadataShedSpec] = string(raw)
			if _, err := c.Create(context.Background(), spec); !errors.Is(err, provider.ErrInvalid) {
				t.Errorf("Create = %v, want ErrInvalid", err)
			}
			if n := len(f.requestsFor("POST", `/instances$`)); n != 0 {
				t.Errorf("%d insert requests, want none", n)
			}
		})
	}
	t.Run("an ssh login name is not a credential", func(t *testing.T) {
		f := newFakeCompute(t)
		c := testClient(t, f)
		if _, err := c.Create(context.Background(), goldenSpec(t, "ssh://git@host/o/r.git", time.Hour)); err != nil {
			t.Errorf("Create = %v", err)
		}
	})
}
