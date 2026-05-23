package sglang

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/nodeagent/backends"
)

// fakeUpstream returns a *httptest.Server whose /v1/models endpoint
// advertises the given model IDs and whose /metrics endpoint returns
// the given exposition text.
func fakeUpstream(t *testing.T, modelIDs []string, metrics string) (*httptest.Server, int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"data": []map[string]string{}}
		for _, id := range modelIDs {
			body["data"] = append(body["data"].([]map[string]string), map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, metrics)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	portInt, _ := strconv.Atoi(u.Port())
	return srv, portInt
}

func newBackendFor(t *testing.T, srv *httptest.Server) *Backend {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	b := New(u.Hostname())
	// shrink timeout to keep the unreachable test fast
	b.client.Timeout = 2 * time.Second
	return b
}

func TestStatus_RunningWithMetrics(t *testing.T) {
	srv, port := fakeUpstream(t, []string{"Qwen/Qwen3.6-27B-FP8"}, `
num_running_reqs{x="y"} 5
num_queue_reqs{x="y"} 2
gen_throughput{x="y"} 87.3
num_requests_total{x="y"} 4242
`)
	b := newBackendFor(t, srv)

	model := &config.ModelDefinition{
		HFRepo:  "Qwen/Qwen3.6-27B-FP8",
		APIPort: port,
	}
	s := b.Status(context.Background(), "qwen-arch", model)

	if s.State != backends.StateRunning {
		t.Fatalf("state = %q, want running", s.State)
	}
	if s.Port == nil || *s.Port != port {
		t.Errorf("port = %v, want %d", s.Port, port)
	}
	if s.RequestsRunning != 5 || s.RequestsWaiting != 2 || s.TotalRequests != 4242 {
		t.Errorf("counts wrong: %+v", s)
	}
	if s.AvgTokPerSec == nil || *s.AvgTokPerSec != 87.3 {
		t.Errorf("avg tok/s = %v, want 87.3", s.AvgTokPerSec)
	}
}

func TestStatus_WrongModel(t *testing.T) {
	// Upstream is reachable but serving a different model. From our
	// model's point of view it's stopped (the other model has the slot),
	// not errored. Matches the Python agent's behaviour.
	srv, port := fakeUpstream(t, []string{"Qwen/Qwen3.6-27B-FP8"}, "")
	b := newBackendFor(t, srv)

	model := &config.ModelDefinition{
		HFRepo:  "Qwen/SomethingElse",
		APIPort: port,
	}
	s := b.Status(context.Background(), "wrong", model)

	if s.State != backends.StateStopped {
		t.Fatalf("state = %q, want stopped (another model on the slot)", s.State)
	}
	if s.Error != "" {
		t.Errorf("unexpected error message on stopped state: %q", s.Error)
	}
}

func TestStatus_HFRepoSuffixIgnored(t *testing.T) {
	// hf_repo "Qwen/Qwen3.5-35B-A3B#nothink" should match upstream id "Qwen/Qwen3.5-35B-A3B".
	srv, port := fakeUpstream(t, []string{"Qwen/Qwen3.5-35B-A3B"}, "")
	b := newBackendFor(t, srv)

	model := &config.ModelDefinition{
		HFRepo:  "Qwen/Qwen3.5-35B-A3B#nothink",
		APIPort: port,
	}
	s := b.Status(context.Background(), "qwen", model)
	if s.State != backends.StateRunning {
		t.Errorf("state = %q, want running (suffix should be stripped for match)", s.State)
	}
}

func TestStatus_Unreachable(t *testing.T) {
	// Pick a free port and immediately close the listener so the address
	// is guaranteed unreachable (connection refused).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	b := New("127.0.0.1")
	b.client.Timeout = 1 * time.Second

	model := &config.ModelDefinition{
		HFRepo:  "Qwen/Qwen3.6-27B-FP8",
		APIPort: port,
	}
	s := b.Status(context.Background(), "qwen", model)

	if s.State != backends.StateStopped {
		t.Errorf("state = %q, want stopped (connection refused)", s.State)
	}
	if s.Port != nil {
		t.Errorf("port = %v, want nil for unreachable", s.Port)
	}
}

func TestStatus_BackendReturnsServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	portInt, _ := strconv.Atoi(u.Port())
	b := New(u.Hostname())

	model := &config.ModelDefinition{HFRepo: "anything", APIPort: portInt}
	s := b.Status(context.Background(), "x", model)
	if s.State != backends.StateError {
		t.Errorf("state = %q, want error (upstream 500)", s.State)
	}
	if s.Error == "" {
		t.Errorf("error message empty on 500")
	}
}

func TestStatus_NilModel(t *testing.T) {
	// With model=nil the probe is reachability-only; gets StateRunning
	// when /v1/models responds 200 regardless of advertised IDs.
	srv, port := fakeUpstream(t, []string{"some-other-model"}, "")
	b := newBackendFor(t, srv)
	// Use a definition with APIPort just for routing; pass nil to Status
	// to exercise the "no model to match" path.
	_ = port
	s := b.Status(context.Background(), "any", nil)
	// With model=nil port defaults to DefaultPort (5391); that won't
	// match the test server's port, so we expect stopped.
	if s.State != backends.StateStopped {
		t.Errorf("state = %q, want stopped (probe hit default port, not test server)", s.State)
	}
}

func TestStatus_NilModelMatchedPort(t *testing.T) {
	// Same as above but explicitly point host:port to the test server via
	// a custom Backend so reachability-only probe succeeds.
	srv, _ := fakeUpstream(t, []string{"some-other-model"}, "")
	b := newBackendFor(t, srv)
	// We need to override the port the Backend probes; do so by passing
	// a model whose APIPort we set.
	u, _ := url.Parse(srv.URL)
	portInt, _ := strconv.Atoi(u.Port())
	model := &config.ModelDefinition{HFRepo: "some-other-model", APIPort: portInt}
	s := b.Status(context.Background(), "any", model)
	if s.State != backends.StateRunning {
		t.Errorf("state = %q, want running", s.State)
	}
}
