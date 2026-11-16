package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// reservedBackend answers GetShed for the reserved name only, and records the
// principal it was asked as.
type reservedBackend struct {
	backend.Backend
	shed *config.Shed
	err  error
	kind backend.PrincipalKind
	name string
}

func (b *reservedBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	b.name = name
	if p, ok := backend.PrincipalFrom(ctx); ok {
		b.kind = p.Kind
	}
	return b.shed, b.err
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// TestReportReservedSheds: a shed named git is reported with what to do; not
// found is silent; another error is logged and returns (start-up goes on). The
// lookup is made as the server's own principal.
func TestReportReservedSheds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		be       *reservedBackend
		wantLogs []string
		silent   bool
	}{
		{"found", &reservedBackend{shed: &config.Shed{Name: "git"}}, []string{"WARNING", `"git"`, "SSH to it is refused", "Snapshot it and recreate under another name, or delete it"}, false},
		{"not found", &reservedBackend{err: config.ErrShedNotFoundSentinel}, nil, true},
		{"not found, wrapped", &reservedBackend{err: errors.Join(errors.New("ctx"), config.ErrShedNotFoundSentinel)}, nil, true},
		{"other error", &reservedBackend{err: errors.New("cloud list failed")}, []string{"Could not check", "cloud list failed"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			reportReservedSheds(tc.be)
			out := buf.String()
			if tc.silent && out != "" {
				t.Errorf("logged %q, want nothing", out)
			}
			for _, want := range tc.wantLogs {
				if !strings.Contains(out, want) {
					t.Errorf("log %q lacks %q", out, want)
				}
			}
			if tc.name == "other error" && strings.Contains(out, "WARNING: a shed named") {
				t.Errorf("an error was reported as a found shed: %q", out)
			}
			if tc.be.name != "git" || tc.be.kind != backend.PrincipalSystem {
				t.Errorf("asked for %q as principal kind %v, want git as the system principal", tc.be.name, tc.be.kind)
			}
		})
	}
}
