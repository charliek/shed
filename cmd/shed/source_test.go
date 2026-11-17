package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

// `shed source ls|use|status` against an httptest server, driven through
// cobra's own argument parsing (rootCmd.Execute), as a shell would.

var (
	srcLaptop = config.SourceMachine{ID: "9a60b03902c5bf02d5b6da8f8cc14657", Name: "laptop", KeyFingerprint: "SHA256:laptopkey", FirstSeen: time.Now().Add(-48 * time.Hour), Connected: true, Namespaces: []string{"docker-credential", "ssh-agent"}}
	srcDesk   = config.SourceMachine{ID: "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1", Name: "Zo\u00eb\u2019s desk", KeyFingerprint: "SHA256:deskkey", FirstSeen: time.Now().Add(-time.Hour), Namespaces: []string{}}
	srcDash   = config.SourceMachine{ID: "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2", Name: "-odd name", Namespaces: []string{}}
	srcEq     = config.SourceMachine{ID: "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3", Name: "=x", Namespaces: []string{}}
	srcSpace  = config.SourceMachine{ID: "e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4", Name: "work laptop 2", Namespaces: []string{}}
	// srcSelf's name is a prefix of its own id: one machine matched two ways.
	srcSelf = config.SourceMachine{ID: "a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7", Name: "a7a7a7a7", Namespaces: []string{}}
)

func srcBinding(id string) *config.SourceBinding {
	ref := []config.SourceRef{{Kind: "machine", ID: id}}
	return &config.SourceBinding{Delivery: ref, Signing: ref, BoundAt: time.Now().Add(-time.Hour), BoundBy: "first-connect", Generation: 3}
}

// fakeSourcesServer answers the two source routes from resp, and records
// what it was asked.
type fakeSourcesServer struct {
	mu   sync.Mutex
	resp config.SourcesResponse
	// old answers every source route 404 with no API error, as a server
	// from before the routes does.
	old bool
	// refuse, when set, is the status and error every route answers.
	refuseStatus int
	refuse       config.APIError
	// useRefuse, when set, is what POST answers.
	useStatus int
	useErr    config.APIError
	queries   []string
	uses      []map[string]any
	// beforeUse, when set, runs as a POST arrives, before it moves (with the
	// fake's lock held): another client's move that lands between the CLI's
	// list and its move.
	beforeUse func(f *fakeSourcesServer)
}

