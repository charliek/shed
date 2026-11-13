//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/version"
)

func TestReadLayoutFile(t *testing.T) {
	tests := []struct {
		name    string
		content *string // nil: file missing
		want    int
		wantErr bool
	}{
		{name: "missing is layout 0", content: nil, want: 0},
		{name: "one with newline", content: ptr("1\n"), want: 1},
		{name: "surrounding space", content: ptr("  7 \n"), want: 7},
		{name: "empty", content: ptr(""), wantErr: true},
		{name: "not a number", content: ptr("one\n"), wantErr: true},
		{name: "negative", content: ptr("-1\n"), wantErr: true},
		{name: "two numbers", content: ptr("1 2\n"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "layout")
			if tt.content != nil {
				if err := os.WriteFile(path, []byte(*tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readLayoutFile(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readLayoutFile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("readLayoutFile() = %d, want %d", got, tt.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestRunVersion(t *testing.T) {
	old := version.Version
	version.Version = "v0.9.2"
	defer func() { version.Version = old }()

	dir := t.TempDir()
	layoutOne := filepath.Join(dir, "layout")
	if err := os.WriteFile(layoutOne, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{name: "layout file", args: []string{"--layout-file", layoutOne}, wantOut: `{"version":"v0.9.2","protocol":1,"layout":1}` + "\n"},
		{name: "missing layout file", args: []string{"--layout-file", filepath.Join(dir, "none")}, wantOut: `{"version":"v0.9.2","protocol":1,"layout":0}` + "\n"},
		{name: "bad layout file", args: []string{"--layout-file", bad}, wantCode: 1},
		{name: "extra argument", args: []string{"extra"}, wantCode: 2},
		{name: "unknown flag", args: []string{"--nope"}, wantCode: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runVersion(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("runVersion() = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantCode != 0 {
				if stdout.Len() != 0 {
					t.Errorf("stdout on failure = %q, want empty", stdout.String())
				}
				return
			}
			if got := stdout.String(); got != tt.wantOut {
				t.Errorf("stdout = %q, want %q", got, tt.wantOut)
			}
			if strings.Count(stdout.String(), "\n") != 1 {
				t.Errorf("stdout is not exactly one line: %q", stdout.String())
			}
			// The server's parser reads what the agent prints.
			info, err := guest.ParseAgentInfo(stdout.Bytes())
			if err != nil {
				t.Fatalf("ParseAgentInfo: %v", err)
			}
			if info.Version != "v0.9.2" || info.Protocol != guest.AgentProtocol {
				t.Errorf("parsed %+v", info)
			}
		})
	}
}
