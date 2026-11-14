package gce

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/providertest"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const testEnv = "dev-test"

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

// keyJSON returns a service account key file for the test key.
func keyJSON(t *testing.T, tokenURI string) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	data, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-proj",
		"private_key_id": "kid-1",
		"private_key":    string(pemText),
		"client_email":   "shed-server@test-proj.iam.gserviceaccount.com",
		"token_uri":      tokenURI,
	})
	return data
}

func testCredentials(t *testing.T, f *fakeCompute) *Credentials {
	t.Helper()
	f.pubKey = &rsaKey(t).PublicKey
	creds, err := ParseCredentials(keyJSON(t, f.tokenURL()))
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

func testClient(t *testing.T, f *fakeCompute, mod ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		Project:      f.project,
		Zone:         f.zone,
		Env:          testEnv,
		Network:      "shed-net",
		Subnet:       "shed-subnet",
		Token:        testCredentials(t, f),
		BaseURL:      f.baseURL(),
		PollInterval: time.Millisecond,
		Backoff:      []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		Logf:         t.Logf,
	}
	for _, m := range mod {
		m(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func addImages(f *fakeCompute) {
	f.images[f.project+"/shed-extensions-amd64-v1"] = fImage{name: "shed-extensions-amd64-v1", family: "shed-extensions-amd64", status: "READY", deprecated: "DEPRECATED"}
	f.images[f.project+"/shed-extensions-amd64-v2"] = fImage{name: "shed-extensions-amd64-v2", family: "shed-extensions-amd64", status: "READY"}
	f.images[f.project+"/shed-old"] = fImage{name: "shed-old", status: "READY", deprecated: "OBSOLETE"}
	f.images[f.project+"/shed-pending"] = fImage{name: "shed-pending", status: "PENDING"}
	f.images["ubuntu-os-cloud/ubuntu-2404-noble-amd64-v20261105"] = fImage{name: "ubuntu-2404-noble-amd64-v20261105", family: "ubuntu-2404-lts-amd64", status: "READY"}
}

func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Harness{
		Env:    testEnv,
		Size:   provider.DefaultSizes()[0],
		Image:  "shed-extensions-amd64-v1",
		Family: provider.ImagePrefixFamily + "shed-extensions-amd64",
		New: func(t *testing.T) (provider.Provider, providertest.Controls) {
			f := newFakeCompute(t)
			addImages(f)
			c := testClient(t, f)
			return c, providertest.Controls{
				Seed: func(t *testing.T, name string, labels map[string]string) provider.Ref {
					f.seed(&fInst{name: name, labels: labels})
					return makeRef(f.zone, name)
				},
				PublishHostKey: func(t *testing.T, ref provider.Ref, key string) {
					_, name, _ := parseRef("test", ref)
					f.mu.Lock()
					defer f.mu.Unlock()
					// The boot unit publishes the bare key; a comment must not leak through.
					f.instances[name].guestAttrs = map[string]string{"shed/host-key": key + " root@shed"}
				},
				RefuseNextCreate: func(t *testing.T, kind error) {
					if kind == provider.ErrQuota {
						f.failNext("POST", `/instances$`, 403, "quotaExceeded", "Quota 'CPUS' exceeded.  Limit: 8.0 in region us-central1.", 1)
					} else {
						f.failNext("POST", `/instances$`, 503, "ZONE_RESOURCE_POOL_EXHAUSTED", "The zone does not have enough resources available to fulfill the request.", 1)
					}
				},
				FailNextCreateOperation: func(t *testing.T, kind error) {
					if kind == provider.ErrQuota {
						f.failNextOp("insert", "QUOTA_EXCEEDED", "Quota 'IN_USE_ADDRESSES' exceeded.  Limit: 4.0 in region us-central1.")
					} else {
						f.failNextOp("insert", "ZONE_RESOURCE_POOL_EXHAUSTED", "The zone 'projects/test-proj/zones/us-central1-a' does not have enough resources available to fulfill the request.")
					}
				},
				SetFirewall: func(t *testing.T, rules []provider.FirewallRule) { f.setFirewall(rules) },
			}
		},
	})
}

func goldenSpec(t *testing.T, repo string, maxRun time.Duration) provider.Spec {
	t.Helper()
	id := provider.Identity{Env: testEnv, ID: "abcdefghij0123456789klmnop", Name: "web-app", Owner: "nocturn"}
	shed := provider.ShedSpec{
		CreatedAt:  time.Date(2026, 11, 13, 1, 2, 3, 0, time.UTC),
		Repo:       repo,
		Network:    "isolated",
		LandingDir: "/home/shed/r",
	}
	spec, err := provider.NewSpec(id, shed, []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGolden shed-server"},
		provider.DefaultSizes()[0], 50, "shed-extensions-amd64-v2", maxRun)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestInsertGolden(t *testing.T) {
	cases := []struct {
		name   string
		repo   string
		maxRun time.Duration
		tier   string
	}{
		{"token_repo", "https://token@host/o/r.git", 24 * time.Hour, ""},
		{"userpass_repo", "https://u:p@host/o/r.git", 30 * time.Hour, TierStandard},
		{"no_run_limit", "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompute(t)
			c := testClient(t, f, func(cfg *Config) { cfg.NetworkTier = tc.tier })
			if _, err := c.Create(context.Background(), goldenSpec(t, tc.repo, tc.maxRun)); err != nil {
				t.Fatal(err)
			}
			reqs := f.requestsFor("POST", `/instances$`)
			if len(reqs) != 1 {
				t.Fatalf("got %d insert requests", len(reqs))
			}
			if rid := reqs[0].Query["requestId"]; len(rid) != 1 || len(rid[0]) != 36 {
				t.Errorf("requestId = %v, want one UUID", rid)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, []byte(reqs[0].Body), "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			for _, secret := range []string{"token@", "u:p", "p@host"} {
				if strings.Contains(pretty.String(), secret) {
					t.Errorf("insert body carries repo credentials %q", secret)
				}
			}
			golden := filepath.Join("testdata", "insert_"+tc.name+".json")
			if *update {
				if err := os.WriteFile(golden, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(pretty.Bytes(), want) {
				t.Errorf("insert body differs from %s:\n%s", golden, pretty.String())
			}
		})
	}
}

func seedMine(f *fakeCompute, name string, status string) provider.Ref {
	id := provider.Identity{Env: testEnv, ID: "id" + strings.ReplaceAll(name, "-", "") + "000000", Name: name}
	f.seed(&fInst{name: id.InstanceName(), labels: id.Labels(provider.ShedStateReady), status: status})
	return makeRef(f.zone, id.InstanceName())
}

func TestListPaging(t *testing.T) {
	f := newFakeCompute(t)
	f.pageSize = 2
	for _, n := range []string{"e", "a", "d", "c", "b"} {
		seedMine(f, "shed"+n, "RUNNING")
	}
	other := provider.Identity{Env: "other", ID: "zzzzzzzzzz", Name: "x"}
	f.seed(&fInst{name: other.InstanceName(), labels: other.Labels(provider.ShedStateReady)})
	f.seed(&fInst{name: "unlabelled-vm"})
	c := testClient(t, f)
	ctx := context.Background()

	t.Run("env", func(t *testing.T) {
		got, err := c.List(ctx, provider.Selector{Env: testEnv})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, i := range got {
			names = append(names, i.Labels[provider.LabelName])
		}
		if strings.Join(names, ",") != "sheda,shedb,shedc,shedd,shede" {
			t.Errorf("names = %v", names)
		}
		lists := f.requestsFor("GET", `/instances$`)
		if len(lists) != 3 {
			t.Errorf("list calls = %d, want 3 pages", len(lists))
		}
		if q := lists[0].Query; q.Get("filter") != `labels.shed_env = "dev-test"` || q.Get("maxResults") != "500" {
			t.Errorf("first page query = %v", q)
		}
		if lists[1].Query.Get("pageToken") != "page-2" || lists[1].Query.Get("filter") == "" {
			t.Errorf("second page query = %v", lists[1].Query)
		}
	})
	t.Run("all", func(t *testing.T) {
		got, err := c.List(ctx, provider.Selector{})
		if err != nil || len(got) != 7 {
			t.Fatalf("List(all) = %d, %v; want 7", len(got), err)
		}
	})
	t.Run("a failed page fails the list", func(t *testing.T) {
		c := testClient(t, f, func(cfg *Config) { cfg.Backoff = []time.Duration{} })
		f.mu.Lock()
		f.failCalls = append(f.failCalls, fFail{method: "GET", match: regexp.MustCompile(`/instances$`), query: regexp.MustCompile(`pageToken=page-4`), status: 500, reason: "backendError", msg: "boom", times: 1})
		f.mu.Unlock()
		got, err := c.List(ctx, provider.Selector{Env: testEnv})
		if err == nil || got != nil {
			t.Errorf("List = %d items, %v; want an error and no items", len(got), err)
		}
	})
	t.Run("a retried page succeeds", func(t *testing.T) {
		f.failNext("GET", `/instances$`, 503, "backendError", "try again", 1)
		got, err := c.List(ctx, provider.Selector{Env: testEnv})
		if err != nil || len(got) != 5 {
			t.Errorf("List = %d, %v; want 5", len(got), err)
		}
	})
	t.Run("bad env", func(t *testing.T) {
		if _, err := c.List(ctx, provider.Selector{Env: `x" OR "1`}); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("List(bad env) = %v, want ErrInvalid", err)
		}
	})
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(f *fakeCompute)
		onCreate  error // error kind from Create
		onWait    error // error kind from Wait (when Create succeeded)
		wantCalls int   // insert calls made, 0 = don't check
	}{
		{
			name: "quota on the call",
			setup: func(f *fakeCompute) {
				f.failNext("POST", `/instances$`, 403, "quotaExceeded", "Quota 'CPUS' exceeded.", 1)
			},
			onCreate: provider.ErrQuota, wantCalls: 1,
		},
		{
			name: "capacity on the call",
			setup: func(f *fakeCompute) {
				f.failNext("POST", `/instances$`, 503, "resourcePoolExhausted", "The zone does not have enough resources available to fulfill the request.", 1)
			},
			onCreate: provider.ErrCapacity, wantCalls: 1,
		},
		{
			name:   "capacity on the operation",
			setup:  func(f *fakeCompute) { f.failNextOp("insert", "ZONE_RESOURCE_POOL_EXHAUSTED", "no room") },
			onWait: provider.ErrCapacity,
		},
		{
			name: "capacity with details on the operation",
			setup: func(f *fakeCompute) {
				f.failNextOp("insert", "ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", "A e2-standard-2 VM instance is currently unavailable in the us-central1-a zone.")
			},
			onWait: provider.ErrCapacity,
		},
		{
			name:   "quota on the operation",
			setup:  func(f *fakeCompute) { f.failNextOp("insert", "QUOTA_EXCEEDED", "Quota 'SSD_TOTAL_GB' exceeded.") },
			onWait: provider.ErrQuota,
		},
		{
			name: "rate limit retried",
			setup: func(f *fakeCompute) {
				f.failNext("POST", `/instances$`, 403, "rateLimitExceeded", "Rate Limit Exceeded", 2)
			},
			wantCalls: 3,
		},
		{
			name: "rate limit outlasting the retries is a quota refusal",
			setup: func(f *fakeCompute) {
				f.failNext("POST", `/instances$`, 403, "rateLimitExceeded", "Rate Limit Exceeded", 10)
			},
			onCreate: provider.ErrQuota, wantCalls: 4,
		},
		{
			name:      "429 retried",
			setup:     func(f *fakeCompute) { f.failNext("POST", `/instances$`, 429, "rateLimitExceeded", "slow down", 1) },
			wantCalls: 2,
		},
		{
			name:      "5xx retried",
			setup:     func(f *fakeCompute) { f.failNext("POST", `/instances$`, 503, "backendError", "backend", 3) },
			wantCalls: 4,
		},
		{
			name: "permission denied is not retried",
			setup: func(f *fakeCompute) {
				f.failNext("POST", `/instances$`, 403, "forbidden", "Required 'compute.instances.create' permission", 5)
			},
			onCreate: nil, wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompute(t)
			c := testClient(t, f)
			tc.setup(f)
			ctx := context.Background()
			pend, err := c.Create(ctx, goldenSpec(t, "", time.Hour))
			permission := strings.Contains(tc.name, "permission")
			switch {
			case permission:
				var pe *provider.Error
				if err == nil || !errors.As(err, &pe) || pe.Kind != nil || !strings.Contains(err.Error(), "compute.instances.create") {
					t.Errorf("Create = %v, want a kind-less error naming the permission", err)
				}
			case tc.onCreate != nil:
				if !errors.Is(err, tc.onCreate) {
					t.Errorf("Create = %v, want %v", err, tc.onCreate)
				}
			case err != nil:
				t.Fatalf("Create = %v", err)
			default:
				_, werr := c.Wait(ctx, pend)
				if tc.onWait == nil && werr != nil {
					t.Errorf("Wait = %v", werr)
				}
				if tc.onWait != nil && !errors.Is(werr, tc.onWait) {
					t.Errorf("Wait = %v, want %v", werr, tc.onWait)
				}
				if tc.onWait != nil && f.instance(goldenSpec(t, "", 0).Name) != nil {
					t.Errorf("a failed insert left an instance")
				}
			}
			if tc.wantCalls > 0 {
				reqs := f.requestsFor("POST", `/instances$`)
				if len(reqs) != tc.wantCalls {
					t.Errorf("insert calls = %d, want %d", len(reqs), tc.wantCalls)
				}
				ids := map[string]bool{}
				for _, r := range reqs {
					ids[r.Query["requestId"][0]] = true
				}
				if len(ids) != 1 {
					t.Errorf("retries used %d requestIds, want the same one", len(ids))
				}
			}
		})
	}
}