func (f *fakeSourcesServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.old {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if f.refuseStatus != 0 {
		w.WriteHeader(f.refuseStatus)
		_ = json.NewEncoder(w).Encode(f.refuse)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/sources":
		f.queries = append(f.queries, r.URL.RawQuery)
		resp := f.resp
		if u := r.URL.Query().Get("user"); u != "" {
			resp.User = u
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodPost && r.URL.Path == "/api/sources/use":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.uses = append(f.uses, body)
		if f.useStatus != 0 {
			w.WriteHeader(f.useStatus)
			_ = json.NewEncoder(w).Encode(f.useErr)
			return
		}
		if f.beforeUse != nil {
			f.beforeUse(f)
		}
		// As the server does: a move to another machine takes a new
		// generation; a move to the machine already bound changes nothing.
		id, _ := body["machine_id"].(string)
		if f.resp.Binding.MachineID() != id {
			var gen uint64 = 1
			if f.resp.Binding != nil {
				gen = f.resp.Binding.Generation + 1
			}
			f.resp.Binding = srcBinding(id)
			f.resp.Binding.Generation = gen
			f.resp.Binding.BoundBy = "user alice with key SHA256:x"
		}
		_ = json.NewEncoder(w).Encode(f.resp)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSourcesServer) usesSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.uses...)
}

func (f *fakeSourcesServer) queriesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// sourcesRig is a client config whose default server "srv" is a fake.
func sourcesRig(t *testing.T, machines ...config.SourceMachine) *fakeSourcesServer {
	t.Helper()
	testClientConfig(t)
	f := &fakeSourcesServer{resp: config.SourcesResponse{User: "alice", Machines: machines}}
	if f.resp.Machines == nil {
		f.resp.Machines = []config.SourceMachine{}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	putServerEntry(t, "srv", config.ServerEntry{APIURL: srv.URL})
	if err := clientConfig.Update(func(c *config.ClientConfig) { c.DefaultServer = "srv" }); err != nil {
		t.Fatal(err)
	}
	return f
}

// runShed runs the CLI with args through cobra and returns what it wrote to
// stdout. Flags are package state: they are put back after the test.
func runShed(t *testing.T, args ...string) (string, error) {
	t.Helper()
	user, asJSON, server, cfgFlag := sourceUserFlag, jsonFlag, serverFlag, configFlag
	t.Cleanup(func() {
		sourceUserFlag, jsonFlag, serverFlag, configFlag = user, asJSON, server, cfgFlag
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	})
	sourceUserFlag, jsonFlag, serverFlag, configFlag = "", false, "", ""
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)
	return out.String(), err
}

func TestSourceLs(t *testing.T) {
	f := sourcesRig(t, srcLaptop, srcDesk)
	f.resp.Binding = srcBinding(srcLaptop.ID)

	out, err := runShed(t, "source", "ls")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("ls printed %d lines, want a header and two rows:\n%s", len(lines), out)
	}
	for _, col := range []string{"MACHINE", "ID", "KEY", "BOUND", "CONNECTED", "FIRST SEEN"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q lacks %s", lines[0], col)
		}
	}
	if !strings.Contains(lines[1], "laptop") || !strings.Contains(lines[1], "9a60b039 ") || strings.Contains(lines[1], srcLaptop.ID) ||
		!strings.Contains(lines[1], "SHA256:laptopkey") || !strings.Contains(lines[1], "yes    yes") || !strings.Contains(lines[1], "2 days ago") {
		t.Errorf("bound row = %q", lines[1])
	}
	if !strings.Contains(lines[2], "Zo\u00eb\u2019s desk") || !strings.Contains(lines[2], "b1b1b1b1 ") || !strings.Contains(lines[2], "no     no") {
		t.Errorf("other row = %q", lines[2])
	}

	out, err = runShed(t, "source", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got config.SourcesResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, out)
	}
	if got.User != "alice" || got.Binding.MachineID() != srcLaptop.ID || len(got.Machines) != 2 || got.Machines[1].ID != srcDesk.ID || got.Machines[1].Name != srcDesk.Name {
		t.Fatalf("--json = %+v", got)
	}

	// Nobody yet.
	f.resp = config.SourcesResponse{User: "alice", Machines: []config.SourceMachine{}}
	if out, err = runShed(t, "source", "ls"); err != nil || !strings.Contains(out, "No machine has presented itself for you on srv yet") {
		t.Fatalf("empty ls = %q, %v", out, err)
	}
}

// TestSourceUseResolves: every way of naming a machine, through cobra's own
// argument parsing.
func TestSourceUseResolves(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want config.SourceMachine
	}{
		{"full id", []string{srcDesk.ID}, srcDesk},
		{"id prefix of 8", []string{srcDesk.ID[:8]}, srcDesk},
		{"id prefix in capitals", []string{strings.ToUpper(srcDesk.ID[:10])}, srcDesk},
		{"name", []string{"laptop"}, srcLaptop},
		{"non-ASCII name", []string{"Zo\u00eb\u2019s desk"}, srcDesk},
		{"name with spaces", []string{"work laptop 2"}, srcSpace},
		{"name after --", []string{"--", "laptop"}, srcLaptop},
		{"name beginning with - after --", []string{"--", "-odd name"}, srcDash},
		{"name beginning with =", []string{"=x"}, srcEq},
		{"name beginning with = after --", []string{"--", "=x"}, srcEq},
		{"one machine matched by name and id prefix", []string{"a7a7a7a7"}, srcSelf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sourcesRig(t, srcLaptop, srcDesk, srcDash, srcEq, srcSpace, srcSelf)
			f.resp.Binding = srcBinding(srcLaptop.ID)
			args := append([]string{"-s", "srv", "source", "use"}, tc.args...)
			out, err := runShed(t, args...)
			if err != nil {
				t.Fatalf("use %q: %v", tc.args, err)
			}
			uses := f.usesSnapshot()
			if len(uses) != 1 || uses[0]["machine_id"] != tc.want.ID {
				t.Fatalf("use %q posted %v, want machine %s", tc.args, uses, tc.want.ID)
			}
			if _, named := uses[0]["user"]; named {
				t.Fatalf("use without --user named a user: %v", uses[0])
			}
			if tc.want.ID == srcLaptop.ID {
				if !strings.Contains(out, "is already the credential source for you on srv") {
					t.Fatalf("output = %q", out)
				}
				return
			}
			if !strings.Contains(out, "is now the credential source for you on srv") || !strings.Contains(out, `"laptop" (9a60b039) no longer answers`) {
				t.Fatalf("output = %q", out)
			}
		})
	}
}

