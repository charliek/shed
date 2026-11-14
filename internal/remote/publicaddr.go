package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// What remote.firewall.mode: auto asks an echo service. An echo service is an
// https URL that answers a GET with the caller's public address and nothing
// else. Whoever runs one (or sits on the path to it) can lie, so one answer
// is never believed: see agreed.
const (
	// echoTimeout bounds one request to one echo service.
	echoTimeout = 5 * time.Second
	// echoMaxBody is the longest answer that is read. An IPv4 address is at
	// most 15 bytes; anything longer than this is not an address.
	echoMaxBody = 64
	// echoQuorum is how many echo services must report the same address
	// before it is believed.
	echoQuorum = 2
)

// cgnat is the shared address space of RFC 6598. An address in it is not one
// the internet can see.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicIPv4 reports whether addr could be a server's public address: an
// IPv4 unicast address that is not private, loopback, link-local,
// unspecified, multicast, broadcast or carrier-grade NAT space. The firewall
// rule is never pointed at anything else.
func publicIPv4(addr netip.Addr) bool {
	return addr.Is4() && addr.IsGlobalUnicast() && !addr.IsPrivate() && !cgnat.Contains(addr)
}

// addrFinder learns the server's public IPv4 address from echo services.
type addrFinder struct {
	urls   []string
	client *http.Client
}

// newAddrFinder builds a finder over the configured echo URLs. base, when
// set, supplies the transport (tests pass one that trusts their servers);
// otherwise the finder dials directly, over IPv4 only, with no proxy: the
// address that matters is the one a shed sees the server's SSH connection
// come from. Redirects are never followed.
func newAddrFinder(urls []string, base *http.Client) *addrFinder {
	client := &http.Client{
		Timeout: echoTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if base != nil && base.Transport != nil {
		client.Transport = base.Transport
	} else {
		dialer := &net.Dialer{Timeout: echoTimeout}
		client.Transport = &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
			TLSHandshakeTimeout:    echoTimeout,
			ResponseHeaderTimeout:  echoTimeout,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 16 << 10,
		}
	}
	return &addrFinder{urls: append([]string(nil), urls...), client: client}
}

// ask reads one echo service's answer. It is an error unless the URL is
// https, the reply is a 200 (a redirect is not followed and so is not one),
// the body is at most echoMaxBody bytes and, trimmed, is exactly one public
// IPv4 address.
func (f *addrFinder) ask(ctx context.Context, rawURL string) (netip.Addr, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return netip.Addr{}, errors.New("not an https URL")
	}
	ctx, cancel := context.WithTimeout(ctx, echoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return netip.Addr{}, errors.New("not a usable URL")
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "shed-server")
	resp, err := f.client.Do(req)
	if err != nil {
		// A *url.Error repeats the URL; the caller already names the host.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return netip.Addr{}, fmt.Errorf("no answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("answered HTTP %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, echoMaxBody+1))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("reading the answer: %w", err)
	}
	if len(body) > echoMaxBody {
		return netip.Addr{}, fmt.Errorf("answered more than %d bytes", echoMaxBody)
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil || !addr.Is4() {
		return netip.Addr{}, errors.New("the answer is not one IPv4 address")
	}
	if !publicIPv4(addr) {
		return netip.Addr{}, fmt.Errorf("answered %s, which is not a public address", addr)
	}
	return addr, nil
}

// agreed asks every echo service at once. It returns an address only when at
// least echoQuorum services reported that same address and no other address
// was reported as often. The summary says, in plain words, what each service
// did; it is for the log and for the refusal an operator reads.
func (f *addrFinder) agreed(ctx context.Context) (addr netip.Addr, ok bool, summary string) {
	type answer struct {
		addr netip.Addr
		err  error
	}
	answers := make([]answer, len(f.urls))
	var wg sync.WaitGroup
	for i, u := range f.urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := f.ask(ctx, u)
			answers[i] = answer{addr: a, err: err}
		}()
	}
	wg.Wait()

	votes := make(map[netip.Addr]int)
	notes := make([]string, 0, len(answers))
	for i, a := range answers {
		host := f.urls[i]
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
		if a.err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", host, a.err))
			continue
		}
		votes[a.addr]++
		notes = append(notes, fmt.Sprintf("%s: %s", host, a.addr))
	}
	summary = strings.Join(notes, "; ")

	type tally struct {
		addr netip.Addr
		n    int
	}
	tallies := make([]tally, 0, len(votes))
	for a, n := range votes {
		tallies = append(tallies, tally{a, n})
	}
	sort.Slice(tallies, func(i, k int) bool {
		if tallies[i].n != tallies[k].n {
			return tallies[i].n > tallies[k].n
		}
		return tallies[i].addr.Less(tallies[k].addr)
	})
	if len(tallies) == 0 || tallies[0].n < echoQuorum {
		return netip.Addr{}, false, summary
	}
	if len(tallies) > 1 && tallies[1].n == tallies[0].n {
		// Two addresses with equal support: neither is believed.
		return netip.Addr{}, false, summary
	}
	return tallies[0].addr, true, summary
}
