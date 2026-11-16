package sshd

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// rawSSH is a minimal SSH client for tests that need what
// golang.org/x/crypto/ssh's client cannot do: change the username between
// authentication requests on one connection (its ClientConfig.User is copied
// once at dial and used for every request). It speaks one fixed suite
// (curve25519-sha256, ssh-ed25519 host key, aes128-ctr, hmac-sha2-256) built
// from the standard library and x/crypto's exported key and signature types,
// and only as much of the protocol as these tests use. The server's host key
// signature is verified.
//
// Each channel it opens gets its own client channel id, and every channel
// message is delivered by its recipient id (readFor), so a channel left open
// (a port forward) never takes another channel's traffic.
type rawSSH struct {
	t         *testing.T
	conn      net.Conn
	r         *bufio.Reader
	sessionID []byte

	// nextChan is the client channel id the next open uses.
	nextChan uint32
	// pending holds channel messages read while waiting on another channel,
	// by the client channel id they are addressed to.
	pending map[uint32][][]byte

	// Set once keys are in use (after NEWKEYS).
	encOut, encIn cipher.Stream
	macOut, macIn hash.Hash
	seqOut, seqIn uint32
}

// SSH message numbers (RFC 4250).
const (
	msgDisconnect           = 1
	msgIgnore               = 2
	msgUnimplemented        = 3
	msgDebug                = 4
	msgServiceRequest       = 5
	msgServiceAccept        = 6
	msgKexInit              = 20
	msgNewKeys              = 21
	msgKexECDHInit          = 30
	msgKexECDHReply         = 31
	msgUserAuthRequest      = 50
	msgUserAuthFailure      = 51
	msgUserAuthSuccess      = 52
	msgUserAuthBanner       = 53
	msgUserAuthPubKeyOK     = 60
	msgGlobalRequest        = 80
	msgRequestSuccess       = 81
	msgRequestFailure       = 82
	msgChannelOpen          = 90
	msgChannelOpenConfirm   = 91
	msgChannelOpenFailure   = 92
	msgChannelWindowAdjust  = 93
	msgChannelData          = 94
	msgChannelExtendedData  = 95
	msgChannelEOF           = 96
	msgChannelClose         = 97
	msgChannelRequest       = 98
	msgChannelSuccess       = 99
	msgChannelFailure       = 100
	rawClientVersion        = "SSH-2.0-shedtest-raw"
	rawAuthService          = "ssh-connection"
	rawChannelWindow        = 1 << 20
	rawChannelMaxPacket     = 1 << 15
	rawEncryptedBlockSize   = aes.BlockSize
	rawUnencryptedBlockSize = 8
)

// wire builds an SSH payload.
type wire struct{ b []byte }

func (w *wire) byte(v byte) *wire { w.b = append(w.b, v); return w }
func (w *wire) u32(v uint32) *wire {
	w.b = binary.BigEndian.AppendUint32(w.b, v)
	return w
}
func (w *wire) bool(v bool) *wire {
	if v {
		return w.byte(1)
	}
	return w.byte(0)
}
func (w *wire) str(v []byte) *wire { w.u32(uint32(len(v))); w.b = append(w.b, v...); return w }
func (w *wire) s(v string) *wire   { return w.str([]byte(v)) }

// mpint appends v (an unsigned big-endian integer) as an SSH mpint.
func (w *wire) mpint(v []byte) *wire {
	n := new(big.Int).SetBytes(v).Bytes()
	if len(n) > 0 && n[0]&0x80 != 0 {
		n = append([]byte{0}, n...)
	}
	return w.str(n)
}

// reader parses an SSH payload; a short read fails the test.
type reader struct {
	t *testing.T
	b []byte
}

