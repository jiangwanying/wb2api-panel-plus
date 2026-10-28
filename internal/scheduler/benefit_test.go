package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/pool"
	"github.com/jiangwanying/wb2api-panel-plus/internal/upstream"
)

// giftStub 记录新手礼包两端点的调用次数（body 为空时按「无礼包」应答）。
// 其余路径一律 404（成长/连登链路在此处自然失败早退，不影响礼包断言）。
type giftStub struct {
	mu              sync.Mutex
	paths           map[string]int
	giftStatusBody  string
	giftClaimBody   string
	giftClaimCalls  int
	giftStatusCalls int
}

func (c *giftStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		if c.paths == nil {
			c.paths = map[string]int{}
		}
		c.paths[r.URL.Path]++
		c.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-gift-claimed"):
			c.mu.Lock()
			c.giftStatusCalls++
			body := c.giftStatusBody
			c.mu.Unlock()
			if body == "" {
				body = `{"code":0,"msg":"OK","data":{"active":false,"claimed":false,"credit_num":0}}`
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/claim-gift"):
			c.mu.Lock()
			c.giftClaimCalls++
			body := c.giftClaimBody
			c.mu.Unlock()
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
}

// giftScheduler 构造只含一个 CN 个人号的调度器（礼包链路的最小上下文）。
func giftScheduler(t *testing.T, stub *giftStub) *Scheduler {
	t.Helper()
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up})
}

// TestStreakBonusGlobalRunsBillingBenefits 国际号在连登链路里仍要跑 billing 域礼包
// （D4 门控只覆盖 CN 成长体系；一并跳过会静默漏领国际区活动 —— 线上实测 workbuddy.ai
// 上礼包端点返回 200）。
func TestStreakBonusGlobalRunsBillingBenefits(t *testing.T) {
	stub := &giftStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":1000}}`,
		giftClaimBody:  `{"code":0,"msg":"OK","data":{"credit":1000}}`,
	}
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	g := &auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	auth.BackfillRealmFor(g, "global")
	p.Add(g)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	New(Config{Pool: p, Upstream: up}).RunStreakBonusNow()

	if stub.giftStatusCalls != 1 || stub.giftClaimCalls != 1 {
		t.Errorf("国际号礼包 status=%d claim=%d want 1/1", stub.giftStatusCalls, stub.giftClaimCalls)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for path := range stub.paths {
		if strings.Contains(path, "/activity/") {
			t.Errorf("国际号不该发起 CN 成长域调用: %s（D4 门控仍在）", path)
		}
	}
}

// TestStreakBonusSkipsGiftClaimWhenAlreadyClaimed 已领过礼包时**不得**再发起领取：
// 旧写法盲调 ClaimGift，上游对「已领过」返回 200 且无 credit 字段，于是每个已领号
// 每天打一行假的「🎊 新手礼包 +0c」。本用例在盲领代码上必红（会命中 /claim-gift）。
func TestStreakBonusSkipsGiftClaimWhenAlreadyClaimed(t *testing.T) {
	stub := &giftStub{giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":true,` +
		`"claimed_at":"2026-09-24 18:15:15","active":true,"credit_num":1500,` +
		`"validity_period":1,"start_time":"2026-04-01 00:00:00","end_time":"2026-10-31 23:59:59"}}`}
	s := giftScheduler(t, stub)
	s.RunStreakBonusNow()

	if stub.giftStatusCalls != 1 {
		t.Errorf("礼包状态查询次数=%d want 1", stub.giftStatusCalls)
	}
	if stub.giftClaimCalls != 0 {
		t.Errorf("已领过却发起了 %d 次领取（盲领回归）", stub.giftClaimCalls)
	}
}

