package router

import "testing"

// E2: "<base>-<egress>" model names where <base> is a tool-proxy model resolve
// to <base> + an Egress spec forwarded to the tool proxy as X-Egress.
func TestResolveModel_EgressAlias(t *testing.T) {
	rt := newTestRouter(t, nil)

	cases := []struct {
		model      string
		wantID     string
		wantEgress string
	}{
		{"research-se", "nemotron-3-super", "se"},          // alias base + country
		{"nemotron-3-super-us", "nemotron-3-super", "us"},  // model-id base (has hyphens) + country
		{"research-us-atl", "nemotron-3-super", "us-atl"},  // multi-part egress spec
		{"thinker-eu", "nemotron-3-super", "eu"},           // another alias + EU group
		{"research-any", "nemotron-3-super", "any"},        // random
	}
	for _, c := range cases {
		res, err := rt.resolveModel(c.model, false)
		if err != nil {
			t.Errorf("resolveModel(%q): %v", c.model, err)
			continue
		}
		if res.ModelID != c.wantID || res.Egress != c.wantEgress || !res.ViaToolProxy {
			t.Errorf("resolveModel(%q) = id=%q egress=%q viaTP=%v; want id=%q egress=%q viaTP=true",
				c.model, res.ModelID, res.Egress, res.ViaToolProxy, c.wantID, c.wantEgress)
		}
	}

	// A non-tool-proxy base must NOT become an egress alias (coder routes direct).
	if _, err := rt.resolveModel("coder-se", false); err == nil {
		t.Error("coder-se: expected unknown-model error (coder isn't tool_proxy)")
	}
	// forceDirect endpoints (/v1/embeddings etc.) must not do egress-alias resolution.
	if _, err := rt.resolveModel("research-se", true); err == nil {
		t.Error("research-se with forceDirect: expected unknown-model error")
	}
	// A plain bogus model still errors.
	if _, err := rt.resolveModel("totally-bogus", false); err == nil {
		t.Error("totally-bogus: expected unknown-model error")
	}
	// A plain known model is unaffected (no egress).
	if res, err := rt.resolveModel("research", false); err != nil || res.Egress != "" {
		t.Errorf("research: egress=%q err=%v; want empty egress, no error", res.Egress, err)
	}
}
