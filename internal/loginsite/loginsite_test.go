package loginsite

import (
	"net/url"
	"testing"
)

// TestFor 覆盖站点归一化：已知 id 精确命中，空/未知回落国内站（老调用零回归）。
func TestFor(t *testing.T) {
	cases := []struct {
		id        string
		wantID    string
		wantRealm string
		wantAPI   string
	}{
		{id: "", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "cn", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "CN", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "intl", wantID: SiteIntl, wantRealm: RealmGlobal, wantAPI: "https://www.codebuddy.ai"},
		{id: "workbuddy", wantID: SiteWorkBuddy, wantRealm: RealmGlobal, wantAPI: "https://www.workbuddy.ai"},
		// 未知值不猜域，回落国内站。
		{id: "global", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "codebuddy.ai", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
	}
	for _, c := range cases {
		s := For(c.id)
		if s.ID != c.wantID || s.Realm != c.wantRealm || s.API != c.wantAPI {
			t.Errorf("For(%q)={id:%s realm:%s api:%s} want {id:%s realm:%s api:%s}",
				c.id, s.ID, s.Realm, s.API, c.wantID, c.wantRealm, c.wantAPI)
		}
	}
}

// TestForRealm 覆盖 realm → 站点映射：global 保持历史语义（旧桌面端 workbuddy.ai），
// 官方国际站必须显式 SiteIntl——这是「老脚本零回归 + 新站点可选中」的分界。
func TestForRealm(t *testing.T) {
	cases := []struct {
		realm   string
		wantID  string
		wantAPI string
	}{
		{realm: "", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"},
		{realm: "cn", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"},
		{realm: "global", wantID: SiteWorkBuddy, wantAPI: "https://www.workbuddy.ai"},
		{realm: "GLOBAL", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"}, // 大小写不敏感未覆盖：按未知回落
	}
	for _, c := range cases {
		s := ForRealm(c.realm)
		if s.ID != c.wantID || s.API != c.wantAPI {
			t.Errorf("ForRealm(%q)={id:%s api:%s} want {id:%s api:%s}", c.realm, s.ID, s.API, c.wantID, c.wantAPI)
		}
	}
}

// TestEndpoints 覆盖三端点拼接：必须同域且 platform=CLI。Origin 与 API 同域是
// 国际站的约束（跨域 Origin 会被上游按非法请求拒绝）；国内站是**已知例外**——
// API 在 copilot.tencent.com、Origin 在 www.codebuddy.cn（与官方客户端一致）。
func TestEndpoints(t *testing.T) {
	for _, id := range []string{SiteCN, SiteIntl, SiteWorkBuddy} {
		s := For(id)
		state, token, account, origin := s.Endpoints()
		if want := s.API + "/v2/plugin/auth/state?platform=CLI"; state != want {
			t.Errorf("%s state=%q want %q", id, state, want)
		}
		if want := s.API + "/v2/plugin/auth/token?state="; token != want {
			t.Errorf("%s token=%q want %q", id, token, want)
		}
		if want := s.API + "/v2/plugin/login/account?state="; account != want {
			t.Errorf("%s account=%q want %q", id, account, want)
		}
		apiHost := mustHost(t, s.API)
		originHost := mustHost(t, origin)
		if id == SiteCN {
			// 国内站跨域是既定形态，锁死具体值防被「统一同域」误改。
			if apiHost != "copilot.tencent.com" || originHost != "www.codebuddy.cn" {
				t.Errorf("cn 站点域被改动：api=%s origin=%s", apiHost, originHost)
			}
			continue
		}
		if apiHost != originHost {
			t.Errorf("%s Origin 与 API 不同域：api=%s origin=%s", id, apiHost, originHost)
		}
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

// TestCodeSemantics 覆盖官方登录码分类：只有 11217/12151 是 pending，
// 12005/11212/11216 是终态。混类会让「席位满」白等满 TTL 才报超时。
func TestCodeSemantics(t *testing.T) {
	pending := []int{CodeRetryFetchToken, CodeRetryFetchAccount}
	for _, c := range pending {
		if !CodePending(c) {
			t.Errorf("CodePending(%d)=false want true", c)
		}
		if CodeFatal(c) {
			t.Errorf("CodeFatal(%d)=true want false（pending 不是终态）", c)
		}
	}
	fatal := []int{CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired}
	for _, c := range fatal {
		if !CodeFatal(c) {
			t.Errorf("CodeFatal(%d)=false want true", c)
		}
		if CodePending(c) {
			t.Errorf("CodePending(%d)=true want false（终态不该继续轮询）", c)
		}
	}
	// 其它码两类都不属（调用方按「可重试错误」处理）。
	for _, c := range []int{0, 10001, 11102, 14051, 99999} {
		if CodePending(c) || CodeFatal(c) {
			t.Errorf("code %d 不该被归入 pending 或 fatal", c)
		}
	}
}

// TestDecorateAuthURL 覆盖授权链接参数补齐（对齐官方 appendAuthUrlParams）：
// 每个站点都补 version；ww_device_nonce 只对 platform=workbuddy 且 nonce 非空时补。
func TestDecorateAuthURL(t *testing.T) {
	const base = "https://example.com/login?platform=CLI&state=abc"

	// 所有站点都补 version。
	for _, id := range []string{SiteCN, SiteIntl, SiteWorkBuddy} {
		got := DecorateAuthURL(base, For(id), "")
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", id, got, err)
		}
		if v := u.Query().Get("version"); v != ClientVersion {
			t.Errorf("%s version=%q want %q", id, v, ClientVersion)
		}
		// 原有参数不得丢。
		if s := u.Query().Get("state"); s != "abc" {
			t.Errorf("%s state=%q want abc（原有参数被破坏）", id, s)
		}
		if p := u.Query().Get("platform"); p != "CLI" {
			t.Errorf("%s platform=%q want CLI", id, p)
		}
	}

	// 只有 workbuddy 站带 nonce。
	if got := DecorateAuthURL(base, For(SiteWorkBuddy), "nonce-1"); !containsParam(t, got, "ww_device_nonce", "nonce-1") {
		t.Errorf("workbuddy 站未补 ww_device_nonce: %s", got)
	}
	for _, id := range []string{SiteCN, SiteIntl} {
		if got := DecorateAuthURL(base, For(id), "nonce-1"); containsParam(t, got, "ww_device_nonce", "") {
			t.Errorf("%s 站不该补 ww_device_nonce: %s", id, got)
		}
	}
	// nonce 为空时 workbuddy 站也不补（不伪造设备指纹）。
	if got := DecorateAuthURL(base, For(SiteWorkBuddy), ""); containsParam(t, got, "ww_device_nonce", "") {
		t.Errorf("nonce 为空时不该补 ww_device_nonce: %s", got)
	}

	// 上游已带 version 时不覆盖（尊重上游口径）。
	if got := DecorateAuthURL(base+"&version=9.9.9", For(SiteCN), ""); !containsParam(t, got, "version", "9.9.9") {
		t.Errorf("已有 version 被覆盖: %s", got)
	}

	// 非法 URL 原样返回（不阻断登录）。
	if got := DecorateAuthURL("://bad", For(SiteCN), ""); got != "://bad" {
		t.Errorf("非法 URL 未原样返回: %q", got)
	}
}

func containsParam(t *testing.T, raw, key, want string) bool {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	v, ok := u.Query()[key]
	if want == "" {
		return ok
	}
	return ok && len(v) == 1 && v[0] == want
}

// TestBaseForDomain 覆盖按 auth 文件 domain 选上游 base：国际域两套主机名并存，
// 用 realm 决定 base 会把一半账号打到非签发域。
func TestBaseForDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   string
	}{
		{domain: "www.codebuddy.ai", want: BaseOfficial},
		{domain: "codebuddy.ai", want: BaseOfficial},
		{domain: "api.codebuddy.ai", want: BaseOfficial},
		{domain: "www.workbuddy.ai", want: BaseWorkBuddy},
		{domain: "workbuddy.ai", want: BaseWorkBuddy},
		{domain: "WWW.CODEBUDDY.AI", want: BaseOfficial}, // 大小写不敏感
		{domain: " www.codebuddy.ai ", want: BaseOfficial},
		// 国内域/空/非法 → 空串（调用方回落配置或默认，不猜域）。
		{domain: "", want: ""},
		{domain: "www.codebuddy.cn", want: ""},
		{domain: "copilot.tencent.com", want: ""},
		{domain: "evil-codebuddy.ai", want: ""}, // 后缀匹配不能误吞
	}
	for _, c := range cases {
		if got := BaseForDomain(c.domain); got != c.want {
			t.Errorf("BaseForDomain(%q)=%q want %q", c.domain, got, c.want)
		}
	}
}

// TestValidID 覆盖站点标识校验（CLI flag 解析用）。
func TestValidID(t *testing.T) {
	for _, ok := range []string{"cn", "intl", "workbuddy", "CN", " Intl "} {
		if !ValidID(ok) {
			t.Errorf("ValidID(%q)=false want true", ok)
		}
	}
	for _, bad := range []string{"", "global", "codebuddy", "www.codebuddy.ai"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q)=true want false", bad)
		}
	}
}

// TestClientUA 覆盖 UA 与 version 同源（升级版本号时两处不能漂移）。
func TestClientUA(t *testing.T) {
	want := "CLI/" + ClientVersion + " CodeBuddy/" + ClientVersion
	if ClientUA != want {
		t.Errorf("ClientUA=%q want %q", ClientUA, want)
	}
}
