package bootstrap

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestArgs(t *testing.T) {
	args := sshArgs(Params{Host: "vps.example.com", Port: 2222, KnownHostsPath: "/home/u/.shed/known_hosts", Scope: "control", ClientKind: "cli"})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-T", // no pty so the JSON bundle lands clean on stdout
		"-p 2222",
		"BatchMode=yes",
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/home/u/.shed/known_hosts",
		"GlobalKnownHostsFile=/dev/null", // the shed file must be the SOLE trust root
		"VerifyHostKeyDNS=no",
		"KnownHostsCommand=none",
		"PreferredAuthentications=publickey",
		"PasswordAuthentication=no",
		"ForwardAgent=no",         // no config-driven side effects during the mint
		"ClearAllForwardings=yes", //
		"PermitLocalCommand=no",   //
		"-l _bootstrap",           // separate -l, not _bootstrap@host
		"vps.example.com",
		"control",
		"cli",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	// The host must never be fused with the user (avoids username-parsing ambiguity).
	if strings.Contains(joined, "_bootstrap@") {
		t.Errorf("host must not be _bootstrap@host form: %v", args)
	}
	// scope must precede clientKind — the server parses Fields()[0] as the scope.
	si, ci := slices.Index(args, "control"), slices.Index(args, "cli")
	if si < 0 || ci < 0 || si >= ci {
		t.Errorf("scope must come before clientKind: %v", args)
	}
	// An empty clientKind omits the trailing arg, so the scope stays final.
	noKind := sshArgs(Params{Host: "h", Port: 22, KnownHostsPath: "kh", Scope: "control"})
	if noKind[len(noKind)-1] != "control" {
		t.Errorf("empty clientKind should leave scope as the final arg: %v", noKind)
	}
}

func TestValidate(t *testing.T) {
	base := Params{Host: "h", Port: 22, KnownHostsPath: "kh", Scope: "control", ClientKind: "cli"}
	tests := []struct {
		name    string
		mutate  func(p *Params)
		wantErr bool
	}{
		{"valid", func(*Params) {}, false},
		{"valid no kind", func(p *Params) { p.ClientKind = "" }, false},
		{"empty host", func(p *Params) { p.Host = "" }, true},
		{"host option injection", func(p *Params) { p.Host = "-oProxyCommand=touch /tmp/x" }, true},
		{"host with @", func(p *Params) { p.Host = "root@h" }, true},
		{"host with space", func(p *Params) { p.Host = "a b" }, true},
		{"host with newline", func(p *Params) { p.Host = "a\nb" }, true},
		{"port zero", func(p *Params) { p.Port = 0 }, true},
		{"port too high", func(p *Params) { p.Port = 70000 }, true},
		{"empty known_hosts", func(p *Params) { p.KnownHostsPath = "" }, true},
		{"empty scope", func(p *Params) { p.Scope = "" }, true},
		{"multi-token scope", func(p *Params) { p.Scope = "control cli" }, true},
		{"multi-token kind", func(p *Params) { p.ClientKind = "cli extra" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			err := validate(p)
			if (err != nil) != tc.wantErr {
				t.Errorf("validate(%+v) err = %v, wantErr %v", p, err, tc.wantErr)
			}
		})
	}
}

// Real OpenSSH stderr samples. The CHANGED banner is the only terminal signal.
const (
	stderrChanged = `@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!
Someone could be eavesdropping on you right now (man-in-the-middle attack)!
Host key verification failed.`

	stderrMissingPin = `No ED25519 host key is known for [vps.example.com]:2222 and you have requested strict checking.
Host key verification failed.`

	stderrPermDenied  = `_bootstrap@vps.example.com: Permission denied (publickey).`
	stderrConnRefused = `ssh: connect to host vps.example.com port 2222: Connection refused`
)

