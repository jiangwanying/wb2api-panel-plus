package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
)

// TestBillingBaseForFollowsAuthRealm 计费域选择收敛到 auth.IsGlobal 口径
// （显式 realm=global 或 domain 落 workbuddy.ai 家族），不再手抄推断。
func TestBillingBaseForFollowsAuthRealm(t *testing.T) {
	cases := []struct {
		name string
		a    *auth.Auth
		want string
	}{
		{"显式 global", parseAuth(t, `{"realm":"global","accessToken":"t"}`), billingBaseGlobal},
		{"domain 家族", parseAuth(t, `{"accessToken":"t","domain":"www.workbuddy.ai"}`), billingBaseGlobal},
		{"CN", parseAuth(t, `{"accessToken":"t","domain":"www.codebuddy.cn"}`), billingBaseCN},
		{"缺省", parseAuth(t, `{"accessToken":"t"}`), billingBaseCN},
	}
	for _, c := range cases {
		if got := billingBaseFor(c.a); got != c.want {
			t.Errorf("%s: billingBaseFor = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestCreditLoadsFlatAuthFile 扁平形手写 auth 文件（无 auth/account 嵌套）也
// 能解出 token——此前 credit 自定义结构只认嵌套形，扁平形 token 解析为空、
// 报误导性的 "no accessToken"。
func TestCreditLoadsFlatAuthFile(t *testing.T) {
	dir := t.TempDir()
	flat := filepath.Join(dir, "workbuddy-flat.json")
	if err := os.WriteFile(flat, []byte(`{"accessToken":"at-1","refreshToken":"rt-1","uid":"u1","expiresAt":9999999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(flat)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("扁平形解析失败: %v", err)
	}
	if a.AccessToken != "at-1" || a.UID != "u1" {
		t.Fatalf("flat parse: token=%q uid=%q", a.AccessToken, a.UID)
	}
}

func parseAuth(t *testing.T, js string) *auth.Auth {
	t.Helper()
	a, err := auth.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
