package vmutil

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/guest"
)

// Run, the exec path delivery writes through, against agents that misbehave
// on purpose: its errors are fixed, its input and output are fenced when it
// returns, and its output is bounded. Run these under -race.

const (
	runConsolePort = 1024
	runNotifyPort  = 1026
)

// portDialer answers each port with its own handler, over a pipe.
type portDialer struct {
	console func(conn net.Conn)
	notify  func(conn net.Conn)
}

func (d *portDialer) Dial(_ context.Context, port uint32) (net.Conn, error) {
	h := d.console
	if port == runNotifyPort {
		h = d.notify
	}
	if h == nil {
		return nil, errors.New("no such port")
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		h(server)
	}()
	return client, nil
}

// readExec reads the exec request.
func readExec(conn net.Conn) bool {
	t, _, err := agentproto.ReadMessage(conn)
	return err == nil && t == agentproto.MsgTypeExecRequest
}

// drainStdin reads stdin frames until the EOF frame, in the background.
func drainStdin(conn net.Conn) <-chan []byte {
	got := make(chan []byte, 1)
	go func() {
		var b bytes.Buffer
		for {
			t, data, err := agentproto.ReadMessage(conn)
			if err != nil || t == agentproto.MsgTypeStdinEOF {
				got <- b.Bytes()
				return
			}
			b.Write(data)
		}
	}()
	return got
}

// probeReply answers the probe with protocol, or (when reply is set) with
// that raw envelope.
func probeReply(protocol int, reply func(id string) []byte) func(conn net.Conn) {
	return func(conn net.Conn) {
		_, data, err := agentproto.ReadMessage(conn)
		if err != nil {
			return
		}
		var req plugin.Envelope
		_ = json.Unmarshal(data, &req)
		if reply != nil {
			_ = agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, reply(req.ID))
			return
		}
		info, _ := json.Marshal(guest.AgentInfo{Version: "v0", Protocol: protocol, Layout: 1})
		out, _ := json.Marshal(plugin.NewResponse(req.ID, plugin.NamespaceHealth, info))
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, out)
	}
}

// TestRunErrorsAreFixed: an exit frame whose code is a number too large
// for an int makes Exec's decoder quote it; Run returns its fixed error,
// never that text.
func TestRunErrorsAreFixed(t *testing.T) {
	const big = "73577357735773577357735773577357"
	c := NewAgentClient(&portDialer{console: func(conn net.Conn) {
		if !readExec(conn) {
			return
		}
		drainStdin(conn)
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypeExitCode, []byte(`{"code":`+big+`}`))
	}}, runConsolePort, runNotifyPort)
	// Exec itself quotes the number (what Run must not pass on).
	err := c.Exec(context.Background(), execOpts())
	if err == nil || !strings.Contains(err.Error(), big) {
		t.Fatalf("test set-up: Exec = %v, want it to quote the number", err)
	}
	code, err := c.Run(context.Background(), []string{"x"}, strings.NewReader("in"), io.Discard, 1024)
	if code != -1 || !errors.Is(err, ErrRunFailed) || strings.Contains(err.Error(), big) {
		t.Fatalf("Run = %d, %v; want -1 and the fixed error", code, err)
	}
}

// TestRunFencesItsInput: the agent exits at once, before it has read its
// stdin, which the exec is still feeding when Run returns (the agent keeps
// taking it, so the pump keeps reading); the caller then clears its input.
// Under -race, a stdin pump still reading it would be a race. Also an agent
// that reads none of it.
func TestRunFencesItsInput(t *testing.T) {
	c := NewAgentClient(&portDialer{console: func(conn net.Conn) {
		if !readExec(conn) {
			return
		}
		got := drainStdin(conn)
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypeExitCode, []byte(`{"code":0}`))
		<-got
	}}, runConsolePort, runNotifyPort)
	silent := NewAgentClient(&portDialer{console: func(conn net.Conn) {
		if !readExec(conn) {
			return
		}
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypeExitCode, []byte(`{"code":0}`))
	}}, runConsolePort, runNotifyPort)
	for range 20 {
		input := bytes.Repeat([]byte("s"), 1<<20)
		code, err := silent.Run(context.Background(), []string{"x"}, bytes.NewReader(input), io.Discard, 1024)
		clear(input)
		if code != 0 || err != nil {
			t.Fatalf("Run = %d, %v", code, err)
		}
	}
	for range 20 {
		input := bytes.Repeat([]byte("s"), 1<<20)
		code, err := c.Run(context.Background(), []string{"x"}, &slowReader{b: input}, io.Discard, 1024)
		for i := range input { // a loop, which the race detector sees
			input[i] = 0
		}
		if code != 0 || err != nil {
			t.Fatalf("Run = %d, %v", code, err)
		}
	}
}

