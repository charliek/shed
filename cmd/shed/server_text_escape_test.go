package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// Server text that would act on a terminal printed raw: an OSC title
// sequence, a screen clear and a line break that starts a line of its own.
const (
	hostileOSC   = "\x1b]0;pwned\x07"
	hostileClear = "\x1b[2J"
)

// rawControl reports whether s holds the hostile sequences raw (a BEL, an OSC
// or a screen clear), or the line break the hostile text carries.
func rawControl(s, brokenLine string) bool {
	return strings.Contains(s, "\x07") || strings.Contains(s, "\x1b]") || strings.Contains(s, hostileClear) ||
		strings.Contains(s, brokenLine)
}

// TestSSEErrorEscapesServerText: an event stream's error event (a malformed
// payload, or a structured error's code and message) is escaped before it is
// shown, and printable text is unchanged.
func TestSSEErrorEscapesServerText(t *testing.T) {
	cases := []struct {
		name, data, want string
	}{
		{"malformed", "bad" + hostileOSC + "thing", `server error: bad\x1b]0;pwned\athing`},
		{"structured", `{"error":{"code":"BAD\u001b[2J","message":"no\nHost *\u001b]0;pwned\u0007"}}`,
			`BAD\x1b[2J: no\nHost *\x1b]0;pwned\a`},
		{"structured, printable", `{"error":{"code":"SHED_EXISTS","message":"shed \"box\" already exists"}}`,
			`SHED_EXISTS: shed "box" already exists`},
	}
	for _, tc := range cases {
		stream := "event: error\ndata: " + tc.data + "\n\n"
		_, err := (&APIClient{}).readSSEStream(strings.NewReader(stream), nil)
		if err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
		if got := err.Error(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if rawControl(err.Error(), "no\nHost") {
			t.Errorf("%s: a raw control character reached the message: %q", tc.name, err)
		}
	}
}

// TestProgressEscapesServerText: a create or pull's progress messages,
// warnings and live-block labels are the server's, and are escaped where they
// are printed.
func TestProgressEscapesServerText(t *testing.T) {
	msg := "Booting" + hostileOSC + "\nHost *" + hostileClear

	t.Run("live renderer, plain line and blob label", func(t *testing.T) {
		var buf bytes.Buffer
		r := newLiveRenderer(&buf, func() (int, int) { return 200, 24 })
		r.handle(backend.ProgressEvent{Kind: backend.KindBlob, ID: "sha256:" + strings.Repeat("a", 64),
			Message: "layer" + hostileOSC + "\nHost *", Status: "downloading", Current: 1, Total: 2})
		r.handle(backend.ProgressEvent{Message: msg})
		r.finish()
		out := buf.String()
		if rawControl(out, "\nHost *") {
			t.Errorf("raw server text reached the live block: %q", out)
		}
		for _, want := range []string{`layer\x1b]0;pwned\a\nHost *`, `Booting\x1b]0;pwned\a\nHost *\x1b[2J`} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q lacks the escaped %q", out, want)
			}
		}
	})

	t.Run("plain lines and warnings", func(t *testing.T) {
		onProgress, finish, _ := progressSink(false)
		var stderr string
		stdout := captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				onProgress(backend.ProgressEvent{Message: msg})
				onProgress(backend.ProgressEvent{Message: "warn" + hostileOSC + "\nHost *", Warning: true})
				finish()
			})
		})
		if rawControl(stdout, "\nHost *") || rawControl(stderr, "\nHost *") {
			t.Errorf("raw server text reached the output: stdout %q, stderr %q", stdout, stderr)
		}
		if want := `  Booting\x1b]0;pwned\a\nHost *\x1b[2J` + "\n"; stdout != want {
			t.Errorf("stdout %q, want %q", stdout, want)
		}
		if want := `  Warning: warn\x1b]0;pwned\a\nHost *` + "\n"; stderr != want {
			t.Errorf("stderr %q, want %q", stderr, want)
		}
	})

	t.Run("printable text unchanged", func(t *testing.T) {
		onProgress, _, _ := progressSink(false)
		out := captureStdout(t, func() { onProgress(backend.ProgressEvent{Message: "Pulling layer 1/3 (12 MB)…"}) })
		if out != "  Pulling layer 1/3 (12 MB)…\n" {
			t.Errorf("printable message changed: %q", out)
		}
	})
}

