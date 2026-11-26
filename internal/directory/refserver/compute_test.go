package refserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The compute service, held to the compute contract's fixtures: every
// exchange of fixtures/exchanges runs against the reference service in the
// directory contract's world (with the compute world beside it), and every
// reply of fixtures/replies the service can be told to send is sent and
// compared (compute_replies_test.go).

// computeDir is tests/compute-contract/v1 from this package's directory.
const computeDir = "../../../tests/compute-contract/v1"

// operationNameRx names the {{operation_N}} placeholders.
var operationNameRx = regexp.MustCompile(`^operation_[1-9][0-9]*$`)

func readCompute(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(computeDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func computeFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(computeDir, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			out = append(out, dir+"/"+e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no .json files in %s", dir)
	}
	sort.Strings(out)
	return out
}

// resolvePath fills a request path's {{operation_N}} with its bound value.
func (s *scenario) resolvePath(p string) string {
	if !strings.Contains(p, "{{") {
		return p
	}
	if !s.compute {
		s.t.Fatalf("a placeholder in the path %q", p)
	}
	return regexp.MustCompile(`\{\{([a-z_0-9]+)\}\}`).ReplaceAllStringFunc(p, func(m string) string {
		name := m[2 : len(m)-2]
		v, ok := s.ops[name]
		if !operationNameRx.MatchString(name) || !ok {
			s.t.Fatalf("%s in a path is not a bound {{operation_N}}", m)
		}
		return v
	})
}

// bindOperation applies the {{operation_N}} rule: an operation id under
// the grammar; its first appearance binds it, every later one must be
// equal, and two different N are different values.
func (s *scenario) bindOperation(where, name, got string) {
	if !operationRx.MatchString(got) {
		s.t.Errorf("%s: %q is not an operation id", where, got)
	}
	if s.ops == nil {
		s.ops = map[string]string{}
	}
	if prev, ok := s.ops[name]; ok {
		if prev != got {
			s.t.Errorf("%s: {{%s}} is %q, bound to %q", where, name, got, prev)
		}
		return
	}
	for n, v := range s.ops {
		if v == got {
			s.t.Errorf("%s: {{%s}} is %q, the value of {{%s}}", where, name, got, n)
		}
	}
	s.ops[name] = got
}

// TestComputeExchanges runs every compute exchange against the reference
// service.
func TestComputeExchanges(t *testing.T) {
	h := newHarness(t)
	n := 0
	for _, rel := range computeFiles(t, "fixtures/exchanges") {
		raw := readCompute(t, rel)
		n++
		t.Run(strings.TrimSuffix(filepath.Base(rel), ".json"), func(t *testing.T) {
			var ex exchange
			decodeClosed(t, rel, raw, &ex)
			if ex.Name != strings.TrimSuffix(filepath.Base(rel), ".json") {
				t.Fatalf("name %q is not the file's", ex.Name)
			}
			s := h.newScenario(t, &ex)
			s.compute = true
			s.run(&ex)
		})
	}
	if n < 40 {
		t.Fatalf("only %d exchanges", n)
	}
}

// TestComputeWorldIsTheDefault holds the reference service's own world
// equal to the contract's fixtures/world.json, which it starts from.
func TestComputeWorldIsTheDefault(t *testing.T) {
	raw := readCompute(t, "fixtures/world.json")
	var w ComputeWorld
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("world.json: %v", err)
	}
	if want := DefaultComputeWorld(); !reflect.DeepEqual(w, want) {
		t.Errorf("world.json is\n%+v\nthe reference service starts from\n%+v", w, want)
	}
}

// TestComputeHostKeyMismatchLogged: a later, different host key leaves the
// first in place and logs the fixed mismatch line (with the shed id and no
// key); the same key published again logs nothing.
func TestComputeHostKeyMismatchLogged(t *testing.T) {
	h := newHarness(t)
	s := h.newScenario(t, &exchange{Name: "mismatch"})
	var lines []string
	s.srv.compute.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	const id = "q6c3g4ymb5m2dkxwe7kyz5hhzu"
	keyA := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOTs41aR1tvqm53eXWSYYaVa4vzPYlXSTKnKr5nW/t9C"
	keyB := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINuwJ0bFhq+q03EoO6dPkxG3KlHZ8Nd5XizdDiB//C19"
	s.srv.compute.machines[mkey{"srv-a", id}] = &cmachine{server: "srv-a", shedID: id, state: stateRunning, labels: map[string]string{}}
	step := func(key string) {
		raw, _ := json.Marshal([]string{"srv-a", id, key})
		if err := s.srv.applyComputeStep("guest.publish_key", raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	step(keyA)
	step(keyA)
	if len(lines) != 0 {
		t.Fatalf("logged %q for the same key", lines)
	}
	step(keyB)
	if len(lines) != 1 || lines[0] != HostKeyMismatchLine+": "+id {
		t.Fatalf("logged %q", lines)
	}
	if strings.Contains(lines[0], "AAAA") {
		t.Error("the line holds key material")
	}
	if got := s.srv.compute.machines[mkey{"srv-a", id}].hostKey; got != keyA {
		t.Errorf("kept %q", got)
	}
}

// TestComputeListLimits: a list of more than 500 machines, or one whose
// body would pass 4 MiB, is never sent: the service answers 503
// UNAVAILABLE.
func TestComputeListLimits(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	fill := func(n int, spec string) *computeState {
		s := h.newScenario(t, &exchange{Name: "limits"})
		c := s.srv.compute
		for i := 0; i < n; i++ {
			m := &cmachine{server: "srv-a", shedID: seriesID(i), name: "m", user: "ada", state: stateStopped,
				size: c.world.Catalog.Sizes[0], diskGB: 50, imageReq: "default", imageRes: "x",
				created: now, expires: now.Add(time.Hour), maxExpires: now.Add(time.Hour), labels: map[string]string{}, spec: spec}
			c.machines[mkey{"srv-a", m.shedID}] = m
		}
		return c
	}
	for _, tc := range []struct {
		name string
		n    int
		spec string
		ok   bool
	}{
		{"500 machines", 500, "", true},
		{"501 machines", 501, "", false},
		// A 4096-byte spec of NULs is 24 KiB once escaped: 200 machines pass
		// 4 MiB.
		{"over 4 MiB", 200, strings.Repeat("\x00", 4096), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fill(tc.n, tc.spec)
			body, r := c.listJSON("srv-a", now)
			switch {
			case tc.ok && (r != nil || len(body) > maxComputeList):
				t.Fatalf("refused: %v", r)
			case !tc.ok && (r == nil || r.status != 503 || r.code != "UNAVAILABLE"):
				t.Fatalf("answered %d bytes, refusal %v", len(body), r)
			}
		})
	}
}