// slowReader reads b a little at a time, slowly, so a read is often in
// progress when the exec ends.
type slowReader struct {
	b   []byte
	off int
}

func (r *slowReader) Read(p []byte) (int, error) {
	time.Sleep(200 * time.Microsecond)
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

// plainBuffer is a plain buffer: Run's fence is what keeps it race-free.
type plainBuffer struct{ b []byte }

func (p *plainBuffer) Write(d []byte) (int, error) { p.b = append(p.b, d...); return len(d), nil }

// TestRunFencesItsOutputOnCancel: the agent writes stdout without end;
// ctx ends; once Run returns, its output is read with no write running.
func TestRunFencesItsOutputOnCancel(t *testing.T) {
	c := NewAgentClient(&portDialer{console: func(conn net.Conn) {
		if !readExec(conn) {
			return
		}
		drainStdin(conn)
		for {
			if agentproto.WriteMessage(conn, agentproto.MsgTypeData, []byte("x")) != nil {
				return
			}
		}
	}}, runConsolePort, runNotifyPort)
	for range 10 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		out := &plainBuffer{}
		code, err := c.Run(ctx, []string{"x"}, strings.NewReader("in"), out, 0)
		cancel()
		_ = len(out.b)
		out.b = nil
		if code != -1 || !errors.Is(err, ErrRunFailed) {
			t.Fatalf("Run = %d, %v", code, err)
		}
	}
}

// TestRunStopsPastItsOutputLimit: output past the limit, in many small
// frames or in stderr, stops the exec at once (the agent's next write
// fails), and a frame larger than the limit is refused from its header.
func TestRunStopsPastItsOutputLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		msgType byte
		want    error
	}{
		{"stdout", agentproto.MsgTypeData, ErrRunOutputTooLarge},
		{"stderr", agentproto.MsgTypeStderr, ErrRunOutputTooLarge},
		// Frames of a type a bounded exec does not take end it at once:
		// none of them may be read past the output budget.
		{"unexpected frames", 0x20, ErrRunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopped := make(chan int, 1)
			c := NewAgentClient(&portDialer{console: func(conn net.Conn) {
				if !readExec(conn) {
					return
				}
				drainStdin(conn)
				for n := 0; ; n++ {
					if agentproto.WriteMessage(conn, tc.msgType, bytes.Repeat([]byte("x"), 1000)) != nil {
						stopped <- n
						return
					}
				}
			}}, runConsolePort, runNotifyPort)
			code, err := c.Run(context.Background(), []string{"x"}, nil, io.Discard, 64<<10)
			if code != -1 || !errors.Is(err, tc.want) {
				t.Fatalf("Run = %d, %v", code, err)
			}
			select {
			case n := <-stopped:
				if n > 70 {
					t.Fatalf("the agent wrote %d frames before it was stopped", n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the agent was not stopped")
			}
		})
	}
	t.Run("one frame over the limit", func(t *testing.T) {
		c := NewAgentClient(&portDialer{console: func(conn net.Conn) {
			if !readExec(conn) {
				return
			}
			drainStdin(conn)
			header := make([]byte, 5)
			header[0] = agentproto.MsgTypeData
			binary.BigEndian.PutUint32(header[1:], 10<<20)
			_, _ = conn.Write(header)
			time.Sleep(time.Second) // the payload never comes; the header is enough
		}}, runConsolePort, runNotifyPort)
		start := time.Now()
		code, err := c.Run(context.Background(), []string{"x"}, nil, io.Discard, 64<<10)
		if code != -1 || err == nil || time.Since(start) > 900*time.Millisecond {
			t.Fatalf("Run = %d, %v after %s; want refused from the header", code, err, time.Since(start))
		}
	})
}