// fixedShedGetter answers every GetShed with shed.
type fixedShedGetter struct{ shed config.Shed }

func (g fixedShedGetter) GetShed(string) (*config.Shed, error) { s := g.shed; return &s, nil }

// TestStatusReasonEscaped: a shed's status and status_reason, and its name in
// the wait line, are the server's and are escaped where printed; printable
// text is unchanged.
func TestStatusReasonEscaped(t *testing.T) {
	if got := statusWithReason(config.Shed{Status: "stopped", StatusReason: "capacity"}); got != "stopped (capacity)" {
		t.Errorf("printable: %q", got)
	}
	hostile := config.Shed{Status: "stopped" + hostileClear, StatusReason: "cap" + hostileOSC + "\nHost *"}
	got := statusWithReason(hostile)
	if want := `stopped\x1b[2J (cap\x1b]0;pwned\a\nHost *)`; got != want {
		t.Errorf("statusWithReason = %q, want %q", got, want)
	}

	shortSettle(t, time.Second)
	var progress bytes.Buffer
	first := &config.Shed{Name: "a1" + hostileOSC, Status: config.StatusStarting}
	settled, err := waitSettled(fixedShedGetter{config.Shed{Name: "a1" + hostileOSC, Status: config.StatusStopped,
		StatusReason: "cap" + hostileOSC + "\nHost *"}}, first, &progress)
	if err != nil {
		t.Fatal(err)
	}
	if rawControl(progress.String(), "\nHost *") || !strings.Contains(progress.String(), `a1\x1b]0;pwned\a`) {
		t.Errorf("wait line %q: want the name escaped", progress.String())
	}
	refusal := requireSettledStatus(settled, config.StatusRunning)
	if refusal == nil || rawControl(refusal.Error(), "\nHost *") ||
		!strings.Contains(refusal.Error(), `stopped (cap\x1b]0;pwned\a\nHost *)`) {
		t.Errorf("requireSettledStatus = %v, want the status and reason escaped", refusal)
	}
}

// TestAddedServerNameEscaped: the name an added entry is filed under may be
// the one the server reported, and its API URL comes from the server's
// bundle: both are escaped in the add's report.
func TestAddedServerNameEscaped(t *testing.T) {
	testClientConfig(t)
	origJSON := jsonFlag
	t.Cleanup(func() { jsonFlag = origJSON })
	jsonFlag = false
	name := "srv" + hostileOSC + "\nHost *"
	out := captureStdout(t, func() {
		if err := reportAddedServer(name, config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222,
			APIURL: "https://127.0.0.1:8443/" + hostileClear}); err != nil {
			t.Error(err)
		}
	})
	if rawControl(out, "\nHost *") {
		t.Errorf("raw server text reached the add's report: %q", out)
	}
	if want := `Added server srv\x1b]0;pwned\a\nHost * (https://127.0.0.1:8443/\x1b[2J)`; !strings.Contains(out, want) {
		t.Errorf("report %q lacks %q", out, want)
	}
}

// TestServerListEscapesNames: `shed server list` shows each stored name (a
// server may have named it at add) escaped, and a printable name unchanged.
func TestServerListEscapesNames(t *testing.T) {
	testClientConfig(t)
	origJSON := jsonFlag
	t.Cleanup(func() { jsonFlag = origJSON })
	jsonFlag = false
	port := freeClosedPort(t)
	putServerEntry(t, "srv"+hostileOSC+"\nHost *", config.ServerEntry{Host: "127.0.0.1", HTTPPort: port, SSHPort: 2222})
	putServerEntry(t, "work", config.ServerEntry{Host: "127.0.0.1", HTTPPort: port, SSHPort: 2222})
	out := listOutput(t)
	if rawControl(out, "\nHost *") {
		t.Errorf("raw server text reached the list: %q", out)
	}
	if !strings.Contains(out, `srv\x1b]0;pwned\a\nHost *`) {
		t.Errorf("list %q lacks the escaped name", out)
	}
	if !strings.Contains(out, "\nwork ") {
		t.Errorf("list %q lacks the printable name as it is", out)
	}
}
