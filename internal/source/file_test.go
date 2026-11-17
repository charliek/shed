package source

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testID(n int) string { return fmt.Sprintf("%032x", n) }

func testMachine(n int) Machine {
	return Machine{ID: testID(n), Name: fmt.Sprintf("machine-%d", n), KeyFingerprint: fmt.Sprintf("SHA256:key%d", n)}
}

func openTemp(t *testing.T) (*FileBindings, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	return openPath(t, path), path
}

// openPath opens the store at path and closes it when the test ends.
func openPath(t *testing.T, path string) *FileBindings {
	t.Helper()
	fb, _, err := OpenFileBindings(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb.Close() })
	return fb
}

// reopen closes fb and opens its file again, as a restart would.
func reopen(t *testing.T, fb *FileBindings) *FileBindings {
	t.Helper()
	if err := fb.Close(); err != nil {
		t.Fatal(err)
	}
	return openPath(t, fb.Path())
}

// onlyStoreFiles checks that dir holds the store and its lock file and
// nothing else (no temporary file left behind).
func onlyStoreFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got, want := strings.Join(names, ","), FileName+","+FileName+LockSuffix; got != want {
		t.Fatalf("directory holds %s; want %s", got, want)
	}
}

func TestOpenMissingFileStartsAnEmptyStoreOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", FileName)
	fb, created, err := OpenFileBindings(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb.Close() })
	if !created {
		t.Fatal("a missing file was not reported as a new store")
	}
	if _, ok := fb.Get(ImplicitUser); ok {
		t.Fatal("binding in a new store")
	}
	if ms := fb.Machines(ImplicitUser); len(ms) != 0 {
		t.Fatalf("machines in a new store: %+v", ms)
	}
	if fb.Path() != path {
		t.Fatalf("Path = %q", fb.Path())
	}
	// The empty store is on disk at once, private, and loads.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the empty store was not written at open: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", fi.Mode().Perm())
	}
	onlyStoreFiles(t, filepath.Dir(path))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(data); err != nil {
		t.Fatalf("the empty store does not load: %v\n%s", err, data)
	}
	// A restart finds it, and says so.
	if err := fb.Close(); err != nil {
		t.Fatal(err)
	}
	fb2, created, err := OpenFileBindings(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb2.Close() })
	if created {
		t.Fatal("an existing empty store was reported as new")
	}
	// The warning the caller logs says what follows from it.
	w := StartedEmptyWarning(path)
	for _, part := range []string{path, "no user has a credential source", "first machine to connect", "shed source status"} {
		if !strings.Contains(w, part) {
			t.Fatalf("warning %q does not say %q", w, part)
		}
	}
}

func TestOpenFailsWhenTheEmptyStoreCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	// The lock file can be opened, but nothing new can be made beside it.
	if err := os.WriteFile(path+LockSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	fb, created, err := OpenFileBindings(path, Options{})
	if err == nil {
		_ = fb.Close()
		t.Fatalf("opened (created %v) a store it cannot write", created)
	}
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "does not start") {
		t.Fatalf("err = %v; want a permission error naming the path", err)
	}
	// The failed open let go of the lock.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	openPath(t, path)
}

func TestSecondOpenOfOnePathFails(t *testing.T) {
	fb, path := openTemp(t)
	if _, err := fb.Admit("alice", testMachine(1)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path + LockSuffix)
	if err != nil {
		t.Fatalf("no lock file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %v; want 0600", fi.Mode().Perm())
	}
	onDisk, _ := os.ReadFile(path)

	second, _, err := OpenFileBindings(path, Options{})
	if err == nil {
		_ = second.Close()
		t.Fatal("a second open of the same path succeeded")
	}
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v; want ErrLocked naming %s", err, path)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, onDisk) {
		t.Fatal("a refused open changed the file")
	}
	// The first store is unaffected and keeps working.
	if _, err := fb.Admit("bob", testMachine(2)); err != nil {
		t.Fatalf("first store after a refused second open: %v", err)
	}

	// Close releases the lock; the store refuses changes after it but still
	// answers reads.
	if err := fb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fb.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := fb.Admit("carol", testMachine(3)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Admit after Close = %v; want ErrClosed", err)
	}
	if b, ok := fb.Get("alice"); !ok || b.MachineID() != testID(1) {
		t.Fatalf("Get after Close = %+v, %v", b, ok)
	}
	re := openPath(t, path)
	if b, ok := re.Get("bob"); !ok || b.MachineID() != testID(2) {
		t.Fatalf("after reopen bob = %+v, %v", b, ok)
	}
	if _, ok := re.Get("carol"); ok {
		t.Fatal("a change refused after Close was saved")
	}
}

