package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// makeupCell 一条 heatmap 单元格（date→score）。
type makeupCell struct {
	date  string
	score int
}

// makeupStub 模拟补签链路三个端点：heatmap / streak / makeup-cards/use。
// today 由响应给出（不依赖宿主机时钟），窗口判定因此可确定性断言。
type makeupStub struct {
	today      string // heatmap 的 today.date，缺省 2026-10-11
	cells      []makeupCell
	balance    int  // makeup_cards.balance
	useErr     bool // use 返回业务错误（上游拒绝）
	heatmapErr bool // heatmap 500

	heatmapHits atomic.Int32
	streakHits  atomic.Int32

	mu   sync.Mutex
	used []string // 实际提交的 target_date，按提交顺序
}

func (s *makeupStub) usedDates() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.used))
	copy(out, s.used)
	return out
}

func (s *makeupStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/heatmap":
			s.heatmapHits.Add(1)
			if s.heatmapErr {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			today := s.today
			if today == "" {
				today = "2026-10-11"
			}
			var b strings.Builder
			fmt.Fprintf(&b, `{"code":0,"data":{"today":{"date":"%s","score":2},"cells":[`, today)
			for i, c := range s.cells {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"date":"%s","score":%d}`, c.date, c.score)
			}
			b.WriteString(`]}}`)
			w.Write([]byte(b.String()))
		case "/activity/growth/streak":
			s.streakHits.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"makeup_cards":{"balance":%d,"max":4}}}`, s.balance)
		case "/activity/growth/makeup-cards/use":
			var body struct {
				Target string `json:"target_date"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.used = append(s.used, body.Target)
			s.mu.Unlock()
			if s.useErr {
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"date is not broken"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// makeupScheduler 构造只含一个账号的调度器，指向 stub。
func makeupScheduler(t *testing.T, stub *makeupStub) (*Scheduler, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), a
}

// 本月三种格子 + 两个边界：上月活跃（不许补）、今天 0 分（未结束，不许补）。
func makeupCells() []makeupCell {
	return []makeupCell{
		{"2026-09-29", 5},
		{"2026-10-01", 2}, {"2026-10-02", 2}, {"2026-10-03", 2}, {"2026-10-04", 2},
		{"2026-10-05", 0}, {"2026-10-06", 0}, {"2026-10-07", 0},
		{"2026-10-08", 2}, {"2026-10-09", 2}, {"2026-10-10", 2},
		{"2026-10-11", 0},
	}
}

// TestMakeupMissedFillsNearestFirst 三处漏签、两张卡：只补最近两天，且从近到远。
func TestMakeupMissedFillsNearestFirst(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 2}
	s, a := makeupScheduler(t, stub)

	s.makeupMissed(a)

	got := stub.usedDates()
	want := []string{"2026-10-07", "2026-10-06"}
	if len(got) != len(want) {
		t.Fatalf("补签提交=%v want %v（卡数封顶 + 只补本月漏签）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("补签提交=%v want %v（最近的优先）", got, want)
		}
	}
}

// TestMakeupMissedNoBrokenNoWrite 本月无漏签：一个写请求都不发。
func TestMakeupMissedNoBrokenNoWrite(t *testing.T) {
	cells := makeupCells()
	for i := range cells {
		if cells[i].score == 0 {
			cells[i].score = 2
		}
	}
	stub := &makeupStub{cells: cells, balance: 2}
	s, a := makeupScheduler(t, stub)

	s.makeupMissed(a)

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（无漏签）", got)
	}
}

// TestMakeupMissedNoCardNoWrite 有漏签但无补签卡：不发写请求。
func TestMakeupMissedNoCardNoWrite(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 0}
	s, a := makeupScheduler(t, stub)

	s.makeupMissed(a)

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（无卡）", got)
	}
}

// TestMakeupMissedStopsOnReject 上游拒绝（date is not broken）：立即停，不继续往前补。
// 注册前的日子同样是 0 分，无法与漏签区分，靠拒绝兜底——所以拒绝必须终止本轮。
func TestMakeupMissedStopsOnReject(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 3, useErr: true}
	s, a := makeupScheduler(t, stub)

	s.makeupMissed(a)

	got := stub.usedDates()
	if len(got) != 1 || got[0] != "2026-10-07" {
		t.Errorf("补签提交=%v want [2026-10-07]（拒绝即停，最新一天先试）", got)
	}
}

// TestMakeupMissedHeatmapErrorSilent heatmap 查询失败：静默返回，不发写请求。
func TestMakeupMissedHeatmapErrorSilent(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 2, heatmapErr: true}
	s, a := makeupScheduler(t, stub)

	s.makeupMissed(a) // 不应 panic

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（查询失败不猜）", got)
	}
}

// TestMakeupMissedRepeatedRunStopsAfterCardsUsed 幂等性护栏：卡用光后下一轮不再发写请求。
func TestMakeupMissedRepeatedRunStopsAfterCardsUsed(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 1}
	s, a := makeupScheduler(t, stub)

	s.makeupMissed(a)
	stub.balance = 0 // 上游已扣卡
	s.makeupMissed(a)

	got := stub.usedDates()
	if len(got) != 1 || got[0] != "2026-10-07" {
		t.Errorf("两轮补签提交=%v want [2026-10-07]（第二轮无卡不发写）", got)
	}
}
