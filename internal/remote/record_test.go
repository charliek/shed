package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// One shed record on every backend, on remote: every record reports the
// explicitly empty mount selection, its architecture where its record has
// one, and (through the access guard) its isolation, while the stored
// record keeps only what it stored.

// withArmSize adds a size "big" on arm64 beside the amd64 standard size.
func withArmSize() envOpt {
	return func(e *env, _ *fake.Options) {
		e.cfg.Sizes["big"] = config.RemoteSizeConfig{MachineType: "t2a-standard-8", CPUs: 8, MemoryMB: 32768, Arch: provider.ArchARM64}
	}
}

// storedRecord reads a record's file as it is on disk.
func storedRecord(t *testing.T, e *env, name string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.b.store.Dir(), "sheds", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var shed map[string]json.RawMessage
	if err := json.Unmarshal(rec["shed"], &shed); err != nil {
		t.Fatal(err)
	}
	return shed
}

func TestRemoteRecordMountsAndArch(t *testing.T) {
	e := newEnv(t, withArmSize())
	ctx := ctxT(t)
	if _, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "small", NoProvision: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "arm", NoProvision: true, CPUs: 8, Network: config.NetworkProfileIsolated}); err != nil {
		t.Fatal(err)
	}
	e.settled("small")
	e.settled("arm")
	wantArch := map[string]string{"small": provider.ArchAMD64, "arm": provider.ArchARM64}

	before := map[string][]byte{}
	for name := range wantArch {
		data, err := os.ReadFile(filepath.Join(e.b.store.Dir(), "sheds", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}

	check := func(label string, sh *config.Shed) {
		t.Helper()
		if sh.Mounts == nil || sh.Mounts.Mode != config.MountModeSelected || len(sh.Mounts.Names) != 0 {
			t.Errorf("%s %s: mounts %+v, want {mode: selected}", label, sh.Name, sh.Mounts)
		}
		if hr := sh.HostResources(); len(hr) != 0 {
			t.Errorf("%s %s: host resources %v, want none", label, sh.Name, hr)
		}
		if sh.Arch != wantArch[sh.Name] {
			t.Errorf("%s %s: arch %q, want %q", label, sh.Name, sh.Arch, wantArch[sh.Name])
		}
		data, _ := json.Marshal(sh)
		if !bytes.Contains(data, []byte(`"mounts":{"mode":"selected"}`)) {
			t.Errorf("%s %s: JSON %s lacks the empty selection", label, sh.Name, data)
		}
	}
	for name := range wantArch {
		sh, err := e.b.GetShed(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		check("GetShed", sh)
	}
	list, err := e.b.ListSheds(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListSheds = %d, %v", len(list), err)
	}
	for i := range list {
		check("ListSheds", &list[i])
	}

	// Through the access guard: isolation by topology for the isolated shed,
	// none for the open one, and mounts and arch as above.
	g := backend.NewGuard(e.b)
	gctx := asUser(t, backend.ImplicitPrincipal())
	wantIso := map[string]string{"small": backend.IsolationNone, "arm": backend.IsolationTopology}
	guarded, err := g.ListSheds(gctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range guarded {
		check("guarded ListSheds", &guarded[i])
		if guarded[i].Isolation != wantIso[guarded[i].Name] {
			t.Errorf("guarded %s: isolation %q, want %q", guarded[i].Name, guarded[i].Isolation, wantIso[guarded[i].Name])
		}
	}
	if sh, err := g.GetShed(gctx, "arm"); err != nil || sh.Isolation != backend.IsolationTopology {
		t.Errorf("guarded GetShed arm = %+v, %v; want isolation topology", sh, err)
	}

	// The stored record is unchanged by any of it: no mounts, no isolation,
	// the arch it stored at create.
	for name, data := range before {
		after, err := os.ReadFile(filepath.Join(e.b.store.Dir(), "sheds", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, data) {
			t.Errorf("%s: the stored record changed\nbefore: %s\nafter:  %s", name, data, after)
		}
		shed := storedRecord(t, e, name)
		if _, ok := shed["mounts"]; ok {
			t.Errorf("%s: the stored record carries mounts: %s", name, shed["mounts"])
		}
		if _, ok := shed["isolation"]; ok {
			t.Errorf("%s: the stored record carries isolation: %s", name, shed["isolation"])
		}
		if string(shed["arch"]) != `"`+wantArch[name]+`"` {
			t.Errorf("%s: stored arch %s, want %q", name, shed["arch"], wantArch[name])
		}
	}
	e.b.mu.Lock()
	for name, r := range e.b.records {
		if r.Shed.Mounts != nil || r.Shed.Isolation != "" {
			t.Errorf("%s: the record in memory carries mounts %+v isolation %q", name, r.Shed.Mounts, r.Shed.Isolation)
		}
	}
	e.b.mu.Unlock()
}

// TestRemoteAdoptedRecordTakesTheInstanceArch: a record rebuilt from its
// instance (the cache lost) takes the instance's architecture.
func TestRemoteAdoptedRecordTakesTheInstanceArch(t *testing.T) {
	e := newEnv(t, withArmSize())
	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "arm", NoProvision: true, CPUs: 8}); err != nil {
		t.Fatal(err)
	}
	e.settled("arm")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	e.b = e.open()
	e.settled("arm")
	sh, err := e.b.GetShed(context.Background(), "arm")
	if err != nil {
		t.Fatal(err)
	}
	if sh.Arch != provider.ArchARM64 {
		t.Errorf("adopted arch %q, want %q", sh.Arch, provider.ArchARM64)
	}
	if string(storedRecord(t, e, "arm")["arch"]) != `"arm64"` {
		t.Errorf("the adopted record did not store the instance's arch")
	}
}

// TestRemoteRecordWithNoArchReportsNone: a record written before records
// carried an arch reports none, directly and through the guard; nothing
// fills it in.
func TestRemoteRecordWithNoArchReportsNone(t *testing.T) {
	e := newEnv(t)
	if _, err := e.create("old"); err != nil {
		t.Fatal(err)
	}
	e.settled("old")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	// Strip the arch from the stored record, as one written before it.
	path := filepath.Join(e.dir, "sheds", "old.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var shed map[string]json.RawMessage
	if err := json.Unmarshal(rec["shed"], &shed); err != nil {
		t.Fatal(err)
	}
	if _, ok := shed["arch"]; !ok {
		t.Fatalf("the created record stored no arch: %s", data)
	}
	delete(shed, "arch")
	if rec["shed"], err = json.Marshal(shed); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e.b = e.open()
	e.settled("old")

	sh, err := e.b.GetShed(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if sh.Arch != "" {
		t.Errorf("arch %q, want none", sh.Arch)
	}
	g := backend.NewGuard(e.b)
	gsh, err := g.GetShed(asUser(t, backend.ImplicitPrincipal()), "old")
	if err != nil {
		t.Fatal(err)
	}
	if gsh.Arch != "" {
		t.Errorf("guarded arch %q, want none", gsh.Arch)
	}
	if out, _ := json.Marshal(gsh); bytes.Contains(out, []byte(`"arch"`)) {
		t.Errorf("the reported record names an arch: %s", out)
	}
	if _, ok := storedRecord(t, e, "old")["arch"]; ok {
		t.Error("the stored record was given an arch")
	}
}