// TestStreakBonusClaimsGiftWhenPending 待领时恰好领一次，并带上有效期。
func TestStreakBonusClaimsGiftWhenPending(t *testing.T) {
	stub := &giftStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,` +
			`"credit_num":1500,"validity_period":1,"end_time":"2026-10-31 23:59:59"}}`,
		giftClaimBody: `{"code":0,"msg":"OK","data":{"credit":1500}}`,
	}
	s := giftScheduler(t, stub)
	s.RunStreakBonusNow()

	if stub.giftStatusCalls != 1 || stub.giftClaimCalls != 1 {
		t.Errorf("status=%d claim=%d want 1/1", stub.giftStatusCalls, stub.giftClaimCalls)
	}
}

// TestStreakBonusGiftStatusFailureSkipsClaim 礼包状态无待领时不领（旧盲领会照领）。
func TestStreakBonusGiftStatusFailureSkipsClaim(t *testing.T) {
	stub := &giftStub{giftStatusBody: `{"code":0,"msg":"OK","data":{"active":false,"claimed":false,"credit_num":0}}`}
	s := giftScheduler(t, stub)
	s.RunStreakBonusNow()
	if stub.giftClaimCalls != 0 {
		t.Errorf("无待领礼品却领了 %d 次", stub.giftClaimCalls)
	}
}

// TestBenefitClaimWithoutCreditReportsZero 回执不填 credit 时按「本次到账 0」如实记录。
//
// 这条断言在 2026-10-20 被反转：此前断言「回落到状态额度」，理由是「钱已到账、
// 显示 0 是在说反话」。但用 credit-history 一验就穿了——18 点后 340 条「🎊 +500c」日志，
// 而全量 2000 条余额流水里 delta=+500 出现 **0 次**（同期只有签到 +10/+60）。
// 真相：上游对「已领过」回 code=0 且不带 credit 字段 = **本次没有发放**。
// 现在严格只认回执，0 就是 0，绝不拿「可领额度」冒充「已到账」。
func TestBenefitClaimWithoutCreditReportsZero(t *testing.T) {
	stub := &giftStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":1000,"end_time":"2026-10-31 23:59:59"}}`,
		giftClaimBody:  `{"code":0,"msg":"OK","data":{}}`,
	}
	s := giftScheduler(t, stub)

	a := s.cfg.Pool.AuthByUID("u1")
	if got := s.claimGiftQuiet(a); got != 0 {
		t.Errorf("回执无 credit 应记 0（本次到账 0），实得 %d —— 这正是假 +1000c 的来源", got)
	}
}

// TestGiftGuardSkipsRepeatClaim 同一期（同 uid + 同 end_time）只 claim 一次。
//
// 背景：上游 status 端点的 claimed 字段**恒为 false**（已领过也回 false），Pending() 恒真
// → 旧代码每次签到都把全池号重复 claim 一遍，白打请求却永远拿不到新发放。
func TestGiftGuardSkipsRepeatClaim(t *testing.T) {
	stub := &giftStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":1000,"end_time":"2026-10-31 23:59:59"}}`,
		giftClaimBody:  `{"code":0,"msg":"OK","data":{"credit":1000}}`,
	}
	s := giftScheduler(t, stub)
	a := s.cfg.Pool.AuthByUID("u1")

	if got := s.claimGiftQuiet(a); got != 1000 {
		t.Fatalf("首发应到账 1000，实得 %d", got)
	}
	if got := s.claimGiftQuiet(a); got != 0 {
		t.Errorf("同一期重复调用应返回 0（已被闸门拦下），实得 %d", got)
	}
	if stub.giftClaimCalls != 1 {
		t.Errorf("gift claim 调用次数=%d want 1（闸门未生效）", stub.giftClaimCalls)
	}
}

// TestGiftGuardResetsOnNewPeriod end_time 变了（上游新开一期）必须重新尝试。
func TestGiftGuardResetsOnNewPeriod(t *testing.T) {
	stub := &giftStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":1000,"end_time":"2026-10-31 23:59:59"}}`,
		giftClaimBody:  `{"code":0,"msg":"OK","data":{"credit":1000}}`,
	}
	s := giftScheduler(t, stub)
	a := s.cfg.Pool.AuthByUID("u1")

	s.claimGiftQuiet(a)
	stub.mu.Lock()
	stub.giftStatusBody = `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":800,"end_time":"2026-11-30 23:59:59"}}`
	stub.mu.Unlock()
	s.claimGiftQuiet(a)

	if stub.giftClaimCalls != 2 {
		t.Errorf("换期后应重新 claim，累计调用 %d 次（期望 2）", stub.giftClaimCalls)
	}
}

// TestBenefitClaimCreditWinsOverStatus 回执有 credit 时以回执为准（兜底不得覆盖真实回执）。
func TestBenefitClaimCreditWinsOverStatus(t *testing.T) {
	stub := &giftStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":1000}}`,
		giftClaimBody:  `{"code":0,"msg":"OK","data":{"credit":300}}`, // 上游按实际发放，可少于可领额度
	}
	s := giftScheduler(t, stub)
	if got := s.claimGiftQuiet(s.cfg.Pool.AuthByUID("u1")); got != 300 {
		t.Errorf("want 回执值 300，实得 %d", got)
	}
}
