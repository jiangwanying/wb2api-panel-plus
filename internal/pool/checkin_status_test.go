// checkin_status_test.go 钉住「今日已签到」观测：NoteCheckinDone 标记当日，
// statusOf 输出 CheckinDone（跨零点自然过期），持久化往返不丢。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/cst"
)

func TestNoteCheckinDoneMarksToday(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if st, _ := p.Status("u1"); st.CheckinDone {
		t.Fatal("未签到账号不应显示已签")
	}
	p.NoteCheckinDone("u1")
	st, ok := p.Status("u1")
	if !ok || !st.CheckinDone {
		t.Fatalf("u1 标记后应显示已签: %+v ok=%v", st, ok)
	}
	if st, _ := p.Status("u2"); st.CheckinDone {
		t.Fatal("u2 未标记，不应显示已签")
	}
	// 未知 uid 不 panic、不影响其他账号。
	p.NoteCheckinDone("no-such-uid")
}

func TestCheckinDonePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinDone("u1")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"last_checkin_day"`) {
		t.Fatalf("state.json 缺少 last_checkin_day:\n%s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); !st.CheckinDone {
		t.Fatal("重启后当日已签状态丢失")
	}
}

// TestCheckinDoneExpiredYesterday 昨日标记不算今日。
//
// 「今天」必须用 cst.Day 取，不能用宿主的 time.Now().Format：生产侧判定是
// state.go 的 `e.lastCheckinDay == cst.Day(now)`（固定 UTC+8），而 CI runner 跑在
// UTC——UTC 16:00–23:59 期间两者差一天，替换串找不到，state 里留着的仍是「今天」，
// 断言就必然失败（binaries #317 实测，此前所有 run 都落在 16:00 UTC 之前才一直绿）。
func TestCheckinDoneExpiredYesterday(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinDone("u1")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	yesterday := cst.Day(time.Now().AddDate(0, 0, -1))
	raw = []byte(strings.Replace(string(raw), cst.Day(time.Now()), yesterday, 1))
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.CheckinDone {
		t.Fatal("昨日签到记录不应显示为今日已签")
	}
}

// TestSetNicknamePersists（issue #94 同步）：昵称更新写入 auths 文件并往返无损；
// 未变化不写盘；空昵称/未知 uid 拒绝。
func TestSetNicknamePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", Nickname: "旧名字", AccessToken: "at", FilePath: fp}
	p := New("")
	p.Add(a)
	if !p.SetNickname("u1", "新名字") {
		t.Fatal("昵称变化应返回 true")
	}
	reloaded, err := auth.Parse(mustRead(t, fp))
	if err != nil || reloaded.Nickname != "新名字" {
		t.Fatalf("回写后昵称=%q err=%v, want 新名字", reloaded.Nickname, err)
	}
	if p.SetNickname("u1", "新名字") {
		t.Fatal("未变化不应再写盘")
	}
	if p.SetNickname("u1", "") || p.SetNickname("no-such", "x") {
		t.Fatal("空昵称/未知 uid 应拒绝")
	}
	st, _ := p.Status("u1")
	if st.Nickname != "新名字" {
		t.Fatalf("Status 昵称=%q, want 新名字", st.Nickname)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestCheckinStatusIsHostTimezoneIndependent 把 time.Local 换成 UTC 再跑一遍上面的
// 断言：CI runner 是 UTC、开发机多是 CST，本地永远绿而 CI 在 UTC 16:00 之后必红
// （binaries #317 实测：checkin_status_test.go:74 昨日记录被判成今日已签）。
// 钉住「这条断言与宿主时区无关」，避免有人把 cst.Day 改回 time.Now().Format。
func TestCheckinStatusIsHostTimezoneIndependent(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	defer func() { time.Local = old }()
	if time.Now().Format("2006-01-02") == cst.Day(time.Now()) {
		t.Log("当前 UTC 与 CST 同日：此用例此刻只验证不崩，区分度要等 UTC 16:00 之后")
	}
	TestCheckinDoneExpiredYesterday(t)
}
