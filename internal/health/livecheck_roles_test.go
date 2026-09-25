package health

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

func TestLiveCheckRoleMembers(t *testing.T) {
	reg, err := config.LoadBytes([]byte(`
roles:
  chat:
    candidates: [lms/qwen3-8b, lms/qwen3-typo]
  tools:
    require: {capabilities: [tool_calling]}
    candidates: [lms/qwen3-8b, corp/gpt-x]
models: {}
discovery:
  - {api_base: "http://lms.example/v1", prefix: lms/, adopt: all}
  - {api_base: "https://corp.example/v1", prefix: corp/, adopt: all, capabilities: [text, tool_calling]}
`))
	if err != nil {
		t.Fatal(err)
	}
	list := func(_ context.Context, root, _, _ string) (Listing, error) {
		switch root {
		case "http://lms.example":
			return Listing{"qwen3-8b": {ID: "qwen3-8b"}}, nil
		default:
			return Listing{"gpt-x": {ID: "gpt-x"}}, nil
		}
	}
	diags := LiveCheck(context.Background(), reg, "", func(string) string { return "" }, list, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	var got []string
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "role-member") {
			got = append(got, d.Code+" "+d.Subject+": "+d.Message)
		}
	}
	joined := strings.Join(got, "\n")
	if len(got) != 2 ||
		!strings.Contains(joined, `role-member-not-listed chat: member lms/qwen3-typo: lms/ (http://lms.example/v1) does not list "qwen3-typo"`) ||
		!strings.Contains(joined, `role-member-capability tools: member lms/qwen3-8b lacks required capability "tool_calling"`) {
		t.Fatalf("diagnostics:\n%s", joined)
	}
}
