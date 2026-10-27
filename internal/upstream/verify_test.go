package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
)

// VerifySessionByRefresh 的三分支判据。
//
// 为什么值得测：网关 401 的「封禁 / 瞬时拦截 / 登录失效」三种成因，响应体完全一样
// （APISIX HTML 页），只能靠 refresh 的结果区分。判错的代价是把健康号永久禁用
// （本仓踩过两次：chat 路径 issue #66、面板探活）。

// verifyProbe 构造一个只处理 refresh 端点的假上游。
type verifyProbe struct {
	status int
	body   string
	ct     string
}

func (v verifyProbe) client() *Client {
	return &Client{
		ChatBaseCN: "https://fake.test",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: v.status,
				Header:     http.Header{"Content-Type": []string{v.ct}},
				Body:       io.NopCloser(strings.NewReader(v.body)),
			}, nil
		})},
	}
}

func newAuthForVerify() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
}

// TestVerifyAliveOnRefreshOK refresh 成功 → VerifyAlive（会话活，401 只是那次被拦）。
func TestVerifyAliveOnRefreshOK(t *testing.T) {
	c := verifyProbe{200, `{"code":0,"data":{"accessToken":"new","refreshToken":"new-rt","expiresIn":7200}}`, "application/json"}.client()
	if got := c.VerifySessionByRefresh(newAuthForVerify()); got != VerifyAlive {
		t.Fatalf("verdict = %v, want VerifyAlive", got)
	}
}

// TestVerifyDeadOnAppReject refresh 被应用层明确拒绝（invalid_grant）→ VerifyDead。
func TestVerifyDeadOnAppReject(t *testing.T) {
	c := verifyProbe{400, `{"error":"invalid_grant"}`, "application/json"}.client()
	if got := c.VerifySessionByRefresh(newAuthForVerify()); got != VerifyDead {
		t.Fatalf("verdict = %v, want VerifyDead", got)
	}
}

// TestVerifyInconclusiveOnGateway401 ⭐ 关键分支：refresh 端点**自己也回 401 HTML**
// （边缘拦截）时不得判死 —— 那是「拦截」不是「判词」。
//
// 这条是两次误杀的同一个根因：把 refresh 的失败一律当死号，等于在同一层再杀一次。
func TestVerifyInconclusiveOnGateway401(t *testing.T) {
	html := "<html>\r\n<head><title>401 Authorization Required</title></head>\r\n<hr><center>openresty</center></html>"
	c := verifyProbe{401, html, "text/html"}.client()
	if got := c.VerifySessionByRefresh(newAuthForVerify()); got != VerifyInconclusive {
		t.Fatalf("verdict = %v, want VerifyInconclusive（refresh 自己也被拦，没有判词）", got)
	}
}

// TestVerifyInconclusiveOn5xx 上游 5xx 不是 token 失效的证据。
func TestVerifyInconclusiveOn5xx(t *testing.T) {
	c := verifyProbe{503, "service unavailable", "text/plain"}.client()
	if got := c.VerifySessionByRefresh(newAuthForVerify()); got != VerifyInconclusive {
		t.Fatalf("verdict = %v, want VerifyInconclusive", got)
	}
}

// TestVerifyInconclusiveOn429 限流也不是 token 失效。
func TestVerifyInconclusiveOn429(t *testing.T) {
	c := verifyProbe{429, `{"error":"rate limit"}`, "application/json"}.client()
	if got := c.VerifySessionByRefresh(newAuthForVerify()); got != VerifyInconclusive {
		t.Fatalf("verdict = %v, want VerifyInconclusive", got)
	}
}

// TestSessionDeadButUsable 判据只看 AT 是否还有效，不看上游文案/azp（issue #83）。
func TestSessionDeadButUsable(t *testing.T) {
	future := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: time.Now().Add(26 * 24 * time.Hour).Unix()}
	if !SessionDeadButUsable(future) {
		t.Error("AT 还剩 26 天 → 应判定仍可用")
	}
	expired := &auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 1}
	if SessionDeadButUsable(expired) {
		t.Error("AT 已过期 → 不可用，应计 session dead")
	}
	noExpiry := &auth.Auth{UID: "u3", AccessToken: "at"} // ExpiresAt=0：NeedsRefresh 为 true
	if SessionDeadButUsable(noExpiry) {
		t.Error("无过期时间 → 按需刷新语义视为不可用")
	}
	if SessionDeadButUsable(nil) {
		t.Error("nil 应判 false")
	}
}

// TestVerifyInconclusiveWithoutRefreshToken 无 RT 时无从求证，不得盲判死。
func TestVerifyInconclusiveWithoutRefreshToken(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	c := verifyProbe{200, "{}", "application/json"}.client()
	if got := c.VerifySessionByRefresh(a); got != VerifyInconclusive {
		t.Fatalf("verdict = %v, want VerifyInconclusive（无 RT 无法求证）", got)
	}
}
