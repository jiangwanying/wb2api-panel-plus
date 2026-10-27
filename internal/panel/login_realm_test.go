package panel

import (
	"errors"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/loginsite"
)

// TestLoginEndpoints realm→端点映射：global 保持历史语义（workbuddy.ai），
// cn/空/非法值 → copilot.tencent.com（零回归兜底）。
func TestLoginEndpoints(t *testing.T) {
	cases := []struct {
		realm      string
		wantState  string
		wantOrigin string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=CLI", "https://www.workbuddy.ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
	}
	for _, c := range cases {
		t.Run(c.realm, func(t *testing.T) {
			st, tok, acct, origin := loginEndpoints(c.realm)
			if st != c.wantState {
				t.Errorf("realm=%q state=%q want %q", c.realm, st, c.wantState)
			}
			if origin != c.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", c.realm, origin, c.wantOrigin)
			}
			// token/account 端点必与 state 同 base。
			if c.realm == "global" {
				if tok[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" ||
					acct[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" {
					t.Errorf("global token/account endpoints should be workbuddy.ai: %q %q", tok, acct)
				}
			} else {
				if tok[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" ||
					acct[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" {
					t.Errorf("cn token/account endpoints should be copilot.tencent.com: %q %q", tok, acct)
				}
			}
		})
	}
}

// TestSiteOrDefault 覆盖面板侧站点归一化：空/未知回落 cn；intl 指向官方国际站
// （product.json externalDomain），workbuddy 指向旧桌面端。
func TestSiteOrDefault(t *testing.T) {
	cases := []struct {
		id        string
		wantID    string
		wantRealm string
		wantAPI   string
	}{
		{id: "", wantID: siteCN, wantRealm: "cn", wantAPI: "https://copilot.tencent.com"},
		{id: "cn", wantID: siteCN, wantRealm: "cn", wantAPI: "https://copilot.tencent.com"},
		{id: "intl", wantID: siteIntl, wantRealm: "global", wantAPI: "https://www.codebuddy.ai"},
		{id: "workbuddy", wantID: siteWorkBuddy, wantRealm: "global", wantAPI: "https://www.workbuddy.ai"},
		// 未知值不猜域。
		{id: "global", wantID: siteCN, wantRealm: "cn", wantAPI: "https://copilot.tencent.com"},
	}
	for _, c := range cases {
		s := siteOrDefault(c.id)
		if s.ID != c.wantID || s.Realm != c.wantRealm || s.API != c.wantAPI {
			t.Errorf("siteOrDefault(%q)={%s %s %s} want {%s %s %s}",
				c.id, s.ID, s.Realm, s.API, c.wantID, c.wantRealm, c.wantAPI)
		}
	}
}

// TestLoginCodeSemantics 覆盖官方登录码在面板侧的分类：11217/12151 继续轮询，
// 12005/11212/11216 立刻终态。这两类混为一谈时，「席位满」要白等满 loginTTL
// 才报超时，用户根本看不到真实原因。
func TestLoginCodeSemantics(t *testing.T) {
	for _, c := range []int{loginCodeRetryFetchToken, loginCodeRetryFetchAccount} {
		if !loginCodePending(c) {
			t.Errorf("code %d 应判 pending", c)
		}
		if loginCodeFatal(c) {
			t.Errorf("code %d 不应判终态", c)
		}
	}
	for _, c := range []int{loginCodeLicenseSeatLimit, loginCodeLicenseExpired, loginCodeTrialExpired} {
		if !loginCodeFatal(c) {
			t.Errorf("code %d 应判终态", c)
		}
		if loginCodePending(c) {
			t.Errorf("code %d 不应判 pending", c)
		}
	}
	// 码值本身锁死（防手滑改数字导致语义漂移）。
	if loginCodeRetryFetchToken != 11217 || loginCodeRetryFetchAccount != 12151 ||
		loginCodeLicenseSeatLimit != 12005 || loginCodeLicenseExpired != 11212 ||
		loginCodeTrialExpired != 11216 {
		t.Error("登录码常量与官方码表不一致")
	}
}

// TestLoginEnvelopeErrorAs 覆盖错误链：doJSON 返回的 *loginEnvelopeError 必须能被
// errors.As 取出 code——轮询分支靠它区分 pending / 终态，退化成字符串解析就脆了。
func TestLoginEnvelopeErrorAs(t *testing.T) {
	var err error = &loginEnvelopeError{Code: 12005, Msg: "seat limit", Status: 200}
	var envErr *loginEnvelopeError
	if !errors.As(err, &envErr) {
		t.Fatal("errors.As 未能取出 *loginEnvelopeError")
	}
	if envErr.Code != 12005 || envErr.Msg != "seat limit" {
		t.Errorf("解出 %+v，字段不符", envErr)
	}
	if got, want := err.Error(), "code=12005 msg=seat limit"; got != want {
		t.Errorf("Error()=%q want %q", got, want)
	}
}

// TestClientUAOfficial 覆盖 UA 与官方 CLI 版本同源（升级版本号时 UA/version 不能漂移）。
func TestClientUAOfficial(t *testing.T) {
	if clientUA != loginsite.ClientUA {
		t.Errorf("面板 UA=%q 与 loginsite.ClientUA=%q 不一致", clientUA, loginsite.ClientUA)
	}
	if loginsite.ClientVersion != "2.176.0" {
		t.Errorf("官方 CLI 版本变了（%s），请同步核对登录页参数与码表", loginsite.ClientVersion)
	}
}
