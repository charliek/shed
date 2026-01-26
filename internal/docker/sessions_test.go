package docker

import (
	"testing"
	"time"
)

func TestParseTmuxSessions(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		shedName  string
		wantCount int
		wantFirst string
	}{
		{
			name:      "single session",
			output:    "default:1706200000:1:1\n",
			shedName:  "myproj",
			wantCount: 1,
			wantFirst: "default",
		},
		{
			name:      "multiple sessions",
			output:    "default:1706200000:1:1\ndebug:1706200100:0:2\nexperiment:1706200200:0:1\n",
			shedName:  "myproj",
			wantCount: 3,
			wantFirst: "default",
		},
		{
			name:      "empty output",
			output:    "",
			shedName:  "myproj",
			wantCount: 0,
			wantFirst: "",
		},
		{
			name:      "whitespace only",
			output:    "  \n  \n",
			shedName:  "myproj",
			wantCount: 0,
			wantFirst: "",
		},
		{
			name:      "malformed line skipped",
			output:    "default:1706200000:1:1\nbadline\ngood:1706200100:0:2\n",
			shedName:  "myproj",
			wantCount: 2,
			wantFirst: "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := parseTmuxSessions(tt.output, tt.shedName)

			if len(sessions) != tt.wantCount {
				t.Errorf("parseTmuxSessions() returned %d sessions, want %d", len(sessions), tt.wantCount)
			}

			if tt.wantCount > 0 && sessions[0].Name != tt.wantFirst {
				t.Errorf("first session name = %q, want %q", sessions[0].Name, tt.wantFirst)
			}

			// Verify all sessions have the correct shed name
			for _, s := range sessions {
				if s.ShedName != tt.shedName {
					t.Errorf("session ShedName = %q, want %q", s.ShedName, tt.shedName)
				}
			}
		})
	}
}

func TestParseTmuxSessionsFields(t *testing.T) {
	// Test that all fields are correctly parsed
	output := "mysession:1706200000:1:3\n"
	sessions := parseTmuxSessions(output, "testproj")

	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	s := sessions[0]

	if s.Name != "mysession" {
		t.Errorf("Name = %q, want %q", s.Name, "mysession")
	}

	if s.ShedName != "testproj" {
		t.Errorf("ShedName = %q, want %q", s.ShedName, "testproj")
	}

	expectedTime := time.Unix(1706200000, 0)
	if !s.CreatedAt.Equal(expectedTime) {
		t.Errorf("CreatedAt = %v, want %v", s.CreatedAt, expectedTime)
	}

	if !s.Attached {
		t.Error("Attached = false, want true")
	}

	if s.WindowCount != 3 {
		t.Errorf("WindowCount = %d, want 3", s.WindowCount)
	}
}

func TestParseTmuxSessionsAttachedStatus(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		attached bool
	}{
		{"attached", "sess:1000:1:1\n", true},
		{"detached", "sess:1000:0:1\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := parseTmuxSessions(tt.output, "proj")
			if len(sessions) != 1 {
				t.Fatalf("expected 1 session")
			}
			if sessions[0].Attached != tt.attached {
				t.Errorf("Attached = %v, want %v", sessions[0].Attached, tt.attached)
			}
		})
	}
}
