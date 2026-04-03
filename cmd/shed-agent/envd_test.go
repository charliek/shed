package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvironmentD(t *testing.T) {
	dir := t.TempDir()

	// Write two conf files — 10 should be read before 20
	if err := os.WriteFile(filepath.Join(dir, "10-base.conf"), []byte(
		"FOO=bar\nBAZ=qux\n",
	), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20-extensions.conf"), []byte(
		"SSH_AUTH_SOCK=/run/shed-extensions/ssh-agent.sock\nAWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:499/credentials\n",
	), 0644); err != nil {
		t.Fatal(err)
	}

	env := loadEnvironmentD(dir)

	expected := []string{
		"FOO=bar",
		"BAZ=qux",
		"SSH_AUTH_SOCK=/run/shed-extensions/ssh-agent.sock",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:499/credentials",
	}

	if len(env) != len(expected) {
		t.Fatalf("got %d vars, want %d: %v", len(env), len(expected), env)
	}
	for i, want := range expected {
		if env[i] != want {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want)
		}
	}
}

func TestLoadEnvironmentDCommentsAndBlankLines(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "test.conf"), []byte(
		"# This is a comment\n\nVALID=yes\n  # indented comment\n  \nALSO_VALID=true\n",
	), 0644); err != nil {
		t.Fatal(err)
	}

	env := loadEnvironmentD(dir)

	if len(env) != 2 {
		t.Fatalf("got %d vars, want 2: %v", len(env), env)
	}
	if env[0] != "VALID=yes" {
		t.Errorf("env[0] = %q, want %q", env[0], "VALID=yes")
	}
	if env[1] != "ALSO_VALID=true" {
		t.Errorf("env[1] = %q, want %q", env[1], "ALSO_VALID=true")
	}
}

func TestLoadEnvironmentDMissingDir(t *testing.T) {
	env := loadEnvironmentD("/nonexistent/path")
	if len(env) != 0 {
		t.Fatalf("expected empty slice for missing dir, got %v", env)
	}
}

func TestLoadEnvironmentDEmptyDir(t *testing.T) {
	dir := t.TempDir()
	env := loadEnvironmentD(dir)
	if len(env) != 0 {
		t.Fatalf("expected empty slice for empty dir, got %v", env)
	}
}

func TestLoadEnvironmentDFileOrder(t *testing.T) {
	dir := t.TempDir()

	// Write files in reverse alphabetical order to verify sorting
	if err := os.WriteFile(filepath.Join(dir, "z-last.conf"), []byte("ORDER=last\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a-first.conf"), []byte("ORDER=first\n"), 0644); err != nil {
		t.Fatal(err)
	}

	env := loadEnvironmentD(dir)

	if len(env) != 2 {
		t.Fatalf("got %d vars, want 2: %v", len(env), env)
	}
	// a-first.conf should be read first
	if env[0] != "ORDER=first" {
		t.Errorf("env[0] = %q, want %q", env[0], "ORDER=first")
	}
	if env[1] != "ORDER=last" {
		t.Errorf("env[1] = %q, want %q", env[1], "ORDER=last")
	}
}
