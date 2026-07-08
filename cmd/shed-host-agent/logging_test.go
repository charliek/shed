package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLogWriterStderrWhenEmpty(t *testing.T) {
	if w := newLogWriter(""); w != io.Writer(os.Stderr) {
		t.Fatalf("empty log file should write to stderr, got %T", w)
	}
}

func TestNewLogWriterWritesToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w := newLogWriter(path)
	if c, ok := w.(io.Closer); ok {
		defer c.Close()
	}
	if _, err := io.WriteString(w, "hello rotation\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "hello rotation") {
		t.Fatalf("log file missing the written line: %q", string(data))
	}
}
