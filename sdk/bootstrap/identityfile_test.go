package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSSHArgsExactVector pins the bootstrap argv element for element, without
// an identity file (exactly the argv before the field existed: the 17 `-o`
// options, then `-l _bootstrap`) and with one, where `-i <path> -o
// IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none` sits after the
// options and before `-l`. The Rust broker's ssh_args must produce the same
// vector for the same input.
func TestSSHArgsExactVector(t *testing.T) {
	base := Params{Host: "shed.example.test", Port: 2222, KnownHostsPath: "/home/pat/.shed/known_hosts", Scope: "control", ClientKind: "cli"}
	options := []string{
		"-T",
		"-p", "2222",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=/home/pat/.shed/known_hosts",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "VerifyHostKeyDNS=no",
		"-o", "KnownHostsCommand=none",
		"-o", "UpdateHostKeys=no",
		"-o", "CheckHostIP=no",
		"-o", "PreferredAuthentications=publickey",
		"-o", "PubkeyAuthentication=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "ChallengeResponseAuthentication=no",
		"-o", "NumberOfPasswordPrompts=0",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
	}
	tail := []string{"-l", "_bootstrap", "shed.example.test", "control", "cli"}

	t.Run("without an identity file the argv is unchanged", func(t *testing.T) {
		want := slices.Concat(options, tail)
		if got := sshArgs(base); !slices.Equal(got, want) {
			t.Errorf("argv =\n %q\nwant\n %q", got, want)
		}
		if n := strings.Count(strings.Join(options, " "), "-o "); n != 17 {
			t.Fatalf("test vector has %d -o options, want 17", n)
		}
	})

	t.Run("with an identity file, -i, IdentitiesOnly and no shared connection sit before -l", func(t *testing.T) {
		p := base
		p.IdentityFile = "/home/pat/.ssh/id work"
		want := slices.Concat(options, []string{"-i", "/home/pat/.ssh/id work", "-o", "IdentitiesOnly=yes",
			"-o", "ControlMaster=no", "-o", "ControlPath=none"}, tail)
		got, err := Args(p)
		if err != nil {
			t.Fatalf("Args: %v", err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("argv =\n %q\nwant\n %q", got, want)
		}
	})
}

// identityValues is the shared table of the value rule: accepted values, and
// each refused one with the reason it must report. Non-ASCII characters are
// written as byte escapes so the source shows which one each case holds:
// \xc2\xa0 is U+00A0 (no-break space), \xc2\x85 U+0085 (next line),
// \xe2\x80\xa8 U+2028 (line separator), \xe3\x80\x80 U+3000 (ideographic
// space).
var identityValues = []struct {
	name   string
	path   string
	reason string // "" when accepted
}{
	{"plain", "/home/pat/.ssh/id_work", ""},
	{"a space", "/home/pat/.ssh/id work", ""},
	{"an apostrophe", "/home/pat/.ssh/pat's key", ""},
	{"a colon", "/home/pat/.ssh/id:work", ""},
	{"non-ASCII", "/home/pat/.ssh/cl\xc3\xa9_travail", ""},
	{"a tab inside", "/home/pat/.ssh/id\twork", ""},
	{"a no-break space inside", "/home/pat/.ssh/id\xc2\xa0work", ""},
	{"empty", "", "it is empty"},
	{"only spaces", "   ", "it is only whitespace"},
	{"only a tab", "\t", "it is only whitespace"},
	{"NUL", "/home/pat/.ssh/id\x00work", "NUL"},
	{"CR", "/home/pat/.ssh/id\rwork", "carriage return"},
	{"LF", "/home/pat/.ssh/id\nwork", "line feed"},
	{"double quote", `/home/pat/.ssh/id"work`, "double quote"},
	{"backslash", `/home/pat/.ssh/id\work`, "backslash"},
	{"hash", "/home/pat/.ssh/id#work", "#"},
	{"percent", "/home/pat/.ssh/id_%h", "%"},
	{"dollar", "/home/pat/.ssh/${KEY}", "$"},
	{"leading space", " /home/pat/.ssh/id_work", "begins with whitespace"},
	{"trailing space", "/home/pat/.ssh/id_work ", "ends with whitespace"},
	{"leading tab", "\t/home/pat/.ssh/id_work", "begins with whitespace"},
	{"leading no-break space (U+00A0)", "\xc2\xa0/home/pat/.ssh/id_work", "begins with whitespace"},
	{"trailing no-break space (U+00A0)", "/home/pat/.ssh/id_work\xc2\xa0", "ends with whitespace"},
	{"trailing next line (U+0085)", "/home/pat/.ssh/id_work\xc2\x85", "ends with whitespace"},
	{"trailing line separator (U+2028)", "/home/pat/.ssh/id_work\xe2\x80\xa8", "ends with whitespace"},
	{"leading ideographic space (U+3000)", "\xe3\x80\x80/home/pat/.ssh/id_work", "begins with whitespace"},
	{"relative", ".ssh/id_work", "not an absolute path"},
	{"a tilde path", "~/.ssh/id_work", "not an absolute path"},
	{"invalid UTF-8", "/home/pat/.ssh/id\xffwork", "not valid UTF-8"},
	{"invalid UTF-8 at the end", "/home/pat/.ssh/id_work\xc3", "not valid UTF-8"},
}

// TestValidateIdentityFile holds the one value rule: each accepted value
// passes, each refused one is refused with its own reason.
func TestValidateIdentityFile(t *testing.T) {
	for _, tc := range identityValues {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIdentityFile(tc.path)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("ValidateIdentityFile(%q) = %v, want accepted", tc.path, err)
				}
				return
			}
			var ie *IdentityFileError
			if !errors.As(err, &ie) {
				t.Fatalf("ValidateIdentityFile(%q) = %v, want an *IdentityFileError", tc.path, err)
			}
			if !strings.Contains(ie.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to say %q", ie.Reason, tc.reason)
			}
		})
	}
}

