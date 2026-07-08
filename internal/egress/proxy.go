package egress

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Resolver maps a hostname to its addresses. Injected for testability; the proxy
// does its OWN resolution (it never trusts a guest-supplied IP) and pins the
// dialed address for the connection lifetime (DNS-rebinding defense).
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// Dialer opens the upstream connection to the pinned address.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// AuditRecord is one egress decision. It deliberately records host/port/IP/
// verdict/rule only — NEVER full URLs or paths, which can carry tokens.
type AuditRecord struct {
	Time       time.Time `json:"ts"`
	Shed       string    `json:"shed"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	ResolvedIP string    `json:"resolved_ip,omitempty"`
	Protocol   string    `json:"protocol"`
	Verdict    string    `json:"verdict"` // "allow" | "deny"
	Reason     string    `json:"reason"`
}

// ConnHandler applies one shed's policy to its forward-proxy connections.
type ConnHandler struct {
	Shed    string
	Token   string // per-shed proxy-auth token (binds the listener port to this shed)
	Policy  *Policy
	Resolve Resolver
	Dial    Dialer
	Audit   func(AuditRecord)
	now     func() time.Time
}

func (h *ConnHandler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// decide resolves the host, applies the always-on guards to EVERY resolved
// address (deny if any is a guard CIDR — rebinding/metadata defense), pins the
// first non-guard address, and evaluates the policy. Returns the pinned dial IP
// on allow, or nil + a deny record. Pure w.r.t. injected Resolve, so testable.
func (h *ConnHandler) decide(ctx context.Context, host string, port int, proto string) (net.IP, AuditRecord) {
	rec := AuditRecord{Time: h.clock(), Shed: h.Shed, Host: host, Port: port, Protocol: proto, Verdict: "deny"}
	canon, err := CanonicalizeHost(host)
	if err != nil {
		rec.Reason = "bad-host:" + err.Error()
		return nil, rec
	}
	rec.Host = canon
	ips, err := h.Resolve(ctx, canon)
	if err != nil || len(ips) == 0 {
		rec.Reason = "resolve-failed"
		return nil, rec
	}
	var pinned net.IP
	for _, ip := range ips {
		if cidr, denied := h.Policy.MatchGuard(ip); denied {
			rec.Reason = "guard:" + cidr
			rec.ResolvedIP = ip.String()
			return nil, rec
		}
		if pinned == nil {
			pinned = ip
		}
	}
	rec.ResolvedIP = pinned.String()
	v, reason := h.Policy.Evaluate(ConnContext{Host: canon, Port: port, ResolvedIP: pinned.String(), Protocol: proto, Shed: h.Shed})
	rec.Verdict = v.String()
	rec.Reason = reason
	if v == VerdictAllow {
		return pinned, rec
	}
	return nil, rec
}

// proxyTarget is the parsed first request of a forward-proxy connection.
type proxyTarget struct {
	proto string // "https" (CONNECT) | "http" (absolute-form)
	host  string
	port  int
	req   *http.Request // retained for plain-HTTP forwarding
}

// parseProxyRequest reads the first request. CONNECT host:port → https tunnel;
// an absolute-form request (GET http://host/…) → plain http (deny-by-default in
// policy). Testable from a bufio.Reader over a request string.
func parseProxyRequest(br *bufio.Reader) (proxyTarget, error) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return proxyTarget{}, err
	}
	if req.Method == http.MethodConnect {
		host, port := splitHostPort(req.Host, 443)
		return proxyTarget{proto: "https", host: host, port: port, req: req}, nil
	}
	if req.URL == nil || req.URL.Host == "" {
		return proxyTarget{}, fmt.Errorf("non-absolute-form request to proxy")
	}
	// The absolute-URI host is authoritative (http.ReadRequest promotes it to
	// req.Host per RFC 7230 §5.4), and it is what we both filter on and dial —
	// so there is no Host-header/URI split to exploit. Origin-form requests are
	// rejected above (proxies require absolute-form for HTTP).
	port := 80
	if p := req.URL.Port(); p != "" {
		if n, e := strconv.Atoi(p); e == nil {
			port = n
		}
	}
	return proxyTarget{proto: "http", host: req.URL.Hostname(), port: port, req: req}, nil
}

func splitHostPort(hostport string, defPort int) (string, int) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, defPort
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, defPort
	}
	return h, n
}

// Handle services one accepted forward-proxy client connection end-to-end:
// token check → parse → decide → splice (allow) / refuse (deny) → audit.
func (h *ConnHandler) Handle(client net.Conn) {
	defer client.Close()
	br := bufio.NewReader(client)
	target, err := parseProxyRequest(br)
	if err != nil {
		return
	}
	// Per-shed token gate (binds this listener port to this shed).
	if h.Token != "" && !proxyAuthOK(target.req, h.Token) {
		writeStatus(client, target.proto, http.StatusProxyAuthRequired, "egress: proxy authentication required")
		h.emit(AuditRecord{Time: h.clock(), Shed: h.Shed, Host: target.host, Port: target.port, Protocol: target.proto, Verdict: "deny", Reason: "bad-token"})
		return
	}

	ctx := context.Background()
	pinned, rec := h.decide(ctx, target.host, target.port, target.proto)
	h.emit(rec)
	if pinned == nil {
		writeStatus(client, target.proto, http.StatusForbidden, "egress: denied by policy ("+rec.Reason+")")
		return
	}

	upstream, err := h.dial(ctx, net.JoinHostPort(pinned.String(), strconv.Itoa(target.port)))
	if err != nil {
		writeStatus(client, target.proto, http.StatusBadGateway, "egress: upstream dial failed")
		return
	}
	defer upstream.Close()

	if target.proto == "https" {
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
	} else {
		// Strip the proxy-only headers before forwarding so the per-shed proxy
		// token (and proxy connection-management) never leak to the upstream
		// HTTP server. (CONNECT/https is unaffected — it splices raw bytes.)
		target.req.Header.Del("Proxy-Authorization")
		target.req.Header.Del("Proxy-Connection")
		if err := target.req.Write(upstream); err != nil {
			return
		}
	}
	splice(client, br, upstream)
}

func (h *ConnHandler) dial(ctx context.Context, addr string) (net.Conn, error) {
	if h.Dial != nil {
		return h.Dial(ctx, "tcp", addr)
	}
	var d net.Dialer
	d.Timeout = 10 * time.Second
	return d.DialContext(ctx, "tcp", addr)
}

func (h *ConnHandler) emit(rec AuditRecord) {
	if h.Audit != nil {
		h.Audit(rec)
	}
}

// splice copies bytes both directions until either side closes. The client read
// side comes from clientRd (a bufio.Reader over client) so any bytes already
// buffered past the proxied request — a POST body, a pipelined request, or a TLS
// ClientHello from a client that didn't wait for the CONNECT 200 — are not
// stranded.
func splice(client net.Conn, clientRd io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientRd)
		if c, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if c, ok := client.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// proxyAuthOK validates the per-shed token in the Proxy-Authorization header.
// The injected proxy URL is http://<token>@<gateway>:<port>, so cooperating
// clients (curl/git/dockerd) send Basic auth with the token as the username
// (empty password) — i.e. "Basic base64(<token>:)". We decode and compare the
// username in constant time. A raw bearer-style value is also accepted.
func proxyAuthOK(req *http.Request, token string) bool {
	if req == nil {
		return false
	}
	h := strings.TrimSpace(req.Header.Get("Proxy-Authorization"))
	if h == "" {
		return false
	}
	if rest, ok := strings.CutPrefix(h, "Basic "); ok {
		if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest)); err == nil {
			user, _, _ := strings.Cut(string(dec), ":")
			if ctEqual(user, token) {
				return true
			}
		}
	}
	return ctEqual(h, token)
}

// ctEqual is a constant-time string compare (avoids a token-timing oracle).
func ctEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func writeStatus(w io.Writer, proto string, code int, msg string) {
	if proto == "https" {
		fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n\r\n", code, http.StatusText(code))
		return
	}
	body := msg + "\n"
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}