const lockHelperEnv = "SHED_SOURCE_LOCK_HELPER_PATH"

// TestLockHelperProcess is not a test: TestSecondOpenFromAnotherProcessFails
// runs the test binary with it to hold a store open in another process.
func TestLockHelperProcess(t *testing.T) {
	path := os.Getenv(lockHelperEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	fb, _, err := OpenFileBindings(path, Options{})
	if err != nil {
		fmt.Fprintf(os.Stdout, "error: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin) // hold until the parent closes stdin
	_ = fb.Close()
	os.Exit(0)
}

func TestSecondOpenFromAnotherProcessFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper said %q, %v", line, err)
	}

	fb, _, err := OpenFileBindings(path, Options{})
	if err == nil {
		_ = fb.Close()
		t.Fatal("opened a store another process holds")
	}
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v; want ErrLocked naming %s", err, path)
	}

	// Once the other process exits, the store opens.
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	openPath(t, path)
}

func TestFileIsPrivateVersionedAndSorted(t *testing.T) {
	fb, path := openTemp(t)
	if _, err := fb.Admit("bob", testMachine(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := fb.Admit(ImplicitUser, testMachine(1)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Version        int `json:"version"`
		LastGeneration int `json:"last_generation"`
		Bindings       []struct {
			User string `json:"user"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Version != FileVersion || f.LastGeneration != 2 || len(f.Bindings) != 2 || f.Bindings[0].User != "" || f.Bindings[1].User != "bob" {
		t.Fatalf("stored = %s", data)
	}
	onlyStoreFiles(t, filepath.Dir(path))
}

func TestRepeatAdmitDoesNotRewriteTheFile(t *testing.T) {
	fb, path := openTemp(t)
	if _, err := fb.Admit("alice", testMachine(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fb.Admit("alice", testMachine(2)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := fb.Admit("alice", testMachine(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := fb.Admit("alice", testMachine(2)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(old) {
		t.Fatalf("file rewritten: mtime %v; want %v", after.ModTime(), old)
	}
	if !os.SameFile(before, after) {
		t.Fatal("file replaced")
	}
}

func TestFailedSyncFailsTheChangeAndKeepsMemory(t *testing.T) {
	syncErr := errors.New("sync failed")
	for _, c := range []struct {
		name     string
		file     func(*os.File) error
		dir      func(string) error
		fileSame bool // the file on disk is untouched
		// restartBound is the machine a restart straight after the failure
		// finds bound: a failure before the rename leaves the old file; one
		// syncing the directory leaves the new file in place (an uncertain
		// commit: reported failed, yet what a restart reads).
		restartBound int
	}{
		{"file sync", func(*os.File) error { return syncErr }, syncDir, true, 1},
		{"directory sync", (*os.File).Sync, func(string) error { return syncErr }, false, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			fb, path := openTemp(t)
			if _, err := fb.Admit("alice", testMachine(1)); err != nil {
				t.Fatal(err)
			}
			if _, err := fb.Admit("alice", testMachine(2)); err != nil {
				t.Fatal(err)
			}
			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := fb.Get("alice")
			fb.write = func(p string, data []byte) error { return writeDurable(p, data, c.file, c.dir) }

			if _, err := fb.Use("alice", testID(2), "mover"); !errors.Is(err, syncErr) {
				t.Fatalf("Use = %v; want the sync failure", err)
			}
			if after, _ := fb.Get("alice"); after.MachineID() != before.MachineID() || after.Generation != before.Generation {
				t.Fatalf("memory changed by a failed save: %+v -> %+v", before, after)
			}
			if !fb.Current("alice", testID(1), before.Generation) {
				t.Fatal("old binding not current after a failed move")
			}
			now, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.fileSame != bytes.Equal(now, onDisk) {
				t.Fatalf("file unchanged = %v; want %v", bytes.Equal(now, onDisk), c.fileSame)
			}
			onlyStoreFiles(t, filepath.Dir(path))

			// A restart straight after the failure, before any retry.
			re := reopen(t, fb)
			b, ok := re.Get("alice")
			if !ok || b.MachineID() != testID(c.restartBound) {
				t.Fatalf("after a restart bound = %+v, %v; want machine %d", b, ok, c.restartBound)
			}
			if c.restartBound == 1 && b.Generation != before.Generation {
				t.Fatalf("after a restart generation = %d; want the old %d", b.Generation, before.Generation)
			}
			if c.restartBound == 2 && (b.Generation <= before.Generation || b.BoundBy != "mover") {
				t.Fatalf("after a restart binding = %+v; want the move, past generation %d", b, before.Generation)
			}
			if got := len(re.Machines("alice")); got != 2 {
				t.Fatalf("after a restart machines = %d; want 2", got)
			}
			// The restarted store takes the move (a no-op where it landed).
			mv, err := re.Use("alice", testID(2), "mover")
			if err != nil || mv.Moved != (c.restartBound == 1) || mv.Binding.MachineID() != testID(2) {
				t.Fatalf("Use after the restart = %+v, %v", mv, err)
			}
		})
	}
}

// validStore is a well-formed stored file, to be broken one way at a time.
func validStore() map[string]any {
	m1, m2 := testID(1), testID(2)
	ref := func(id string) []any { return []any{map[string]any{"kind": "machine", "id": id}} }
	return map[string]any{
		"version":         1,
		"last_generation": 3,
		"bindings": []any{map[string]any{
			"user": "alice", "delivery": ref(m1), "signing": ref(m1),
			"bound_at": "2026-11-16T09:00:00Z", "bound_by": "first-connect", "generation": 3,
		}},
		"machines": []any{
			map[string]any{"user": "alice", "id": m1, "name": "one", "key_fingerprint": "SHA256:a", "first_seen": "2026-11-16T09:00:00Z"},
			map[string]any{"user": "alice", "id": m2, "name": "two", "key_fingerprint": "", "first_seen": "2026-11-16T09:01:00Z"},
		},
	}
}

func binding0(s map[string]any) map[string]any { return s["bindings"].([]any)[0].(map[string]any) }
func machine0(s map[string]any) map[string]any { return s["machines"].([]any)[0].(map[string]any) }

func TestOpenValidStore(t *testing.T) {
	data, _ := json.Marshal(validStore())
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fb, created, err := OpenFileBindings(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb.Close() })
	if created {
		t.Fatal("an existing store was reported as new")
	}
	b, ok := fb.Get("alice")
	if !ok || b.MachineID() != testID(1) || b.Generation != 3 {
		t.Fatalf("Get = %+v, %v", b, ok)
	}
	// The counter continues from last_generation.
	mv, err := fb.Use("alice", testID(2), "mover")
	if err != nil || mv.Binding.Generation != 4 {
		t.Fatalf("Use = %+v, %v; want generation 4", mv, err)
	}
}

func TestOpenRefusesBadFilesAndNeverRewritesThem(t *testing.T) {
	type mutate func(s map[string]any)
	cases := []struct {
		name  string
		raw   string // used instead of the mutated valid store when set
		edit  mutate
		want  error
		about string // a word the error must contain
	}{
		{name: "empty file", raw: " ", want: ErrCorrupt},
		{name: "zero bytes", raw: "", want: ErrCorrupt},
		{name: "not JSON", raw: "{bindings", want: ErrCorrupt},
		{name: "trailing data", raw: `{"version":1,"last_generation":0,"bindings":[],"machines":[]} {}`, want: ErrCorrupt, about: "after"},
		{name: "no version", edit: func(s map[string]any) { delete(s, "version") }, want: ErrCorrupt, about: "version"},
		{name: "version zero", edit: func(s map[string]any) { s["version"] = 0 }, want: ErrCorrupt},
		{name: "newer version", edit: func(s map[string]any) { s["version"] = 2; s["new_field"] = true }, want: ErrNewerVersion},
		{name: "unknown field", edit: func(s map[string]any) { s["extra"] = 1 }, want: ErrCorrupt, about: "extra"},
		{name: "unknown field in a binding", edit: func(s map[string]any) { binding0(s)["extra"] = 1 }, want: ErrCorrupt},
		{name: "empty delivery slot", edit: func(s map[string]any) { binding0(s)["delivery"] = []any{} }, want: ErrCorrupt, about: "exactly one"},
		{name: "missing signing slot", edit: func(s map[string]any) { delete(binding0(s), "signing") }, want: ErrCorrupt, about: "signing"},
		{name: "two refs in a slot", edit: func(s map[string]any) {
			b := binding0(s)
			b["signing"] = append(b["signing"].([]any), map[string]any{"kind": "machine", "id": testID(2)})
		}, want: ErrCorrupt, about: "exactly one"},
		{name: "slots name different machines", edit: func(s map[string]any) {
			binding0(s)["signing"] = []any{map[string]any{"kind": "machine", "id": testID(2)}}
		}, want: ErrCorrupt, about: "different"},
		{name: "unknown ref kind", edit: func(s map[string]any) {
			binding0(s)["delivery"] = []any{map[string]any{"kind": "phone", "id": testID(1)}}
			binding0(s)["signing"] = []any{map[string]any{"kind": "phone", "id": testID(1)}}
		}, want: ErrCorrupt, about: "kind"},
		{name: "bad ref id", edit: func(s map[string]any) {
			binding0(s)["delivery"] = []any{map[string]any{"kind": "machine", "id": "nothex"}}
			binding0(s)["signing"] = []any{map[string]any{"kind": "machine", "id": "nothex"}}
		}, want: ErrCorrupt},
		{name: "bound machine not remembered", edit: func(s map[string]any) {
			s["machines"] = s["machines"].([]any)[1:]
		}, want: ErrCorrupt, about: "bound machine"},
		{name: "binding for a user with no machines", edit: func(s map[string]any) { binding0(s)["user"] = "bob" }, want: ErrCorrupt, about: "bound machine"},
		{name: "two bindings for one user", edit: func(s map[string]any) {
			s["bindings"] = append(s["bindings"].([]any), binding0(s))
		}, want: ErrCorrupt, about: "second binding"},
		{name: "generation zero", edit: func(s map[string]any) { binding0(s)["generation"] = 0 }, want: ErrCorrupt, about: "generation"},
		{name: "generation past the counter", edit: func(s map[string]any) { binding0(s)["generation"] = 4 }, want: ErrCorrupt, about: "generation"},
		{name: "no bound_at", edit: func(s map[string]any) { delete(binding0(s), "bound_at") }, want: ErrCorrupt},
		{name: "empty bound_by", edit: func(s map[string]any) { binding0(s)["bound_by"] = "" }, want: ErrCorrupt},
		{name: "invalid binding user", edit: func(s map[string]any) { binding0(s)["user"] = "Alice" }, want: ErrCorrupt},
		{name: "invalid machine user", edit: func(s map[string]any) { machine0(s)["user"] = "a b" }, want: ErrCorrupt},
		{name: "invalid machine id", edit: func(s map[string]any) { machine0(s)["id"] = strings.Repeat("A", 32) }, want: ErrCorrupt},
		{name: "invalid machine name", edit: func(s map[string]any) { machine0(s)["name"] = "a\x01b" }, want: ErrCorrupt},
		{name: "empty machine name", edit: func(s map[string]any) { machine0(s)["name"] = "" }, want: ErrCorrupt},
		{name: "machine listed twice", edit: func(s map[string]any) {
			s["machines"] = append(s["machines"].([]any), machine0(s))
		}, want: ErrCorrupt, about: "twice"},
		{name: "no first_seen", edit: func(s map[string]any) { delete(machine0(s), "first_seen") }, want: ErrCorrupt},
		{name: "wrong type", edit: func(s map[string]any) { s["last_generation"] = "3" }, want: ErrCorrupt},
		// Incomplete files: never read as fewer records.
		{name: "version only", raw: `{"version":1}`, want: ErrCorrupt, about: "no last_generation"},
		{name: "no bindings", edit: func(s map[string]any) { delete(s, "bindings") }, want: ErrCorrupt, about: "no bindings"},
		{name: "null bindings", edit: func(s map[string]any) { s["bindings"] = nil }, want: ErrCorrupt, about: "null bindings"},
		{name: "no machines", edit: func(s map[string]any) { delete(s, "machines") }, want: ErrCorrupt, about: "no machines"},
		{name: "null machines", edit: func(s map[string]any) { s["machines"] = nil }, want: ErrCorrupt, about: "null machines"},
		{name: "no last_generation", edit: func(s map[string]any) { delete(s, "last_generation") }, want: ErrCorrupt, about: "no last_generation"},
		{name: "null last_generation", edit: func(s map[string]any) { s["last_generation"] = nil }, want: ErrCorrupt, about: "null last_generation"},
		{name: "null version", edit: func(s map[string]any) { s["version"] = nil }, want: ErrCorrupt, about: "version"},
		{name: "machines but no binding", edit: func(s map[string]any) { s["bindings"] = []any{} }, want: ErrCorrupt, about: "no binding"},
		{name: "null binding", edit: func(s map[string]any) { s["bindings"] = []any{nil} }, want: ErrCorrupt, about: "null"},
		{name: "a user's names removed", edit: func(s map[string]any) {
			// Every record of alice falls to the implicit user if a missing
			// user field is read as "".
			delete(binding0(s), "user")
			for _, m := range s["machines"].([]any) {
				delete(m.(map[string]any), "user")
			}
		}, want: ErrCorrupt, about: "no user"},
		{name: "null machine user", edit: func(s map[string]any) { machine0(s)["user"] = nil }, want: ErrCorrupt, about: "null user"},
		{name: "no key_fingerprint", edit: func(s map[string]any) { delete(machine0(s), "key_fingerprint") }, want: ErrCorrupt, about: "key_fingerprint"},
		{name: "no ref kind", edit: func(s map[string]any) {
			delete(binding0(s)["delivery"].([]any)[0].(map[string]any), "kind")
		}, want: ErrCorrupt, about: "no kind"},
		{name: "more machines than the limit", edit: func(s map[string]any) {
			ms := s["machines"].([]any)
			for n := 3; n <= MaxMachinesPerUser+1; n++ {
				ms = append(ms, map[string]any{"user": "alice", "id": testID(n), "name": fmt.Sprintf("m%d", n), "key_fingerprint": "", "first_seen": "2026-11-16T09:02:00Z"})
			}
			s["machines"] = ms
		}, want: ErrCorrupt, about: "at most"},
		{name: "invalid UTF-8 in a name", raw: strings.Replace(mustJSON(validStore()), `"name":"two"`, "\"name\":\"t\xffo\"", 1), want: ErrCorrupt, about: "UTF-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := []byte(c.raw)
			if c.edit != nil {
				s := validStore()
				c.edit(s)
				var err error
				if data, err = json.Marshal(s); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, data, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o640); err != nil { // past the umask
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			fb, _, err := OpenFileBindings(path, Options{})
			if err == nil {
				_ = fb.Close()
				t.Fatalf("opened: %+v", fb)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v; want %v", err, c.want)
			}
			if c.about != "" && !strings.Contains(err.Error(), c.about) {
				t.Fatalf("err = %v; want it to mention %q", err, c.about)
			}
			for _, part := range []string{path, "move it aside", "does not start"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("err = %v; want it to say %q", err, part)
				}
			}
			assertUntouched(t, path, data, old, 0o640)
		})
	}
}

func TestOpenRefusesAnEntryThatCannotBeRead(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := OpenFileBindings(path, Options{}); err == nil {
			t.Fatal("opened a directory")
		}
	})
	t.Run("dangling symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		if err := os.Symlink(filepath.Join(dir, "gone"), path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := OpenFileBindings(path, Options{}); err == nil {
			t.Fatal("a dangling symlink was taken as no store")
		}
		if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink changed: %v", err)
		}
	})
	t.Run("no permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		path := filepath.Join(t.TempDir(), FileName)
		data, _ := json.Marshal(validStore())
		if err := os.WriteFile(path, data, 0o000); err != nil {
			t.Fatal(err)
		}
		_, _, err := OpenFileBindings(path, Options{})
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("err = %v; want a permission error", err)
		}
		if !strings.Contains(err.Error(), "does not start") {
			t.Fatalf("err = %v", err)
		}
		_ = os.Chmod(path, 0o600)
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, data) {
			t.Fatal("file rewritten")
		}
	})
}

// assertUntouched checks that a refused file is exactly as it was.
func assertUntouched(t *testing.T, path string, data []byte, mtime time.Time, mode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("file rewritten:\n%s", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) || fi.Mode().Perm() != mode {
		t.Fatalf("file touched: mtime %v mode %v", fi.ModTime(), fi.Mode().Perm())
	}
	onlyStoreFiles(t, filepath.Dir(path))
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestInvalidUTF8IsNotRepairedByTheDecoder(t *testing.T) {
	// Go's JSON decoder turns a damaged byte into U+FFFD, which is valid
	// UTF-8 and passes the name check: only a check of the raw bytes sees it.
	data := strings.Replace(mustJSON(validStore()), `"name":"two"`, "\"name\":\"t\xffo\"", 1)
	var f fileData
	if err := json.Unmarshal([]byte(data), &f); err != nil || f.Machines[1].Name != "t\uFFFDo" {
		t.Fatalf("decoder no longer repairs; name = %q, %v", f.Machines[1].Name, err)
	}
	if _, err := decode([]byte(data)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("decode = %v; want ErrCorrupt", err)
	}
}

func TestGenerationExhaustionIsRefusedNotWrapped(t *testing.T) {
	s := validStore()
	s["last_generation"] = uint64(math.MaxUint64)
	binding0(s)["generation"] = uint64(math.MaxUint64)
	// The implicit user's records, to adopt.
	m9 := testID(9)
	s["bindings"] = append(s["bindings"].([]any), map[string]any{
		"user": "", "delivery": []any{map[string]any{"kind": "machine", "id": m9}}, "signing": []any{map[string]any{"kind": "machine", "id": m9}},
		"bound_at": "2026-11-16T09:00:00Z", "bound_by": "first-connect", "generation": 3,
	})
	s["machines"] = append(s["machines"].([]any), map[string]any{"user": "", "id": m9, "name": "nine", "key_fingerprint": "", "first_seen": "2026-11-16T09:00:00Z"})
	data := []byte(mustJSON(s))
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fb := openPath(t, path) // a counter at its maximum still loads
	before, _ := fb.Get("alice")

	if _, err := fb.Use("alice", testID(2), "mover"); !errors.Is(err, ErrGenerationExhausted) {
		t.Fatalf("Use = %v; want ErrGenerationExhausted", err)
	}
	if _, err := fb.Admit("bob", testMachine(3)); !errors.Is(err, ErrGenerationExhausted) {
		t.Fatalf("first Admit for a user = %v; want ErrGenerationExhausted", err)
	}
	if _, err := fb.AdoptImplicit("carol"); !errors.Is(err, ErrGenerationExhausted) {
		t.Fatalf("AdoptImplicit = %v; want ErrGenerationExhausted", err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, data) {
		t.Fatal("a refused operation wrote the file")
	}
	if b, _ := fb.Get("alice"); b.MachineID() != before.MachineID() || b.Generation != before.Generation {
		t.Fatalf("binding changed: %+v -> %+v", before, b)
	}
	if _, ok := fb.Get("bob"); ok {
		t.Fatal("bob bound with an exhausted counter")
	}
	// What needs no new generation still works.
	if d, err := fb.Admit("alice", testMachine(1)); err != nil || !d.Admitted {
		t.Fatalf("Admit of the bound machine = %+v, %v", d, err)
	}
	if _, err := fb.Drop("alice"); err != nil {
		t.Fatalf("Drop = %v", err)
	}
	re := reopen(t, fb)
	if _, ok := re.Get("alice"); ok {
		t.Fatal("drop not saved")
	}
}

func TestEncodeRefusesAStateItWouldNotLoad(t *testing.T) {
	m := Machine{ID: testID(1), Name: "one", FirstSeen: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	machines := func(n int) map[string]Machine {
		out := map[string]Machine{}
		for i := 1; i <= n; i++ {
			mm := m
			mm.ID = testID(i)
			out[mm.ID] = mm
		}
		return out
	}
	for _, c := range []struct {
		name  string
		st    *state
		about string
	}{
		{"machines with no binding", &state{users: map[string]*userState{"alice": {machines: machines(1)}}}, "no binding"},
		{"more machines than the limit", &state{lastGeneration: 1, users: map[string]*userState{"alice": {
			binding: newBinding("alice", testID(1), m.FirstSeen, BoundByFirstConnect, 1), machines: machines(MaxMachinesPerUser + 1)}}}, "at most"},
		{"generation past the counter", &state{lastGeneration: 1, users: map[string]*userState{"alice": {
			binding: newBinding("alice", testID(1), m.FirstSeen, BoundByFirstConnect, 2), machines: machines(1)}}}, "generation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := encode(c.st)
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), c.about) {
				t.Fatalf("encode = %v; want ErrCorrupt about %q", err, c.about)
			}
		})
	}
	// And a store holding such a state cannot save it: the change fails and
	// memory is kept.
	fb, path := openTemp(t)
	if _, err := fb.Admit("alice", testMachine(1)); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.ReadFile(path)
	bad := fb.cur.Load().clone()
	bad.users["bob"] = &userState{machines: machines(1)}
	fb.cur.Store(bad)
	if _, err := fb.Admit("alice", testMachine(2)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Admit over a bad state = %v; want ErrCorrupt", err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, onDisk) {
		t.Fatal("a state that would not load was written")
	}
}

func TestRefusedMachineChurnDoesNotGrowTheFile(t *testing.T) {
	fb, path := openTemp(t)
	if _, err := fb.Admit("alice", testMachine(1)); err != nil {
		t.Fatal(err)
	}
	churn := func(i int) {
		t.Helper()
		m := testMachine(2)
		m.Name = []string{"name-a", "name-b"}[i%2]
		m.KeyFingerprint = []string{"SHA256:x", "SHA256:y"}[(i/2)%2]
		if _, err := fb.Admit("alice", m); err != nil {
			t.Fatal(err)
		}
	}
	size := func() int64 {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	for i := 0; i < 4; i++ {
		churn(i)
	}
	after4 := size()
	for i := 4; i < 200; i++ {
		churn(i)
	}
	if got := size(); got != after4 {
		t.Fatalf("file grew with churn: %d bytes after 4 rounds, %d after 200", after4, got)
	}
	if n := len(fb.Machines("alice")); n != 2 {
		t.Fatalf("machines = %d; want 2", n)
	}
}

func TestValidMachineIDAndName(t *testing.T) {
	for _, c := range []struct {
		id string
		ok bool
	}{
		{testID(1), true},
		{strings.Repeat("f", 32), true},
		{"", false},
		{strings.Repeat("f", 31), false},
		{strings.Repeat("f", 33), false},
		{strings.Repeat("F", 32), false},
		{strings.Repeat("g", 32), false},
		{"0123456789abcdef0123456789abcde\n", false},
	} {
		if ValidMachineID(c.id) != c.ok {
			t.Errorf("ValidMachineID(%q) = %v", c.id, !c.ok)
		}
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"x", true},
		{"Charlie’s MacBook", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 61) + "é", true},
		{"", false},
		{strings.Repeat("a", 64), false},
		{strings.Repeat("a", 62) + "é", false},
		{"a\tb", false},
		{"a\u009fb", false},
		{"\xc3", false},
	} {
		if ValidMachineName(c.name) != c.ok {
			t.Errorf("ValidMachineName(%q) = %v", c.name, !c.ok)
		}
	}
}

// TestWriteDurableSyncsBeforeRenameAndDirAfter checks the order of the
// durable write's steps.
func TestWriteDurableSyncsBeforeRenameAndDirAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	var steps []string
	err := writeDurable(path, []byte("x"), func(f *os.File) error {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			steps = append(steps, "file-sync-after-rename")
		}
		steps = append(steps, "file")
		return f.Sync()
	}, func(d string) error {
		if _, err := os.Lstat(path); err != nil {
			steps = append(steps, "dir-sync-before-rename")
		}
		steps = append(steps, "dir:"+filepath.Base(d))
		return syncDir(d)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(steps, ","); got != "file,dir:"+filepath.Base(dir) {
		t.Fatalf("steps = %s", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}
