package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/charliek/shed/internal/config"
)

// partialThenErr yields its bytes and then a non-EOF error.
type partialThenErr struct {
	data string
	done bool
}

func (p *partialThenErr) Read(b []byte) (int, error) {
	if p.done {
		return 0, errors.New("boom")
	}
	p.done = true
	return copy(b, p.data), errors.New("boom")
}

// withConfirmInput swaps the confirm seams for one test and restores them.
func withConfirmInput(t *testing.T, in io.Reader, tty bool) {
	t.Helper()
	oldIn, oldTTY := confirmIn, stdinIsTTY
	t.Cleanup(func() {
		confirmIn, stdinIsTTY = oldIn, oldTTY
		resetConfirmReader()
	})
	confirmIn = in
	stdinIsTTY = func() bool { return tty }
	resetConfirmReader()
}

// captureStdout runs fn and returns what it wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestConfirmAction(t *testing.T) {
	cases := []struct {
		name    string
		in      io.Reader
		tty     bool
		want    bool
		wantErr string
	}{
		{"y newline", strings.NewReader("y\n"), false, true, ""},
		{"yes newline", strings.NewReader("yes\n"), false, true, ""},
		{"YES mixed case padded", strings.NewReader("  YeS \n"), false, true, ""},
		{"y then EOF no newline", strings.NewReader("y"), false, true, ""},
		{"yes then EOF no newline", strings.NewReader("yes"), false, true, ""},
		{"n", strings.NewReader("n\n"), false, false, ""},
		{"other answer", strings.NewReader("maybe\n"), true, false, ""},
		{"empty line", strings.NewReader("\n"), false, false, ""},
		{"EOF no bytes, not a terminal", strings.NewReader(""), false, false, "re-run with --force"},
		{"EOF no bytes, terminal (Ctrl-D)", strings.NewReader(""), true, false, ""},
		{"read error, no bytes", iotest.ErrReader(errors.New("boom")), false, false, "boom"},
		{"read error on a terminal", iotest.ErrReader(errors.New("boom")), true, false, "boom"},
		{"partial bytes then read error", &partialThenErr{data: "y"}, false, false, "boom"},
		{"full line returned with a read error", &partialThenErr{data: "y\n"}, true, false, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withConfirmInput(t, tc.in, tc.tty)
			var got bool
			var err error
			out := captureStdout(t, func() { got, err = confirmAction("Go? ", "--force") })
			if out != "Go? " {
				t.Errorf("stdout = %q, want the prompt", out)
			}
			if got != tc.want {
				t.Errorf("confirmed = %v, want %v", got, tc.want)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfirmActionSequentialPrompts(t *testing.T) {
	withConfirmInput(t, strings.NewReader("y\nn\nyes\n"), false)
	want := []bool{true, false, true}
	captureStdout(t, func() {
		for i, w := range want {
			got, err := confirmAction("? ", "--force")
			if err != nil || got != w {
				t.Errorf("prompt %d = (%v, %v), want (%v, nil)", i, got, err, w)
			}
		}
	})
}

// confirmCmdEnv points the CLI at a fake server that records mutating requests.
func confirmCmdEnv(t *testing.T) (deletes *atomic.Int32) {
	t.Helper()
	deletes = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/images":
			_ = jsonWrite(w, config.ImagesResponse{})
		default:
			_ = jsonWrite(w, config.Shed{Name: "myshed"})
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("HOME", t.TempDir())
	oldCfg, oldServer, oldJSON := clientConfig, serverFlag, jsonFlag
	oldDel, oldImg := deleteForce, imageDeleteForce
	t.Cleanup(func() {
		clientConfig, serverFlag, jsonFlag = oldCfg, oldServer, oldJSON
		deleteForce, imageDeleteForce = oldDel, oldImg
	})
	clientConfig = &config.ClientConfig{
		DefaultServer: "fake",
		Servers:       map[string]config.ServerEntry{"fake": {Host: "fake", APIURL: srv.URL, SSHPort: 2222}},
	}
	serverFlag, jsonFlag = "fake", false
	deleteForce, imageDeleteForce = false, false
	return deletes
}

func jsonWrite(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func TestDestructiveCommandsConfirmation(t *testing.T) {
	cmds := []struct {
		name  string
		run   func() error
		force *bool
	}{
		{"delete", func() error { return runDelete(nil, []string{"myshed"}) }, &deleteForce},
		{"image rm", func() error { return runImageDelete(nil, []string{"img"}) }, &imageDeleteForce},
	}
	for _, c := range cmds {
		t.Run(c.name+" EOF without a terminal sends nothing", func(t *testing.T) {
			deletes := confirmCmdEnv(t)
			withConfirmInput(t, strings.NewReader(""), false)
			var err error
			captureStdout(t, func() { err = c.run() })
			if err == nil || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("err = %v, want one naming --force", err)
			}
			if n := deletes.Load(); n != 0 {
				t.Errorf("%d DELETE request(s) reached the server, want 0", n)
			}
		})

		t.Run(c.name+" --force skips stdin", func(t *testing.T) {
			deletes := confirmCmdEnv(t)
			withConfirmInput(t, iotest.ErrReader(errors.New("stdin must not be read")), false)
			*c.force = true
			var err error
			out := captureStdout(t, func() { err = c.run() })
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n := deletes.Load(); n != 1 {
				t.Errorf("%d DELETE request(s), want 1", n)
			}
			if strings.Contains(out, "[y/N]") {
				t.Errorf("stdout carries a prompt: %q", out)
			}
		})

		t.Run(c.name+" --json without --force is rejected without a prompt", func(t *testing.T) {
			deletes := confirmCmdEnv(t)
			withConfirmInput(t, strings.NewReader("y\n"), false)
			jsonFlag = true
			var err error
			out := captureStdout(t, func() { err = c.run() })
			if err == nil || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("err = %v, want one naming --force", err)
			}
			if out != "" {
				t.Errorf("stdout not clean: %q", out)
			}
			if n := deletes.Load(); n != 0 {
				t.Errorf("%d DELETE request(s) reached the server, want 0", n)
			}
		})
	}
}
