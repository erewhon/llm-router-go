package router

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Same-origin proxies for the other smithy tool UIs.
//
// The dashboard is the one web page (decision 2026-09-26, PITF "Unified web
// UI"): agent-monitor and tokenator are reached THROUGH it, at /monitor/*
// and /tokens/*, instead of the browser talking to them directly. Same
// origin means no CORS on the tools, no cross-site cookie problem for the
// SSO in front of the dashboard, no Chrome local-network prompt, and one
// gate for everything: the identity gate that fronts the other
// identity-bearing routes fronts these too, so a request straight at the
// listener (bypassing the front proxy) is refused exactly like /api/requests
// would be. The targets are addresses the ROUTER can reach — loopback on a
// laptop, the coding box's LAN address at home (the replicas sit on OVN and
// egress from 192.168.11.0/24, which that box's firewall admits).
//
// Prefix handling: /monitor/api/agents → <target>/api/agents. The upstream
// learns where it lives from X-Forwarded-Prefix (tokenator's base-path mode
// reads it) and path-absolute Location headers it sends back are re-prefixed
// so redirects stay inside the proxy. Streaming (agent-monitor's SSE
// /api/events) is passed through unbuffered.

// dashProxyPrefixHeader tells the upstream the path prefix the browser sees.
const dashProxyPrefixHeader = "X-Forwarded-Prefix"

// newDashProxy builds the reverse proxy for one prefix ("/monitor") and
// target base URL. prefix has no trailing slash.
func newDashProxy(prefix, target string) (http.Handler, error) {
	u, err := url.Parse(strings.TrimRight(target, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy target for %s must be an absolute http(s) URL, got %q", prefix, target)
	}
	basePath := strings.TrimRight(u.Path, "/")
	rp := &httputil.ReverseProxy{
		// Negative: flush as bytes arrive. SSE and long transcript pages
		// both want that; nothing here is throughput-bound.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
			rest := strings.TrimPrefix(pr.In.URL.Path, prefix)
			if rest == "" {
				rest = "/"
			}
			pr.Out.URL.Path = basePath + rest
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Header.Set(dashProxyPrefixHeader, prefix)
		},
		ModifyResponse: func(resp *http.Response) error {
			// A path-absolute redirect from the upstream ("/session/x") would
			// escape the prefix; keep it under it. Absolute URLs pointing at
			// the upstream itself get the same treatment.
			loc := resp.Header.Get("Location")
			if loc == "" {
				return nil
			}
			if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, prefix+"/") && loc != prefix {
				resp.Header.Set("Location", prefix+strings.TrimPrefix(loc, basePath))
			} else if lu, err := url.Parse(loc); err == nil && lu.Host == u.Host && lu.Scheme == u.Scheme {
				resp.Header.Set("Location", prefix+strings.TrimPrefix(lu.Path, basePath)+optQuery(lu.RawQuery))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeDashError(w, http.StatusBadGateway,
				fmt.Sprintf("%s upstream %s is unreachable: %v", prefix, u.Redacted(), err))
		},
	}
	return rp, nil
}

func optQuery(q string) string {
	if q == "" {
		return ""
	}
	return "?" + q
}

// dashProxyMissing answers a prefix whose target is not configured: a plain
// 404 that says which flag turns it on, so a stale bookmark or a tab that
// probes for the proxy learns why instead of seeing a generic not-found.
func dashProxyMissing(prefix, flag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDashError(w, http.StatusNotFound,
			fmt.Sprintf("%s/ is not proxied on this router: start it with %s", prefix, flag))
	})
}

// mountDashProxy wires prefix (and its slash-less form) into mux: the proxy
// when target is set, the explanatory 404 otherwise. gate fronts the proxy.
func mountDashProxy(mux *http.ServeMux, gate func(http.Handler) http.Handler, prefix, flag, target string) error {
	var h http.Handler
	if target == "" {
		h = dashProxyMissing(prefix, flag)
	} else {
		p, err := newDashProxy(prefix, target)
		if err != nil {
			return err
		}
		h = gate(p)
	}
	mux.Handle(prefix+"/", h)
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/"+optQuery(r.URL.RawQuery), http.StatusMovedPermanently)
	})
	return nil
}
