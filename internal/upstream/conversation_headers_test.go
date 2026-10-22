// conversation_headers_test.go chat 出站渠道校验三件套的不变量。
//
// 上游按 X-Conversation-ID / Request-ID / Message-ID 是否**齐全**做渠道校验，缺任一头
// 即 400 code=11-128 "conversation id missing"（官方客户端 chat 与 speech 都带）。
// 零值 ChatMeta 的调用方（夜猫子对话、面板模型任务）与手搓头的桌面指纹路径是历史缺口，
// 这里钉住「chat 出站必发三件套」，防止将来某条路径又悄悄留空。
package upstream

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestChatHeadersAlwaysSendConversationTrio(t *testing.T) {
	c := &Client{}
	trio := []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"}
	cases := []struct {
		name string
		meta ChatMeta
	}{
		{"零值 ChatMeta", ChatMeta{}},
		{"只有轮级主键", ChatMeta{ConversationRequestID: "req-1"}},
		{"客户端声明会话", ChatMeta{ConversationID: "conv-client"}},
		{"只有缓存键来源", ChatMeta{CacheKeyID: "sess-1"}},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.ChatHeaders(req, &auth.Auth{UID: "uid-abcdef123", AccessToken: "at"}, "", tc.meta)
		for _, h := range trio {
			if req.Header.Get(h) == "" {
				t.Errorf("%s: 缺 %s（上游渠道校验要求三件套齐全）", tc.name, h)
			}
		}
	}
}

// TestChatHeadersConversationIDPrefersClientValue 透传优先：客户端声明的会话值不得被改写
// （该头同时是前缀缓存槽来源，issue #169）。
func TestChatHeadersConversationIDPrefersClientValue(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{UID: "uid-abcdef123", AccessToken: "at"}, "", ChatMeta{ConversationID: "conv-from-client"})
	if got := req.Header.Get("X-Conversation-ID"); got != "conv-from-client" {
		t.Errorf("X-Conversation-ID=%q want 原值透传", got)
	}
}
