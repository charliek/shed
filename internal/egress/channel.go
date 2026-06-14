package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ControlMsg is one newline-delimited JSON envelope on the shed-server↔proxy
// UDS. shed-server sends configure/remove and waits for a matching ack (so a
// failed listener open aborts the shed's create/start); the proxy streams audit
// asynchronously. ID correlates a request with its ack.
type ControlMsg struct {
	Type     string        `json:"type"` // "configure" | "remove" | "ack" | "audit"
	ID       string        `json:"id,omitempty"`
	Shed     string        `json:"shed,omitempty"`
	Port     int           `json:"port,omitempty"`
	Token    string        `json:"token,omitempty"`
	Gateway  string        `json:"gateway,omitempty"` // VM-facing bind IP (guests reach the proxy here)
	Profiles []ProfileSpec `json:"profiles,omitempty"`
	Error    string        `json:"error,omitempty"` // set on a failed ack
	Audit    *AuditRecord  `json:"audit,omitempty"`
}

// ProxyServer is the data plane: it manages one forward-proxy listener per shed,
// driven by configure/remove from shed-server, and streams audit back. Listeners
// persist across control-connection drops (a server restart re-pushes; guests
// are not disrupted).
type ProxyServer struct {
	// Resolve/Dial are injectable (tests use fakes); nil ⇒ real network.
	Resolve Resolver
	Dial    Dialer

	mu        sync.Mutex
	listeners map[string]net.Listener
	// sink is the current control connection's outbound audit buffer. It is
	// never closed (the writer goroutine exits via a quit signal instead), so
	// the data path can always send into it without a send-on-closed panic. nil
	// when no control connection is attached.
	sink chan AuditRecord
}

func NewProxyServer() *ProxyServer {
	return &ProxyServer{listeners: map[string]net.Listener{}}
}

func defaultResolver(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

func bindHost(gateway string) string {
	if gateway != "" {
		return gateway
	}
	return "127.0.0.1"
}

// Configure opens (or replaces) a shed's listener with a freshly compiled policy.
// Fails closed: a bad profile or a port already in use returns an error, which
// the control loop relays as a failed ack so shed-server aborts the create/start.
func (s *ProxyServer) Configure(msg ControlMsg) error {
	pol, err := CompilePolicy(msg.Profiles, msg.Gateway)
	if err != nil {
		return fmt.Errorf("egress configure %s: %w", msg.Shed, err)
	}
	resolve := s.Resolve
	if resolve == nil {
		resolve = defaultResolver
	}
	h := &ConnHandler{Shed: msg.Shed, Token: msg.Token, Policy: pol, Resolve: resolve, Dial: s.Dial, Audit: s.audit}

	// Close any existing listener for this shed BEFORE binding, so a live
	// re-Configure (e.g. `egress set` changing profiles) that reuses the same
	// persisted port does not fail with "address already in use". Configure
	// calls are serialized by the control decode loop, so there is no
	// concurrent Configure for the same shed racing this close→listen.
	s.mu.Lock()
	if old := s.listeners[msg.Shed]; old != nil {
		old.Close()
		delete(s.listeners, msg.Shed)
	}
	s.mu.Unlock()

	bind := net.JoinHostPort(bindHost(msg.Gateway), strconv.Itoa(msg.Port))
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return fmt.Errorf("egress configure %s: listen %s: %w", msg.Shed, bind, err)
	}
	s.mu.Lock()
	s.listeners[msg.Shed] = ln
	s.mu.Unlock()
	go s.acceptLoop(ln, h)
	return nil
}

// Remove closes and forgets a shed's listener. Idempotent.
func (s *ProxyServer) Remove(shed string) {
	s.mu.Lock()
	if ln := s.listeners[shed]; ln != nil {
		ln.Close()
		delete(s.listeners, shed)
	}
	s.mu.Unlock()
}

func (s *ProxyServer) acceptLoop(ln net.Listener, h *ConnHandler) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed via Remove/replace
		}
		go h.Handle(c)
	}
}

// audit hands one record to the current control connection's sink (non-blocking;
// dropped if the buffer is full or no control connection is attached).
func (s *ProxyServer) audit(rec AuditRecord) {
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()
	if sink == nil {
		return
	}
	select {
	case sink <- rec:
	default: // drop, never block the data path
	}
}

