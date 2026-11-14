package vmutil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
)

func TestCappedBuffer(t *testing.T) {
	tests := []struct {
		name     string
		writes   []string
		want     string
		wantOver bool
	}{
		{"under", []string{"ab", "cd"}, "abcd", false},
		{"exact", []string{"abcd", "efgh"}, "abcdefgh", false},
		{"over in one write", []string{"abcdefghij"}, "abcdefgh", true},
		{"over across writes", []string{"abcdef", "ghij", "kl"}, "abcdefgh", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c := &cappedBuffer{max: 8, onOver: func() { calls++ }}
			for _, w := range tt.writes {
				if n, err := c.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if c.String() != tt.want || c.over != tt.wantOver {
				t.Errorf("got %q over=%v, want %q over=%v", c.String(), c.over, tt.want, tt.wantOver)
			}
			if want := map[bool]int{true: 1, false: 0}[tt.wantOver]; calls != want {
				t.Errorf("onOver called %d times, want %d", calls, want)
			}
		})
	}
}

func TestTailBuffer(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{"under", []string{"ab", "cd"}, "abcd"},
		{"rolls", []string{"abcdef", "ghij"}, "cdefghij"},
		{"one large write", []string{"ab", "0123456789abcdef"}, "89abcdef"},
		{"many", []string{"abc", "def", "ghi", "jkl"}, "efghijkl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := &tailBuffer{max: 8}
			for _, w := range tt.writes {
				if n, err := tb.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if tb.String() != tt.want {
				t.Errorf("got %q, want %q", tb.String(), tt.want)
			}
			if cap(tb.buf) > 4*8 {
				t.Errorf("buffer grew to %d for a tail of 8", cap(tb.buf))
			}
		})
	}
}

// TestLoadConfigBoundsGuestOutput: a guest whose provision config never ends
// (a link to an endless device) must not be read without bound. The read is
// ended and the load fails; it is not retried.
func TestLoadConfigBoundsGuestOutput(t *testing.T) {
	orig := provisionReadBackoffs
	provisionReadBackoffs = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { provisionReadBackoffs = orig }()

	var calls, sent int64
	chunk := bytes.Repeat([]byte("x"), 32*1024)
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			atomic.AddInt64(&calls, 1)
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			for {
				if err := agentproto.WriteData(conn, chunk); err != nil {
					return
				}
				atomic.AddInt64(&sent, int64(len(chunk)))
			}
		},
	}
	p := NewProvisioner(NewAgentClient(dialer, 1024, 1026), "test")

	done := make(chan error, 1)
	go func() {
		_, err := p.LoadConfig(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errGuestReadTooLarge) {
			t.Fatalf("LoadConfig() error = %v, want errGuestReadTooLarge", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("LoadConfig kept reading an endless config")
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("read attempts = %d, want 1 (an oversized config is not retried)", got)
	}
	// What passes the bound is dropped, not kept, and the stream is closed
	// as soon as the cancel lands; a few more chunks can race it.
	if got := atomic.LoadInt64(&sent); got > 64*maxGuestFileRead {
		t.Errorf("the guest got %d bytes through before the read ended; the bound is %d", got, maxGuestFileRead)
	}
}

func TestReadLineBounded(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		rest    string
		wantErr bool
	}{
		{"line", "OK\nrest", "OK\n", "rest", false},
		{"at the limit", strings.Repeat("a", 15) + "\n", strings.Repeat("a", 15) + "\n", "", false},
		{"no newline", strings.Repeat("a", 64), "", "", true},
		{"eof first", "OK", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tt.in))
			got, err := readLineBounded(r, 16)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("readLineBounded() = %q, %v; want %q, error %v", got, err, tt.want, tt.wantErr)
			}
			if err == nil {
				rest, _ := io.ReadAll(r)
				if string(rest) != tt.rest {
					t.Errorf("left %q unread, want %q", rest, tt.rest)
				}
			}
		})
	}
}

// TestDialServiceBoundsReply: a proxy that streams without a newline fails the
// dial instead of growing the reply.
func TestDialServiceBoundsReply(t *testing.T) {
	d, _ := vzDialer(strings.Repeat("A", 4*maxProxyReplyLine), false)
	conn, err := DialService(context.Background(), d, 1028, 1029)
	if err == nil {
		conn.Close()
		t.Fatal("DialService accepted a reply with no newline")
	}
	if !strings.Contains(err.Error(), "no newline") {
		t.Errorf("error = %v, want the bounded-reply error", err)
	}
}
