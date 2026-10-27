package reqlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 造一个归档目录：每天一个 requests-*.jsonl；markUnreadable 文件里塞一行超过
// scanner 缓冲（1MB）的超长行——read() 只要碰它就会返回 scanner 错误，用这个
// 副作用证明「该文件被短路、根本没被读」。
func writeArchive(t *testing.T, dir string, days map[string][]string, unreadable map[string]bool) {
	t.Helper()
	for day, events := range days {
		var b strings.Builder
		for _, e := range events {
			b.WriteString(e + "\n")
		}
		if unreadable[day] {
			b.WriteString(`{"filler":"` + strings.Repeat("x", 2<<20) + `"}` + "\n")
		}
		if err := os.WriteFile(filepath.Join(dir, "requests-"+day+".jsonl"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// cst8 期望值与夹具都用固定 +08:00：夹具里的事件写的就是 +08:00，期望值若跟宿主
// 时区走，在 UTC 主机（CI runner）上会差 8 小时而假红——本机跑绿只是因为本机
// 恰好是 CST。
var cst8 = time.FixedZone("CST", 8*3600)

func newArchiveForTest(t *testing.T, dir string) *archiveWriter {
	return &archiveWriter{cfg: Config{Enabled: true, Dir: dir, RetentionDays: 30}, done: make(chan struct{})}
}

// TestReadSkipsFilesOutsideWindow 带时间窗的查询跳过窗口外的整档文件：
// 窗口外的文件即使内容不可解析也照样成功（证明没被读），返回事件全部落在窗口内。
func TestReadSkipsFilesOutsideWindow(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir,
		map[string][]string{
			"2026-01-01": {`{"time":"2026-01-01T10:00:00+08:00","path":"/v1/chat/completions","status":200,"ok":true,"outcome":"completed"}`},
			"2026-01-02": {`{"time":"2026-01-02T10:00:00+08:00","path":"/v1/chat/completions","status":200,"ok":true,"outcome":"completed"}`},
			"2026-01-03": {`{"time":"2026-01-03T10:00:00+08:00","path":"/v1/chat/completions","status":200,"ok":true,"outcome":"completed"}`},
		},
		map[string]bool{"2026-01-01": true}, // 1 号档不可解析：被读过必报错
	)
	w := newArchiveForTest(t, dir)
	rows, err := w.read(100, Filter{From: time.Date(2026, 1, 2, 12, 0, 0, 0, cst8)})
	if err != nil {
		t.Fatalf("窗口外文件不应被读取（读取会撞上不可解析行）: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("窗口内应恰有 1 条事件, got %d", len(rows))
	}
	if !rows[0].Time.Equal(time.Date(2026, 1, 3, 10, 0, 0, 0, cst8)) {
		t.Fatalf("应只返回窗口内事件, got %v", rows[0].Time)
	}
}

// TestReadStopsWhenLimitCollected 「最近 N 条」从最新档往回收集，集满即停：
// 更早的档（含不可解析的）不再被读，结果与全量扫描一致。
func TestReadStopsWhenLimitCollected(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir,
		map[string][]string{
			"2026-01-01": {`{"time":"2026-01-01T10:00:00+08:00","status":200,"ok":true,"outcome":"completed"}`},
			"2026-01-02": {`{"time":"2026-01-02T10:00:00+08:00","status":200,"ok":true,"outcome":"completed"}`},
			"2026-01-03": {
				`{"time":"2026-01-03T08:00:00+08:00","status":200,"ok":true,"outcome":"completed"}`,
				`{"time":"2026-01-03T09:00:00+08:00","status":200,"ok":true,"outcome":"completed"}`,
			},
		},
		map[string]bool{"2026-01-01": true, "2026-01-02": true},
	)
	w := newArchiveForTest(t, dir)
	rows, err := w.read(2, Filter{})
	if err != nil {
		t.Fatalf("集满后不应再读更早的不可解析档: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// read 返回最新在前。
	if !rows[0].Time.Equal(time.Date(2026, 1, 3, 9, 0, 0, 0, cst8)) {
		t.Fatalf("第一条应为全局最新事件, got %v", rows[0].Time)
	}
	if !rows[1].Time.Equal(time.Date(2026, 1, 3, 8, 0, 0, 0, cst8)) {
		t.Fatalf("第二条应为次新事件, got %v", rows[1].Time)
	}
}

// TestReadFullScanUnchanged 无窗口、limit 大于总量时行为与全量扫描一致
// （含跨档排序与「最新在前」的返回顺序）。
func TestReadFullScanUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir,
		map[string][]string{
			"2026-01-01": {`{"time":"2026-01-01T10:00:00+08:00","status":200,"ok":true,"outcome":"completed"}`},
			"2026-01-03": {`{"time":"2026-01-03T10:00:00+08:00","status":200,"ok":true,"outcome":"completed"}`},
			"2026-01-02": {`{"time":"2026-01-02T10:00:00+08:00","status":200,"ok":true,"outcome":"completed"}`},
		},
		nil,
	)
	w := newArchiveForTest(t, dir)
	rows, err := w.read(100, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	want := []time.Time{
		time.Date(2026, 1, 3, 10, 0, 0, 0, cst8),
		time.Date(2026, 1, 2, 10, 0, 0, 0, cst8),
		time.Date(2026, 1, 1, 10, 0, 0, 0, cst8),
	}
	for i, wt := range want {
		if !rows[i].Time.Equal(wt) {
			t.Errorf("rows[%d]=%v want %v", i, rows[i].Time, wt)
		}
	}
}
