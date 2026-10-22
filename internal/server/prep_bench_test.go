package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// BenchmarkChatPrep measures the full inbound handler path up to and including
// the outbound request preparation. The fake upstream returns a fixed SSE stream,
// so upstream network latency is excluded; the delta before/after reflects local
// JSON work only.
func BenchmarkChatPrep(b *testing.B) {
	for _, variant := range []string{"agent", "plain"} {
		for _, size := range []int{50 << 10, 300 << 10, 1 << 20} {
			body := benchChatBody(size, variant == "agent")
			up := &upstreamClientForBench{}
			p := testPoolWith(&auth.Auth{UID: "bench", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{
				Pool:       p,
				Upstream:   up.client(),
				PromptMode: "passthrough",
			})
			b.Run(fmt.Sprintf("%s/%dKB", variant, len(body)>>10), func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						b.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
				}
			})
		}
	}
}

// upstreamClientForBench returns a client whose transport answers with sseOK.
type upstreamClientForBench struct{}

func (u *upstreamClientForBench) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func benchChatBody(target int, withTools bool) []byte {
	obj := map[string]any{
		"model":           "cn:glm-5.2",
		"stream":          true,
		"conversation_id": "bench-conv",
		"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("系统提示词占位。", 400)},
		},
	}
	if withTools {
		obj["tools"] = []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       "read_file",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}}
	}
	chunk := strings.Repeat("这是一段用于填充上下文的中文内容，模拟长对话。", 20)
	for {
		obj["messages"] = append(obj["messages"].([]any), map[string]any{"role": "user", "content": chunk})
		raw, err := json.Marshal(obj)
		if err != nil {
			panic(err)
		}
		if len(raw) >= target {
			return raw
		}
	}
}
