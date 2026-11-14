package users

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

// A GitHub login is followed by its numeric account id, not by its name. A
// login can be renamed, deleted and registered again by someone else; the id
// cannot. The server pins each login to the id it resolved at its first
// successful fetch (github_pins.json in the state dir) and accepts a login's
// keys only from a fetch that shows the login still names that id.
//
// One fetch is at least two REST API requests: resolve the login to its
// account id (GET /users/<login>), compare it with the pin, then list the
// keys of that id (GET /user/<id>/keys, the largest page the API allows,
// following the Link header's next page). The key list is addressed by the
// id, so the keys are that account's whatever the login names by the time
// they are read. API requests never follow a redirect, a next-page link is
// followed only on the same host and the same id-addressed path, and a key
// list is parsed strictly: a page that is cut short, over the size limit, not
// the expected shape, an entry with a duplicated member or a key string that
// is corrupt, a failed page, or more pages than githubKeysMaxPages, is an
// outage, never a shorter key set. Nothing of a fetch is used until every
// page has been read. (Checked by hand, unauthenticated, against a
// well-known public account that has a key: /user/<id>/keys answered 200
// with the same {id, key} entries as /users/<login>/keys, and one key a line
// it renders byte-for-byte as github.com/<login>.keys.)
//
// github.com/<login>.keys (another host, outside the API's rate limit) is
// read only to notice, cheaply, that a login's keys may have changed when an
// unknown key logs in. Nothing it says is ever accepted: a change there since
// the detector last looked leads to a full, id-addressed fetch, whose API
// requests come out of their own small hourly budget (unknownKeyAPIBudget),
// so strangers offering unknown keys can never spend the API allowance the
// scheduled and admin refreshes need.

const (
	// githubAPIMaxBytes caps a REST API user response.
	githubAPIMaxBytes = 64 << 10
	// githubKeysMaxBytes caps a key list: the API's JSON or a .keys body. A
	// longer body is an outage, never cut to a prefix.
	githubKeysMaxBytes = 1 << 20
	// githubMaxBackoff bounds how long a rate-limit answer stops API requests.
	githubMaxBackoff = time.Hour
	// githubPinsVersion is the github_pins.json schema version.
	githubPinsVersion = 1
	// maxRejectedPerLogin bounds the fingerprints a pin remembers as dropped
	// for a mismatch; the oldest go first.
	maxRejectedPerLogin = 64
	// PinsFileName is the pin store's file name in the server state dir.
	PinsFileName = "github_pins.json"
	// githubUserAgent is the User-Agent of every GitHub request: generic, it
	// names no server and no operator.
	githubUserAgent = "shed-server"
	// githubKeysPerPage is the page size asked of the key list: the largest
	// the REST API allows.
	githubKeysPerPage = 100
	// githubKeysMaxPages bounds the pages of one key list. A list with more
	// is an outage, never cut to its first pages.
	githubKeysMaxPages = 10
	// unknownKeyAPIBudget is how many REST API requests fetches started by
	// unknown keys may make in any hour, for the whole server. The scheduled
	// and admin refreshes do not draw on it.
	unknownKeyAPIBudget = 20
	// unknownKeyAPIWindow is the window of unknownKeyAPIBudget.
	unknownKeyAPIWindow = time.Hour
)

// githubAPIBaseURL is the GitHub REST API base; overridable in tests.
var githubAPIBaseURL = "https://api.github.com"

// errLoginGone is a REST API 404 for a login: no account has that name now.
var errLoginGone = errors.New("no GitHub account has this login")

// errRateLimited is a REST API rate-limit response (403 or 429). It means
// "try later", never "the login is gone".
var errRateLimited = errors.New("GitHub API rate limit reached")

// errBodyTooLarge is a response longer than its limit: an outage, because a
// prefix of a key list is not the key list.
var errBodyTooLarge = errors.New("GitHub response is larger than its limit")

// errBudgetSpent is an API request an unknown-key fetch may not make: the
// hour's unknownKeyAPIBudget is spent. An outage for that fetch.
var errBudgetSpent = errors.New("the hourly GitHub API budget for unknown keys is spent")

// apiBudget is a sliding-window allowance of REST API requests. It is safe
// for concurrent use.
type apiBudget struct {
	clock  clock
	limit  int
	window time.Duration

	mu    sync.Mutex
	spent []time.Time // the requests of the current window, oldest first
	// warned is set when the budget runs out and cleared when it refills, so
	// running out is logged once.
	warned bool
}