func TestInsertOperationFailedAtOnce(t *testing.T) {
	// An insert reply that is already DONE with an error is a refusal on the
	// call, with nothing to wait for.
	f := newFakeCompute(t)
	f.doneAtOnce = true
	f.failNextOp("insert", "ZONE_RESOURCE_POOL_EXHAUSTED", "no room")
	c := testClient(t, f)
	_, err := c.Create(context.Background(), goldenSpec(t, "", time.Hour))
	if !errors.Is(err, provider.ErrCapacity) {
		t.Errorf("Create = %v, want ErrCapacity", err)
	}
	if n := len(f.requestsFor("POST", `/wait$`)); n != 0 {
		t.Errorf("%d wait calls, want none", n)
	}
}

func TestWaitPolls(t *testing.T) {
	f := newFakeCompute(t)
	f.opDelay = 3
	c := testClient(t, f)
	ctx := context.Background()
	pend, err := c.Create(ctx, goldenSpec(t, "", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := c.Wait(ctx, pend)
	if err != nil || inst.State != provider.StateRunning {
		t.Fatalf("Wait = %s, %v", inst.State, err)
	}
	if n := len(f.requestsFor("POST", `/operations/[^/]+/wait$`)); n != 4 {
		t.Errorf("wait calls = %d, want 4", n)
	}
	if inst.PlannedStopAt.IsZero() || inst.MaxRun != time.Hour {
		t.Errorf("planned stop %v, max run %v", inst.PlannedStopAt, inst.MaxRun)
	}
	if !inst.PlannedStopAt.Equal(inst.StartedAt.Add(time.Hour)) {
		t.Errorf("planned stop %v, want start %v + 1h", inst.PlannedStopAt, inst.StartedAt)
	}

	t.Run("unknown operation follows the state", func(t *testing.T) {
		got, err := c.Wait(ctx, provider.Pending{Ref: pend.Ref, Operation: "operation-gone", Want: provider.StateRunning})
		if err != nil || got.State != provider.StateRunning {
			t.Errorf("Wait = %s, %v", got.State, err)
		}
	})
	t.Run("instance in transition is followed", func(t *testing.T) {
		f.mu.Lock()
		in := f.instances[goldenSpec(t, "", 0).Name]
		in.status = "STOPPING"
		f.mu.Unlock()
		go func() {
			time.Sleep(20 * time.Millisecond)
			f.mu.Lock()
			in.status = "TERMINATED"
			in.natIP = ""
			f.mu.Unlock()
		}()
		got, err := c.Wait(ctx, provider.Pending{Ref: pend.Ref, Want: provider.StateStopped})
		if err != nil || got.State != provider.StateStopped || got.Address != "" {
			t.Errorf("Wait = %s %q, %v", got.State, got.Address, err)
		}
	})
	t.Run("deadline ends the wait", func(t *testing.T) {
		f.mu.Lock()
		f.instances[goldenSpec(t, "", 0).Name].status = "STAGING"
		f.mu.Unlock()
		dctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		_, err := c.Wait(dctx, provider.Pending{Ref: pend.Ref, Want: provider.StateRunning})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Wait = %v, want DeadlineExceeded", err)
		}
	})
}

func TestCreateAlreadyExists(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	spec := goldenSpec(t, "", time.Hour)
	t.Run("same shed id is success", func(t *testing.T) {
		f.seed(&fInst{name: spec.Name, labels: spec.Labels})
		pend, err := c.Create(ctx, spec)
		if err != nil || pend.Operation != "" || pend.Want != provider.StateRunning {
			t.Fatalf("Create = %+v, %v", pend, err)
		}
		if _, err := c.Wait(ctx, pend); err != nil {
			t.Errorf("Wait = %v", err)
		}
	})
	t.Run("another shed id is ErrExists", func(t *testing.T) {
		labels := maps.Clone(spec.Labels)
		labels[provider.LabelID] = "someoneelse0000000000"
		f.seed(&fInst{name: spec.Name, labels: labels})
		if _, err := c.Create(ctx, spec); !errors.Is(err, provider.ErrExists) {
			t.Errorf("Create = %v, want ErrExists", err)
		}
	})
}

func TestStartRunLimit(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	ref := seedMine(f, "limit", "TERMINATED")
	_, name, _ := parseRef("t", ref)
	f.mu.Lock()
	f.instances[name].scheduling = map[string]any{
		"automaticRestart": true, "onHostMaintenance": "MIGRATE", "provisioningModel": "STANDARD",
		"instanceTerminationAction": "STOP", "maxRunDuration": map[string]any{"seconds": "7200"},
	}
	f.mu.Unlock()

	steps := []struct {
		maxRun   time.Duration
		wantSets int
	}{
		{2 * time.Hour, 0}, // same limit: no setScheduling
		{0, 0},             // zero: leave it
		{3 * time.Hour, 1}, // changed: one setScheduling, before the start
	}
	for _, s := range steps {
		pend, err := c.Start(ctx, ref, provider.StartOptions{MaxRun: s.maxRun})
		if err != nil {
			t.Fatalf("Start(%v): %v", s.maxRun, err)
		}
		inst, err := c.Wait(ctx, pend)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		sets := f.requestsFor("POST", `/setScheduling$`)
		if len(sets) != s.wantSets {
			t.Errorf("Start(%v): %d setScheduling calls, want %d", s.maxRun, len(sets), s.wantSets)
		}
		if s.wantSets > 0 {
			var body map[string]any
			_ = json.Unmarshal([]byte(sets[0].Body), &body)
			mrd, _ := body["maxRunDuration"].(map[string]any)
			if mrd["seconds"] != "10800" || body["instanceTerminationAction"] != "STOP" || body["automaticRestart"] != true {
				t.Errorf("setScheduling body = %s", sets[0].Body)
			}
			if inst.MaxRun != 3*time.Hour {
				t.Errorf("MaxRun after start = %v", inst.MaxRun)
			}
		}
		pend, err = c.Stop(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Wait(ctx, pend); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBusy(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	staging := seedMine(f, "staging", "STAGING")
	stopping := seedMine(f, "stopping", "STOPPING")
	if _, err := c.Stop(ctx, staging); !errors.Is(err, provider.ErrBusy) {
		t.Errorf("Stop(staging) = %v, want ErrBusy", err)
	}
	if _, err := c.Start(ctx, stopping, provider.StartOptions{}); !errors.Is(err, provider.ErrBusy) {
		t.Errorf("Start(stopping) = %v, want ErrBusy", err)
	}
	if p, err := c.Start(ctx, staging, provider.StartOptions{}); err != nil || p.Want != provider.StateRunning || p.Operation != "" {
		t.Errorf("Start(staging) = %+v, %v; want a no-op pending", p, err)
	}
	f.failNext("DELETE", `/instances/`, 400, "resourceNotReady", "The resource is not ready", 1)
	if _, err := c.Destroy(ctx, staging); !errors.Is(err, provider.ErrBusy) {
		t.Errorf("Destroy(not ready) = %v, want ErrBusy", err)
	}
}

func TestFingerprintRetry(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	ref := seedMine(f, "fp", "RUNNING")
	f.failNext("POST", `/setLabels$`, 412, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed", 1)
	f.failNext("POST", `/setMetadata$`, 412, "conditionNotMet", "Supplied fingerprint does not match current metadata fingerprint.", 2)
	if err := c.SetLabels(ctx, ref, map[string]string{provider.LabelState: "deleting"}); err != nil {
		t.Errorf("SetLabels = %v", err)
	}
	if err := c.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: "ssh-ed25519 AAAA"}); err != nil {
		t.Errorf("SetMetadata = %v", err)
	}
	if n := len(f.requestsFor("POST", `/setMetadata$`)); n != 3 {
		t.Errorf("setMetadata calls = %d, want 3", n)
	}
	// Metadata the GCE switches put there survives a shed write.
	f.mu.Lock()
	f.instances[mustName(ref)].metadata[MetadataBlockProjectSSHKeys] = "TRUE"
	f.mu.Unlock()
	if err := c.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: "ssh-ed25519 BBBB"}); err != nil {
		t.Fatal(err)
	}
	if v := f.instance(mustName(ref)).metadata[MetadataBlockProjectSSHKeys]; v != "TRUE" {
		t.Errorf("block-project-ssh-keys = %q after SetMetadata", v)
	}
	if err := c.SetMetadata(ctx, ref, map[string]string{MetadataBlockProjectSSHKeys: "FALSE"}); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("SetMetadata(GCE switch) = %v, want ErrInvalid", err)
	}
}

