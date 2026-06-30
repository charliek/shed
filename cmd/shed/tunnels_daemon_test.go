package main

import (
	"slices"
	"strings"
	"testing"
)

func TestDaemonChildArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "appends --daemon",
			in:   []string{"tunnels", "start", "myshed", "-t", "3000", "-d"},
			want: []string{"tunnels", "start", "myshed", "-t", "3000", "-d", "--daemon"},
		},
		{
			name: "preserves global flags",
			in:   []string{"-s", "srv", "-c", "/cfg.yaml", "tunnels", "start", "myshed", "-p", "dev", "-d"},
			want: []string{"-s", "srv", "-c", "/cfg.yaml", "tunnels", "start", "myshed", "-p", "dev", "-d", "--daemon"},
		},
		{
			name: "dedups an existing --daemon",
			in:   []string{"tunnels", "start", "myshed", "--daemon", "-d"},
			want: []string{"tunnels", "start", "myshed", "-d", "--daemon"},
		},
		{
			name: "inserts before a -- terminator",
			in:   []string{"tunnels", "start", "myshed", "-d", "--", "extra"},
			want: []string{"tunnels", "start", "myshed", "-d", "--daemon", "--", "extra"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := daemonChildArgs(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("daemonChildArgs(%v)\n  = %v\n want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDaemonChildArgsNoTokenInjected documents the token-hygiene contract: the
// child argv is exactly the parent's argv (+ --daemon). The credentials token
// comes from config, never the command line, so it cannot leak into the worker's
// argv / `ps`.
func TestDaemonChildArgsNoTokenInjected(t *testing.T) {
	in := []string{"tunnels", "start", "myshed", "-d"}
	got := daemonChildArgs(in)
	for _, a := range got {
		if a != "--daemon" && !slices.Contains(in, a) {
			t.Errorf("daemonChildArgs injected an unexpected arg %q (token leak risk)", a)
		}
	}
}

func TestParseReadyMessage(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr bool
		wantMsg string // substring expected in the error, when wantErr
	}{
		{name: "ok", line: "OK\n", wantErr: false},
		{name: "ok no newline", line: "OK", wantErr: false},
		{name: "error carries message", line: "ERR:port 3000 in use\n", wantErr: true, wantMsg: "port 3000 in use"},
		{name: "empty is unexpected", line: "", wantErr: true, wantMsg: "unexpected"},
		{name: "garbage is unexpected", line: "weird line\n", wantErr: true, wantMsg: "unexpected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := parseReadyMessage(tc.line)
			if tc.wantErr != (err != nil) {
				t.Fatalf("parseReadyMessage(%q) err = %v, wantErr = %v", tc.line, err, tc.wantErr)
			}
			if tc.wantErr && tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("parseReadyMessage(%q) err = %q, want substring %q", tc.line, err.Error(), tc.wantMsg)
			}
		})
	}
}
