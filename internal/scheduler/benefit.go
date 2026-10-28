// benefit.go 福利活动的执行侧（新手礼包）。
//
// 历史：这里原本还承载「官方补偿」（上游 PR #196 / issue #189）——排程盲领是
// `if credit, err := ClaimCompensation(...); err == nil { log }`，让「没有补偿 / 已领过 /
// 真失败」三种情形同构，实测每天多一行**假的**「🎊 补偿领取 +0c」。补偿带截止日，
// 版本一度补上了「先查状态再决定是否领」的两端（排程 + 面板）。
// 该活动已于 2026-10 结束，整条链路（状态查询 / 领取 / 排程 / 面板卡片）一并下线；
// 保留下面的新手礼包，它踩的是同一个坑、用同一份纪律：
// **要么成功、要么显形**——无待领静默（正确形态），查询/领取失败一律 WARN。
package scheduler

import (
	"log"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
	"github.com/jiangwanying/wb2api-panel-plus/internal/logfmt"
)

// claimGiftQuiet 排程侧单账号新手礼包：先查状态，待领才领。
// 旧写法 `if credit, err := ClaimGift(a); err == nil { log }` 对「已领过」也返回 (0, nil)
// （上游 200 + 无 credit 字段），于是每个已领号每天打一行假的「🎊 新手礼包 +0c」。
// 待领才领：无活动/已领/无额度静默，查询与领取失败 WARN（含截止，礼包也带有效期）。
func (s *Scheduler) claimGiftQuiet(a *auth.Auth) int64 {
	label := logfmt.Label(a.UID, a.Nickname)
	src, err := s.cfg.Upstream.GetGiftStatus(a)
	if err != nil {
		// 已知没有个人礼包体系的账号（企业 / 已禁用）查询失败属预期，静默——否则每天固定
		// 刷出几行噪声；其余号的查询失败必须 WARN（旧盲领正是在这里无痕）。国际号不在此列：
		// 它的礼包查询在 workbuddy.ai 上是正常 200（billing 域按 realm 走 base）。
		if st, ok := s.cfg.Pool.Status(a.UID); ok && (st.Enterprise || st.Disabled) {
			return 0
		}
		log.Printf("WARN: streak-bonus %s: 礼包状态查询失败: %v", label, err)
		return 0
	}
	if !src.Pending() {
		return 0
	}
	// 本期（同 uid + 同 end_time）已试过就不再重复 claim。
	guardKey := "gift|" + a.UID + "|" + src.EndTime
	if s.benefitGuarded(guardKey) {
		return 0
	}
	credit, err := s.cfg.Upstream.ClaimGift(a)
	s.markBenefitGuarded(guardKey) // 无论成败都记：重试交给下一期或面板手动
	if err != nil {
		log.Printf("WARN: streak-bonus %s: 礼包领取失败（可领 %dc，截止 %s）: %v", label, src.CreditNum, src.EndTime, err)
		return 0
	}
	// ★ 上游对「已领过」回 200 + 无 credit 字段（2026-10-20 与补偿一并修的坑）：
	// 旧代码用 src.CreditNum 兜底，把「没发」写成「🎊 +1000c 已到账」。
	// 现在只认真实回执：credit<=0 就是本次到账 0，静默返回。
	if credit <= 0 {
		return 0
	}
	log.Printf("streak-bonus %s: 🎊 新手礼包 +%dc（截止 %s）", label, credit, src.EndTime)
	return credit
}

// benefitGuarded 报告本期福利是否已在本进程内尝试过。
func (s *Scheduler) benefitGuarded(key string) bool {
	s.benefitGuardMu.Lock()
	defer s.benefitGuardMu.Unlock()
	return s.benefitGuard[key]
}

// markBenefitGuarded 记下「本期已尝试」。懒初始化 map（零值 Scheduler 也可用）。
func (s *Scheduler) markBenefitGuarded(key string) {
	s.benefitGuardMu.Lock()
	defer s.benefitGuardMu.Unlock()
	if s.benefitGuard == nil {
		s.benefitGuard = make(map[string]bool)
	}
	s.benefitGuard[key] = true
}
