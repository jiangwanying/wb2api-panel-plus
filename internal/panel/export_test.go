package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/pool"
)

// 账号导出：AuthDir 下 *.json 全部导出（cockpit 兼容字段），notes.txt / *.tmp 跳过。
func TestAccountsExport(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", `{"accessToken":"at-1","refreshToken":"rt-1","uid":"u1","nickname":"小明","expiresAt":1700000000,"domain":"https://copilot.tencent.com"}`)
	write("notes.txt", "not json")
	write("b.json.tmp", "atomic write leftover")

	p := New(Config{Version: "t", APIKey: "k", Pool: pool.New(""), AuthDir: dir})
	req := httptest.NewRequest("GET", "/panel/api/accounts/export", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var out struct {
		Count    int `json:"count"`
		Accounts []struct {
			UID          string `json:"uid"`
			Nickname     string `json:"nickname"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    int64  `json:"expires_at"`
			Realm        string `json:"realm"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || len(out.Accounts) != 1 {
		t.Fatalf("count=%d accounts=%d（notes.txt/.tmp 应被跳过）", out.Count, len(out.Accounts))
	}
	a := out.Accounts[0]
	if a.UID != "u1" || a.Nickname != "小明" || a.RefreshToken != "rt-1" {
		t.Fatalf("字段不符: %+v", a)
	}
	if a.ExpiresAt != 1700000000*1000 {
		t.Fatalf("expires_at 应为毫秒口径（import 会 /1000），得到 %d", a.ExpiresAt)
	}
	if a.Realm != "cn" {
		t.Fatalf("CN 域名应回落 realm=cn，得到 %q", a.Realm)
	}
}

// AuthDir 未配置（nil 部署形态）→ 501，而不是空数组假装成功。
func TestAccountsExportNoDir501(t *testing.T) {
	p := New(Config{Version: "t", APIKey: "k", Pool: pool.New("")})
	req := httptest.NewRequest("GET", "/panel/api/accounts/export", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 501 {
		t.Fatalf("code=%d want 501", rr.Code)
	}
}

// 与其他 /panel/api/* 同口径鉴权：无 key 必须 401（导出的是明文凭证）。
func TestAccountsExportRequiresAuth(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "t", APIKey: "k", Pool: pool.New(""), AuthDir: dir})
	req := httptest.NewRequest("GET", "/panel/api/accounts/export", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("code=%d want 401", rr.Code)
	}
}