func TestImages(t *testing.T) {
	f := newFakeCompute(t)
	addImages(f)
	c := testClient(t, f)
	ctx := context.Background()
	cases := []struct {
		in, want string
		err      error
	}{
		{"shed-extensions-amd64-v2", "shed-extensions-amd64-v2", nil},
		{"family/shed-extensions-amd64", "shed-extensions-amd64-v2", nil},
		{"projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64", "projects/ubuntu-os-cloud/global/images/ubuntu-2404-noble-amd64-v20261105", nil},
		{"projects/ubuntu-os-cloud/global/images/ubuntu-2404-noble-amd64-v20261105", "projects/ubuntu-os-cloud/global/images/ubuntu-2404-noble-amd64-v20261105", nil},
		{"projects/test-proj/global/images/shed-extensions-amd64-v2", "shed-extensions-amd64-v2", nil},
		{"family/none", "", provider.ErrNotFound},
		{"missing-image", "", provider.ErrNotFound},
		{"shed-old", "", provider.ErrNotFound},
		{"shed-pending", "", provider.ErrNotFound},
		{"Bad_Name", "", provider.ErrInvalid},
		{"family/", "", provider.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := c.ResolveImage(ctx, tc.in)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Errorf("ResolveImage = %q, %v; want %v", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ResolveImage = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("cross-project image boots from its path", func(t *testing.T) {
		if got := c.sourceImage("projects/ubuntu-os-cloud/global/images/x"); got != "projects/ubuntu-os-cloud/global/images/x" {
			t.Errorf("sourceImage = %q", got)
		}
		if got := c.sourceImage("shed-x"); got != "projects/test-proj/global/images/shed-x" {
			t.Errorf("sourceImage = %q", got)
		}
	})
}

func TestDisks(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	mine := map[string]string{provider.LabelEnv: testEnv, provider.LabelSchema: "1"}
	f.disks["orphan"] = &fDisk{name: "orphan", labels: mine, sizeGB: 50}
	f.disks["attached"] = &fDisk{name: "attached", labels: mine, users: []string{"shed-dev-test-x"}, sizeGB: 50}
	f.disks["theirs"] = &fDisk{name: "theirs", labels: map[string]string{provider.LabelEnv: "prod"}, sizeGB: 50}
	f.pageSize = 1

	disks, err := c.ListDisks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 || disks[0].Name != "attached" || disks[0].Users[0] != "shed-dev-test-x" || disks[1].SizeGB != 50 {
		t.Errorf("ListDisks = %+v", disks)
	}
	if err := c.DeleteDisk(ctx, "attached"); !errors.Is(err, provider.ErrBusy) {
		t.Errorf("DeleteDisk(attached) = %v, want ErrBusy", err)
	}
	if err := c.DeleteDisk(ctx, "theirs"); !errors.Is(err, provider.ErrForeign) {
		t.Errorf("DeleteDisk(theirs) = %v, want ErrForeign", err)
	}
	if err := c.DeleteDisk(ctx, "orphan"); err != nil {
		t.Errorf("DeleteDisk(orphan) = %v", err)
	}
	if _, ok := f.disks["orphan"]; ok {
		t.Errorf("orphan still exists")
	}
	if err := c.DeleteDisk(ctx, "orphan"); err != nil {
		t.Errorf("DeleteDisk(gone) = %v, want nil", err)
	}
}

func TestNetworkReads(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	f.networks["shed-net"] = map[string]any{
		"name": "shed-net", "autoCreateSubnetworks": false,
		"subnetworks": []any{f.selfLink("projects/test-proj/regions/us-central1/subnetworks/shed-subnet")},
	}
	f.subnets["shed-subnet"] = map[string]any{
		"name": "shed-subnet", "region": f.selfLink("projects/test-proj/regions/us-central1"),
		"network": f.selfLink("projects/test-proj/global/networks/shed-net"), "ipCidrRange": "10.10.0.0/24", "stackType": "IPV4_ONLY",
	}
	f.firewalls = []map[string]any{
		{"name": "shed-dev-test-ssh", "network": f.selfLink("projects/test-proj/global/networks/shed-net"), "direction": "INGRESS", "priority": 1000,
			"sourceRanges": []any{"192.0.2.10/32"}, "targetTags": []any{"shed-dev-test"}, "allowed": []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"22"}}}},
		{"name": "default-allow-ssh", "network": f.selfLink("projects/test-proj/global/networks/default"), "direction": "INGRESS", "priority": 65534,
			"sourceRanges": []any{"0.0.0.0/0"}, "allowed": []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"22"}}}},
		{"name": "stray", "network": f.selfLink("projects/test-proj/global/networks/shed-net"), "direction": "INGRESS", "priority": 900,
			"sourceRanges": []any{"10.0.0.0/8"}, "allowed": []any{map[string]any{"IPProtocol": "all"}}},
	}
	f.pageSize = 1

	n, err := c.GetNetwork(ctx)
	if err != nil || n.Name != "shed-net" || n.AutoCreateSubnetworks || len(n.Subnetworks) != 1 || n.Subnetworks[0] != "regions/us-central1/subnetworks/shed-subnet" {
		t.Errorf("GetNetwork = %+v, %v", n, err)
	}
	s, err := c.GetSubnetwork(ctx)
	if err != nil || s.Region != "us-central1" || s.Network != "shed-net" || s.StackType != "IPV4_ONLY" || s.IPCIDRRange != "10.10.0.0/24" {
		t.Errorf("GetSubnetwork = %+v, %v", s, err)
	}
	rules, err := c.ListFirewalls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].Name != "shed-dev-test-ssh" || rules[1].Name != "stray" {
		t.Fatalf("ListFirewalls = %+v", rules)
	}
	r := rules[0]
	if r.Direction != "INGRESS" || r.SourceRanges[0] != "192.0.2.10/32" || r.TargetTags[0] != "shed-dev-test" || r.Allowed[0].Protocol != "tcp" || r.Allowed[0].Ports[0] != "22" {
		t.Errorf("rule = %+v", r)
	}
	if rules[1].Allowed[0].Protocol != "all" || len(rules[1].TargetTags) != 0 {
		t.Errorf("stray = %+v", rules[1])
	}
}

