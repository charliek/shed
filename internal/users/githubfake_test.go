package users

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain points both GitHub bases at an address that refuses connections,
// so a test that forgets to install a fake fails instead of reaching GitHub.
func TestMain(m *testing.M) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	githubAPIBaseURL = deadURL
	githubKeysBaseURL = deadURL
	os.Exit(m.Run())
}

// fakeGitHub serves the GitHub endpoints the lookup uses: GET /users/<login>
// (the REST API: {"login","id"}), GET /user/<id>/keys (the REST API's
// id-addressed key list: [{"id","key"}]) and GET /<login>.keys (the change
// detector on github.com).
type fakeGitHub struct {
	srv *httptest.Server

	mu sync.Mutex
	// accounts maps a login (lower case) to the account id it names; a login
	// not in it is a 404 on the API and on .keys.
	accounts map[string]int64
	// keysByID maps an account id to its keys, one authorized_keys line each
	// (as an authorized_keys body).
	keysByID map[int64]string
	// dotKeys, when it has a login, is that login's .keys body instead of its
	// account's keys.
	dotKeys map[string]string
	// accountBody, when it has a login, is the 200 body of /users/<login>.
	accountBody map[string]string
	// apiStatus, when set, is what the API answers every request, with
	// apiHeader.
	apiStatus int
	apiHeader http.Header
	// idKeysStatus, when set, is what /user/<id>/keys answers; idKeysBody,
	// when set, is its 200 body; idKeysRedirect, when set, is where it
	// redirects (that path itself answers as usual).
	idKeysStatus   int
	idKeysBody     string
	idKeysRedirect string
	// keysStatus, when set, is what .keys answers every request.
	keysStatus int
	// onKeys runs while a .keys request is being served, before it answers.
	onKeys func(login string)
	// onIDKeys runs while an id-addressed key list is being served, before
	// it answers.
	onIDKeys func(id int64)
	// pageSize, when set, is the key list's page size whatever per_page
	// asks; otherwise it is per_page, at most 100, 30 when absent (as the
	// REST API pages).
	pageSize int
	// pageStatus, when it has a page number, is what that page answers.
	pageStatus map[int]int
	// linkOverride, when set, is the Link header of the key list's first
	// page (a request with no page parameter) instead of the computed one.
	linkOverride string
	// perPage records the per_page of each key list request.
	perPage []string
	// rateClock and ratePerHour, when set, give the API GitHub's
	// unauthenticated rate limit: ratePerHour requests in a window of an
	// hour that starts at its first request, then 403 with
	// x-ratelimit-remaining 0 until the window resets.
	rateClock   *fakeClock
	ratePerHour int
	rateStart   time.Time
	rateUsed    int
	rateRefused int

	apiCalls  atomic.Int64
	keysCalls atomic.Int64
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		accounts:    map[string]int64{},
		keysByID:    map[int64]string{},
		dotKeys:     map[string]string{},
		accountBody: map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	origAPI, origKeys := githubAPIBaseURL, githubKeysBaseURL
	githubAPIBaseURL, githubKeysBaseURL = f.srv.URL, f.srv.URL
	t.Cleanup(func() { githubAPIBaseURL, githubKeysBaseURL = origAPI, origKeys })
	return f
}