// TestSourceUseRefusesWithoutChanging: an ambiguous name or prefix lists the
// candidates, and an unknown machine is named; none of them posts anything.
func TestSourceUseRefusesWithoutChanging(t *testing.T) {
	twinA := config.SourceMachine{ID: "aaaaaaaa11111111111111111111111a", Name: "twin", Namespaces: []string{}}
	twinB := config.SourceMachine{ID: "aaaaaaaa22222222222222222222222b", Name: "twin", Namespaces: []string{}}
	nameIsPrefix := config.SourceMachine{ID: "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f", Name: "b1b1b1b1", Namespaces: []string{}}
	nameIsID := config.SourceMachine{ID: "1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e", Name: srcDesk.ID, Namespaces: []string{}}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"ambiguous name", []string{"twin"}, []string{`"twin" matches 2 machines`, twinA.ID + "  twin  (matched by name)", twinB.ID + "  twin  (matched by name)", "nothing changed on srv"}},
		{"ambiguous prefix", []string{"aaaaaaaa"}, []string{`"aaaaaaaa" matches 2 machines`, twinA.ID + "  twin  (matched by id prefix)", twinB.ID + "  twin  (matched by id prefix)"}},
		// Across kinds: one machine's name is another's id prefix, or
		// another's full id.
		{"a name that is another machine's id prefix", []string{"b1b1b1b1"}, []string{`"b1b1b1b1" matches 2 machines`, srcDesk.ID + "  " + srcDesk.Name + "  (matched by id prefix)", nameIsPrefix.ID + "  b1b1b1b1  (matched by name)", "nothing changed on srv"}},
		{"a name that is another machine's full id", []string{srcDesk.ID}, []string{"matches 2 machines", srcDesk.ID + "  " + srcDesk.Name + "  (matched by id)", nameIsID.ID + "  " + srcDesk.ID + "  (matched by name)"}},
		{"unknown", []string{"nowhere"}, []string{`no machine named "nowhere"`, "shed source ls", "nothing changed"}},
		{"prefix shorter than 8", []string{srcDesk.ID[:7]}, []string{"no machine named"}},
		{"name differing in case", []string{"LAPTOP"}, []string{"no machine named"}},
		{"name beginning with - without --", []string{"-odd name"}, []string{"unknown shorthand flag"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sourcesRig(t, srcLaptop, srcDesk, twinA, twinB, srcDash, nameIsPrefix, nameIsID)
			f.resp.Binding = srcBinding(srcLaptop.ID)
			args := append([]string{"source", "use"}, tc.args...)
			_, err := runShed(t, args...)
			if err == nil {
				t.Fatalf("use %q succeeded", tc.args)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if uses := f.usesSnapshot(); len(uses) != 0 {
				t.Fatalf("a refused use posted %v", uses)
			}
		})
	}
}

