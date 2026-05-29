package vmutil

import (
	"os"
	"os/exec"
	"testing"
)

func TestIsProcessAlive(t *testing.T) {
	t.Run("invalid pid is not alive", func(t *testing.T) {
		if IsProcessAlive(0) {
			t.Errorf("IsProcessAlive(0) = true, want false")
		}
		if IsProcessAlive(-1) {
			t.Errorf("IsProcessAlive(-1) = true, want false")
		}
	})

	t.Run("own pid is alive", func(t *testing.T) {
		if !IsProcessAlive(os.Getpid()) {
			t.Errorf("IsProcessAlive(self) = false, want true")
		}
	})

	t.Run("live child is alive, reaped child is not", func(t *testing.T) {
		// Spawn a short-lived child we can observe both alive and dead.
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleep: %v", err)
		}
		pid := cmd.Process.Pid
		if !IsProcessAlive(pid) {
			t.Errorf("IsProcessAlive(live child) = false, want true")
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill child: %v", err)
		}
		if err := cmd.Wait(); err == nil {
			// Wait returns *exec.ExitError when killed; either way, child is reaped.
			t.Logf("child Wait returned nil after Kill (unusual but acceptable)")
		}
		if IsProcessAlive(pid) {
			t.Errorf("IsProcessAlive(reaped child) = true, want false")
		}
	})

	t.Run("definitely-dead pid is not alive", func(t *testing.T) {
		// PID 1 always exists on Unix, so use a clearly out-of-range pid.
		// On Linux pid_max defaults to 4194304; on macOS it's 99998.
		// 0x7fffffff is above both. ESRCH expected.
		if IsProcessAlive(0x7fffffff) {
			t.Errorf("IsProcessAlive(impossibly-large pid) = true, want false")
		}
	})
}
