package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecidePlanShed(t *testing.T) {
	cases := []struct {
		name  string
		found bool
		repo  string
		want  planShedAction
	}{
		{"existing, no repo", true, "", planUseExisting},
		{"existing + repo warns", true, "owner/repo", planWarnIgnoreRepo},
		{"missing + repo creates", false, "owner/repo", planCreateMissing},
		{"missing, no repo errors", false, "", planErrorMissingNoRepo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decidePlanShed(tc.found, tc.repo); got != tc.want {
				t.Errorf("decidePlanShed(%v, %q) = %d, want %d", tc.found, tc.repo, got, tc.want)
			}
		})
	}
}

func TestReadPlanArg(t *testing.T) {
	t.Run("reads a file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plan.md")
		if err := os.WriteFile(path, []byte("# Plan\n\nDo it.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := readPlanArg(path)
		if err != nil {
			t.Fatalf("readPlanArg: %v", err)
		}
		if !strings.Contains(got, "Do it.") {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, err := readPlanArg(filepath.Join(t.TempDir(), "nope.md")); err == nil {
			t.Fatal("want error for missing file")
		}
	})

	t.Run("empty plan rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.md")
		if err := os.WriteFile(path, []byte("   \n\t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readPlanArg(path)
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("want empty error, got %v", err)
		}
	})

	t.Run("non-UTF8 plan rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bin.md")
		if err := os.WriteFile(path, []byte("a\xffb"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readPlanArg(path)
		if err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("want UTF-8 error, got %v", err)
		}
	})

	t.Run("oversized plan rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "big.md")
		big := make([]byte, (1<<20)+1)
		for i := range big {
			big[i] = 'a'
		}
		if err := os.WriteFile(path, big, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readPlanArg(path)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("want oversize error, got %v", err)
		}
	})
}

// planFailAfterCreate must report BOTH facts (shed created + plan failed) and state
// the shed was NOT deleted when the shed was freshly created; it passes the cause
// through unchanged for an already-existing shed.
func TestPlanFailAfterCreate(t *testing.T) {
	cause := os.ErrPermission

	created := planFailAfterCreate(true, "my-shed", cause)
	msg := created.Error()
	for _, want := range []string{"was created", "NOT deleted", "my-shed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("created-shed error missing %q: %s", want, msg)
		}
	}

	existing := planFailAfterCreate(false, "my-shed", cause)
	if existing != cause {
		t.Errorf("existing-shed path must pass the cause through, got %v", existing)
	}
}