func TestSourceStatus(t *testing.T) {
	f := sourcesRig(t, srcLaptop, srcDesk)

	f.resp.Binding = srcBinding(srcLaptop.ID)
	out, err := runShed(t, "source", "status")
	if err != nil || !strings.Contains(out, `Credential source for you on srv: "laptop" (9a60b039), connected (docker-credential, ssh-agent).`) ||
		!strings.Contains(out, "by the first machine to connect") || strings.Count(out, "\n") != 2 {
		t.Fatalf("connected status = %q, %v", out, err)
	}

	f.resp.Binding = srcBinding(srcDesk.ID)
	out, err = runShed(t, "source", "status")
	if err != nil || !strings.Contains(out, "\"Zo\u00eb\u2019s desk\" (b1b1b1b1), not connected") {
		t.Fatalf("disconnected status = %q, %v", out, err)
	}

	f.resp.Binding = nil
	out, err = runShed(t, "source", "status")
	if err != nil {
		t.Fatalf("no binding is not an error: %v", err)
	}
	if !strings.Contains(out, "No credential source for you on srv yet") {
		t.Fatalf("no-binding status = %q", out)
	}

	f.resp.Binding = srcBinding(srcLaptop.ID)
	out, err = runShed(t, "source", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st sourceStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if st.Server != "srv" || st.User != "alice" || st.Source == nil || st.Source.ID != srcLaptop.ID || !st.Source.Connected || len(st.Source.Namespaces) != 2 {
		t.Fatalf("status --json = %+v", st)
	}
	f.resp.Binding = nil
	if out, err = runShed(t, "source", "status", "--json"); err != nil || !strings.Contains(out, `"source": null`) {
		t.Fatalf("no-binding status --json = %q, %v", out, err)
	}
}

// TestSourceUserFlag: --user is sent as the query parameter and the body
// field; a malformed name is refused before anything is sent.
func TestSourceUserFlag(t *testing.T) {
	f := sourcesRig(t, srcLaptop, srcDesk)
	f.resp.Binding = srcBinding(srcLaptop.ID)
	out, err := runShed(t, "source", "status", "--user", "bob")
	if err != nil || !strings.Contains(out, "Credential source for user bob on srv") {
		t.Fatalf("status --user = %q, %v", out, err)
	}
	if _, err := runShed(t, "source", "ls", "--user", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := runShed(t, "source", "use", "--user", "bob", "--", srcDesk.Name); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.queriesSnapshot() {
		if q != "user=bob" {
			t.Fatalf("queries = %v, want user=bob on each", f.queriesSnapshot())
		}
	}
	if uses := f.usesSnapshot(); len(uses) != 1 || uses[0]["user"] != "bob" || uses[0]["machine_id"] != srcDesk.ID {
		t.Fatalf("use --user posted %v", uses)
	}
	n := len(f.queriesSnapshot())
	if _, err := runShed(t, "source", "ls", "--user", "Not A Name"); err == nil || !strings.Contains(err.Error(), "not a valid user name") {
		t.Fatalf("malformed --user = %v", err)
	}
	if len(f.queriesSnapshot()) != n {
		t.Fatal("a malformed --user reached the server")
	}
}

// TestSourceOlderServer: a server without the routes is named as not
// supporting credential sources, by every command.
func TestSourceOlderServer(t *testing.T) {
	f := sourcesRig(t)
	f.old = true
	for _, args := range [][]string{{"source", "ls"}, {"source", "status"}, {"source", "use", "laptop"}} {
		_, err := runShed(t, args...)
		if err == nil || !strings.Contains(err.Error(), "server srv does not support credential sources") {
			t.Errorf("%v against an older server = %v", args, err)
		}
	}
}

// TestSourceRefusalsShown: the server's refusals reach the user with their
// code and message, worded like `shed assign`'s.
func TestSourceRefusalsShown(t *testing.T) {
	f := sourcesRig(t, srcLaptop, srcDesk)
	f.refuseStatus = http.StatusForbidden
	f.refuse = config.NewAPIError(config.ErrForbidden, "only an admin can see or move another user's credential source")
	_, err := runShed(t, "source", "ls", "--user", "bob")
	if err == nil || err.Error() != "list credential sources on srv: FORBIDDEN: only an admin can see or move another user's credential source" {
		t.Fatalf("403 = %v", err)
	}

	f.refuseStatus = 0
	f.useStatus = http.StatusNotFound
	f.useErr = config.NewAPIError(config.ErrSourceMachineNotFound, "no machine with id b1b1 has presented itself")
	_, err = runShed(t, "source", "use", srcDesk.ID)
	if err == nil || !strings.Contains(err.Error(), "move the credential source on srv: SOURCE_MACHINE_NOT_FOUND") || strings.Contains(err.Error(), "does not support") {
		t.Fatalf("404 from the route = %v", err)
	}
}

// TestSourceNamesNeverReachTheTerminalRaw: control characters, a
// bidirectional override, a zero-width character and a line separator in a
// machine name (or key) are escaped in every output, JSON included.
func TestSourceNamesNeverReachTheTerminalRaw(t *testing.T) {
	evil := config.SourceMachine{ID: "f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5", Name: "\x1b[31mred\u202egnp.exe\u200b\u2028x\t", KeyFingerprint: "SHA256:\x07bell", Connected: true, Namespaces: []string{"ssh\u202eagent"}}
	twin := config.SourceMachine{ID: "f5f5f5f5aaaaaaaaaaaaaaaaaaaaaaaa", Name: evil.Name, Namespaces: []string{}}
	f := sourcesRig(t, evil, twin)
	f.resp.Binding = srcBinding(evil.ID)
	f.resp.Binding.BoundBy = "user alice with key \u202e"
	raw := []string{"\x1b", "\u202e", "\u200b", "\u2028", "\x07", "\t"}
	check := func(what, out string) {
		t.Helper()
		for _, r := range raw {
			if strings.Contains(out, r) {
				t.Errorf("%s writes %q raw:\n%q", what, r, out)
			}
		}
	}
	for _, args := range [][]string{
		{"source", "ls"},
		{"source", "ls", "--json"},
		{"source", "status"},
		{"source", "status", "--json"},
		{"source", "use", "--json", evil.ID},
		{"source", "use", twin.ID},
	} {
		out, err := runShed(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		check(strings.Join(args, " "), out)
	}
	out, _ := runShed(t, "source", "ls")
	if !strings.Contains(out, `\u001b[31mred\u202egnp.exe\u200b\u2028x\u0009`) {
		t.Errorf("ls does not show the name escaped:\n%s", out)
	}
	out, _ = runShed(t, "source", "ls", "--json")
	var got config.SourcesResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Machines[0].Name != evil.Name {
		t.Errorf("--json does not decode to the exact name: %v %+v", err, got.Machines)
	}
	// An ambiguous name lists the machines escaped.
	_, err := runShed(t, "source", "use", "--", evil.Name)
	if err == nil {
		t.Fatal("an ambiguous name was accepted")
	}
	check("the ambiguity error", err.Error())
}

func TestTermSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain name":            "plain name",
		"Zo\u00eb\u2019s Mac":   "Zo\u00eb\u2019s Mac",
		"a\x1bb":                `a\u001bb`,
		"rtl\u202eoverride":     `rtl\u202eoverride`,
		"zero\u200bwidth":       `zero\u200bwidth`,
		"bom\ufeff":             `bom\ufeff`,
		"line\u2028sep":         `line\u2028sep`,
		"bad\xffutf8":           `bad\xffutf8`,
		"nbsp\u00a0":            `nbsp\u00a0`,
		"tag\U000E0041":         `tag\U000e0041`,
		"emoji \U0001F600 fine": "emoji \U0001F600 fine",
	} {
		if got := termSafe(in); got != want {
			t.Errorf("termSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// captureStderr runs fn and returns what it wrote to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

// TestAssignWithoutServerSendsOnlyWhereTheShedIsSeen: without --server an
// assignment is sent only to a server whose lookup shows the shed; when none
// does (it is hidden from the caller, missing, or the server failed to
// answer) the one error names the shed and suggests --server, nothing else
// is printed and nothing is sent. With --server it is sent directly and the
// server's answer is the one message (403 to a user who is not an admin,
// 404 to an admin for a shed that does not exist).
func TestAssignWithoutServerSendsOnlyWhereTheShedIsSeen(t *testing.T) {
	var (
		mu      sync.Mutex
		status  int
		answer  config.APIError
		visible bool // GET /api/sheds/box shows the shed
		broken  bool // every GET fails with a server error
		posts   int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sheds/box/assign":
			posts++
			w.WriteHeader(status)
			if status == http.StatusOK {
				_ = json.NewEncoder(w).Encode(config.Shed{Name: "box", User: "bob"})
				return
			}
			_ = json.NewEncoder(w).Encode(answer)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sheds/box" && broken:
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(config.NewAPIError(config.ErrInternalError, "boom"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/sheds/box" && visible:
			_ = json.NewEncoder(w).Encode(config.Shed{Name: "box"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(config.NewAPIError(config.ErrShedNotFound, "shed not found: box"))
		}
	}))
	defer srv.Close()
	testClientConfig(t)
	putServerEntry(t, "srv", config.ServerEntry{APIURL: srv.URL})
	if err := clientConfig.Update(func(c *config.ClientConfig) { c.DefaultServer = "srv" }); err != nil {
		t.Fatal(err)
	}
	const notShown = "assign shed box: no configured server shows a shed named box to you (an unassigned shed is visible only to an admin, and a server that could not be reached shows nothing), so nothing was sent; to send the assignment to a server and see its answer, name it with -s/--server"
	forbidden := config.NewAPIError(config.ErrForbidden, "admin only: assigning a shed to a user")
	notFound := config.NewAPIError(config.ErrShedNotFound, "shed not found: box")

	for _, tc := range []struct {
		name            string
		args            []string
		visible, broken bool
		status          int
		answer          config.APIError
		want            string // the error; "" for success
		wantPosts       int
	}{
		{"non-admin, no -s: the shed is hidden", []string{"assign", "box", "bob"}, false, false, http.StatusForbidden, forbidden, notShown, 0},
		{"admin, no -s, no such shed", []string{"assign", "box", "bob"}, false, false, http.StatusNotFound, notFound, notShown, 0},
		{"no -s, every lookup failed", []string{"assign", "box", "bob"}, false, true, http.StatusOK, config.APIError{}, notShown, 0},
		{"no -s, the lookup found it", []string{"assign", "box", "bob"}, true, false, http.StatusOK, config.APIError{}, "", 1},
		{"non-admin with -s", []string{"-s", "srv", "assign", "box", "bob"}, false, false, http.StatusForbidden, forbidden,
			"assign shed box to bob on srv: FORBIDDEN: admin only: assigning a shed to a user", 1},
		{"admin with -s, no such shed", []string{"-s", "srv", "assign", "box", "bob"}, false, false, http.StatusNotFound, notFound,
			"assign shed box to bob on srv: SHED_NOT_FOUND: shed not found: box", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			status, answer, visible, broken, posts = tc.status, tc.answer, tc.visible, tc.broken, 0
			mu.Unlock()
			clientConfig.RemoveShedCache("box")
			var err error
			stderr := captureStderr(t, func() {
				captureStdout(t, func() { _, err = runShed(t, tc.args...) })
			})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("assign = %v, want success", err)
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("assign = %v, want %q", err, tc.want)
			}
			if stderr != "" {
				t.Fatalf("assign also printed %q: the error must be the one message", stderr)
			}
			mu.Lock()
			got := posts
			mu.Unlock()
			if got != tc.wantPosts {
				t.Fatalf("assign sent %d assignment(s), want %d", got, tc.wantPosts)
			}
		})
	}
}

// TestSourceUseSaysWhatItsOwnMoveDid: the message of `shed source use` comes
// from the POST's answer, not the list read before it. Here the list shows
// the laptop bound; another client moves the binding to the desk before this
// POST arrives, and this POST moves it back: the laptop is newly the source,
// not "already" it.
func TestSourceUseSaysWhatItsOwnMoveDid(t *testing.T) {
	f := sourcesRig(t, srcLaptop, srcDesk)
	f.resp.Binding = srcBinding(srcLaptop.ID)
	f.beforeUse = func(f *fakeSourcesServer) {
		gen := f.resp.Binding.Generation + 1
		f.resp.Binding = srcBinding(srcDesk.ID)
		f.resp.Binding.Generation = gen
	}
	out, err := runShed(t, "source", "use", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "already") || !strings.Contains(out, `Machine "laptop" (9a60b039) is now the credential source for you on srv.`) {
		t.Fatalf("use after another client's move = %q, want it reported as a move", out)
	}
	if strings.Contains(out, `"laptop" (9a60b039) no longer answers`) {
		t.Fatalf("use named the new source as the one that no longer answers: %q", out)
	}

	// With nothing in between, the same request is a no-op and says so.
	f.beforeUse = nil
	if out, err = runShed(t, "source", "use", "laptop"); err != nil || !strings.Contains(out, "is already the credential source") {
		t.Fatalf("use of the bound machine = %q, %v", out, err)
	}
}

// runShedC is runShed that also returns the command cobra ran, as main has
// it for reportError.
func runShedC(t *testing.T, args ...string) (*cobra.Command, error) {
	t.Helper()
	user, asJSON, server, cfgFlag := sourceUserFlag, jsonFlag, serverFlag, configFlag
	t.Cleanup(func() {
		sourceUserFlag, jsonFlag, serverFlag, configFlag = user, asJSON, server, cfgFlag
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	})
	sourceUserFlag, jsonFlag, serverFlag, configFlag = "", false, "", ""
	rootCmd.SetOut(io.Discard)
	rootCmd.SetArgs(args)
	cmd, err := rootCmd.ExecuteC()
	rootCmd.SetArgs(nil)
	return cmd, err
}

// TestSourceFlagErrorsAreEscaped: a machine name that begins with "--" and
// holds a bidirectional override and a zero-width character, given to
// `shed source use` without the -- separator, is refused by the flag parser;
// the error main prints is escaped like the command's other output (and so is
// the JSON error), and tells the user to put -- before such a name. Nothing
// is sent.
func TestSourceFlagErrorsAreEscaped(t *testing.T) {
	evil := "--bad\u202egnp.exe\u200b"
	f := sourcesRig(t, srcLaptop)
	raw := []string{"\u202e", "\u200b"}
	for _, tc := range []struct {
		name string
		args []string
		json bool
	}{
		{"text", []string{"source", "use", evil}, false},
		{"json", []string{"source", "use", "--json", evil}, true},
		{"text, after --user", []string{"source", "use", "--user", "bob", evil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cmd *cobra.Command
			var err error
			var asJSON bool
			stderr := captureStderr(t, func() {
				cmd, err = runShedC(t, tc.args...)
				asJSON = jsonFlag
				if err != nil {
					reportError(cmd, err)
				}
			})
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if asJSON != tc.json {
				t.Fatalf("test set-up: --json parsed = %v", asJSON)
			}
			for _, r := range raw {
				if strings.Contains(stderr, r) {
					t.Errorf("the error writes %q raw:\n%q", r, stderr)
				}
			}
			if !strings.Contains(stderr, `\u202egnp.exe\u200b`) && !strings.Contains(stderr, `\\u202egnp.exe\\u200b`) {
				t.Errorf("the error does not show the argument escaped:\n%s", stderr)
			}
			const hint = "put -- before a machine name that begins with '-': shed source use -- <name>"
			msg := stderr
			if tc.json {
				var obj struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal([]byte(stderr), &obj); err != nil || !strings.Contains(obj.Error, "unknown flag") {
					t.Errorf("the JSON error is not JSON naming the flag: %v\n%s", err, stderr)
				}
				msg = obj.Error
			} else if !strings.HasPrefix(stderr, "Error: unknown flag: ") {
				t.Errorf("text error = %q", stderr)
			}
			if !strings.Contains(msg, hint) {
				t.Errorf("the error lacks the hint to use --:\n%s", stderr)
			}
			if uses := f.usesSnapshot(); len(uses) != 0 {
				t.Fatalf("a refused argument posted %v", uses)
			}
		})
	}
	// Errors of the source commands' own making keep their line breaks.
	stderr := captureStderr(t, func() { reportError(sourceUseCmd, errors.New("a\u202eb\nsecond line")) })
	if stderr != "Error: a\\u202eb\nsecond line\n" {
		t.Errorf("a source error of two lines = %q", stderr)
	}
	// A flag error on another source command is escaped but carries no
	// machine-name hint.
	stderr = captureStderr(t, func() {
		cmd, err := runShedC(t, "source", "ls", evil)
		if err == nil {
			t.Error("ls accepted an unknown flag")
			return
		}
		reportError(cmd, err)
	})
	if strings.Contains(stderr, "\u202e") || strings.Contains(stderr, "shed source use --") {
		t.Errorf("ls flag error = %q", stderr)
	}
}

// TestReportErrorLeavesOtherCommandsAlone: the escaping is the source
// commands' only; another command's error is printed as it always was.
func TestReportErrorLeavesOtherCommandsAlone(t *testing.T) {
	err := errors.New("x\u202ey")
	for _, cmd := range []*cobra.Command{rootCmd, assignCmd, versionCmd} {
		if got := captureStderr(t, func() { reportError(cmd, err) }); got != "Error: x\u202ey\n" {
			t.Errorf("%s error = %q, want it unchanged", cmd.Name(), got)
		}
	}
	asJSON := jsonFlag
	jsonFlag = true
	defer func() { jsonFlag = asJSON }()
	if got := captureStderr(t, func() { reportError(assignCmd, err) }); got != "{\n  \"error\": \"x\u202ey\"\n}\n" {
		t.Errorf("assign JSON error = %q, want it unchanged", got)
	}
}
