package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// compStub 记录补偿两端点的调用次数，并按 statusCode/statusBody 回固定形态。
// 其余路径一律 404（成长/连登链路在此处自然失败早退，不影响补偿断言）。
type compStub struct {
	mu          sync.Mutex
	paths       map[string]int
	statusCode  int
	statusBody  string
	claimBody   string
	claimCalls  int
	statusCalls int
	// 新手礼包（与补偿同结构、不同端点）：body 为空时按「无礼包」应答。
	giftStatusBody  string
	giftClaimBody   string
	giftClaimCalls  int
	giftStatusCalls int
}

func (c *compStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		if c.paths == nil {
			c.paths = map[string]int{}
		}
		c.paths[r.URL.Path]++
		c.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/compensation-status"):
			c.mu.Lock()
			c.statusCalls++
			code, body := c.statusCode, c.statusBody
			c.mu.Unlock()
			if code != 0 {
				http.Error(w, body, code)
				return
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/claim-compensation"):
			c.mu.Lock()
			c.claimCalls++
			body := c.claimBody
			c.mu.Unlock()
			_, _ = w.Write([]byte(body))
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

func (c *compStub) counts() (status, claim int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusCalls, c.claimCalls
}

// compScheduler 构造只含一个 CN 个人号的调度器（补偿链路的最小上下文）。
func compScheduler(t *testing.T, stub *compStub) *Scheduler {
	t.Helper()
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up})
}

// TestStreakBonusSkipsClaimWhenNotPending 无待领时**不得**发起领取——这是本改动的核心：
// 旧写法是无条件盲领，于是「无补偿 / 已领过」也打了假的「🎊 补偿领取 +0c」成功行
// （线上一个月 20 行全是 +0c）。本用例在旧代码上必红（盲领会命中 /claim-compensation）。
func TestStreakBonusSkipsClaimWhenNotPending(t *testing.T) {
	stub := &compStub{statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":false,"credit_num":0}}`}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	status, claim := stub.counts()
	if status != 1 {
		t.Errorf("状态查询次数=%d want 1（每号每天一次）", status)
	}
	if claim != 0 {
		t.Errorf("无待领却发起了 %d 次领取（盲领回归）", claim)
	}
}

// TestStreakBonusClaimsWhenPending 待领时恰好领一次。
func TestStreakBonusClaimsWhenPending(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{"credit":500}}`,
	}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	status, claim := stub.counts()
	if status != 1 || claim != 1 {
		t.Errorf("status=%d claim=%d want 1/1", status, claim)
	}
}

// TestStreakBonusStatusFailureDoesNotClaim 状态查询失败（5xx）时不领取、不重试，
// 且不得影响后续链路（本函数返回后 streakBonusAccount 继续跑连登/兑换）。
func TestStreakBonusStatusFailureDoesNotClaim(t *testing.T) {
	stub := &compStub{statusCode: http.StatusInternalServerError, statusBody: `{"code":500,"msg":"boom"}`}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	status, claim := stub.counts()
	if status != 1 {
		t.Errorf("状态查询次数=%d want 1（失败不重试）", status)
	}
	if claim != 0 {
		t.Errorf("状态未知时不该领取，实际 %d 次", claim)
	}
}

// TestStreakBonusClaimFailureNotRetried 领取失败只发一次（不重试、不掩盖）。
func TestStreakBonusClaimFailureNotRetried(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500}}`,
		claimBody:  `{"code":500,"msg":"boom"}`,
	}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	if _, claim := stub.counts(); claim != 1 {
		t.Errorf("领取次数=%d want 1（失败不重试）", claim)
	}
}

// compensationScheduler 构造含五类账号的调度器（池顺序 = UID 升序）：
// a-cn 普通 / b-paused 暂停选号 / c-disabled 禁用 / d-enterprise 企业版 / e-global 国际区。
func compensationScheduler(t *testing.T, stub *compStub) *Scheduler {
	t.Helper()
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	mk := func(uid string) *auth.Auth {
		return &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	}
	a, b, c, d, e := mk("a-cn"), mk("b-paused"), mk("c-disabled"), mk("d-enterprise"), mk("e-global")
	d.EnterpriseID = "ent-1"
	auth.BackfillRealmFor(e, "global")
	for _, x := range []*auth.Auth{a, b, c, d, e} {
		p.Add(x)
	}
	p.Pause("b-paused")
	p.Disable("c-disabled", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up})
}