func TestClassify(t *testing.T) {
	// changed mirrors what capWriter's streaming detector reports for the sample.
	tests := []struct {
		name         string
		exit         int
		changed      bool
		stderr       string
		wantSentinel error
		// notSentinels must NOT match (guards against over-classification).
		notSentinels []error
	}{
		{"changed key is terminal mismatch", 255, true, stderrChanged, ErrHostKeyMismatch, []error{ErrHostKeyVerificationFailed, ErrNoSSHIdentities}},
		{"missing pin is non-terminal", 255, false, stderrMissingPin, ErrHostKeyVerificationFailed, []error{ErrHostKeyMismatch}},
		{"permission denied is identities error", 255, false, stderrPermDenied, ErrNoSSHIdentities, []error{ErrHostKeyMismatch, ErrHostKeyVerificationFailed}},
		{"connection refused is generic", 255, false, stderrConnRefused, nil, []error{ErrHostKeyMismatch, ErrHostKeyVerificationFailed, ErrNoSSHIdentities}},
		// 255 is a GATE: a CHANGED banner without exit 255 must NOT latch terminal.
		{"changed banner but wrong exit code", 1, true, "WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!", nil, []error{ErrHostKeyMismatch}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classify(errors.New("exit status"), tc.exit, tc.changed, tc.stderr)
			if err == nil {
				t.Fatal("classify returned nil error")
			}
			if tc.wantSentinel != nil && !errors.Is(err, tc.wantSentinel) {
				t.Errorf("error %v is not %v", err, tc.wantSentinel)
			}
			for _, ns := range tc.notSentinels {
				if errors.Is(err, ns) {
					t.Errorf("error %v must NOT match %v (over-classification)", err, ns)
				}
			}
		})
	}
}

func TestCapWriterMarkerDetection(t *testing.T) {
	marker := "REMOTE HOST IDENTIFICATION HAS CHANGED"

	t.Run("marker split across writes", func(t *testing.T) {
		w := &capWriter{max: 1024, marker: []byte(marker)}
		// Split the marker across two writes — the carry buffer must bridge them.
		mid := len(marker) / 2
		_, _ = w.Write([]byte("noise " + marker[:mid]))
		_, _ = w.Write([]byte(marker[mid:] + " more"))
		if !w.markerSeen {
			t.Error("marker split across writes was not detected")
		}
	})

	t.Run("marker after the cap is full", func(t *testing.T) {
		w := &capWriter{max: 8, marker: []byte(marker)}
		// Fill far past the 8-byte cap with junk, THEN print the banner.
		_, _ = w.Write([]byte(strings.Repeat("x", 100_000)))
		_, _ = w.Write([]byte("\n" + marker + "!\n"))
		if !w.markerSeen {
			t.Error("marker past the buffer cap was not detected")
		}
		if w.buf.Len() > 8 {
			t.Errorf("buffer exceeded cap: %d", w.buf.Len())
		}
	})

	t.Run("absent marker", func(t *testing.T) {
		w := &capWriter{max: 1024, marker: []byte(marker)}
		_, _ = w.Write([]byte("Permission denied (publickey)."))
		if w.markerSeen {
			t.Error("false positive marker detection")
		}
	})
}

func TestDecodeBundle(t *testing.T) {
	const token = "shed_control_supersecret"
	valid := `{"https_port":8443,"tls_cert_fingerprint":"sha256:abc","token":"` + token + `","scope":"control","token_id":"t1"}`

	t.Run("valid", func(t *testing.T) {
		b, err := decodeBundle([]byte(valid), "control")
		if err != nil {
			t.Fatalf("decodeBundle: %v", err)
		}
		if b.Token != token || b.Scope != "control" || b.HTTPSPort != 8443 {
			t.Errorf("bundle = %+v", b)
		}
	})

	bad := []struct {
		name, in, scope string
	}{
		{"empty", "", "control"},
		{"not json", "not json", "control"},
		{"empty token", `{"scope":"control"}`, "control"},
		{"no usable port", `{"token":"x","scope":"control"}`, "control"},
		{"https without fingerprint", `{"token":"x","https_port":8443,"scope":"control"}`, "control"},
		{"trailing garbage", valid + ` extra`, "control"},
		{"scope mismatch", valid, "credentials"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeBundle([]byte(tc.in), tc.scope)
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			// The token must NEVER appear in an error (stdout carries the secret).
			if strings.Contains(err.Error(), token) {
				t.Errorf("error leaked the token: %v", err)
			}
		})
	}
}