func newAPIBudget(clk clock, limit int, window time.Duration) *apiBudget {
	return &apiBudget{clock: clk, limit: limit, window: window}
}

// prune drops requests older than the window. Called with mu held.
func (b *apiBudget) prune(now time.Time) {
	i := 0
	for i < len(b.spent) && !now.Before(b.spent[i].Add(b.window)) {
		i++
	}
	b.spent = b.spent[i:]
	if len(b.spent) < b.limit {
		b.warned = false
	}
}

// available reports whether a request may be made now.
func (b *apiBudget) available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(b.clock.Now())
	return len(b.spent) < b.limit
}

// take spends one request, and reports whether there was one to spend.
func (b *apiBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	b.prune(now)
	if len(b.spent) >= b.limit {
		if !b.warned {
			b.warned = true
			log.Printf("auth.ssh: unknown keys have used their %d GitHub API requests for the hour; "+
				"until it refills an unknown key starts no fetch (the scheduled refresh and `shed users refresh` are not affected)", b.limit)
		}
		return false
	}
	b.spent = append(b.spent, now)
	return true
}

// budgetKey is the context key of the apiBudget a fetch's API requests draw
// on; a fetch with none (scheduled, admin, start-up) is not budgeted here.
type budgetKey struct{}

func withBudget(ctx context.Context, b *apiBudget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

func budgetFrom(ctx context.Context) *apiBudget {
	b, _ := ctx.Value(budgetKey{}).(*apiBudget)
	return b
}

// githubAccount is the part of a REST API user response the pin reads.
type githubAccount struct {
	Login string
	ID    int64
}

// newGitHubHTTPClient is the client of every GitHub request: bounded in
// time, and never following a redirect, so an answer is only ever taken from
// the URL that was asked.
func newGitHubHTTPClient() *http.Client {
	return &http.Client{
		Timeout: githubFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// githubClient makes the requests of one fetch. It is safe for concurrent
// use: logins are fetched concurrently.
type githubClient struct {
	http    *http.Client
	apiBase string
	keyBase string
	clock   clock

	mu sync.Mutex
	// limitedUntil is when the REST API said its rate limit resets. Until
	// then no API request is made: each would fail the same way and spend
	// nothing but time.
	limitedUntil time.Time
}

// get makes one GET and returns its status and body. A body longer than max
// is an error (errBodyTooLarge), never a prefix.
func (c *githubClient) get(ctx context.Context, target string, accept string, max int64) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", githubUserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("reading GitHub response: %w", err)
	}
	if int64(len(body)) > max {
		return 0, nil, nil, fmt.Errorf("%w (%d bytes)", errBodyTooLarge, max)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// apiGet is a REST API GET of path. A rate-limit answer is errRateLimited
// and stops API requests until the limit resets; any other status is
// returned for the caller to read.
func (c *githubClient) apiGet(ctx context.Context, path string, max int64) (int, []byte, error) {
	status, _, body, err := c.apiGetURL(ctx, c.apiBase+path, max)
	return status, body, err
}

// apiGetURL is apiGet of a full URL, with the response headers. A request a
// budgeted fetch (see withBudget) has no budget left for is errBudgetSpent,
// and is not made.
func (c *githubClient) apiGetURL(ctx context.Context, target string, max int64) (int, http.Header, []byte, error) {
	c.mu.Lock()
	until := c.limitedUntil
	c.mu.Unlock()
	if now := c.clock.Now(); now.Before(until) {
		return 0, nil, nil, fmt.Errorf("%w (until %s)", errRateLimited, until.UTC().Format(time.RFC3339))
	}
	if b := budgetFrom(ctx); b != nil && !b.take() {
		return 0, nil, nil, errBudgetSpent
	}
	status, header, body, err := c.get(ctx, target, "application/vnd.github+json", max)
	if err != nil {
		return 0, nil, nil, err
	}
	if status == http.StatusTooManyRequests || status == http.StatusForbidden && rateLimitHeaders(header) {
		c.noteRateLimit(header)
		return 0, nil, nil, fmt.Errorf("%w (HTTP %d)", errRateLimited, status)
	}
	return status, header, body, nil
}

// resolve returns the account the login names now. errLoginGone means a 404;
// every other failure (network, 5xx, a redirect, a rate limit, a body that is
// not a user with a login and a positive id) is an outage and reads as
// "unknown", never as "gone" or "renamed".
func (c *githubClient) resolve(ctx context.Context, login string) (githubAccount, error) {
	status, body, err := c.apiGet(ctx, "/users/"+login, githubAPIMaxBytes)
	if err != nil {
		return githubAccount{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return githubAccount{}, errLoginGone
	default:
		// A 403 without rate-limit headers, or a redirect, is still not
		// "gone": the API did not say the account is missing.
		return githubAccount{}, fmt.Errorf("GitHub API returned %d", status)
	}
	var raw struct {
		Login *string `json:"login"`
		ID    *int64  `json:"id"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return githubAccount{}, fmt.Errorf("GitHub API response: %w", err)
	}
	if raw.Login == nil || *raw.Login == "" {
		return githubAccount{}, errors.New("GitHub API response has no login")
	}
	if raw.ID == nil || *raw.ID <= 0 {
		return githubAccount{}, errors.New("GitHub API response has no account id")
	}
	return githubAccount{Login: *raw.Login, ID: *raw.ID}, nil
}

// keysByID returns the keys of the account with this id, from the
// id-addressed key list, as an authorized_keys body (one canonical key a
// line; empty, not nil, for an account with no keys). The list is read a
// page of githubKeysPerPage at a time, following the Link header's next page
// until there is none. Anything but a 200 with a well-formed list on every
// page, a next link that leaves the list, or more than githubKeysMaxPages
// pages, is an error, and nothing read so far is returned.
func (c *githubClient) keysByID(ctx context.Context, id int64) ([]byte, error) {
	first, err := url.Parse(c.apiBase + "/user/" + strconv.FormatInt(id, 10) + "/keys")
	if err != nil {
		return nil, err
	}
	q := first.Query()
	q.Set("per_page", strconv.Itoa(githubKeysPerPage))
	first.RawQuery = q.Encode()
	out := []byte{}
	next := first.String()
	for page := 1; next != ""; page++ {
		if page > githubKeysMaxPages {
			return nil, fmt.Errorf("GitHub key list has more than %d pages", githubKeysMaxPages)
		}
		status, header, body, err := c.apiGetURL(ctx, next, githubKeysMaxBytes)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("page %d: GitHub API returned %d", page, status)
		}
		keys, err := parseKeyList(body)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		out = append(out, keys...)
		if next, err = nextPage(header, first); err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
	}
	return out, nil
}

// nextPage returns the next-page URL of a key list page from its Link
// header, or "" on the last page. A Link header that cannot be read, more
// than one next link, or a next link anywhere but the same scheme, host and
// path as the first page (another account's list, another host) is an
// error: the rest of the list cannot be had.
func nextPage(h http.Header, first *url.URL) (string, error) {
	values := h.Values("Link")
	if len(values) == 0 {
		return "", nil
	}
	nexts, err := parseLinkNext(strings.Join(values, ","))
	if err != nil {
		return "", fmt.Errorf("GitHub key list Link header: %w", err)
	}
	switch len(nexts) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", errors.New("GitHub key list Link header has more than one next page")
	}
	ref, err := url.Parse(nexts[0])
	if err != nil {
		return "", fmt.Errorf("GitHub key list next page: %w", err)
	}
	u := first.ResolveReference(ref)
	if u.Scheme != first.Scheme || !strings.EqualFold(u.Host, first.Host) || u.User != nil ||
		u.Path != first.Path || u.EscapedPath() != first.EscapedPath() || u.Fragment != "" {
		return "", fmt.Errorf("GitHub key list next page %q leaves the key list", u.Redacted())
	}
	return u.String(), nil
}

// parseLinkNext returns the targets of an RFC 8288 Link header value whose
// rel includes "next". A value it cannot read is an error.
func parseLinkNext(s string) ([]string, error) {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' }
	var nexts []string
	i := 0
	for {
		for i < len(s) && (isSpace(s[i]) || s[i] == ',') {
			i++
		}
		if i == len(s) {
			return nexts, nil
		}
		if s[i] != '<' {
			return nil, errors.New("a link does not start with '<'")
		}
		end := strings.IndexByte(s[i+1:], '>')
		if end < 0 {
			return nil, errors.New("a link has no closing '>'")
		}
		target := s[i+1 : i+1+end]
		i += end + 2
		var rels []string
		for {
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			if i == len(s) || s[i] == ',' {
				break
			}
			if s[i] != ';' {
				return nil, errors.New("a link parameter does not start with ';'")
			}
			i++
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			start := i
			for i < len(s) && !strings.ContainsRune("=;, \t\"", rune(s[i])) {
				i++
			}
			name := strings.ToLower(s[start:i])
			if name == "" {
				return nil, errors.New("a link parameter has no name")
			}
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			val := ""
			if i < len(s) && s[i] == '=' {
				i++
				for i < len(s) && isSpace(s[i]) {
					i++
				}
				if i < len(s) && s[i] == '"' {
					q := strings.IndexByte(s[i+1:], '"')
					if q < 0 {
						return nil, errors.New("a link parameter has an unterminated quote")
					}
					val = s[i+1 : i+1+q]
					i += q + 2
				} else {
					start := i
					for i < len(s) && !strings.ContainsRune(";, \t", rune(s[i])) {
						i++
					}
					val = s[start:i]
				}
			}
			if name == "rel" {
				rels = strings.Fields(strings.ToLower(val))
			}
		}
		if slices.Contains(rels, "next") {
			nexts = append(nexts, target)
		}
	}
}

// parseKeyList reads one page of the REST API's key list: a JSON array of
// objects, each with exactly one non-empty "key" string member. A body that
// is not exactly that (cut short, null, an object, an HTML page, trailing
// bytes, an entry with a member named twice in any case) is an error, and so
// is a key string that is corrupt (see parseGitHubKey). Only a well-formed
// key of an algorithm this build does not support is skipped and logged:
// GitHub can add algorithms, and one key this build cannot read must not
// stop the others.
func parseKeyList(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("GitHub key list is not a JSON array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, fmt.Errorf("GitHub key list: %w", err)
	}
	out := []byte{}
	for i, raw := range entries {
		s, err := keyListEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("GitHub key list entry %d: %w", i, err)
		}
		key, unsupported, err := parseGitHubKey(s)
		if err != nil {
			return nil, fmt.Errorf("GitHub key list entry %d: %w", i, err)
		}
		if key == nil {
			log.Printf("auth.ssh: skipping GitHub key list entry %d: algorithm %q is not supported by this server", i, unsupported)
			continue
		}
		out = append(out, gossh.MarshalAuthorizedKey(key)...)
	}
	return out, nil
}

// keyListEntry returns the "key" member of one key list entry: an object
// whose member names are all distinct (compared ignoring case, as
// encoding/json matches them) and whose "key" is a non-empty string.
func keyListEntry(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", errors.New("not an object")
	}
	seen := map[string]bool{}
	var key *string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		name, ok := tok.(string)
		if !ok {
			return "", errors.New("a member name is not a string")
		}
		folded := strings.ToLower(name)
		if seen[folded] {
			return "", fmt.Errorf("member %q appears more than once", name)
		}
		seen[folded] = true
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return "", err
		}
		if name == "key" {
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return "", errors.New(`"key" is not a string`)
			}
			key = &s
		}
	}
	if key == nil || *key == "" {
		return "", errors.New("has no key")
	}
	return *key, nil
}

// githubKeyAlgorithms are the key formats this build parses (those
// gossh.ParsePublicKey reads, plus the RSA signature names it refuses as key
// formats). A key string naming one of them must parse; a key string naming
// any other is a format this build does not know.
var githubKeyAlgorithms = map[string]bool{
	// DSA by name: x/crypto still parses it, and deprecates the constants.
	gossh.KeyAlgoRSA: true, "ssh-dss": true,
	gossh.KeyAlgoECDSA256: true, gossh.KeyAlgoECDSA384: true, gossh.KeyAlgoECDSA521: true,
	gossh.KeyAlgoSKECDSA256: true, gossh.KeyAlgoED25519: true, gossh.KeyAlgoSKED25519: true,
	gossh.KeyAlgoMLDSA44: true, gossh.KeyAlgoMLDSA65: true, gossh.KeyAlgoMLDSA87: true,
	gossh.KeyAlgoRSASHA256: true, gossh.KeyAlgoRSASHA512: true,
	gossh.CertAlgoRSAv01: true, "ssh-dss-cert-v01@openssh.com": true,
	gossh.CertAlgoECDSA256v01: true, gossh.CertAlgoECDSA384v01: true, gossh.CertAlgoECDSA521v01: true,
	gossh.CertAlgoSKECDSA256v01: true, gossh.CertAlgoED25519v01: true, gossh.CertAlgoSKED25519v01: true,
	gossh.CertAlgoMLDSA44v01Go: true, gossh.CertAlgoMLDSA65v01Go: true, gossh.CertAlgoMLDSA87v01Go: true,
	gossh.CertAlgoRSASHA256v01: true, gossh.CertAlgoRSASHA512v01: true,
}

// parseGitHubKey reads one key list key string: "<algorithm> <base64 blob>"
// with an optional comment, on one line. It returns the key, or, for a
// well-formed key of an algorithm this build does not know, a nil key and
// that algorithm's name. Everything else is corrupt, and an error.
//
// Corrupt and unsupported are told apart by the key's own wire format, whose
// first field is the algorithm name: the string is well-formed when it is one
// line, its algorithm field is a valid SSH algorithm name (RFC 4251 §6:
// printable US-ASCII, no comma, at most 64 bytes), its blob is valid base64,
// and the blob starts with an SSH string equal to that algorithm field. Such
// a key of an algorithm in githubKeyAlgorithms must then parse completely as
// that algorithm, or it is corrupt; one of any other algorithm is
// unsupported.
func parseGitHubKey(s string) (gossh.PublicKey, string, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return nil, "", errors.New("key is not one line")
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return nil, "", errors.New("key is not an algorithm and a blob")
	}
	algo := fields[0]
	if !validAlgorithmName(algo) {
		return nil, "", errors.New("key does not start with an algorithm name")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return nil, "", fmt.Errorf("%s key blob is not base64", algo)
	}
	if len(blob) < 4 {
		return nil, "", fmt.Errorf("%s key blob is too short", algo)
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) || string(blob[4:4+n]) != algo {
		return nil, "", fmt.Errorf("%s key blob is not a %s key", algo, algo)
	}
	if !githubKeyAlgorithms[algo] {
		return nil, algo, nil
	}
	key, err := gossh.ParsePublicKey(blob)
	if err != nil {
		return nil, "", fmt.Errorf("%s key is corrupt: %w", algo, err)
	}
	if key.Type() != algo {
		return nil, "", fmt.Errorf("%s key parses as %s", algo, key.Type())
	}
	return key, "", nil
}

// validAlgorithmName reports whether s may be an SSH algorithm name (RFC
// 4251 §6): 1 to 64 bytes of printable US-ASCII, with no comma.
func validAlgorithmName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' || s[i] == ',' {
			return false
		}
	}
	return true
}

// rateLimitHeaders reports whether a 403 is the REST API's rate limit: the
// primary limit says so with x-ratelimit-remaining: 0, a secondary limit with
// retry-after.
func rateLimitHeaders(h http.Header) bool {
	return h.Get("X-Ratelimit-Remaining") == "0" || h.Get("Retry-After") != ""
}

// noteRateLimit records when the API may be asked again: retry-after
// (seconds) when given, else x-ratelimit-reset (Unix seconds), else a
// minute; never more than githubMaxBackoff ahead. Each header is bounded
// before it becomes a duration, so a huge value cannot overflow into a time
// in the past.
func (c *githubClient) noteRateLimit(h http.Header) {
	now := c.clock.Now()
	maxSecs := int64(githubMaxBackoff / time.Second)
	until := now.Add(time.Minute)
	if s, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && s > 0 {
		until = now.Add(time.Duration(min(s, maxSecs)) * time.Second)
	} else if s, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil && s > now.Unix() {
		until = now.Add(time.Duration(min(s-now.Unix(), maxSecs)) * time.Second)
	}
	c.mu.Lock()
	if until.After(c.limitedUntil) {
		c.limitedUntil = until
	}
	c.mu.Unlock()
}

// dotKeys returns the login's github.com/<login>.keys body: a change
// detector only, never a source of accepted keys.
func (c *githubClient) dotKeys(ctx context.Context, login string) ([]byte, error) {
	status, _, body, err := c.get(ctx, c.keyBase+"/"+login+".keys", "", githubKeysMaxBytes)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github returned %d", status)
	}
	return body, nil
}

// fetchOutcome is what one fetch of a login found.
type fetchOutcome int

const (
	// fetchUnavailable: GitHub could not be asked, or did not answer as
	// expected. Last known keys stay; nothing new is accepted.
	fetchUnavailable fetchOutcome = iota
	// fetchConfirmed: the login named an account (the pin, when there is
	// one) and the keys are that account's, from its id-addressed list.
	fetchConfirmed
	// fetchMismatch: the login names an account other than its pin, or none.
	fetchMismatch
	// fetchGone: an unpinned login names no account.
	fetchGone
	// fetchUnchanged: a light check found the login's .keys listing the keys
	// last accepted.
	fetchUnchanged
)

func (o fetchOutcome) String() string {
	switch o {
	case fetchConfirmed:
		return "confirmed"
	case fetchMismatch:
		return "mismatch"
	case fetchGone:
		return "gone"
	case fetchUnchanged:
		return "unchanged"
	default:
		return "unavailable"
	}
}

// fetchResult is one login's fetch.
type fetchResult struct {
	outcome fetchOutcome
	// id is the account the keys belong to (fetchConfirmed), or the account
	// the login named instead of its pin (fetchMismatch; 0 when the login
	// named no account).
	id int64
	// keys is the confirmed account's key list as an authorized_keys body.
	keys []byte
	err  error
}

// fetchConfirmed resolves login to its account and lists that account's keys
// by its id. pin is the id the login must name (0 when it has none yet); a
// pinned login that names any other account, or none, is a mismatch.
func (c *githubClient) fetchConfirmed(ctx context.Context, login string, pin int64) fetchResult {
	acct, err := c.resolve(ctx, login)
	if r, done := judge(login, pin, acct, err); done {
		return r
	}
	data, err := c.keysByID(ctx, acct.ID)
	if err != nil {
		log.Printf("auth.ssh: github login %q names account %d, but the key list of that account (GET /user/%d/keys) is unavailable: %v; "+
			"its last known keys stay and no new key is accepted", login, acct.ID, acct.ID, err)
		return fetchResult{outcome: fetchUnavailable, err: fmt.Errorf("key list of account %d: %w", acct.ID, err)}
	}
	return fetchResult{outcome: fetchConfirmed, id: acct.ID, keys: data}
}

// judge classifies a resolution of a login with pin. done is true when the
// fetch ends here: an outage, the login gone, or a mismatch with the pin.
func judge(login string, pin int64, acct githubAccount, err error) (fetchResult, bool) {
	switch {
	case errors.Is(err, errLoginGone):
		if pin != 0 {
			return fetchResult{outcome: fetchMismatch, err: err}, true
		}
		return fetchResult{outcome: fetchGone, err: err}, true
	case err != nil:
		return fetchResult{outcome: fetchUnavailable, err: err}, true
	case !strings.EqualFold(acct.Login, login):
		// The API answered for another login: this login does not name that
		// account any more.
		if pin != 0 {
			return fetchResult{outcome: fetchMismatch, err: fmt.Errorf("the API answered for login %q", acct.Login)}, true
		}
		return fetchResult{outcome: fetchUnavailable, err: fmt.Errorf("the API answered for login %q", acct.Login)}, true
	case pin != 0 && acct.ID != pin:
		return fetchResult{outcome: fetchMismatch, id: acct.ID}, true
	}
	return fetchResult{}, false
}

// --- the pin store ------------------------------------------------------------

// pinEntry is one login's pin.
type pinEntry struct {
	// ID is the GitHub account id the login is pinned to.
	ID int64 `json:"id"`
	// PinnedAt is when the pin was first written.
	PinnedAt time.Time `json:"pinned_at"`
	// KeysSHA256 is the hex SHA-256 of the key list last confirmed for this
	// pin. The per-login key cache file is trusted, when GitHub cannot be
	// reached at start-up, only when its bytes hash to this: a cache that was
	// written for another account (a mismatch, or an older binary after a
	// rollback) is never read back. "" trusts no cache.
	KeysSHA256 string `json:"keys_sha256,omitempty"`
	// Rejected holds the fingerprints dropped when the login stopped naming
	// this account (at most maxRejectedPerLogin, newest last). They never
	// start an unknown-key fetch, across restarts too, until the login
	// names this account again.
	Rejected []string `json:"rejected,omitempty"`
}

// equal reports whether e and o would be stored the same.
func (e pinEntry) equal(o pinEntry) bool {
	return e.ID == o.ID && e.PinnedAt.Equal(o.PinnedAt) && e.KeysSHA256 == o.KeysSHA256 &&
		slices.Equal(e.Rejected, o.Rejected)
}

// pinsFile is github_pins.json.
type pinsFile struct {
	Version int                 `json:"version"`
	Pins    map[string]pinEntry `json:"pins"`
}

// pinKey is a login's key in the pin store: GitHub logins are
// case-insensitive.
func pinKey(login string) string { return strings.ToLower(login) }

// validFingerprint reports whether s has the form of
// gossh.FingerprintSHA256: "SHA256:" and 43 unpadded base64 characters.
func validFingerprint(s string) bool {
	const prefix = "SHA256:"
	if !strings.HasPrefix(s, prefix) || len(s) != len(prefix)+43 {
		return false
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for _, r := range s[len(prefix):] {
		if !strings.ContainsRune(alphabet, r) {
			return false
		}
	}
	return true
}

// mergeRejected returns old with each fingerprint of add it lacks appended,
// keeping the newest maxRejectedPerLogin.
func mergeRejected(old, add []string) []string {
	out := slices.Clone(old)
	for _, fp := range add {
		if !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	if n := len(out) - maxRejectedPerLogin; n > 0 {
		out = out[n:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// loadPins reads the pin store. A missing file is an empty store. A file that
// cannot be read or parsed, has another version, or holds an entry that is
// not a valid login with a positive id is an error: the caller fails closed
// (shed-server does not start), because treating it as empty would pin every
// login again to whatever account it names today.
func loadPins(path string) (map[string]pinEntry, error) {
	pins := map[string]pinEntry{}
	if path == "" {
		return pins, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return nil, err
	}
	var f pinsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if f.Version != githubPinsVersion {
		return nil, fmt.Errorf("version %d, want %d", f.Version, githubPinsVersion)
	}
	for login, e := range f.Pins {
		if login != pinKey(login) || !config.ValidGitHubUsername(login) {
			return nil, fmt.Errorf("entry %q is not a lower-case GitHub login", login)
		}
		if e.ID <= 0 {
			return nil, fmt.Errorf("entry %q has no account id", login)
		}
		if e.KeysSHA256 != "" {
			if b, err := hex.DecodeString(e.KeysSHA256); err != nil || len(b) != sha256.Size {
				return nil, fmt.Errorf("entry %q has a malformed keys_sha256", login)
			}
		}
		if len(e.Rejected) > maxRejectedPerLogin {
			return nil, fmt.Errorf("entry %q lists more than %d rejected keys", login, maxRejectedPerLogin)
		}
		for _, fp := range e.Rejected {
			if !validFingerprint(fp) {
				return nil, fmt.Errorf("entry %q has a malformed rejected fingerprint", login)
			}
		}
		pins[login] = e
	}
	return pins, nil
}

// encodePins renders the pin store, entries in login order.
func encodePins(pins map[string]pinEntry) ([]byte, error) {
	f := pinsFile{Version: githubPinsVersion, Pins: pins}
	if f.Pins == nil {
		f.Pins = map[string]pinEntry{}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// keysDigest is the hex SHA-256 a pin records for a key list.
func keysDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeFileDurable writes data to path so that, once it returns nil, the new
// content survives a crash: a temporary file in the same directory is
// written, synced and renamed over path, and the directory is synced. Mode
// 0600. An error before the rename leaves path unchanged; an error syncing
// the directory after it leaves the new content at path, perhaps not yet
// durable. Callers treat both as "not written".
func writeFileDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// --- clock --------------------------------------------------------------------

// clock is the lookup's time source; tests drive a fake one.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// fingerprintsOf returns the fingerprints of the keys in an authorized_keys
// body, sorted.
func fingerprintsOf(data []byte) []string {
	set := map[string]keyEntry{}
	addAuthorizedKeys(set, data, "", SourceGitHub, "")
	out := make([]string, 0, len(set))
	for fp := range set {
		out = append(out, fp)
	}
	sort.Strings(out)
	return out
}

// sameFingerprints reports whether two authorized_keys bodies list the same
// keys.
func sameFingerprints(a, b []byte) bool {
	return slices.Equal(fingerprintsOf(a), fingerprintsOf(b))
}