func TestTokens(t *testing.T) {
	t.Run("cached until near expiry", func(t *testing.T) {
		f := newFakeCompute(t)
		creds := testCredentials(t, f)
		now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
		creds.Now = func() time.Time { return now }
		ctx := context.Background()
		a, err := creds.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(50 * time.Minute)
		b, _ := creds.Token(ctx)
		if a != b || f.tokenHits != 1 {
			t.Errorf("token changed or minted again within its life: %s %s hits=%d", a, b, f.tokenHits)
		}
		now = now.Add(6 * time.Minute) // 4 minutes left: refresh
		c, _ := creds.Token(ctx)
		if c == a || f.tokenHits != 2 {
			t.Errorf("token not refreshed near expiry: %s hits=%d", c, f.tokenHits)
		}
	})
	t.Run("a 401 refreshes once", func(t *testing.T) {
		f := newFakeCompute(t)
		c := testClient(t, f)
		ctx := context.Background()
		if _, err := c.List(ctx, provider.Selector{}); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.revoked["tok-1"] = true
		f.mu.Unlock()
		if _, err := c.List(ctx, provider.Selector{}); err != nil {
			t.Fatalf("List after revocation = %v", err)
		}
		reqs := f.requestsFor("GET", `/instances$`)
		if got := reqs[len(reqs)-1].Token; got != "tok-2" {
			t.Errorf("last token = %s, want tok-2", got)
		}
		// Revoked again and again: one refresh per call, then the error.
		f.mu.Lock()
		f.revoked["tok-2"], f.revoked["tok-3"] = true, true
		f.mu.Unlock()
		_, err := c.List(ctx, provider.Selector{})
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("List with every token revoked = %v, want a 401 error", err)
		}
	})
	t.Run("bad signature is an error", func(t *testing.T) {
		f := newFakeCompute(t)
		creds := testCredentials(t, f)
		other, _ := rsa.GenerateKey(rand.Reader, 1024)
		f.pubKey = &other.PublicKey
		_, err := creds.Token(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Invalid JWT Signature") {
			t.Errorf("Token = %v", err)
		}
		if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("error carries key material")
		}
	})
}