// TestCompensationRowsGatesAndKeepsPoolOrder 门控与保序：企业版/禁用跳过，paused 与
// **global 都不跳过**（补偿是 billing 域、按 realm 走 base，国际号实测 200——见
// compSkipReason 注释）；跳过的账号也出行（带原因）且不发上游请求；查询阶段绝不领取。
func TestCompensationRowsGatesAndKeepsPoolOrder(t *testing.T) {
	stub := &compStub{statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`}
	s := compensationScheduler(t, stub)
	rows := s.CompensationRows()

	wantOrder := []string{"a-cn", "b-paused", "c-disabled", "d-enterprise", "e-global"}
	if len(rows) != len(wantOrder) {
		t.Fatalf("行数=%d want %d", len(rows), len(wantOrder))
	}
	for i, uid := range wantOrder {
		if rows[i].UID != uid {
			t.Errorf("第 %d 行 uid=%s want %s（必须按池顺序）", i, rows[i].UID, uid)
		}
	}
	byUID := map[string]CompensationRow{}
	for _, r := range rows {
		byUID[r.UID] = r
	}
	for _, uid := range []string{"c-disabled", "d-enterprise"} {
		if !byUID[uid].Skip || byUID[uid].SkipReason == "" {
			t.Errorf("%s 应跳过并给出原因: %+v", uid, byUID[uid])
		}
	}
	for _, uid := range []string{"b-paused", "e-global"} {
		if byUID[uid].Skip {
			t.Errorf("%s 不该跳过补偿: %+v", uid, byUID[uid])
		}
	}
	if byUID["a-cn"].Error != "" || !byUID["a-cn"].Active {
		t.Errorf("普通号应查到活动状态: %+v", byUID["a-cn"])
	}
	status, claim := stub.counts()
	if status != 3 {
		t.Errorf("状态查询次数=%d want 3（a-cn / b-paused / e-global；被门控的不可发请求）", status)
	}
	if claim != 0 {
		t.Errorf("只读查询不得领取，实际 %d 次", claim)
	}
}

// TestCompensateOneClaimsOnlyPending 逐号领取：待领才领、门控号直接返回跳过、不重复领。
func TestCompensateOneClaimsOnlyPending(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{"credit":500}}`,
	}
	s := compensationScheduler(t, stub)

	row := s.CompensateOne("a-cn")
	if row.Error != "" || !row.Claimed || row.Credit != 500 {
		t.Errorf("待领号应领取成功: %+v", row)
	}
	if _, claim := stub.counts(); claim != 1 {
		t.Errorf("领取次数=%d want 1", claim)
	}

	// 国际号同样可领（billing 域按 realm 走 base）：stub 说待领，就该领到。
	row = s.CompensateOne("e-global")
	if row.Skip || row.Error != "" || !row.Claimed || row.Credit != 500 {
		t.Errorf("国际号应正常领取: %+v", row)
	}
	if _, claim := stub.counts(); claim != 2 {
		t.Errorf("领取次数=%d want 2（a-cn + e-global）", claim)
	}

	// 企业/禁用仍门控：不发任何请求。
	skip := s.CompensateOne("d-enterprise")
	if !skip.Skip || skip.SkipReason == "" {
		t.Errorf("企业版应跳过: %+v", skip)
	}
	if _, claim := stub.counts(); claim != 2 {
		t.Errorf("门控号不得领取，实际 %d 次", claim)
	}
}