func (r *reader) byte() byte {
	r.t.Helper()
	if len(r.b) < 1 {
		r.t.Fatal("raw ssh: short payload")
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *reader) u32() uint32 {
	r.t.Helper()
	if len(r.b) < 4 {
		r.t.Fatal("raw ssh: short payload")
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) str() []byte {
	r.t.Helper()
	n := int(r.u32())
	if len(r.b) < n {
		r.t.Fatal("raw ssh: short payload")
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// dialRaw connects to addr and completes the key exchange.
func dialRaw(t *testing.T, addr string) *rawSSH {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	c := &rawSSH{t: t, conn: conn, r: bufio.NewReader(conn), pending: map[uint32][][]byte{}}
	c.handshake()
	return c
}

func (c *rawSSH) handshake() {
	t := c.t
	t.Helper()
	if _, err := io.WriteString(c.conn, rawClientVersion+"\r\n"); err != nil {
		t.Fatal(err)
	}
	var serverVersion string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatalf("raw ssh: read version: %v", err)
		}
		if line = strings.TrimRight(line, "\r\n"); strings.HasPrefix(line, "SSH-") {
			serverVersion = line
			break
		}
	}

	cookie := make([]byte, 16)
	_, _ = rand.Read(cookie)
	ki := (&wire{}).byte(msgKexInit)
	ki.b = append(ki.b, cookie...)
	for _, list := range []string{
		"curve25519-sha256", gossh.KeyAlgoED25519,
		gossh.CipherAES128CTR, gossh.CipherAES128CTR,
		gossh.HMACSHA256, gossh.HMACSHA256,
		"none", "none", "", "",
	} {
		ki.s(list)
	}
	ki.bool(false).u32(0)
	clientKexInit := ki.b
	c.write(clientKexInit)
	serverKexInit := c.expect(msgKexInit)

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	qc := priv.PublicKey().Bytes()
	c.write((&wire{}).byte(msgKexECDHInit).str(qc).b)
	rp := &reader{t: t, b: c.expect(msgKexECDHReply)[1:]}
	hostKeyBlob, qs, sigBlob := rp.str(), rp.str(), rp.str()
	peer, err := ecdh.X25519().NewPublicKey(qs)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := priv.ECDH(peer)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256((&wire{}).s(rawClientVersion).s(serverVersion).
		str(clientKexInit).str(serverKexInit).str(hostKeyBlob).
		str(qc).str(qs).mpint(secret).b)
	c.sessionID = h[:]

	hostKey, err := gossh.ParsePublicKey(hostKeyBlob)
	if err != nil {
		t.Fatal(err)
	}
	rs := &reader{t: t, b: sigBlob}
	sig := &gossh.Signature{Format: string(rs.str()), Blob: rs.str()}
	if err := hostKey.Verify(c.sessionID, sig); err != nil {
		t.Fatalf("raw ssh: host key signature: %v", err)
	}

	c.write([]byte{msgNewKeys})
	c.expect(msgNewKeys)
	k := (&wire{}).mpint(secret).b
	derive := func(letter byte, n int) []byte {
		d := sha256.Sum256(append(append(append(append([]byte{}, k...), c.sessionID...), letter), c.sessionID...))
		return d[:n]
	}
	stream := func(key, iv []byte) cipher.Stream {
		b, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		return cipher.NewCTR(b, iv)
	}
	c.encOut = stream(derive('C', 16), derive('A', 16))
	c.encIn = stream(derive('D', 16), derive('B', 16))
	c.macOut = hmac.New(sha256.New, derive('E', 32))
	c.macIn = hmac.New(sha256.New, derive('F', 32))

	c.write((&wire{}).byte(msgServiceRequest).s("ssh-userauth").b)
	c.expect(msgServiceAccept)
}

// write sends one packet.
func (c *rawSSH) write(payload []byte) {
	c.t.Helper()
	block := rawUnencryptedBlockSize
	if c.encOut != nil {
		block = rawEncryptedBlockSize
	}
	pad := block - (5+len(payload))%block
	if pad < 4 {
		pad += block
	}
	pkt := binary.BigEndian.AppendUint32(nil, uint32(1+len(payload)+pad))
	pkt = append(pkt, byte(pad))
	pkt = append(pkt, payload...)
	padding := make([]byte, pad)
	_, _ = rand.Read(padding)
	pkt = append(pkt, padding...)
	var mac []byte
	if c.encOut != nil {
		c.macOut.Reset()
		_ = binary.Write(c.macOut, binary.BigEndian, c.seqOut)
		c.macOut.Write(pkt)
		mac = c.macOut.Sum(nil)
		c.encOut.XORKeyStream(pkt, pkt)
	}
	c.seqOut++
	if _, err := c.conn.Write(append(pkt, mac...)); err != nil {
		c.t.Fatalf("raw ssh: write: %v", err)
	}
}

// read returns the next packet's payload, skipping transport noise. An
// authentication banner fails the test: the server never sends one (its
// BannerCallback only keeps the connection's metadata, and Start refuses a
// configured banner: checkNoBanner).
func (c *rawSSH) read() []byte {
	c.t.Helper()
	for {
		p, err := c.readPacket()
		if err != nil {
			c.t.Fatalf("raw ssh: read: %v", err)
		}
		switch p[0] {
		case msgIgnore, msgDebug, msgUnimplemented, msgGlobalRequest, msgChannelWindowAdjust:
			continue
		case msgUserAuthBanner:
			r := &reader{t: c.t, b: p[1:]}
			c.t.Fatalf("raw ssh: the server sent an authentication banner %q", r.str())
		case msgDisconnect:
			r := &reader{t: c.t, b: p[1:]}
			r.u32()
			c.t.Fatalf("raw ssh: server disconnected: %s", r.str())
		}
		return p
	}
}

func (c *rawSSH) readPacket() ([]byte, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(c.r, head); err != nil {
		return nil, err
	}
	if c.encIn != nil {
		c.encIn.XORKeyStream(head, head)
	}
	n := binary.BigEndian.Uint32(head)
	if n < 5 || n > 256*1024 {
		return nil, fmt.Errorf("bad packet length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	if c.encIn != nil {
		c.encIn.XORKeyStream(body, body)
		mac := make([]byte, c.macIn.Size())
		if _, err := io.ReadFull(c.r, mac); err != nil {
			return nil, err
		}
		c.macIn.Reset()
		_ = binary.Write(c.macIn, binary.BigEndian, c.seqIn)
		c.macIn.Write(head)
		c.macIn.Write(body)
		if !hmac.Equal(mac, c.macIn.Sum(nil)) {
			return nil, errors.New("bad MAC")
		}
	}
	c.seqIn++
	pad := int(body[0])
	if pad >= len(body) {
		return nil, errors.New("bad padding")
	}
	return body[1 : len(body)-pad], nil
}

// expect reads the next packet and fails the test unless it is message t.
func (c *rawSSH) expect(msg byte) []byte {
	c.t.Helper()
	p := c.read()
	if p[0] != msg {
		c.t.Fatalf("raw ssh: got message %d, want %d", p[0], msg)
	}
	return p
}

// pubKeyRequest is the body of a public-key userauth request up to and
// including the key blob.
func pubKeyRequest(user string, signed bool, key gossh.PublicKey) *wire {
	return (&wire{}).byte(msgUserAuthRequest).s(user).s(rawAuthService).s("publickey").
		bool(signed).s(key.Type()).str(key.Marshal())
}

// query asks, without signing, whether key would be accepted for user, and
// reports the server's answer.
func (c *rawSSH) query(user string, key gossh.PublicKey) bool {
	c.t.Helper()
	c.write(pubKeyRequest(user, false, key).b)
	switch p := c.read(); p[0] {
	case msgUserAuthPubKeyOK:
		return true
	case msgUserAuthFailure:
		return false
	default:
		c.t.Fatalf("raw ssh: query answered with message %d", p[0])
		return false
	}
}

// login sends a signed public-key request for user and reports whether it
// authenticated the connection.
func (c *rawSSH) login(user string, signer gossh.Signer) bool {
	c.t.Helper()
	key := signer.PublicKey()
	signed := (&wire{}).str(c.sessionID)
	signed.b = append(signed.b, pubKeyRequest(user, true, key).b...)
	sig, err := signer.Sign(rand.Reader, signed.b)
	if err != nil {
		c.t.Fatal(err)
	}
	c.write(pubKeyRequest(user, true, key).str(gossh.Marshal(sig)).b)
	switch p := c.read(); p[0] {
	case msgUserAuthSuccess:
		return true
	case msgUserAuthFailure:
		return false
	default:
		c.t.Fatalf("raw ssh: login answered with message %d", p[0])
		return false
	}
}

// isChannelMessage reports whether message type m is addressed to a channel
// (its payload starts with the recipient channel id).
func isChannelMessage(m byte) bool {
	return m >= msgChannelOpenConfirm && m <= msgChannelFailure
}

// readFor returns the next channel message addressed to client channel id.
// A channel message for another channel this client opened is kept for that
// channel; one for a channel it never opened, or any message that is not a
// channel message, fails the test.
func (c *rawSSH) readFor(id uint32) []byte {
	c.t.Helper()
	if q := c.pending[id]; len(q) > 0 {
		c.pending[id] = q[1:]
		return q[0]
	}
	for {
		p := c.read()
		if !isChannelMessage(p[0]) {
			c.t.Fatalf("raw ssh: got message %d while waiting on channel %d", p[0], id)
		}
		to := (&reader{t: c.t, b: p[1:]}).u32()
		if to == id {
			return p
		}
		if to >= c.nextChan {
			c.t.Fatalf("raw ssh: message %d for channel %d, which this client never opened", p[0], to)
		}
		c.pending[to] = append(c.pending[to], p)
	}
}

// rawChannel is an open channel: the client's id for it and the server's.
type rawChannel struct{ local, remote uint32 }

// open opens a channel of type kind under a client channel id no other
// channel on this connection has used, and returns the channel, or ok false
// with the server's reason.
func (c *rawSSH) open(kind string, extra []byte) (ch rawChannel, ok bool, reason string) {
	c.t.Helper()
	ch.local = c.nextChan
	c.nextChan++
	w := (&wire{}).byte(msgChannelOpen).s(kind).u32(ch.local).u32(rawChannelWindow).u32(rawChannelMaxPacket)
	w.b = append(w.b, extra...)
	c.write(w.b)
	p := c.readFor(ch.local)
	r := &reader{t: c.t, b: p[1:]}
	r.u32() // the recipient: ch.local
	switch p[0] {
	case msgChannelOpenConfirm:
		ch.remote = r.u32()
		return ch, true, ""
	case msgChannelOpenFailure:
		r.u32()
		return ch, false, string(r.str())
	}
	c.t.Fatalf("raw ssh: channel open answered with message %d", p[0])
	return ch, false, ""
}

// run opens a session channel, asks for request ("exec" with arg as the
// command, or "subsystem" with arg as its name) and returns its stderr and
// exit status once the server closes the channel. Only messages addressed to
// this channel count.
func (c *rawSSH) run(request, arg string) (stderr string, status int) {
	c.t.Helper()
	ch, ok, reason := c.open("session", nil)
	if !ok {
		c.t.Fatalf("raw ssh: session refused: %s", reason)
	}
	c.write((&wire{}).byte(msgChannelRequest).u32(ch.remote).s(request).bool(false).s(arg).b)
	var errOut bytes.Buffer
	status = -1
	for {
		p := c.readFor(ch.local)
		r := &reader{t: c.t, b: p[1:]}
		r.u32() // the recipient: ch.local
		switch p[0] {
		case msgChannelExtendedData:
			r.u32()
			errOut.Write(r.str())
		case msgChannelRequest:
			if string(r.str()) == "exit-status" {
				r.byte()
				status = int(r.u32())
			}
		case msgChannelClose:
			c.write((&wire{}).byte(msgChannelClose).u32(ch.remote).b)
			return errOut.String(), status
		}
	}
}

// forward opens a direct-tcpip channel to localhost:port and reports whether
// the server accepted it, and its reason when it did not. An accepted forward
// stays open.
func (c *rawSSH) forward(port uint32) (bool, string) {
	c.t.Helper()
	_, ok, reason := c.forwardChannel(port)
	return ok, reason
}

// forwardChannel is forward, returning the channel too.
func (c *rawSSH) forwardChannel(port uint32) (rawChannel, bool, string) {
	c.t.Helper()
	extra := (&wire{}).s("localhost").u32(port).s("127.0.0.1").u32(40000).b
	return c.open("direct-tcpip", extra)
}

// send writes data to channel ch.
func (c *rawSSH) send(ch rawChannel, data []byte) {
	c.t.Helper()
	c.write((&wire{}).byte(msgChannelData).u32(ch.remote).str(data).b)
}

// receive returns the data of the next data message addressed to channel ch;
// any other message for ch fails the test.
func (c *rawSSH) receive(ch rawChannel) []byte {
	c.t.Helper()
	p := c.readFor(ch.local)
	if p[0] != msgChannelData {
		c.t.Fatalf("raw ssh: channel %d: got message %d, want data", ch.local, p[0])
	}
	r := &reader{t: c.t, b: p[1:]}
	r.u32()
	return r.str()
}

// closeChannel closes channel ch and waits for the server's close of it,
// dropping anything else still addressed to it.
func (c *rawSSH) closeChannel(ch rawChannel) {
	c.t.Helper()
	c.write((&wire{}).byte(msgChannelClose).u32(ch.remote).b)
	for {
		if p := c.readFor(ch.local); p[0] == msgChannelClose {
			return
		}
	}
}

// globalRequest sends a global request that wants a reply, with payload
// after its name, and reports whether the server granted it. Channel
// messages that arrive first are kept for their channels.
func (c *rawSSH) globalRequest(name string, payload []byte) bool {
	c.t.Helper()
	w := (&wire{}).byte(msgGlobalRequest).s(name).bool(true)
	w.b = append(w.b, payload...)
	c.write(w.b)
	for {
		p := c.read()
		switch {
		case p[0] == msgRequestSuccess:
			return true
		case p[0] == msgRequestFailure:
			return false
		case isChannelMessage(p[0]):
			to := (&reader{t: c.t, b: p[1:]}).u32()
			if to >= c.nextChan {
				c.t.Fatalf("raw ssh: message %d for channel %d, which this client never opened", p[0], to)
			}
			c.pending[to] = append(c.pending[to], p)
		default:
			c.t.Fatalf("raw ssh: global request %s answered with message %d", name, p[0])
		}
	}
}

// TestRawClientKeepsChannelsApart checks the raw client itself: with a port
// forward left open, its traffic stays its own while a session runs on the
// same connection. Data the forward echoes back during the session is
// delivered to the forward afterwards, not taken by the session, and closing
// the forward is not mistaken for any session's close.
func TestRawClientKeepsChannelsApart(t *testing.T) {
	listed := testSigner(t)
	be := &routeBackend{}
	_, addr := routeServer(t, be, KeyAuth{})

	c := dialRaw(t, addr)
	if !c.login("box", listed) {
		t.Fatal("a login signed as box was refused")
	}
	fwd, ok, reason := c.forwardChannel(8080)
	if !ok {
		t.Fatalf("forward refused: %s", reason)
	}
	c.send(fwd, []byte("ping"))
	if stderr, status := c.run("exec", "true"); stderr != "" || status != 0 {
		t.Errorf("exec with the forward open: stderr %q, status %d", stderr, status)
	}
	if got := string(c.receive(fwd)); got != "ping" {
		t.Errorf("the forward received %q, want its own echo %q", got, "ping")
	}
	c.closeChannel(fwd)
	if stderr, status := c.run("subsystem", "sftp"); stderr != "" || status != 0 {
		t.Errorf("sftp after the forward closed: stderr %q, status %d", stderr, status)
	}
	onlyBox(t, be.seen())
}
