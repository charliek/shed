package refserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestComputeControlFile drives the reference compute service as the
// command does, through compute.json in a control directory: steps are
// applied once each, by their id; auto_settle_s and the clock steps settle
// cloud work; host_key is published for a machine when it first runs; a
// forced reply is sent while the file names it; a malformed file is a
// failed lookup (503).
func TestComputeControlFile(t *testing.T) {
	h := newHarness(t)
	dir := controlDir(t)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("users.json", `[{"name":"ada","admin":true,"keys":[{"key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBy7bQtK7HKLg24Jda/ypO5MK6DYapUE7szlye2w4Phe","label":"laptop"}]}]`)
	srv, err := New(Config{
		Issuer: "https://accounts.example.test", Organization: "example", Inputs: openControl(t, dir),
		SigningKey: h.signing, KeyID: fixtureKeyID, Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.addServer("srv-a", srvA, h.jwks["server-ed25519"]); err != nil {
		t.Fatal(err)
	}
	sc := &scenario{t: t, h: h, clock: time.Now()}
	tok := sc.accessToken(json.RawMessage(`{"client_id":"` + srvA + `","scope":["compute:manage"]}`))
	do := func(method, path, body string) (int, map[string]any, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var v map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v, rec.Body.String()
	}
	const id = "q6c3g4ymb5m2dkxwe7kyz5hhzu"
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOTs41aR1tvqm53eXWSYYaVa4vzPYlXSTKnKr5nW/t9C"

	if code, _, _ := do("GET", "/v1/compute/catalog", ""); code != http.StatusForbidden {
		t.Fatalf("before any grant: %d, want 403", code)
	}
	write("compute.json", `{"steps":[{"id":"1","step":{"compute.allow":"srv-a"}}],"auto_settle_s":5,"host_key":"`+key+`"}`)
	if code, _, _ := do("GET", "/v1/compute/catalog", ""); code != http.StatusOK {
		t.Fatalf("after compute.allow: %d", code)
	}
	create := `{"request_id":"00000000-0000-4000-8000-000000000001","name":"widgets","user":"ada","size":"standard","image":"default","labels":{},"authorized_keys":["ssh-ed25519 AAAA"],"spec":"","access_kinds":["ssh"]}`
	if inv := srv.ComputeInventory(); len(inv) != 0 {
		t.Fatalf("inventory before any create: %v", inv)
	}
	if code, v, body := do("PUT", "/v1/compute/machines/"+id, create); code != http.StatusAccepted || v["operation"] == nil {
		t.Fatalf("create: %d %s", code, body)
	}
	// The machine is in the inventory from its create, by its server.
	if inv := srv.ComputeInventory(); len(inv) != 1 || len(inv["srv-a"]) != 1 || inv["srv-a"][0] != id {
		t.Fatalf("inventory after the create: %v", inv)
	}
	// Not yet due: still creating.
	if err := srv.ComputeTick(); err != nil {
		t.Fatal(err)
	}
	if _, v, _ := do("GET", "/v1/compute/machines/"+id, ""); v["machine"].(map[string]any)["state"] != "creating" {
		t.Fatalf("settled before auto_settle_s: %v", v)
	}
	// A clock step moves the compute clock past auto_settle_s: the machine
	// runs, with the control file's host key.
	write("compute.json", `{"steps":[{"id":"1","step":{"compute.allow":"srv-a"}},{"id":"2","step":{"clock.advance":6}}],"auto_settle_s":5,"host_key":"`+key+`"}`)
	if err := srv.ComputeTick(); err != nil {
		t.Fatal(err)
	}
	_, v, body := do("GET", "/v1/compute/machines/"+id, "")
	m := v["machine"].(map[string]any)
	access, _ := m["access"].([]any)
	if m["state"] != "running" || len(access) != 1 || access[0].(map[string]any)["host_key"] != key {
		t.Fatalf("after the clock step: %s", body)
	}
	// A forced reply, while the file names it.
	write("compute.json", `{"steps":[{"id":"1","step":{"compute.allow":"srv-a"}},{"id":"2","step":{"clock.advance":6}}],"replies":{"catalog":{"status":418,"text":"teapot"}}}`)
	if code, _, body := do("GET", "/v1/compute/catalog", ""); code != 418 || body != "teapot" {
		t.Fatalf("forced reply: %d %q", code, body)
	}
	// A step with a new id is applied; old ids are not applied again, so the
	// deny stands though the allow is still in the file.
	write("compute.json", `{"steps":[{"id":"1","step":{"compute.allow":"srv-a"}},{"id":"3","step":{"compute.deny":"srv-a"}}]}`)
	if code, _, _ := do("GET", "/v1/compute/catalog", ""); code != http.StatusForbidden {
		t.Fatalf("after compute.deny: %d, want 403", code)
	}
	// A malformed file: every compute call is 503 until it is fixed.
	write("compute.json", `{"steps":[],"unknown":true}`)
	if code, _, _ := do("GET", "/v1/compute/catalog", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("a malformed compute.json: %d, want 503", code)
	}
	if err := srv.ComputeTick(); err == nil {
		t.Error("ComputeTick read a malformed compute.json")
	}
}

// TestComputeStepsRefuseMalformed holds the set-up steps to their shapes.
func TestComputeStepsRefuseMalformed(t *testing.T) {
	h := newHarness(t)
	s := h.newScenario(t, &exchange{Name: "steps"})
	for name, raw := range map[string]string{
		"compute.allow":     `"srv-z"`,
		"client.register":   `["srv-a"]`,
		"quota.fill":        `["team"]`,
		"cloud.fail":        `["delete","quota"]`,
		"cloud.settle":      `"q6c3g4ymb5m2dkxwe7kyz5hhzu"`,
		"guest.publish_key": `["srv-a","q6c3g4ymb5m2dkxwe7kyz5hhzu","ssh-rsa AAAA"]`,
		"clock.advance":     `-1`,
		"policy.set":        `{"person_max":1,"bogus":2}`,
		"no.such":           `1`,
	} {
		if err := s.srv.applyComputeStep(name, json.RawMessage(raw), func(time.Duration) {}); err == nil {
			t.Errorf("%s %s: accepted", name, raw)
		}
	}
	// Every step keyed by a shed id takes the server too: a pair that names
	// no machine is refused, and so is the old form without the server.
	id := "q6c3g4ymb5m2dkxwe7kyz5hhzu"
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOTs41aR1tvqm53eXWSYYaVa4vzPYlXSTKnKr5nW/t9C"
	s.srv.compute.machines[mkey{"srv-a", id}] = &cmachine{server: "srv-a", shedID: id, state: stateRunning, labels: map[string]string{}}
	for _, st := range []struct{ name, raw string }{
		{"machine.delete", `["srv-b","` + id + `","page"]`},
		{"machine.delete", `["` + id + `","page"]`},
		{"cloud.settle", `["srv-b","` + id + `"]`},
		{"guest.publish_key", `["srv-b","` + id + `","` + key + `"]`},
		{"guest.publish_key", `["` + id + `","` + key + `"]`},
	} {
		if err := s.srv.applyComputeStep(st.name, json.RawMessage(st.raw), nil); err == nil {
			t.Errorf("%s %s: accepted", st.name, st.raw)
		}
	}
	// The server's own pair is taken, and deletes only that machine.
	s.srv.compute.machines[mkey{"srv-b", id}] = &cmachine{server: "srv-b", shedID: id, state: stateRunning, labels: map[string]string{}}
	if err := s.srv.applyComputeStep("machine.delete", json.RawMessage(`["srv-a","`+id+`","page"]`), nil); err != nil {
		t.Fatal(err)
	}
	if s.srv.compute.machines[mkey{"srv-a", id}] != nil || s.srv.compute.machines[mkey{"srv-b", id}] == nil {
		t.Error("machine.delete did not delete exactly srv-a's machine")
	}
}