func TestLoadCredentials(t *testing.T) {
	raw := keyJSON(t, "https://oauth2.example/token")
	b64 := base64.StdEncoding.EncodeToString(raw)
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "key.json")
	b64File := filepath.Join(dir, "key.b64")
	if err := os.WriteFile(jsonFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b64File, []byte(b64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{"base64 env", map[string]string{"KEY_B64": b64}, ""},
		{"json file", map[string]string{"KEY_FILE": jsonFile}, ""},
		{"base64 file", map[string]string{"KEY_FILE": b64File}, ""},
		{"both", map[string]string{"KEY_B64": b64, "KEY_FILE": jsonFile}, "both KEY_B64 and KEY_FILE"},
		{"neither", map[string]string{}, "none of KEY_B64 and KEY_FILE"},
		{"missing file", map[string]string{"KEY_FILE": filepath.Join(dir, "nope")}, "KEY_FILE"},
		{"garbage", map[string]string{"KEY_B64": "not!base64"}, "KEY_B64"},
		{"not a service account", map[string]string{"KEY_B64": base64.StdEncoding.EncodeToString([]byte(`{"type":"authorized_user"}`))}, "not a service_account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := LoadCredentials(env(tc.vars), "KEY_B64", "KEY_FILE")
			if tc.wantErr == "" {
				if err != nil || c.Email() != "shed-server@test-proj.iam.gserviceaccount.com" {
					t.Fatalf("LoadCredentials = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("LoadCredentials = %v, want an error containing %q", err, tc.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), b64[:40])) {
				t.Errorf("error carries key material: %v", err)
			}
		})
	}
}

func TestNewValidates(t *testing.T) {
	ok := Config{Project: "p", Zone: "us-central1-a", Env: "dev", Network: "n", Token: staticToken("x")}
	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"no project", func(c *Config) { c.Project = "" }},
		{"no zone", func(c *Config) { c.Zone = "" }},
		{"zone without region", func(c *Config) { c.Zone = "central" }},
		{"bad env", func(c *Config) { c.Env = "Dev" }},
		{"no network", func(c *Config) { c.Network = "" }},
		{"no token", func(c *Config) { c.Token = nil }},
		{"bad tier", func(c *Config) { c.NetworkTier = "GOLD" }},
		// A Shared VPC: the firewall check would read this project's rules
		// for a network that lives in another.
		{"network in another project", func(c *Config) { c.Network = "projects/host/global/networks/n" }},
		{"network URL in another project", func(c *Config) {
			c.Network = "https://www.googleapis.com/compute/v1/projects/host/global/networks/n"
		}},
		{"subnet in another project", func(c *Config) { c.Subnet = "projects/host/regions/us-central1/subnetworks/s" }},
		{"network path that names no project", func(c *Config) { c.Network = "global/networks/n" }},
	}
	if _, err := New(ok); err != nil {
		t.Fatalf("New(valid) = %v", err)
	}
	for _, same := range []Config{
		{Project: "p", Zone: "us-central1-a", Env: "dev", Network: "projects/p/global/networks/n", Subnet: "projects/p/regions/us-central1/subnetworks/s", Token: staticToken("x")},
		{Project: "p", Zone: "us-central1-a", Env: "dev", Network: "https://www.googleapis.com/compute/v1/projects/p/global/networks/n", Subnet: "s", Token: staticToken("x")},
	} {
		if _, err := New(same); err != nil {
			t.Errorf("New with a path in its own project (%s) = %v", same.Network, err)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ok
			tc.mod(&cfg)
			if _, err := New(cfg); err == nil {
				t.Errorf("New accepted %s", tc.name)
			}
		})
	}
	c, _ := New(ok)
	if c.networkURL != "projects/p/global/networks/n" || c.subnetURL != "" || c.cfg.NetworkTier != TierPremium {
		t.Errorf("defaults: %s %q %s", c.networkURL, c.subnetURL, c.cfg.NetworkTier)
	}
}