// TestStreakBonusGlobalRunsBillingBenefits 国际号在连登链路里仍要跑 billing 域礼包/补偿
// （D4 门控只覆盖 CN 成长体系；一并跳过会静默漏领国际区活动 —— 线上实测 workbuddy.ai
// 上两个端点都是 200）。
func TestStreakBonusGlobalRunsBillingBenefits(t *testing.T) {
	stub := &compStub{
		statusBody:     `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500}}`,
		claimBody:      `{"code":0,"msg":"OK","data":{"credit":500}}`,
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

	status, claim := stub.counts()
	if status != 1 || claim != 1 {
		t.Errorf("国际号补偿 status=%d claim=%d want 1/1", status, claim)
	}
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

// TestCompensateOneUnknownUID 未知 uid 返回跳过 + 错误（面板据此回 404 语义）。
func TestCompensateOneUnknownUID(t *testing.T) {
	s := compensationScheduler(t, &compStub{statusBody: `{"code":0,"data":{}}`})
	if row := s.CompensateOne("nope"); !row.Skip || row.Error == "" {
		t.Errorf("未知 uid 应返回跳过+错误: %+v", row)
	}
}

// TestStreakBonusSkipsGiftClaimWhenAlreadyClaimed 已领过礼包时**不得**再发起领取：
// 旧写法盲调 ClaimGift，上游对「已领过」返回 200 且无 credit 字段，于是每个已领号
// 每天打一行假的「🎊 新手礼包 +0c」。本用例在盲领代码上必红（会命中 /claim-gift）。
func TestStreakBonusSkipsGiftClaimWhenAlreadyClaimed(t *testing.T) {
	stub := &compStub{giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":true,` +
		`"claimed_at":"2026-09-24 18:15:15","active":true,"credit_num":1500,` +
		`"validity_period":1,"start_time":"2026-04-01 00:00:00","end_time":"2026-10-31 23:59:59"}}`}
	s := compScheduler(t, stub)
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
	stub := &compStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,` +
			`"credit_num":1500,"validity_period":1,"end_time":"2026-10-31 23:59:59"}}`,
		giftClaimBody: `{"code":0,"msg":"OK","data":{"credit":1500}}`,
	}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	if stub.giftStatusCalls != 1 || stub.giftClaimCalls != 1 {
		t.Errorf("status=%d claim=%d want 1/1", stub.giftStatusCalls, stub.giftClaimCalls)
	}
}

// TestStreakBonusGiftStatusFailureSkipsClaim 礼包状态查不到时不领（旧盲领会照领）。
func TestStreakBonusGiftStatusFailureSkipsClaim(t *testing.T) {
	stub := &compStub{giftStatusBody: `{"code":0,"msg":"OK","data":{"active":false,"claimed":false,"credit_num":0}}`}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()
	if stub.giftClaimCalls != 0 {
		t.Errorf("无待领礼品却领了 %d 次", stub.giftClaimCalls)
	}
}

// TestBenefitClaimWithoutCreditReportsZero 回执不填 credit 时按「本次到账 0」如实记录。
//
// 这条用例在 2026-10-20 被反转：此前断言「回落到状态额度 500」，理由是「钱已到账、
// 显示 0 是在说反话」。但用 credit-history 一验就穿了——18 点后 340 条「🎊 +500c」日志，
// 而全量 2000 条余额流水里 delta=+500 出现 **0 次**（同期只有签到 +10/+60）。
// 真相：上游对「已领过」回 code=0 且不带 credit 字段 = **本次没有发放**。
// 现在严格只认回执，0 就是 0，绝不拿「可领额度」冒充「已到账」。
func TestBenefitClaimWithoutCreditReportsZero(t *testing.T) {
	stub := &compStub{
		statusBody:     `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`,
		claimBody:      `{"code":0,"msg":"OK","data":{}}`, // 无 credit 字段 = 本次无发放
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":1000,"end_time":"2026-10-31 23:59:59"}}`,
		giftClaimBody:  `{"code":0,"msg":"OK","data":{}}`,
	}
	s := compScheduler(t, stub)

	row := s.CompensateOne("u1")
	if row.Error != "" || !row.Claimed {
		t.Fatalf("处理应成功（Claimed=true 表示已处理过）: %+v", row)
	}
	if row.Credit != 0 {
		t.Errorf("回执无 credit 时应如实记 0（本次到账 0），实得 %d —— 这正是假 +500c 的来源", row.Credit)
	}

	a := s.cfg.Pool.AuthByUID("u1")
	if got := s.claimGiftQuiet(a); got != 0 {
		t.Errorf("礼包同理：回执无 credit 应记 0，实得 %d", got)
	}
}

// TestCompensationGuardSkipsRepeatClaim 同一期（同 uid + 同 end_time）只 claim 一次。
//
// 背景：上游 claimed 字段**恒为 false**（已领过也回 false），Pending() 恒真 →
// 旧代码每次签到都把全池号重复 claim 一遍（350 次/轮，永远拿不到新发放）。
func TestCompensationGuardSkipsRepeatClaim(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{}}`,
	}
	s := compScheduler(t, stub)

	s.RunStreakBonusNow()
	_, first := stub.counts()
	if first != 1 {
		t.Fatalf("第一轮应 claim 1 次，实得 %d", first)
	}

	s.RunStreakBonusNow() // 同一进程第二轮（模拟下一次签到）
	_, second := stub.counts()
	if second != first {
		t.Errorf("同一期不得重复 claim：第一轮 %d 次，第二轮累计 %d 次", first, second)
	}
}

// TestCompensationGuardResetsOnNewPeriod end_time 变了（上游新开一期）必须重新尝试。
func TestCompensationGuardResetsOnNewPeriod(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{}}`,
	}
	s := compScheduler(t, stub)

	s.RunStreakBonusNow()
	_, first := stub.counts()

	// 上游换期：end_time 变了 → guard key 变 → 应再试一次
	stub.mu.Lock()
	stub.statusBody = `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":800,"end_time":"2026-11-30 23:59:59"}}`
	stub.mu.Unlock()

	s.RunStreakBonusNow()
	_, second := stub.counts()
	if second != first+1 {
		t.Errorf("新一期应重新 claim：前一版 %d 次，换期后累计 %d 次（期望 +1）", first, second)
	}
}

// TestGiftGuardSkipsRepeatClaim 礼包同款闸门。
func TestGiftGuardSkipsRepeatClaim(t *testing.T) {
	stub := &compStub{
		giftStatusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":1000,"end_time":"2026-10-31 23:59:59"}}`,
		giftClaimBody:  `{"code":0,"msg":"OK","data":{"credit":1000}}`,
	}
	s := compScheduler(t, stub)
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

// TestBenefitClaimCreditWinsOverStatus 回执有 credit 时以回执为准（兜底不得覆盖真实回执）。
func TestBenefitClaimCreditWinsOverStatus(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{"credit":300}}`, // 上游按实际发放，可少于可领额度
	}
	s := compScheduler(t, stub)
	if row := s.CompensateOne("u1"); row.Credit != 300 {
		t.Errorf("want 回执值 300，实得 %d", row.Credit)
	}
}
