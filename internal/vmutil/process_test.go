package vmutil

import (
	"os"
	"os/exec"
	"testing"
)

func TestIsProcessAlive(t *testing.T) {
	// startSleepChild spawns a sleep process the caller owns for the
	// duration of the test. Returns the pid + a cleanup that kills and
	// reaps the child so subsequent cases don't see a stray sleep.
	startSleepChild := func(t *testing.T) (*exec.Cmd, int) {
		t.Helper()
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleep: %v", err)
		}
		return cmd, cmd.Process.Pid
	}

	cases := []struct {
		name      string
		setup     func(t *testing.T) int // returns pid to check
		wantAlive bool
	}{
		{
			name:      "pid zero is not alive",
			setup:     func(*testing.T) int { return 0 },
			wantAlive: false,
		},
		{
			name:      "negative pid is not alive",
			setup:     func(*testing.T) int { return -1 },
			wantAlive: false,
		},
		{
			name:      "own pid is alive",
			setup:     func(*testing.T) int { return os.Getpid() },
			wantAlive: true,
		},
		{
			name: "live child is alive",
			setup: func(t *testing.T) int {
				cmd, pid := startSleepChild(t)
				t.Cleanup(func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				})
				return pid
			},
			wantAlive: true,
		},
		{
			name: "reaped child is not alive",
			setup: func(t *testing.T) int {
				cmd, pid := startSleepChild(t)
				if err := cmd.Process.Kill(); err != nil {
					t.Fatalf("kill child: %v", err)
				}
				// Wait returns *exec.ExitError when killed; either way,
				// the child is reaped after this returns.
				_ = cmd.Wait()
				return pid
			},
			wantAlive: false,
		},
		{
			name: "impossibly-large pid is not alive",
			// Above Linux pid_max (default 4194304) and macOS pid_max
			// (99998); syscall.Kill returns ESRCH.
			setup:     func(*testing.T) int { return 0x7fffffff },
			wantAlive: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pid := tc.setup(t)
			got := IsProcessAlive(pid)
			if got != tc.wantAlive {
				t.Errorf("IsProcessAlive(%d) = %v, want %v", pid, got, tc.wantAlive)
			}
		})
	}
}
