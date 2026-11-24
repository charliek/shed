package remote

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/vmutil"
)

// answerDialer is a guest agent that answers every exec with code.
type answerDialer struct {
	code  int
	calls atomic.Int32
}

func (d *answerDialer) Dial(context.Context, uint32) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_, _, _ = agentproto.ReadMessage(server) // exec request
		_, _, _ = agentproto.ReadMessage(server) // stdin EOF
		d.calls.Add(1)
		_ = agentproto.WriteExitCode(server, d.code)
	}()
	return client, nil
}

// TestRemoteRepoWithNoProvisionConfigIsReadOnce: a --repo shed's landing dir
// is on the VM's own disk, so a repository with no .shed/provision.yaml is
// read once, without the retries meant for a host mount that has not settled
// (they add up to about 3.75 s).
func TestRemoteRepoWithNoProvisionConfigIsReadOnce(t *testing.T) {
	d := &answerDialer{code: 66} // the landing dir lists content; no config
	rec := &state.Record{Shed: config.Shed{Name: "cloud", LandingDir: config.HomePath + "/widgets"}}
	p := provisionerFor(vmutil.NewAgentClient(d, 1024, 1026), rec)
	start := time.Now()
	cfg, err := p.LoadConfig(context.Background())
	if err != nil || cfg == nil || cfg.HasAnyHooks() {
		t.Fatalf("LoadConfig() = %+v, %v; want no hooks", cfg, err)
	}
	if n := d.calls.Load(); n != 1 {
		t.Fatalf("read attempts = %d, want 1", n)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("LoadConfig took %s, want no retry wait", took)
	}
}
