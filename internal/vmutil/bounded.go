package vmutil

import (
	"bytes"
	"errors"
)

const (
	// maxGuestFileRead is the most the server keeps of a file it reads out of
	// a guest (the provision config, the provisioning state file). What a
	// guest sends is a claim: without a bound, a config that is a link to an
	// endless device would grow the server's memory until it died.
	maxGuestFileRead = 1 << 20
	// hookOutputTail is how much of a hook's output the server keeps to quote
	// in a failure. The whole output is in the guest's log file.
	hookOutputTail = 64 << 10
	// maxProxyReplyLine bounds the tcpproxy's one-line CONNECT reply.
	maxProxyReplyLine = 4096
)

// errGuestReadTooLarge is returned when a guest sent more than
// maxGuestFileRead for a file the server reads whole.
var errGuestReadTooLarge = errors.New("the guest sent more than the server reads")

// cappedBuffer keeps the first max bytes written to it. The first write that
// would pass max sets over and calls onOver, once; the caller passes the
// cancel of the exec's context there, so the guest's stream ends instead of
// being read and thrown away for as long as it likes. Like the buffers it
// replaces it is written by one goroutine.
type cappedBuffer struct {
	buf    bytes.Buffer
	max    int
	over   bool
	onOver func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	}
	if !c.over {
		c.over = true
		if c.onOver != nil {
			c.onOver()
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.max {
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + n - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
