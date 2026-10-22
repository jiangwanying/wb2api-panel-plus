package auth

import (
	"os"
	"testing"
	"time"
)

// TestIsAPIKey 静态 API Key 判定：只看 accessToken 前缀（ck_/pt_）。
//
// 为什么必须在前缀上判：auth 文件不持久化 tokenType（refresh 只认 OAuth 设备流，
// 静态 Key 无法刷新），前缀是落盘形态里唯一稳定、跨进程一致的判据。误判两个方向
// 都有实代价：判成普通号 → 每日对签到/成长端点空发请求、refresh 必然失败把好号冷却；
// 判太宽（比如把 ck 之后的任意串都算）→ 真实 OAuth token 被当成 Key，额度查询走错端点。
func TestIsAPIKey(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"ck_ 前缀（CodeBuddy 控制台签发）", "ck_abcd1234.secretpart", true},
		{"pt_ 前缀（官方 CLI 形态）", "pt_abcd1234.secretpart", true},
		{"裸 ck 无下划线", "ckabcd1234", false},
		{"大写前缀", "CK_abcd1234", false},
		{"JWT 形式 OAuth token", "eyJhbGciOiJSUzI1NiJ9.eyJ1aWQiOiJ1MSJ9.sig", false},
		{"空 token", "", false},
		{"前缀在中间", "Bearer ck_abcd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.token}
			if got := a.IsAPIKey(); got != c.want {
				t.Fatalf("token=%q IsAPIKey()=%v want %v", c.token, got, c.want)
			}
		})
	}

	// nil 接收者不得 panic：panel 侧会用临时 Auth 走判定，防御路径也可能传 nil。
	var nilAuth *Auth
	if nilAuth.IsAPIKey() {
		t.Fatal("nil Auth 不该被判为 API Key")
	}
}

// TestUsesEnterpriseBilling 额度查询分流判据：企业版与静态 API Key 都要走企业口径，
// 因为个人 get-user-resource 对二者恒返回空 Accounts。若这里的判据漏了 API Key，
// 静态 Key 账号的余额会恒为 0，积分保底把它当成耗尽号挡出选号。
func TestUsesEnterpriseBilling(t *testing.T) {
	cases := []struct {
		name string
		a    *Auth
		want bool
	}{
		{"个人 OAuth 号", &Auth{UID: "u1", AccessToken: "eyJ.x.y"}, false},
		{"企业版", &Auth{UID: "u1", AccessToken: "eyJ.x.y", EnterpriseID: "fn7x"}, true},
		{"静态 API Key", &Auth{UID: "u1", AccessToken: "ck_abc.def"}, true},
		{"企业版 + API Key", &Auth{UID: "u1", AccessToken: "pt_abc.def", EnterpriseID: "fn7x"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.UsesEnterpriseBilling(); got != c.want {
				t.Fatalf("UsesEnterpriseBilling()=%v want %v", got, c.want)
			}
		})
	}
}

// TestSupportsPersonalGrowth 个人成长体系门控：企业版与静态 API Key 都不具备，
// scheduler 五类任务与 panel 成长入口据此跳过（否则每日空发一堆注定失败的请求）。
func TestSupportsPersonalGrowth(t *testing.T) {
	cases := []struct {
		name string
		a    *Auth
		want bool
	}{
		{"个人号", &Auth{UID: "u1", AccessToken: "eyJ.x.y"}, true},
		{"企业版", &Auth{UID: "u1", AccessToken: "eyJ.x.y", EnterpriseID: "fn7x"}, false},
		{"静态 API Key", &Auth{UID: "u1", AccessToken: "ck_abc.def"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.SupportsPersonalGrowth(); got != c.want {
				t.Fatalf("SupportsPersonalGrowth()=%v want %v", got, c.want)
			}
			// 执行类门控与读类门控在本项目当前口径下必须一致（两者分开命名只为将来
			// 出现「读得见、记不上账」的账号类型时能单独改一处）。
			if got := c.a.SupportsPersonalEndpoints(); got != c.want {
				t.Fatalf("SupportsPersonalEndpoints()=%v want %v（应与 SupportsPersonalGrowth 同口径）", got, c.want)
			}
		})
	}
}

// TestNeedsRefreshAPIAKeyNever 静态 API Key 永不刷新。
//
// 这是整个接入里最容易踩的一处：Key 没有 expiresAt，若 NeedsRefresh 按「无过期即
// 需刷新」返回 true，请求路径会在每次发请求前先打 refresh 端点——而 refresh 只认
// OAuth 设备流，必然失败，于是这个完全健康的号会被当成坏号冷却/轮换。
func TestNeedsRefreshAPIAKeyNever(t *testing.T) {
	a := &Auth{UID: "u1", AccessToken: "ck_abc.def"} // ExpiresAt 零值
	if a.NeedsRefresh(time.Hour) {
		t.Fatal("静态 API Key 不该需要刷新（refresh 端点只认 OAuth 设备流）")
	}
	if a.NeedsRefresh(0) {
		t.Fatal("静态 API Key 在任何窗口下都不该需要刷新")
	}

	// 反例：普通 OAuth 号无 expiresAt 时仍要刷新（既有语义不能被这次改动带偏）。
	b := &Auth{UID: "u2", AccessToken: "eyJ.x.y"}
	if !b.NeedsRefresh(time.Hour) {
		t.Fatal("无 expiresAt 的 OAuth 账号仍应需要刷新")
	}
}

// TestAPIKeySavedAndReloaded 静态 Key 落盘/重载后判定不变：前缀判据只看 token 本身，
// 不依赖 Parse 时补齐的任何字段——否则重启后账号会「变回」普通号。
func TestAPIKeySavedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	orig := &Auth{UID: "apikey-abc", AccessToken: "ck_abc.secret", Nickname: "key-1"}
	orig.FilePath = dir + "/workbuddy-apikey-abc.json"
	if _, realm := orig.BackfillRealm(); realm == "" {
		t.Fatal("realm 应被补全（静态 Key 恒 CN）")
	}
	if err := orig.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(orig.FilePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.IsAPIKey() {
		t.Fatalf("重载后应仍判为 API Key：%+v", parsed)
	}
	if parsed.RefreshTokenValue() != "" {
		t.Fatalf("静态 Key 不该带 refreshToken：%q", parsed.RefreshTokenValue())
	}
	if parsed.NeedsRefresh(time.Hour) {
		t.Fatal("重载后仍不该需要刷新")
	}
}
