package upstream

import (
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersAPIKeyDoubleHeader 静态 API Key 的 chat 出站必须同时带
// Authorization 与 X-API-Key。
//
// 为什么双头：官方 CLI 对静态 Key 两个头都发，上游任一均可鉴权；但边缘网关/WAF 有
// 只看 X-API-Key 的变体，只发 Authorization 会被判「缺凭证」拦成 403——表现为
// 「这个 Key 网页能用、走网关就 403」。对齐官方形态即可覆盖两种判据。
func TestChatHeadersAPIKeyDoubleHeader(t *testing.T) {
	c := New()
	c.ChatBaseCN = "https://cn.example"
	c.BillingBaseCN = "https://cn.example"
	a := &auth.Auth{UID: "u1", AccessToken: "ck_abc123.secret"}
	req := httptest.NewRequest("POST", "https://cn.example/v1/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("Authorization"); got != "Bearer ck_abc123.secret" {
		t.Errorf("Authorization=%q", got)
	}
	if got := req.Header.Get("X-API-Key"); got != "ck_abc123.secret" {
		t.Errorf("X-API-Key=%q want 与 Authorization 同一个 Key", got)
	}
	// 静态 Key 没有企业身份：不得凭空注入企业头（上游会按企业上下文鉴权）。
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("静态 Key 不该带 X-Enterprise-Id：%q", got)
	}
}

// TestChatHeadersOAuthNoAPIKeyHeader 反例：OAuth token 不该带 X-API-Key。
// 多带一个非空但非 Key 形态的头可能被上游当成 API Key 去校验而拒绝。
func TestChatHeadersOAuthNoAPIKeyHeader(t *testing.T) {
	c := New()
	c.ChatBaseCN = "https://cn.example"
	a := &auth.Auth{UID: "u1", AccessToken: "eyJhbGciOiJSUzI1NiJ9.eyJ9.sig"}
	req := httptest.NewRequest("POST", "https://cn.example/v1/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-API-Key"); got != "" {
		t.Errorf("OAuth 账号不该带 X-API-Key：%q", got)
	}
	if got := req.Header.Get("Authorization"); got == "" {
		t.Error("OAuth 账号仍应带 Authorization")
	}
}

// TestChatHeadersAPIKeyNoRefreshToken 安全红线：静态 Key 的请求同样绝不携带
// X-Refresh-Token（它本来也没有 refreshToken，但这条断言防的是将来「顺手带上」）。
func TestChatHeadersAPIKeyNoRefreshToken(t *testing.T) {
	c := New()
	c.ChatBaseCN = "https://cn.example"
	a := &auth.Auth{UID: "u1", AccessToken: "ck_abc.secret", RefreshToken: "should-not-leak"}
	req := httptest.NewRequest("POST", "https://cn.example/v1/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Refresh-Token"); got != "" {
		t.Fatalf("绝不携带 refresh token：%q", got)
	}
}
