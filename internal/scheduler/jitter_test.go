package scheduler

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

func TestJitterIsStableAndInsideWindow(t *testing.T) {
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, cst.Loc)
	a := nextFireKind(now, []int{9}, taskCheckin, 30)
	b := nextFireKind(now, []int{9}, taskCheckin, 30)
	if !a.Equal(b) {
		t.Fatalf("jitter must be stable: %v vs %v", a, b)
	}
	nominal := time.Date(2026, 9, 25, 9, 0, 0, 0, cst.Loc)
	if a.Before(nominal) || !a.Before(nominal.Add(30*time.Minute)) {
		t.Fatalf("offset %v outside [9:00, 9:30)", a)
	}
	if nextFireKind(now, []int{9}, taskCheckin, 0) != nominal {
		t.Fatal("jitter 0 must stay on the hour")
	}
	if nextFireKind(now, []int{9}, taskTravel, 30).Equal(a) {
		t.Fatal("different tasks should not share one offset")
	}
}

// TestScheduleHourIsCST 定时时点按北京时间解释，不随宿主时区漂移：
// UTC 主机上配 09 点，必须落在北京 09:00（= 01:00Z），而不是 UTC 09:00（= 北京 17:00）。
func TestScheduleHourIsCST(t *testing.T) {
	now := time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)  // 北京 10-21 10:00，当天 9 点已过
	want := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC) // 北京 10-22 09:00
	if got := nextFireKind(now, []int{9}, taskCheckin, 0); !got.Equal(want) {
		t.Fatalf("nextFireKind=%v want %v（北京 09:00）", got, want)
	}
}