// TestValidateRefusesABadIdentityFile: Params.IdentityFile goes through the
// same rule as every other argv element, so a refused value never reaches an
// argv (Args, and so Run, refuse it).
func TestValidateRefusesABadIdentityFile(t *testing.T) {
	base := Params{Host: "h", Port: 22, KnownHostsPath: "kh", Scope: "control", ClientKind: "cli"}
	for _, tc := range identityValues {
		if tc.reason == "" || tc.path == "" {
			continue // accepted, or "" which means no identity file at all
		}
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.IdentityFile = tc.path
			if argv, err := Args(p); err == nil {
				t.Fatalf("Args accepted identity file %q: %q", tc.path, argv)
			}
		})
	}
}

// TestCheckIdentityFile: at the time of use the file must be a readable
// regular file (a symbolic link to one is followed). The open never blocks,
// and the type is that of the file opened.
func TestCheckIdentityFile(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id work")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "dirlink")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nothing"), dangling); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(unreadable, []byte("key"), 0o000); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path, reason string
	}{
		{"a regular file", key, ""},
		{"a link to one", link, ""},
		{"missing", filepath.Join(dir, "gone"), "does not exist"},
		{"a dangling link", dangling, "does not exist"},
		{"a directory", dir, "directory"},
		{"a link to a directory", dirLink, "directory"},
		// Opened without blocking (a FIFO with no writer would block a plain
		// open for ever), then refused by the type of what was opened.
		{"a fifo", fifo, "not a regular file"},
		{"a device", "/dev/null", "not a regular file"},
		{"a refused value is refused before the file is looked at", key + "#", "#"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- CheckIdentityFile(tc.path) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("CheckIdentityFile(%q) did not return: the open blocked", tc.path)
			}
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("CheckIdentityFile(%q) = %v", tc.path, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("CheckIdentityFile(%q) = %v, want %q", tc.path, err, tc.reason)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a 0000 file")
		}
		if err := CheckIdentityFile(unreadable); err == nil || !strings.Contains(err.Error(), "cannot be opened (permission denied)") {
			t.Fatalf("CheckIdentityFile(unreadable) = %v", err)
		}
	})
}

// TestIdentityFileErrorsCarryNoRawControlCharacter: a path may hold a control
// character in the middle (only NUL, CR and LF are refused), and a file system
// error about it must not repeat it raw: the message quotes the path, and the
// reason gives the error's own words without the path, escaped.
func TestIdentityFileErrorsCarryNoRawControlCharacter(t *testing.T) {
	dir := t.TempDir()
	// A name longer than any file system allows, holding an ESC: the open
	// fails with ENAMETOOLONG, whose *fs.PathError repeats the path.
	long := filepath.Join(dir, "id\x1b[31m"+strings.Repeat("k", 300))
	err := CheckIdentityFile(long)
	var ie *IdentityFileError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want an *IdentityFileError", err)
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("the message carries a raw ESC: %q", err.Error())
	}
	if strings.Contains(ie.Reason, dir) {
		t.Errorf("the reason repeats the path: %q", ie.Reason)
	}
	if !strings.Contains(ie.Reason, "file name too long") {
		t.Errorf("reason = %q, want the file system's own words", ie.Reason)
	}
	// A reason that does hold a control character (ESC, U+2028, a stray
	// byte) is escaped in the message.
	raw := &IdentityFileError{Path: "/k", Reason: "bad\x1b[2Jthing\xe2\x80\xa8\xff"}
	got := raw.Error()
	if strings.ContainsAny(got, "\x1b\xe2\xff") || !strings.Contains(got, `bad\x1b[2Jthing\`+`u2028\xff`) {
		t.Errorf("Error() = %q, want the control characters escaped", got)
	}
}

// TestRunChecksTheIdentityFileBeforeSSH: a key removed after it was set fails
// with the identity file's own message, and ssh is never spawned. The ssh on
// PATH here is a fake that leaves a mark when it runs.
func TestRunChecksTheIdentityFileBeforeSSH(t *testing.T) {
	bin := t.TempDir()
	mark := filepath.Join(t.TempDir(), "ssh-ran")
	script := "#!/bin/sh\n: > '" + mark + "'\nexit 1\n" // a shell builtin: PATH holds only this fake
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	p := Params{Host: "127.0.0.1", Port: 2222, KnownHostsPath: "/nonexistent/known_hosts", Scope: "control", ClientKind: "cli",
		IdentityFile: filepath.Join(t.TempDir(), "removed_key")}
	_, err := RunCredential(t.Context(), p)
	var ie *IdentityFileError
	if !errors.As(err, &ie) || !strings.Contains(ie.Reason, "does not exist") {
		t.Fatalf("err = %v, want the identity file's own refusal", err)
	}
	if _, statErr := os.Stat(mark); statErr == nil {
		t.Fatal("ssh was spawned for an identity file that does not exist")
	}

	// The negative control: with no identity file the fake ssh does run, so
	// the mark above would have been seen.
	p.IdentityFile = ""
	_, _ = RunCredential(t.Context(), p)
	if _, statErr := os.Stat(mark); statErr != nil {
		t.Fatalf("the fake ssh did not run without an identity file (%v): the test proves nothing", statErr)
	}
}
