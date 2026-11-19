package sync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

// fakeSSH puts an `ssh` on PATH that writes its argv, one element per line,
// to the returned file, and exits 0. The file's absence means ssh never ran.
func fakeSSH(t *testing.T) (argvFile string) {
	t.Helper()
	bin := t.TempDir()
	argvFile = filepath.Join(t.TempDir(), "argv")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + argvFile + "'; done\ncat > /dev/null\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

// writeTestKey writes a new unencrypted OpenSSH-format ed25519 private key at
// path: an identity file whose public key can be learned.
func writeTestKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSyncSSHArgs pins the argv every sync step runs: without an identity file
// exactly as before the field existed; with one, `-i <path> -o
// IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none` before the
// destination.
func TestSyncSSHArgs(t *testing.T) {
	t.Setenv("HOME", "/home/pat")
	s := NewSyncer(&Config{})
	s.SetServerName("srv")
	entry := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	base := []string{
		"-p", "2222",
		"-o", "UserKnownHostsFile=/home/pat/.shed/known_hosts",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
	}
	got, err := s.sshArgs("box", entry, "true")
	if err != nil {
		t.Fatal(err)
	}
	if want := slices.Concat(base, []string{"box@shed.example.test", "true"}); !slices.Equal(got, want) {
		t.Errorf("argv without an identity file = %q, want %q", got, want)
	}

	key := filepath.Join(t.TempDir(), "id work")
	writeTestKey(t, key)
	entry.IdentityFile = key
	got, err = s.sshArgs("box", entry, "true")
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(base, []string{"-i", key, "-o", "IdentitiesOnly=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"box@shed.example.test", "true"})
	if !slices.Equal(got, want) {
		t.Errorf("argv with an identity file = %q, want %q", got, want)
	}
}

// TestSyncRefusesAnIdentityFileBeforeSSH: each sync step that runs ssh (the
// transfer, a hook, the log) refuses an identity file it cannot use, named for
// its entry, and never spawns ssh; with a usable one, ssh runs with it.
func TestSyncRefusesAnIdentityFileBeforeSSH(t *testing.T) {
	argvFile := fakeSSH(t)
	s := NewSyncer(&Config{})
	s.SetServerName("srv")
	entry := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: filepath.Join(t.TempDir(), "removed")}
	ctx := context.Background()

	steps := map[string]func() error{
		"transfer": func() error {
			return s.transferViaSsh(ctx, []byte("tar"), "/home/shed/x", "/tmp/x", "x", false, "box", entry)
		},
		"hook": func() error { return s.runPostSyncHook(ctx, PostSyncHook{Run: "true"}, "box", entry) },
		"log":  func() error { return s.SaveSyncLog(ctx, "log", "box", entry) },
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			err := step()
			var ie *config.IdentityFileError
			if !errors.As(err, &ie) || ie.Entry != "srv" || !strings.Contains(ie.Reason, "does not exist") {
				t.Fatalf("err = %v, want srv's identity file refused", err)
			}
			if _, err := os.Stat(argvFile); err == nil {
				t.Fatal("ssh was spawned for an identity file that cannot be used")
			}
		})
	}

	// The negative control: with a usable key the same step runs the fake ssh,
	// and its argv carries the key and no shared connection.
	key := filepath.Join(t.TempDir(), "id_work")
	writeTestKey(t, key)
	entry.IdentityFile = key
	if err := s.SaveSyncLog(ctx, "log", "box", entry); err != nil {
		t.Fatalf("SaveSyncLog with a usable key: %v", err)
	}
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the fake ssh did not run: %v", err)
	}
	if !strings.Contains(string(data), "-i\n"+key+"\n-o\nIdentitiesOnly=yes\n-o\nControlMaster=no\n-o\nControlPath=none\n") {
		t.Errorf("ssh argv does not carry the key:\n%s", data)
	}
}
