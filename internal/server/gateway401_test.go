package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/upstream"
)

// 网关 401 的误杀防护：先 refresh 验证，再决定是否计 session dead。
//
// 背景（20261024 实案）：两个账号各累计 3 次「网关 401 invalid_token」（APISIX 的
// HTML 页）被禁用。但人工 revive 后立刻恢复正常（连续 3 次请求全 200）——说明那批
// 401 是网关侧瞬时拦截，不是 token 失效。按 401 次数禁用会误杀健康账号。
//
// 判据：token 真失效时 refresh 端点必然拒绝；瞬时拦截时 refresh 仍成功。故只有
// refresh 失败才该计死。

const gateway401HTML = "<html>\r\n<head><title>401 Authorization Required</title></head>\r\n" +
	"<body><center><h1>401 Authorization Required</h1></center>" +
	"<hr><center>openresty</center></body></html>"

// refreshAwareUpstream 记录上游收到的请求类型（chat / refresh），并分别给出响应。
//
// 为什么不用 newFakeUpstream：它只按 Authorization 头回固定响应，无法区分
// 「这次是 chat 还是 refresh」——而本测试的核心正是「chat 401 后去 refresh 验证」。
type refreshAwareUpstream struct {
	mu       sync.Mutex
	chatN    int
	refreshN int
	// refreshOK 为 true 时 refresh 返回成功（模拟瞬时拦截），false 时返回失败（真失效）。
	refreshOK bool
}

func (u *refreshAwareUpstream) client() *upstream.Client {
	return &upstream.Client{
		// ChatBaseCN 必须设：doRefresh 用它拼 refresh 端点，空值会让请求 URL 无 host。
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			path := r.URL.Path
			u.mu.Lock()
			// 真实路径：<chatBase>/v2/plugin/auth/token/refresh（见 doRefresh）
			isRefresh := strings.Contains(path, "/refresh")
			if isRefresh {
				u.refreshN++
			} else {
				u.chatN++
			}
			chatN, ok := u.chatN, u.refreshOK
			u.mu.Unlock()

			reply := func(status int, ct, body string) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": []string{ct}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			if isRefresh {
				if ok {
					return reply(200, "application/json", `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":7200}}`)
				}
				return reply(400, "application/json", `{"error":"invalid_grant"}`)
			}
			// chat 永远回网关 401
			_ = chatN
			return reply(401, "text/html", gateway401HTML)
		})},
	}
}

func (u *refreshAwareUpstream) counts() (chat, refresh int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chatN, u.refreshN
}

func newGateway401Handler(u *refreshAwareUpstream) *Handler {
	p := testPoolWith(&auth.Auth{
		UID: "gw1", AccessToken: "at-gw1", RefreshToken: "rt-gw1", ExpiresAt: 9999999999,
	})
	return NewHandler(Config{Pool: p, Upstream: u.client(), MaxRotate: 1})
}

// TestGateway401TransientNotDisabled ⭐ 核心：refresh 成功 → 401 判为瞬时拦截，
// **不得禁用账号**，且 session dead 计数被清零。
func TestGateway401TransientNotDisabled(t *testing.T) {
	up := &refreshAwareUpstream{refreshOK: true}
	h := newGateway401Handler(up)
	const uid = "gw1"

	// 连续 3 次网关 401（若按旧逻辑，第 3 次就会禁用）。
	for i := 0; i < 3; i++ {
		doFallbackChat(h, "deepseek-v4.1-flash")
	}

	if st, ok := h.cfg.Pool.Status(uid); ok && st.Disabled {
		t.Fatalf("refresh 成功时不应禁用账号（第 %d 次 401 后已被禁用）", up.chatN)
	}
	if _, refreshN := up.counts(); refreshN == 0 {
		t.Fatal("未观察到 refresh 验证请求 —— 说明 401 分支没走 VerifySessionAlive")
	}
}

// TestGateway401RealDeadStillDisables refresh 也失败 → token 确实失效，
// 连续达阈值仍应禁用（不因本次修改而放过真死号）。
func TestGateway401RealDeadStillDisables(t *testing.T) {
	up := &refreshAwareUpstream{refreshOK: false}
	h := newGateway401Handler(up)
	const uid = "gw1"

	for i := 0; i < 3; i++ {
		doFallbackChat(h, "deepseek-v4.1-flash")
	}

	st, ok := h.cfg.Pool.Status(uid)
	if !ok {
		t.Fatal("账号应仍在池中（禁用不等于删除）")
	}
	if !st.Disabled {
		t.Fatalf("refresh 也失败时，连续 3 次 401 应禁用账号，实际 disabled=%v", st.Disabled)
	}
}
