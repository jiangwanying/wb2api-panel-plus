package scheduler

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// TestSpreadDeterministic 同一天同一任务同一批账号 → 偏移不变（重启不重排）。
func TestSpreadDeterministic(t *testing.T) {
	uids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, cst.Loc)
	first := newAccountSpread(taskCheckin, uids, 30*time.Minute, 0, now)
	second := newAccountSpread(taskCheckin, uids, 30*time.Minute, 0, now)
	if first == nil || second == nil {
		t.Fatal("窗口 > 0 时应生成计划")
	}
	for _, uid := range uids {
		if !first.at[uid].Equal(second.at[uid]) {
			t.Fatalf("%s 偏移不稳定：%v vs %v", uid, first.at[uid], second.at[uid])
		}
	}
	// 换一天应换一批偏移：日期没进哈希就会全同。
	nextDay := now.AddDate(0, 0, 1)
	other := newAccountSpread(taskCheckin, uids, 30*time.Minute, 0, nextDay)
	same := 0
	for _, uid := range uids {
		if first.at[uid].Sub(now) == other.at[uid].Sub(nextDay) {
			same++
		}
	}
	if same == len(uids) {
		t.Fatal("跨天偏移全部相同，日期没进哈希")
	}
}

// TestSpreadRespectsMinGap 偏移撞车时靠后的被推开：同批任意相邻间隔 ≥ minGap，
// 且全部落在窗口内。
func TestSpreadRespectsMinGap(t *testing.T) {
	uids := make([]string, 10)
	for i := range uids {
		uids[i] = string(rune('a' + i))
	}
	window, gap := 30*time.Minute, 90*time.Second
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, cst.Loc)
	plan := newAccountSpread(taskTravel, uids, window, gap, now)

	offs := make([]time.Duration, 0, len(uids))
	for _, uid := range uids {
		off := plan.at[uid].Sub(now)
		if off < 0 || off >= window {
			t.Fatalf("%s 偏移 %v 越出窗口 %v", uid, off, window)
		}
		offs = append(offs, off)
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	for i := 1; i < len(offs); i++ {
		if d := offs[i] - offs[i-1]; d < gap {
			t.Fatalf("排序后相邻间隔 %v < %v", d, gap)
		}
	}
}

// TestSpreadDisabled 窗口 <= 0 与空池不生成计划；nil 计划 / 计划外账号的 wait 立即放行。
func TestSpreadDisabled(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, cst.Loc)
	if plan := newAccountSpread(taskCheckin, []string{"a"}, 0, 0, now); plan != nil {
		t.Fatal("窗口 0 应返回 nil")
	}
	if plan := newAccountSpread(taskCheckin, nil, time.Minute, 0, now); plan != nil {
		t.Fatal("空池应返回 nil")
	}
	var plan *accountSpread
	if !plan.wait(context.Background(), "a") {
		t.Fatal("nil 计划应立即放行")
	}
	// 池在本轮中途新增的账号不在计划里：立即执行，不睡。
	real := newAccountSpread(taskCheckin, []string{"a"}, time.Hour, 0, now)
	if !real.wait(context.Background(), "newcomer") {
		t.Fatal("计划外账号应立即放行")
	}
}

// TestSpreadManualEntryNeverSpreads 面板手动触发不受摊开影响（否则一次点击要等半小时）。
func TestSpreadManualEntryNeverSpreads(t *testing.T) {
	s := New(Config{AccountSpreadMinutes: 30, AccountSpreadMinGapSeconds: 60})
	if plan := s.spreadPlan(taskCheckin, false, time.Now()); plan != nil {
		t.Fatal("手动入口不应生成摊开计划")
	}
	if !s.spreadEnabled() {
		t.Fatal("窗口 > 0 时 spreadEnabled 应为 true")
	}
}
