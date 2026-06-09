package gpu

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeReader struct {
	mu    sync.Mutex
	calls int
	info  Info
	err   error
}

func (f *fakeReader) Read(_ context.Context) (Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.info, f.err
}

func (f *fakeReader) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeReader) set(info Info, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info, f.err = info, err
}

func TestCachedServesWithinTTL(t *testing.T) {
	f := &fakeReader{info: Info{TotalVRAMGB: 16}}
	c := Cached(f, 50*time.Millisecond)

	got, err := c.Read(context.Background())
	if err != nil || got.TotalVRAMGB != 16 {
		t.Fatalf("cold read = %v, %v; want {16}, nil", got, err)
	}
	if f.Calls() != 1 {
		t.Fatalf("cold read should probe once, got %d", f.Calls())
	}

	// Repeated reads within the TTL must hit the cache (no new probes).
	for i := 0; i < 5; i++ {
		if got, _ = c.Read(context.Background()); got.TotalVRAMGB != 16 {
			t.Fatalf("cached read = %v; want {16}", got)
		}
	}
	if f.Calls() != 1 {
		t.Fatalf("reads within TTL should not re-probe, got %d calls", f.Calls())
	}
}

func TestCachedRefreshesAfterTTL(t *testing.T) {
	f := &fakeReader{info: Info{TotalVRAMGB: 16}}
	c := Cached(f, 20*time.Millisecond)
	c.Read(context.Background()) // prime: 16

	f.set(Info{TotalVRAMGB: 24}, nil)
	time.Sleep(30 * time.Millisecond) // let the entry go stale

	// Stale-while-revalidate: the first post-expiry read returns the old
	// value immediately and triggers a background refresh.
	if got, _ := c.Read(context.Background()); got.TotalVRAMGB != 16 {
		t.Fatalf("stale read = %v; want the previous {16}", got)
	}
	// The refreshed value should land shortly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if g, _ := c.Read(context.Background()); g.TotalVRAMGB == 24 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("background refresh never produced the new value {24}")
}

func TestCachedKeepsLastGoodOnError(t *testing.T) {
	f := &fakeReader{info: Info{TotalVRAMGB: 16}}
	c := Cached(f, 10*time.Millisecond)
	c.Read(context.Background()) // good: 16

	f.set(Info{}, errors.New("xpu-smi boom"))
	time.Sleep(20 * time.Millisecond) // expire

	// A probe failure must not blank the last good snapshot.
	if got, err := c.Read(context.Background()); err != nil || got.TotalVRAMGB != 16 {
		t.Fatalf("on stale+error read = %v, %v; want last-good {16}, nil", got, err)
	}
	time.Sleep(30 * time.Millisecond) // allow the background (failing) refresh to run
	if got, err := c.Read(context.Background()); err != nil || got.TotalVRAMGB != 16 {
		t.Fatalf("error refresh blanked the cache: %v, %v", got, err)
	}
}
