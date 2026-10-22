// streak.go 连登管家：签到排程后自动检查连登兑换档位 → 可兑换即兑换 → 按抽奖次数抽奖。
//
// 背景（2026-09-12）：成长中心连登档位（7d/14d/28d）按连续登录天数解锁，兑换发
// credit/energy/补签卡/抽奖次数；抽奖次数只能从兑换获得。兑换按钮在 UI 上恒可点，
// 但未解锁时服务端 403「连续登录天数不足」——所以放在每日签到后跑一遍（幂等），
// 到天数那天自动完成「兑换 → 抽奖」闭环，无需人工盯。
package scheduler

import (
	"encoding/json"
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// RunStreakBonusNow 对所有可用账号执行连登兑换 + 抽奖（幂等：locked/无次数自动跳过）。
// 由签到排程（RunCheckinNow）末尾调用；也可面板手动触发。
func (s *Scheduler) RunStreakBonusNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			// D4 门控只覆盖 CN 成长体系（连登/补签/兑换/抽奖走 CN web base）。礼包与补偿是
			// billing 域、按 realm 走 base，国际号实测 200——一并跳过就会静默漏领国际区活动。
			s.claimGiftQuiet(a)
			s.claimCompensationQuiet(a)
			continue
		}
		if !a.SupportsPersonalGrowth() {
			continue // 企业版 / 静态 API Key 门控：无个人成长体系
		}
		s.streakBonusAccount(a)
	}
}

// streakBonusAccount 单账号：补签保连登 → 礼包/补偿 → 兑换所有已解锁档位 → 抽完所有 chances。
func (s *Scheduler) streakBonusAccount(a *auth.Auth) {
	// 0. 补签保连登：本月漏签且有补签卡则从最近一天往回补（连续天数一断就要重攒 7 天）。
	s.makeupMissed(a)
	// 0.5 礼包 + 补偿：两者都走「先查状态 → 待领才领」（见 compensation.go）。
	// 盲领会把「无活动/已领过」打成假的「+0c」成功行，真失败反倒无痕。
	s.claimGiftQuiet(a)
	s.claimCompensationQuiet(a)

	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := s.cfg.Upstream.GrowthRedeemTier(a, tier.Tier); err != nil {
			// 未解锁（403）属预期，静默；其余记日志。
			log.Printf("WARN: streak-bonus %s: redeem %s: %v", logfmt.Label(a.UID, a.Nickname), tier.Tier, err)
			continue
		}
		log.Printf("streak-bonus %s: ★ 兑换 %s 档（+%dc +%de 卡×%d 抽奖×%d）",
			a.UID, tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
	// 抽奖：按当前 chances 全抽完（兑换刚发的次数已在服务端累加）。
	chances, err := s.cfg.Upstream.LotteryChances(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: lottery summary: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	for i := 0; i < chances; i++ {
		raw, err := s.cfg.Upstream.LotteryDraw(a)
		if err != nil {
			log.Printf("WARN: streak-bonus %s: draw: %v", logfmt.Label(a.UID, a.Nickname), err)
			return
		}
		log.Printf("streak-bonus %s: 🎲 第%d抽 %s", logfmt.Label(a.UID, a.Nickname), i+1, compactJSON(raw))
	}
	if chances > 0 {
		log.Printf("streak-bonus %s: 抽奖完成 %d 次", logfmt.Label(a.UID, a.Nickname), chances)
	}
}

// compactJSON 裁剪奖品载荷（日志单行可读）。
func compactJSON(raw json.RawMessage) string {
	// logfmt.Truncate 回退到 UTF-8 rune 边界：奖品名是中文，按字节切会打出半个字。
	s := logfmt.Truncate(string(raw), 220)
	if len(raw) > 220 {
		s += "…"
	}
	return s
}

// makeupMissed 本月漏签且有补签卡时自动补签（保住连登连续天数）。
// 从最近一天往回补（最近的最值钱），最多补到卡用光。
// 上游拒绝即停：注册前的日子同样是 0 分，无法与漏签区分，只能由拒绝兜底。
// 无卡 / 无漏签 / 查询失败均静默（不影响主流程）。
func (s *Scheduler) makeupMissed(a *auth.Auth) {
	broken, err := s.cfg.Upstream.HeatmapBrokenDates(a)
	if err != nil || len(broken) == 0 {
		return
	}
	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return
	}
	for i, n := 0, min(full.MakeupCards.Balance, len(broken)); i < n; i++ {
		d := broken[len(broken)-1-i]
		if err := s.cfg.Upstream.UseMakeupCard(a, d); err != nil {
			log.Printf("WARN: streak-bonus %s: 补签 %s 失败: %v", logfmt.Label(a.UID, a.Nickname), d, err)
			return
		}
		log.Printf("streak-bonus %s: ★ 已用补签卡补签 %s（保连登）", logfmt.Label(a.UID, a.Nickname), d)
	}
}
