package router

import (
	"testing"
	"time"
)

func TestTokTracker(t *testing.T) {
	tr := newTokTracker()
	base := time.Now()
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }

	// Engine-reported response_token/s wins.
	tr.record("m", f(107.3), i(500), 5000, base)
	if v, ok := tr.get("m", base); !ok || v != 107.3 {
		t.Errorf("engine tps: got (%v,%v), want (107.3,true)", v, ok)
	}

	// Falls back to completion/latency when the engine reports no rate.
	tr.record("m", nil, i(300), 3000, base.Add(time.Second)) // 300 / 3s = 100
	if v, ok := tr.get("m", base.Add(time.Second)); !ok || v != 100 {
		t.Errorf("fallback tps: got (%v,%v), want (100,true)", v, ok)
	}

	// No usable data (embeddings/errors/zero tokens) -> previous reading holds.
	tr.record("m", nil, i(0), 3000, base.Add(2*time.Second))
	tr.record("m", nil, nil, 0, base.Add(2*time.Second))
	if v, ok := tr.get("m", base.Add(2*time.Second)); !ok || v != 100 {
		t.Errorf("held tps: got (%v,%v), want (100,true)", v, ok)
	}

	// Expires after the TTL.
	if _, ok := tr.get("m", base.Add(time.Second+tokStatTTL+time.Second)); ok {
		t.Errorf("after TTL: want no value")
	}

	// Unknown model and nil receiver are safe no-ops.
	if _, ok := tr.get("nope", base); ok {
		t.Errorf("unknown model: want no value")
	}
	var nilT *tokTracker
	nilT.record("x", f(1), i(1), 1, base)
	if _, ok := nilT.get("x", base); ok {
		t.Errorf("nil tracker: want no value")
	}
}