// TestDeliveryWriteThroughAHostileAgent writes a stage through a real
// AgentClient whose agent echoes delivered values back in every channel it
// has: its probe reply, its standard output and error, and an exit frame
// whose code is a value as an overflowing number. None reaches the log or
// the stage's result.
func TestDeliveryWriteThroughAHostileAgent(t *testing.T) {
	const sentinel = "7357735773577357735773577357"
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	svc := delivery.NewService(noPin{}, time.Minute)
	bundle := bundleWithValue(t, sentinel)
	echoing := func(conn net.Conn) {
		if !readExec(conn) {
			return
		}
		in := <-drainStdin(conn)
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypeData, append([]byte(`{"name":"`+sentinel+`","result":"ok"}`+"\n"), in...))
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypeStderr, in)
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypeExitCode, []byte(`{"code":`+sentinel+`}`))
	}
	// A value that is a valid int, sent as the exit code: it decodes, so
	// only the log's fixed wording keeps it out.
	const intSentinel = "735773577"
	intExit := func(conn net.Conn) {
		if !readExec(conn) {
			return
		}
		<-drainStdin(conn)
		_ = agentproto.WriteMessage(conn, agentproto.MsgTypeExitCode, []byte(`{"code":`+intSentinel+`}`))
	}
	var texts []string
	for _, d := range []*portDialer{
		{notify: probeReply(guest.ApplyItemsProtocol, nil), console: echoing},
		{notify: probeReply(guest.ApplyItemsProtocol, nil), console: intExit},
		{notify: probeReply(0, func(id string) []byte {
			return []byte(`{"namespace":"` + sentinel + `","type":"response","in_reply_to":"` + id + `"}`)
		})},
		{notify: probeReply(0, func(id string) []byte { return []byte(sentinel) })},
	} {
		r := svc.Write(context.Background(), NewAgentClient(d, runConsolePort, runNotifyPort), 1, bundle, "")
		if r.State != delivery.StepFailed {
			t.Fatalf("a hostile agent's stage = %+v, want failed", r)
		}
		texts = append(texts, fmt.Sprintf("%+v %#v", r, r))
		if r.Failure != nil {
			texts = append(texts, fmt.Sprintf("%+v", *r.Failure))
		}
	}
	texts = append(texts, logged.String())
	for _, s := range texts {
		if strings.Contains(s, sentinel) || strings.Contains(s, intSentinel) {
			t.Fatalf("a delivered value leaked:\n%s", s)
		}
	}
	if !strings.Contains(logged.String(), "probe failed") || !strings.Contains(logged.String(), "exec failed") {
		t.Fatalf("test set-up: the log does not show the failures:\n%s", logged.String())
	}
}

// noPin binds nobody: Write needs no pin.
type noPin struct{}

func (noPin) DeliveryBound(string) (delivery.SourceMachine, bool) {
	return delivery.SourceMachine{}, false
}
func (noPin) DeliveryStream(string) (delivery.Stream, bool) { return nil, false }
func (noPin) DeliveryCurrent(string, string, uint64) bool   { return false }

// bundleWithValue is the bundle of an accepted answer: one env item
// holding value.
func bundleWithValue(t *testing.T, value string) *delivery.Bundle {
	t.Helper()
	const id = "0123456789abcdef0123456789abcdef"
	a, refusal := delivery.ParseAnswer([]byte(`{"version":1,"id":"`+id+`","items":[`+
		`{"name":"tok","provider":"p","kind":"env","variable":"TOKEN","value":"`+value+`","fingerprint":"f"}],"problems":[]}`),
		&delivery.Request{ID: id})
	if refusal != nil {
		t.Fatal(refusal)
	}
	return delivery.NewBundle(a)
}

func execOpts() backend.ExecOptions {
	return backend.ExecOptions{Cmd: []string{"x"}, Stdin: io.NopCloser(strings.NewReader("in"))}
}
