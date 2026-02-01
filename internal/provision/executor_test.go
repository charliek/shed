package provision

import (
	"testing"
)

func TestGetLastLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		n        int
		expected string
	}{
		{
			name:     "fewer lines than requested",
			input:    "line1\nline2",
			n:        5,
			expected: "line1\nline2",
		},
		{
			name:     "exact number of lines",
			input:    "line1\nline2\nline3",
			n:        3,
			expected: "line1\nline2\nline3",
		},
		{
			name:     "more lines than requested",
			input:    "line1\nline2\nline3\nline4\nline5",
			n:        2,
			expected: "line4\nline5",
		},
		{
			name:     "empty string",
			input:    "",
			n:        3,
			expected: "",
		},
		{
			name:     "single line",
			input:    "single",
			n:        1,
			expected: "single",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLastLines(tt.input, tt.n)
			if got != tt.expected {
				t.Errorf("getLastLines(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.expected)
			}
		})
	}
}

func TestHookError_Error(t *testing.T) {
	err := &HookError{
		HookType:   HookTypeInstall,
		ExitCode:   1,
		LogFile:    "/var/log/shed/install.log",
		LastOutput: "some error output",
	}

	expected := "install hook failed (exit code 1)"
	if got := err.Error(); got != expected {
		t.Errorf("Error() = %q, want %q", got, expected)
	}
}

func TestLogFileForHook(t *testing.T) {
	executor := &Executor{}

	tests := []struct {
		hookType HookType
		expected string
	}{
		{HookTypeInstall, InstallLog},
		{HookTypeStartup, StartupLog},
		{HookType("custom"), "/var/log/shed/custom.log"},
	}

	for _, tt := range tests {
		t.Run(string(tt.hookType), func(t *testing.T) {
			got := executor.logFileForHook(tt.hookType)
			if got != tt.expected {
				t.Errorf("logFileForHook(%q) = %q, want %q", tt.hookType, got, tt.expected)
			}
		})
	}
}

func TestBuildEnv(t *testing.T) {
	cfg := &Config{
		Env: map[string]string{
			"CUSTOM_VAR": "custom_value",
			"OTHER_VAR":  "other_value",
		},
	}

	executor := &Executor{
		shedName: "test-shed",
		config:   cfg,
	}

	env := executor.buildEnv()

	// Check default env vars
	hasDefault := map[string]bool{
		"SHED_CONTAINER=true":       false,
		"SHED_NAME=test-shed":       false,
		"SHED_WORKSPACE=/workspace": false,
	}

	for _, e := range env {
		if _, ok := hasDefault[e]; ok {
			hasDefault[e] = true
		}
	}

	for key, found := range hasDefault {
		if !found {
			t.Errorf("Missing default env var: %s", key)
		}
	}

	// Check custom env vars
	customFound := false
	otherFound := false
	for _, e := range env {
		if e == "CUSTOM_VAR=custom_value" {
			customFound = true
		}
		if e == "OTHER_VAR=other_value" {
			otherFound = true
		}
	}

	if !customFound {
		t.Error("Missing custom env var: CUSTOM_VAR=custom_value")
	}
	if !otherFound {
		t.Error("Missing custom env var: OTHER_VAR=other_value")
	}
}
