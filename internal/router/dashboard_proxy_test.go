package router

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A tool behind the proxy: records what it received and answers a few
// shapes (JSON, a redirect, an SSE stream).
type proxyBackend struct {
	method, path, query, prefixHdr, body string
}

func (b *proxyBackend) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.method, b.path, b.query = r.Method, r.URL.Path, r.URL.RawQuery
		b.prefixHdr = r.Header.Get(dashProxyPrefixHeader)
		body, _ := io.ReadAll(r.Body)
		b.body = string(body)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/session/abc?x=1", http.StatusFound)
		case "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, "data: one\n\n")
			fl.Flush()
			// Hold the stream open: the client must see "one" before this
			// handler returns, or the proxy buffered.
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, "data: two\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	})
}

func proxyDash(t *testing.T, cfg DashboardConfig) *httptest.Server {
	t.Helper()
	rt := newTestRouter(t, nil)
	srv := httptest.NewServer(rt.DashboardHandler(cfg))
	t.Cleanup(srv.Close)
	return srv
}

func TestDashProxy_StripsPrefixAndPassesMethodBodyQuery(t *testing.T) {
	be := &proxyBackend{}
	tool := httptest.NewServer(be.handler())
	defer tool.Close()
	dash := proxyDash(t, DashboardConfig{MonitorURL: tool.URL})

	resp, err := http.Post(dash.URL+"/monitor/api/tasks?project=x", "application/json", strings.NewReader(`{"title":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(out), `"/api/tasks"`) {
		t.Fatalf("proxied POST: %d %s", resp.StatusCode, out)
	}
	if be.method != "POST" || be.path != "/api/tasks" || be.query != "project=x" || be.body != `{"title":"t"}` {
		t.Errorf("upstream saw method=%s path=%s query=%s body=%s", be.method, be.path, be.query, be.body)
	}
	if be.prefixHdr != "/monitor" {
		t.Errorf("X-Forwarded-Prefix = %q, want /monitor", be.prefixHdr)
	}

	// The bare prefix redirects to the slash form; the slash form is the
	// upstream's root.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r2, _ := client.Get(dash.URL + "/monitor?a=b")
	if r2.StatusCode != http.StatusMovedPermanently || r2.Header.Get("Location") != "/monitor/?a=b" {
		t.Errorf("bare prefix: %d %s", r2.StatusCode, r2.Header.Get("Location"))
	}
	r3, _ := http.Get(dash.URL + "/monitor/")
	if r3.StatusCode != 200 || be.path != "/" {
		t.Errorf("prefix root: %d, upstream path %q", r3.StatusCode, be.path)
	}
}

func TestDashProxy_RewritesRedirectsUnderThePrefix(t *testing.T) {
	be := &proxyBackend{}
	tool := httptest.NewServer(be.handler())
	defer tool.Close()
	dash := proxyDash(t, DashboardConfig{TokensURL: tool.URL})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(dash.URL + "/tokens/redirect")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/tokens/session/abc?x=1" {
		t.Errorf("redirect: %d Location=%q, want /tokens/session/abc?x=1", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestDashProxy_StreamsSSEWithoutBuffering(t *testing.T) {
	be := &proxyBackend{}
	tool := httptest.NewServer(be.handler())
	defer tool.Close()
	dash := proxyDash(t, DashboardConfig{MonitorURL: tool.URL})

	start := time.Now()
	resp, err := http.Get(dash.URL + "/monitor/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	line, err := rd.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first event: %q %v", line, err)
	}
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Errorf("first SSE event arrived after %v: the proxy buffered until the upstream finished", d)
	}
}

func TestDashProxy_UnconfiguredIs404WithTheFlag(t *testing.T) {
	dash := proxyDash(t, DashboardConfig{})
	for prefix, flag := range map[string]string{"/monitor/api/agents": "--dashboard-monitor-url", "/tokens/": "--dashboard-tokens-url"} {
		resp, err := http.Get(dash.URL + prefix)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), flag) {
			t.Errorf("%s: %d %s (want 404 naming %s)", prefix, resp.StatusCode, body, flag)
		}
	}
}

// With a front-proxy secret configured, the proxies sit behind the same
// identity gate as /api/requests: a request that bypassed the front door is
// refused, one carrying the secret + identity goes through.
func TestDashProxy_BehindTheIdentityGate(t *testing.T) {
	be := &proxyBackend{}
	tool := httptest.NewServer(be.handler())
	defer tool.Close()
	dash := proxyDash(t, DashboardConfig{TokensURL: tool.URL, AuthSecret: "s3cret"})

	resp, _ := http.Get(dash.URL + "/tokens/session/x")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no identity: %d, want 401", resp.StatusCode)
	}
	if be.path == "/session/x" {
		t.Fatal("the upstream must not have been reached without identity")
	}
	req, _ := http.NewRequest(http.MethodGet, dash.URL+"/tokens/session/x", nil)
	req.Header.Set(DashboardAuthHeader, "s3cret")
	req.Header.Set("X-Auth-Request-Email", "someone@example.org")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || be.path != "/session/x" {
		t.Errorf("with identity: %d, upstream path %q", resp.StatusCode, be.path)
	}
}

func TestDashProxy_UnreachableUpstreamIs502(t *testing.T) {
	dash := proxyDash(t, DashboardConfig{MonitorURL: "http://127.0.0.1:1"})
	resp, err := http.Get(dash.URL + "/monitor/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "unreachable") {
		t.Errorf("down upstream: %d %s", resp.StatusCode, body)
	}
}