// Serve reads control messages from one shed-server connection, dispatches them,
// acks each configure/remove, and streams audit back over the same connection.
// One writer goroutine owns the encoder; audit is dropped under backpressure
// while acks are not (a waiting caller must hear the result).
func (s *ProxyServer) Serve(conn net.Conn) error {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	sink := make(chan AuditRecord, 1024)
	replyCh := make(chan ControlMsg, 16)
	quit := make(chan struct{})

	s.mu.Lock()
	s.sink = sink
	s.mu.Unlock()

	// Detach this sink before the writer stops. Only clear if still ours so an
	// overlapping reconnect's sink is not clobbered. The sink channel is never
	// closed, so a late audit send simply drops on the full/blocked select.
	defer func() {
		s.mu.Lock()
		if s.sink == sink {
			s.sink = nil
		}
		s.mu.Unlock()
		close(quit)
	}()

	go func() {
		for {
			select {
			case rec := <-sink:
				r := rec
				_ = enc.Encode(ControlMsg{Type: "audit", Audit: &r})
			case m := <-replyCh:
				_ = enc.Encode(m)
			case <-quit:
				return
			}
		}
	}()

	dec := json.NewDecoder(bufio.NewReader(conn))
	for {
		var msg ControlMsg
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		ack := ControlMsg{Type: "ack", ID: msg.ID, Shed: msg.Shed}
		switch msg.Type {
		case "configure":
			if err := s.Configure(msg); err != nil {
				ack.Error = err.Error()
			}
		case "remove":
			s.Remove(msg.Shed)
		default:
			continue // unknown message: no ack
		}
		select {
		case replyCh <- ack:
		case <-quit:
			return nil
		}
	}
}

// ListenControl creates the proxy's control UDS (0600), replacing any stale one.
func ListenControl(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return ln, nil
}

// Client is shed-server's side of the control/audit channel. Configure/Remove
// block until the proxy acks (or the channel drops), so a failed listener open
// surfaces as an error the orchestrator hook can unwind on.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	mu   sync.Mutex // serializes writes to enc

	seq     uint64
	wmu     sync.Mutex
	waiters map[string]chan error
}

// DialControl connects to the proxy's control UDS and starts reading replies and
// audit records, delivering each audit to onAudit.
func DialControl(path string, onAudit func(AuditRecord)) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, enc: json.NewEncoder(conn), waiters: map[string]chan error{}}
	go c.readLoop(onAudit)
	return c, nil
}

func (c *Client) readLoop(onAudit func(AuditRecord)) {
	dec := json.NewDecoder(bufio.NewReader(c.conn))
	for {
		var msg ControlMsg
		if err := dec.Decode(&msg); err != nil {
			c.failWaiters(err)
			return
		}
		switch msg.Type {
		case "audit":
			if msg.Audit != nil && onAudit != nil {
				onAudit(*msg.Audit)
			}
		case "ack":
			c.wmu.Lock()
			ch := c.waiters[msg.ID]
			delete(c.waiters, msg.ID)
			c.wmu.Unlock()
			if ch != nil {
				var e error
				if msg.Error != "" {
					e = errors.New(msg.Error)
				}
				ch <- e
			}
		}
	}
}

// failWaiters resolves every pending request with an error so no caller hangs
// after the control channel drops.
func (c *Client) failWaiters(cause error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for id, ch := range c.waiters {
		ch <- fmt.Errorf("egress proxy control channel closed: %w", cause)
		delete(c.waiters, id)
	}
}

// Configure pushes a shed's listener config to the proxy and waits for the ack.
func (c *Client) Configure(shed string, port int, token, gateway string, profiles []ProfileSpec) error {
	return c.call(ControlMsg{Type: "configure", Shed: shed, Port: port, Token: token, Gateway: gateway, Profiles: profiles})
}

// Remove closes a shed's listener on the proxy and waits for the ack.
func (c *Client) Remove(shed string) error {
	return c.call(ControlMsg{Type: "remove", Shed: shed})
}

func (c *Client) call(m ControlMsg) error {
	id := strconv.FormatUint(atomic.AddUint64(&c.seq, 1), 10)
	m.ID = id
	ch := make(chan error, 1)
	c.wmu.Lock()
	c.waiters[id] = ch
	c.wmu.Unlock()

	c.mu.Lock()
	err := c.enc.Encode(m)
	c.mu.Unlock()
	if err != nil {
		c.wmu.Lock()
		delete(c.waiters, id)
		c.wmu.Unlock()
		return err
	}
	select {
	case e := <-ch:
		return e
	case <-time.After(10 * time.Second):
		c.wmu.Lock()
		delete(c.waiters, id)
		c.wmu.Unlock()
		return fmt.Errorf("egress proxy: timeout waiting for ack of %s %s", m.Type, m.Shed)
	}
}

func (c *Client) Close() error { return c.conn.Close() }
