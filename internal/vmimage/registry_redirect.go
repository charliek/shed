package vmimage

import (
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// maxRegistryRedirects is the redirect bound Go's http.Client applies by
// default (its "stopped after 10 redirects").
const maxRegistryRedirects = 10

// redirectLimitTransport restores a redirect-count bound on registry traffic.
//
// go-containerregistry (from v0.21.6) gives its fetcher's http.Client a
// CheckRedirect of its own, an SSRF check that never looks at len(via) and
// passes every same-host redirect. Setting CheckRedirect replaces Go's
// default ten-redirect limit, so a registry that redirects a manifest or blob
// request back to itself would be followed until the context ends, with the
// redirect chain growing all the while. The client is built inside the
// library, so the bound lives one level down: http.Client hands each
// redirected request to the transport with Request.Response set to the
// response that caused it, and that response's Request is the previous hop,
// so the transport can count the hops itself.
type redirectLimitTransport struct {
	inner http.RoundTripper
}

func (t redirectLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	hops := 0
	for r := req; r.Response != nil && r.Response.Request != nil; r = r.Response.Request {
		hops++
		if hops >= maxRegistryRedirects {
			return nil, fmt.Errorf("registry request %s %s: stopped after %d redirects", req.Method, req.URL.Redacted(), maxRegistryRedirects)
		}
	}
	return t.inner.RoundTrip(req)
}

// registryTransport is the transport every registry operation uses: the
// library's default, with the redirect bound restored.
func registryTransport() remote.Option {
	return remote.WithTransport(redirectLimitTransport{inner: remote.DefaultTransport})
}
