//go:build linux

package main

import (
	"bytes"
	"log"
	"net"
	"os"
	"strings"
	"testing"
)

// TestHandleExecLogsRedactedArgvAndNoEnv: the command runs exactly as sent
// (credentials and env reach it), while the agent's log line has URL
// credentials redacted and never names env.
func TestHandleExecLogsRedactedArgvAndNoEnv(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c, s := net.Pipe()
	defer c.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleExecConnection(s, nil)
	}()

	req := ExecRequest{
		Cmd: []string{"sh", "-c", `printf '%s %s' "$1" "$SHED_TEST_TOKEN"`, "sh", "https://u:sentinel-pass@h/r.git"},
		Env: []string{"SHED_TEST_TOKEN=sentinel-env"},
	}
	send(t, c, MsgTypeExecRequest, mustJSON(t, req))
	send(t, c, MsgTypeStdinEOF, nil)
	out, _ := readOutputFrames(t, c)
	_ = c.Close()
	<-done

	if got, want := string(out), "https://u:sentinel-pass@h/r.git sentinel-env"; got != want {
		t.Errorf("command saw %q, want %q", got, want)
	}
	line := logged.String()
	if strings.Contains(line, "sentinel") || strings.Contains(line, "SHED_TEST_TOKEN=") {
		t.Errorf("agent log carries a credential or env: %q", line)
	}
	if !strings.Contains(line, "https://REDACTED@h/r.git") {
		t.Errorf("agent log = %q, want the argv with the URL redacted", line)
	}
}
