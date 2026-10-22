package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// apiKeyPanel 造一个只认企业额度端点的假上游，用于「添加 API Key 账号」测试。
// calls 记录打到的路径，供断言「探测确实走了企业口径、且没有多余请求」。
func apiKeyPanel(t *testing.T, respBody string) (*Panel, *pool.Pool, *[]string) {
	t.Helper()
	var calls []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc2(func(r *http.Request) (*http.Response, error) {
			calls = append(calls, r.URL.Path)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(respBody)),
			}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := pool.New("")
	pn := New(Config{Version: "test", Pool: p, Upstream: up, AuthDir: t.TempDir()})
	return pn, p, &calls
}

type roundTripFunc2 func(*http.Request) (*http.Response, error)

func (f roundTripFunc2) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// enterpriseUsageOK 企业口径额度答复：上游的 credit 是**已用**量，limitNum 是分配上限，
// 剩余 = limit - used（个人口径的 credit 反而是剩余，两套语义不同，取错方向会让
// 面板显示「剩 120」而实际只剩 380）。
const enterpriseUsageOK = `{"code":0,"msg":"OK","data":{"credit":120,"limitNum":500,"cycleEndTime":"2026-11-01 00:00:00"}}`

func postAPIKey(t *testing.T, pn *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/add_apikey", strings.NewReader(body)))
	return rec
}

// TestAddAPIKeySavesAndLoadsIntoPool 正常路径：探测成功 → 落盘（无 refreshToken）→
// 入池且额度为探测值。落盘形态必须与 OAuth 账号一致，否则 LoadDir 重启后读不出来。
func TestAddAPIKeySavesAndLoadsIntoPool(t *testing.T) {
	pn, p, calls := apiKeyPanel(t, enterpriseUsageOK)
	rec := postAPIKey(t, pn, `{"access_token":"ck_abc123.secretpart","nickname":"我的Key"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if resp["api_key"] != true {
		t.Errorf("应标记 api_key=true：%v", resp)
	}
	uid, _ := resp["uid"].(string)
	if uid != "apikey-abc123" {
		t.Fatalf("uid=%q want apikey-abc123（由 key 的 id 段派生）", uid)
	}

	// 探测必须走企业口径端点（静态 Key 的个人资源包恒为空）。
	if len(*calls) == 0 || !strings.Contains((*calls)[0], "get-enterprise-user-usage") {
		t.Fatalf("探测应走 get-enterprise-user-usage，实际 %v", *calls)
	}

	// 入池且额度写入。
	a := p.AuthByUID(uid)
	if a == nil {
		t.Fatal("添加后账号应已入池")
	}
	if !a.IsAPIKey() {
		t.Errorf("入池账号应判为 API Key：%+v", a)
	}
	if a.RefreshTokenValue() != "" {
		t.Errorf("静态 Key 不该带 refreshToken：%q", a.RefreshTokenValue())
	}
	st, ok := p.Status(uid)
	if !ok {
		t.Fatal("状态查询不到该账号")
	}
	if !st.APIKey {
		t.Errorf("状态应带 api_key 标记（面板据此隐藏签到/任务按钮）：%+v", st)
	}
	// credit 是已用量（120），故剩余 380 / 总额 500。
	if st.Credits != 380 || st.CreditsTotal != 500 {
		t.Errorf("额度 credits=%d total=%d want 380/500", st.Credits, st.CreditsTotal)
	}

	// 落盘 → 重载：重启后账号仍然可用（这是「添加成功」的最终判据）。
	dir := pn.cfg.AuthDir
	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(auths) != 1 || !auths[0].IsAPIKey() || auths[0].UID != uid {
		t.Fatalf("落盘账号不可重载：%+v", auths)
	}
}

// TestAddAPIKeyRejectsBadInput 三条拒绝路径都不落盘：前缀不对 / uid 非法 / 探测失败。
// 落盘了坏 Key 的后果是选号侧反复选中它、每次都要冷却换号，表象是「网关时好时坏」。
func TestAddAPIKeyRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非 Key 前缀", `{"access_token":"eyJhbGciOiJSUzI1NiJ9.eyJ9.sig"}`},
		{"空 token", `{"access_token":"  "}`},
		{"uid 路径穿越", `{"access_token":"ck_abc.def","uid":"../../etc/passwd"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pn, p, _ := apiKeyPanel(t, enterpriseUsageOK)
			rec := postAPIKey(t, pn, c.body)
			if rec.Code != 400 {
				t.Fatalf("code=%d body=%s（want 400）", rec.Code, rec.Body)
			}
			if n := len(p.List()); n != 0 {
				t.Fatalf("被拒绝的请求不该入池：%d 个账号", n)
			}
			entries, _ := os.ReadDir(pn.cfg.AuthDir)
			if len(entries) != 0 {
				t.Fatalf("被拒绝的请求不该落盘：%v", entries)
			}
		})
	}
}

// 上游探测失败（无效 Key → 401 业务信封）必须 400 且不落盘。
func TestAddAPIKeyInvalidKeyNotPersisted(t *testing.T) {
	pn, p, _ := apiKeyPanel(t, `{"code":401,"msg":"invalid api key","data":null}`)
	rec := postAPIKey(t, pn, `{"access_token":"ck_dead.dead","nickname":""}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s（want 400）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "API Key") {
		t.Errorf("错误文案应指明是 API Key 的问题：%s", rec.Body)
	}
	if n := len(p.List()); n != 0 {
		t.Fatalf("探测失败不该入池：%d", n)
	}
	entries, _ := os.ReadDir(pn.cfg.AuthDir)
	if len(entries) != 0 {
		t.Fatalf("探测失败不该落盘：%v", entries)
	}
}

// 显式 uid 走白名单校验；派生 uid 时长超限要截断（拼文件名前的兜底）。
func TestResolveAPIKeyUID(t *testing.T) {
	if uid, err := resolveAPIKeyUID("my-key_1", "ck_abc.def"); err != nil || uid != "my-key_1" {
		t.Fatalf("显式 uid 应原样使用：%q %v", uid, err)
	}
	if _, err := resolveAPIKeyUID("bad uid", "ck_abc.def"); err == nil {
		t.Fatal("含空格的 uid 应被拒")
	}
	if uid, err := resolveAPIKeyUID("", "pt_zzz.999"); err != nil || uid != "apikey-zzz" {
		t.Fatalf("派生 uid=%q err=%v want apikey-zzz", uid, err)
	}
	// 无 id 段（前缀后直接是点）→ 必须报错而不是造出 "apikey-"。
	if _, err := resolveAPIKeyUID("", "ck_.secret"); err == nil {
		t.Fatal("无 id 段应报错，请显式指定 uid")
	}
	long := "ck_" + strings.Repeat("a", 200) + ".s"
	uid, err := resolveAPIKeyUID("", long)
	if err != nil {
		t.Fatalf("超长 id 应截断而非报错：%v", err)
	}
	if len(uid) > apiKeyUIDMaxLen {
		t.Fatalf("派生 uid 长度=%d 超过上限 %d", len(uid), apiKeyUIDMaxLen)
	}
}