func TestRefs(t *testing.T) {
	for _, bad := range []provider.Ref{"", "name-only", "zone/", "/name", "us-central1-a/Bad", "a/b/c", "fake/x y"} {
		if _, _, err := parseRef("t", bad); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("parseRef(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	z, n, err := parseRef("t", makeRef("us-central1-a", "shed-dev-x"))
	if err != nil || z != "us-central1-a" || n != "shed-dev-x" {
		t.Errorf("round trip = %s %s %v", z, n, err)
	}
}

func TestStateMapping(t *testing.T) {
	cases := map[string]provider.State{
		"PROVISIONING": provider.StateStarting, "STAGING": provider.StateStarting,
		"RUNNING": provider.StateRunning, "STOPPING": provider.StateStopping,
		"SUSPENDING": provider.StateStopping, "TERMINATED": provider.StateStopped,
		"STOPPED": provider.StateStopped,
		// Statuses in the Compute v1 enum that the lifecycle page also names.
		"PENDING": provider.StateStarting, "PENDING_STOP": provider.StateStopping, "DEPROVISIONING": provider.StateStopping,
		"SUSPENDED": provider.StateUnknown, "REPAIRING": provider.StateUnknown, "": provider.StateUnknown,
	}
	for in, want := range cases {
		if got := mapState(in); got != want {
			t.Errorf("mapState(%q) = %s, want %s", in, got, want)
		}
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func mustName(ref provider.Ref) string {
	_, n, err := parseRef("t", ref)
	if err != nil {
		panic(err)
	}
	return n
}

// TestCreateOwnsTheMetadataSwitches: every instance is made with OS Login
// off, project SSH keys blocked and guest attributes on, whatever the project
// sets, and a caller cannot set any of the three.
func TestCreateOwnsTheMetadataSwitches(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	spec := goldenSpec(t, "", 0)
	if _, err := c.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	got := f.instance(spec.Name).metadata
	for key, want := range map[string]string{
		MetadataEnableOSLogin: "FALSE", MetadataBlockProjectSSHKeys: "TRUE", MetadataEnableGuestAttributes: "TRUE",
	} {
		if got[key] != want {
			t.Errorf("instance metadata %s = %q, want %q", key, got[key], want)
		}
	}
	for _, key := range []string{MetadataEnableOSLogin, MetadataBlockProjectSSHKeys, MetadataEnableGuestAttributes} {
		t.Run(key, func(t *testing.T) {
			spec := goldenSpec(t, "", 0)
			spec.Metadata[key] = "TRUE"
			if _, err := c.Create(context.Background(), spec); !errors.Is(err, provider.ErrInvalid) {
				t.Errorf("Create with %s set by the caller: %v, want ErrInvalid", key, err)
			}
		})
	}
}
