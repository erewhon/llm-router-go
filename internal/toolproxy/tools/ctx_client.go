package tools

import (
	"context"
	"net/http"
)

type clientCtxKey struct{}

// WithClient returns a context carrying the http.Client that network tools
// (web_search, fetch_url, tavily) should use for this request, overriding their
// registration-time default. The tool proxy sets this per request from the
// X-Egress VPN-exit selection; absent it, tools use their bound default client.
func WithClient(ctx context.Context, c *http.Client) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, clientCtxKey{}, c)
}

// pickClient returns the per-request client from ctx if one was set, else the
// fallback (the tool's bound default).
func pickClient(ctx context.Context, fallback *http.Client) *http.Client {
	if c, ok := ctx.Value(clientCtxKey{}).(*http.Client); ok && c != nil {
		return c
	}
	return fallback
}
