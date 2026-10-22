package pool

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestCorruptStateFileLogsAndKeepsFile 损坏/截断的 state.json 此前被静默丢弃：池子以
// 0 账号启动、credits/冷却/熔断全丢，日志里却一个字都没有（运维只看到"账号全没了"）。
// 原文件必须保留（供人工恢复），且必须留下痕迹。
func TestCorruptStateFileLogsAndKeepsFile(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":7`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	p := New(fp)
	defer p.Close()

	if _, ok := p.Status("u1"); ok {
		t.Fatal("损坏的 state.json 不应被当成有效状态加载")
	}
	if !strings.Contains(buf.String(), "state.json 不可用") {
		t.Fatalf("损坏状态被静默丢弃，日志=%q", buf.String())
	}
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("原文件应保留待人工恢复: %v", err)
	}
}

// TestFlushRetriesAfterFailedSave 落盘失败后 dirty 必须置回：此前 Swap(false) 在写盘前
// 执行，一败即永久搁置（磁盘恢复也不补写），这段内存变更要等到下次状态变更才碰巧落盘。
func TestFlushRetriesAfterFailedSave(t *testing.T) {
	dir := t.TempDir()
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 父路径是普通文件 → MkdirAll/WriteFile 必失败（root 也不可绕过）。
	p := New(filepath.Join(block, "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	if !p.dirty.Load() {
		t.Fatal("落盘失败后 dirty 应置回，否则磁盘恢复后也不会补写")
	}

	good := filepath.Join(t.TempDir(), "state.json")
	p.stateFp = good
	p.Flush()
	raw, err := os.ReadFile(good)
	if err != nil || !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("磁盘恢复后应补写成功: %v %s", err, raw)
	}
	if p.dirty.Load() {
		t.Fatal("成功落盘后 dirty 应清")
	}
}

// TestStateFileDropsExpiredCooldownFields 账号级软冷却按墙钟到期、reason/until/cool_kind
// 字段本身不清理（NoteSuccess 不碰），落盘时必须惰性清洗——否则过期 reason 永久留在
// state.json 并被重启原样拾回（issue #207-4；显示层已由 statusOf 门控挡住）。
func TestStateFileDropsExpiredCooldownFields(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Cooldown("u1", CoolSoft, -time.Minute, "429 rate limit") // 已到期，字段残留
	p.Cooldown("u2", CoolSoft, time.Hour, "429 rate limit")    // 仍在冷却

	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	if a := sf.Accounts["u1"]; a.Reason != "" || !a.Until.IsZero() || a.CoolKind != 0 {
		t.Fatalf("过期冷却不应落盘: %+v", a)
	}
	if a := sf.Accounts["u2"]; a.Reason != "429 rate limit" || a.Until.IsZero() || a.CoolKind != CoolSoft {
		t.Fatalf("冷却中的字段必须照常落盘: %+v", a)
	}

	// 禁用原因走同一个字段，不能被清洗掉（否则重启后看不到为什么死）。
	p.Disable("u1", "12153 session dead")
	p.mu.Lock()
	sf = p.stateOverviewLocked()
	p.mu.Unlock()
	if a := sf.Accounts["u1"]; !a.Disabled || a.Reason != "12153 session dead" {
		t.Fatalf("禁用原因必须落盘: %+v", a)
	}
}
