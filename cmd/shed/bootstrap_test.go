package main

import (
	"slices"
	"strings"
	"testing"
)

func TestBootstrapSSHArgs(t *testing.T) {
	args := bootstrapSSHArgs("vps.example.com", 2222, "/home/u/.shed/known_hosts", "control", "cli")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-T", // no pty, so the JSON bundle lands clean on stdout
		"-p 2222",
		"BatchMode=yes",
		"UserKnownHostsFile=/home/u/.shed/known_hosts",
		"StrictHostKeyChecking=yes",
		"_bootstrap@vps.example.com",
		"control",
		"cli",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}

	// scope must precede clientKind — the server parses Fields()[0] as the scope.
	si, ci := slices.Index(args, "control"), slices.Index(args, "cli")
	if si < 0 || ci < 0 || si >= ci {
		t.Errorf("scope must come before clientKind: %v", args)
	}

	// An empty clientKind omits the trailing arg, so there is no bare kind for
	// the server to mis-parse as the scope.
	noKind := bootstrapSSHArgs("h", 22, "kh", "control", "")
	if noKind[len(noKind)-1] != "control" {
		t.Errorf("empty clientKind should leave scope as the final arg: %v", noKind)
	}
}
