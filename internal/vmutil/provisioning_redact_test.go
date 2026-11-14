package vmutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/provision"
)

// syncBuffer is a bytes.Buffer two goroutines may write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestHookOutputRedacted: a hook's output goes to the server log, so URL
// credentials a hook prints are redacted in the streamed output, in its
// stderr and in the "Last output" quoted when it fails. The final line is
// passed on though it has no line break.
func TestHookOutputRedacted(t *testing.T) {
	const secret = "s3cr3t-token"
	tests := []struct {
		name       string
		stdout     string
		stderr     string
		wantStdout string
		wantStderr string
	}{
		{
			name:       "password in a URL",
			stdout:     "fetching https://user:" + secret + "@git.example/repo.git\nstep two\n",
			stderr:     "fatal: unable to access 'https://user:" + secret + "@git.example/repo.git/'\n",
			wantStdout: "fetching https://REDACTED@git.example/repo.git\nstep two\n",
			wantStderr: "fatal: unable to access 'https://REDACTED@git.example/repo.git/'\n",
		},
		{
			name:       "token as the user, no final line break",
			stdout:     "pip install git+https://" + secret + "@git.example/pkg",
			stderr:     "",
			wantStdout: "pip install git+https://REDACTED@git.example/pkg",
		},
		{
			name:       "nothing to hide is passed on unchanged",
			stdout:     "cloning git@git.example:org/repo.git\n",
			stderr:     "warning: plain\n",
			wantStdout: "cloning git@git.example:org/repo.git\n",
			wantStderr: "warning: plain\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialer := &pipeDialer{
				handler: func(conn net.Conn) {
					defer conn.Close()
					_, data, err := agentproto.ReadMessage(conn) // exec request
					if err != nil {
						return
					}
					var req agentproto.ExecRequest
					_ = json.Unmarshal(data, &req)
					agentproto.ReadMessage(conn) // stdin EOF
					if len(req.Cmd) > 0 && req.Cmd[0] == "/bin/sh" {
						// ensureLogDir
						_ = agentproto.WriteExitCode(conn, 0)
						return
					}
					_ = agentproto.WriteData(conn, []byte(tt.stdout))
					if tt.stderr != "" {
						_ = agentproto.WriteStderr(conn, []byte(tt.stderr))
					}
					_ = agentproto.WriteExitCode(conn, 3)
				},
			}
			var out, errOut syncBuffer
			p := NewProvisioner(NewAgentClient(dialer, 1024, 1026), "test")
			p.SetOutput(&out, &errOut)

			err := p.runHook(context.Background(), &provision.Config{}, provision.HookTypeStartup, "hook.sh")
			var hookErr *provision.HookError
			if !errors.As(err, &hookErr) {
				t.Fatalf("runHook() error = %v, want a HookError", err)
			}
			if hookErr.ExitCode != 3 {
				t.Errorf("exit code = %d, want 3", hookErr.ExitCode)
			}
			if got := out.String(); got != tt.wantStdout {
				t.Errorf("streamed output = %q, want %q", got, tt.wantStdout)
			}
			if got := errOut.String(); got != tt.wantStderr {
				t.Errorf("streamed stderr = %q, want %q", got, tt.wantStderr)
			}
			for where, text := range map[string]string{
				"output": out.String(), "stderr": errOut.String(),
				"last output": hookErr.LastOutput, "error": hookErr.Error(),
			} {
				if strings.Contains(text, secret) {
					t.Errorf("%s carries the credential: %q", where, text)
				}
			}
		})
	}
}

// TestHookLastOutputRedactedBeforeTheTailCut: only the last hookOutputTail
// bytes of a hook's output are kept to quote in a failure. The tail is cut
// from the redacted output; cut from the raw output it could begin inside a
// URL's password, which then has no scheme before it and is not recognised.
func TestHookLastOutputRedactedBeforeTheTailCut(t *testing.T) {
	const prefix, secret, rest = "https://user:", "s3cr3t-token", "@git.example/repo.git "
	tests := []struct {
		name string
		// into is how many bytes of the secret lie before the raw tail.
		into int
	}{
		{"the raw tail begins inside the password", 6},
		{"the raw tail begins at the password", 0},
		{"the raw tail begins inside the scheme", -8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			padding := hookOutputTail - (len(secret) - tt.into) - len(rest)
			stdout := prefix + secret + rest + strings.Repeat("p ", padding/2) + strings.Repeat("p", padding%2)
			if got := len(stdout) - hookOutputTail; got != len(prefix)+tt.into {
				t.Fatalf("the fixture's raw tail begins at byte %d, want %d", got, len(prefix)+tt.into)
			}
			dialer := &pipeDialer{
				handler: func(conn net.Conn) {
					defer conn.Close()
					_, data, err := agentproto.ReadMessage(conn) // exec request
					if err != nil {
						return
					}
					var req agentproto.ExecRequest
					_ = json.Unmarshal(data, &req)
					agentproto.ReadMessage(conn) // stdin EOF
					if len(req.Cmd) > 0 && req.Cmd[0] == "/bin/sh" {
						_ = agentproto.WriteExitCode(conn, 0)
						return
					}
					// In pieces, as a hook's output arrives.
					for b := []byte(stdout); len(b) > 0; {
						n := min(len(b), 8000)
						_ = agentproto.WriteData(conn, b[:n])
						b = b[n:]
					}
					_ = agentproto.WriteExitCode(conn, 3)
				},
			}
			var out, errOut syncBuffer
			p := NewProvisioner(NewAgentClient(dialer, 1024, 1026), "test")
			p.SetOutput(&out, &errOut)

			err := p.runHook(context.Background(), &provision.Config{}, provision.HookTypeStartup, "hook.sh")
			var hookErr *provision.HookError
			if !errors.As(err, &hookErr) {
				t.Fatalf("runHook() error = %v, want a HookError", err)
			}
			if len(hookErr.LastOutput) == 0 || len(hookErr.LastOutput) > hookOutputTail {
				t.Errorf("last output is %d bytes, want 1 to %d", len(hookErr.LastOutput), hookOutputTail)
			}
			// No three bytes of the secret may survive, wherever the cut fell.
			for where, text := range map[string]string{"output": out.String(), "last output": hookErr.LastOutput, "error": hookErr.Error()} {
				for i := 0; i+3 <= len(secret); i++ {
					if part := secret[i : i+3]; strings.Contains(text, part) {
						t.Errorf("%s carries %q of the credential", where, part)
						break
					}
				}
			}
		})
	}
}
