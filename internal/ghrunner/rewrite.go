package ghrunner

import (
	"net/http"
	"net/url"
)

// RequestURLRewrite, when set, rewrites EVERY outbound request URL before it is
// sent. The wasm/browser build uses this to route no-CORS Actions hosts (the
// broker and the region-sharded pipelines/Actions service) through a small CORS
// proxy. nil for native runs, which reach GitHub directly.
var RequestURLRewrite func(string) string

// rewriteTransport wraps a base RoundTripper and applies RequestURLRewrite to
// every request. Installing it on the shared clients catches all call sites
// (register, oauth, session create, long-poll, job APIs) without per-site edits.
type rewriteTransport struct{ base http.RoundTripper }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if RequestURLRewrite != nil {
		if nu := RequestURLRewrite(req.URL.String()); nu != "" {
			if parsed, err := url.Parse(nu); err == nil {
				req = req.Clone(req.Context())
				req.URL = parsed
				req.Host = parsed.Host
			}
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func init() {
	httpClient.Transport = rewriteTransport{base: httpClient.Transport}
	pollClient.Transport = rewriteTransport{base: pollClient.Transport}
}