// keyListJSON renders an authorized_keys body as the API's key list.
func keyListJSON(body string) string {
	type entry struct {
		ID  int    `json:"id"`
		Key string `json:"key"`
	}
	list := []entry{}
	for i, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			list = append(list, entry{ID: i + 1, Key: line})
		}
	}
	data, _ := json.Marshal(list)
	return string(data)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("User-Agent") != githubUserAgent {
		http.Error(w, "unexpected user agent", http.StatusBadRequest)
		return
	}
	if f.rateLimited(w, r) {
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/users/"):
		f.apiCalls.Add(1)
		login := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/users/"))
		f.mu.Lock()
		status, header := f.apiStatus, f.apiHeader
		id, ok := f.accounts[login]
		raw, hasRaw := f.accountBody[login]
		f.mu.Unlock()
		if f.apiOverride(w, status, header) {
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","status":"404"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if hasRaw {
			_, _ = w.Write([]byte(raw))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": login, "id": id, "type": "User"})
	case strings.HasPrefix(r.URL.Path, "/user/") && strings.HasSuffix(r.URL.Path, "/keys"):
		f.apiCalls.Add(1)
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/user/"), "/keys"), 10, 64)
		f.mu.Lock()
		status, header := f.apiStatus, f.apiHeader
		keysStatus, keysBody, redirect, hook := f.idKeysStatus, f.idKeysBody, f.idKeysRedirect, f.onIDKeys
		body, ok := f.keysByID[id]
		f.perPage = append(f.perPage, r.URL.Query().Get("per_page"))
		pageSize, link := f.pageSize, f.linkOverride
		page := 1
		if v := r.URL.Query().Get("page"); v != "" {
			page, _ = strconv.Atoi(v)
		}
		pageStatus := f.pageStatus[page]
		if link != relativeLinks && r.URL.Query().Has("page") {
			link = "" // an override is the first page's alone
		}
		f.mu.Unlock()
		if hook != nil {
			hook(id)
		}
		if f.apiOverride(w, status, header) {
			return
		}
		switch {
		case redirect != "" && redirect != r.URL.Path:
			http.Redirect(w, r, redirect, http.StatusFound)
		case keysStatus != 0:
			w.WriteHeader(keysStatus)
		case err != nil || !ok:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","status":"404"}`))
		case pageStatus != 0:
			w.WriteHeader(pageStatus)
		case keysBody != "":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(keysBody))
		default:
			if pageSize == 0 {
				pageSize = 30
				if n, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && n > 0 {
					pageSize = min(n, 100)
				}
			}
			lines := keyLines(body)
			last := max(1, (len(lines)+pageSize-1)/pageSize)
			from, to := min((page-1)*pageSize, len(lines)), min(page*pageSize, len(lines))
			if (link == "" || link == relativeLinks) && last > 1 {
				base := f.srv.URL
				if link == relativeLinks {
					base = ""
				}
				pageURL := func(n int) string {
					return base + r.URL.Path + "?per_page=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(n)
				}
				var rels []string
				if page < last {
					rels = append(rels, "<"+pageURL(page+1)+`>; rel="next"`, "<"+pageURL(last)+`>; rel="last"`)
				}
				if page > 1 {
					rels = append(rels, "<"+pageURL(1)+`>; rel="first"`, "<"+pageURL(page-1)+`>; rel="prev"`)
				}
				link = strings.Join(rels, ", ")
			}
			if link != "" && link != relativeLinks {
				w.Header().Set("Link", link)
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(keyListJSON(strings.Join(lines[from:to], "\n"))))
		}
	case strings.HasSuffix(r.URL.Path, ".keys"):
		f.keysCalls.Add(1)
		login := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".keys"))
		// The answer is read before the hook runs: a hook that blocks holds
		// the response as GitHub had it when the request arrived.
		f.mu.Lock()
		hook := f.onKeys
		status := f.keysStatus
		id, named := f.accounts[login]
		body := f.keysByID[id]
		if over, ok := f.dotKeys[login]; ok {
			body, named = over, true
		}
		f.mu.Unlock()
		if hook != nil {
			hook(login)
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if !named {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// keyLines splits an authorized_keys body into its non-empty lines.
func keyLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// rateLimited answers an API request with GitHub's rate-limit 403 when the
// fake's hourly allowance is spent, and counts it otherwise.
func (f *fakeGitHub) rateLimited(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/user") {
		return false // .keys is another host, outside the API's limit
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rateClock == nil {
		return false
	}
	now := f.rateClock.Now()
	if f.rateStart.IsZero() || !now.Before(f.rateStart.Add(time.Hour)) {
		f.rateStart, f.rateUsed = now, 0
	}
	if f.rateUsed < f.ratePerHour {
		f.rateUsed++
		return false
	}
	f.rateRefused++
	w.Header().Set("X-Ratelimit-Remaining", "0")
	w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(f.rateStart.Add(time.Hour).Unix(), 10))
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	return true
}

// setRateLimit gives the API an hourly allowance on clk.
func (f *fakeGitHub) setRateLimit(clk *fakeClock, perHour int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateClock, f.ratePerHour = clk, perHour
}

// refused is how many API requests the fake's rate limit refused.
func (f *fakeGitHub) refused() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rateRefused
}

// relativeLinks, as setPaging's link, makes the key list's Link header use
// relative references instead of absolute URLs.
const relativeLinks = "relative"

// setPaging sets the key list's forced page size (0 for per_page), the
// pages that fail with a status, and a Link header override ("" for none).
func (f *fakeGitHub) setPaging(pageSize int, pageStatus map[int]int, link string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageSize, f.pageStatus, f.linkOverride = pageSize, pageStatus, link
}

// perPages is the per_page of each key list request so far.
func (f *fakeGitHub) perPages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.perPage...)
}

// apiOverride answers an API request with apiStatus when one is set.
func (f *fakeGitHub) apiOverride(w http.ResponseWriter, status int, header http.Header) bool {
	if status == 0 {
		return false
	}
	for k, v := range header {
		w.Header()[k] = v
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	return true
}

// set makes login name the account id, whose keys are this authorized_keys
// body.
func (f *fakeGitHub) set(login string, id int64, keys string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[strings.ToLower(login)] = id
	f.keysByID[id] = keys
}

// setAccountKeys sets the keys of the account with this id, whatever login
// names it.
func (f *fakeGitHub) setAccountKeys(id int64, keys string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keysByID[id] = keys
}

// setDotKeys makes login's .keys answer body, whatever its account lists.
func (f *fakeGitHub) setDotKeys(login, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dotKeys[strings.ToLower(login)] = body
}

// setAccountBody makes /users/<login> answer 200 with this body.
func (f *fakeGitHub) setAccountBody(login, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountBody[strings.ToLower(login)] = body
}

// remove makes login name no account.
func (f *fakeGitHub) remove(login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.accounts, strings.ToLower(login))
}

func (f *fakeGitHub) setAPIStatus(status int, header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiStatus, f.apiHeader = status, header
}

// setIDKeys overrides the id-addressed key list: a status, a raw 200 body,
// or a redirect target ("" each for none).
func (f *fakeGitHub) setIDKeys(status int, body, redirect string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idKeysStatus, f.idKeysBody, f.idKeysRedirect = status, body, redirect
}

func (f *fakeGitHub) setKeysStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keysStatus = status
}

func (f *fakeGitHub) setOnKeys(hook func(login string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onKeys = hook
}

func (f *fakeGitHub) setOnIDKeys(hook func(id int64)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onIDKeys = hook
}

// calls is the number of API and .keys requests served so far.
func (f *fakeGitHub) calls() (api, keys int64) { return f.apiCalls.Load(), f.keysCalls.Load() }

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	// waiting receives a value each time After is called.
	waiting chan struct{}
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC), waiting: make(chan struct{}, 64)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return ch
}

// Advance moves the clock and fires every timer that is due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}
