package main

import (
	"strings"
	"testing"
)

// buildExecCommand applies the same logic as ExecInContainer for building
// the command to pass to Docker exec. This is extracted for testing.
func buildExecCommand(cmd []string) []string {
	if len(cmd) == 0 {
		return []string{"/bin/bash", "--login"}
	}
	// Wrap command in shell to support operators like &&, ||, |, etc.
	return []string{"/bin/sh", "-c", strings.Join(cmd, " ")}
}

func TestBuildExecCommand_EmptyCommand(t *testing.T) {
	result := buildExecCommand(nil)

	if len(result) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(result))
	}
	if result[0] != "/bin/bash" {
		t.Errorf("expected /bin/bash, got %s", result[0])
	}
	if result[1] != "--login" {
		t.Errorf("expected --login, got %s", result[1])
	}
}

func TestBuildExecCommand_SimpleCommand(t *testing.T) {
	result := buildExecCommand([]string{"echo", "hello"})

	if len(result) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(result))
	}
	if result[0] != "/bin/sh" {
		t.Errorf("expected /bin/sh, got %s", result[0])
	}
	if result[1] != "-c" {
		t.Errorf("expected -c, got %s", result[1])
	}
	if result[2] != "echo hello" {
		t.Errorf("expected 'echo hello', got %s", result[2])
	}
}

func TestBuildExecCommand_CompoundCommand(t *testing.T) {
	// This tests the fix for shell operators like && not being parsed correctly
	result := buildExecCommand([]string{"mkdir", "-p", "/tmp", "&&", "echo", "done"})

	if len(result) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(result))
	}
	if result[0] != "/bin/sh" {
		t.Errorf("expected /bin/sh, got %s", result[0])
	}
	if result[1] != "-c" {
		t.Errorf("expected -c, got %s", result[1])
	}
	expected := "mkdir -p /tmp && echo done"
	if result[2] != expected {
		t.Errorf("expected %q, got %q", expected, result[2])
	}
}

func TestBuildExecCommand_PipeCommand(t *testing.T) {
	// Test pipe operator
	result := buildExecCommand([]string{"cat", "/etc/passwd", "|", "grep", "root"})

	expected := "cat /etc/passwd | grep root"
	if result[2] != expected {
		t.Errorf("expected %q, got %q", expected, result[2])
	}
}

func TestBuildExecCommand_ComplexCommand(t *testing.T) {
	// Test a complex sync-like command
	result := buildExecCommand([]string{"mkdir", "-p", "/usr/local/share/ca-certificates", "&&", "tar", "xzpf", "-", "-C", "/usr/local/share/ca-certificates"})

	expected := "mkdir -p /usr/local/share/ca-certificates && tar xzpf - -C /usr/local/share/ca-certificates"
	if result[2] != expected {
		t.Errorf("expected %q, got %q", expected, result[2])
	}
}
