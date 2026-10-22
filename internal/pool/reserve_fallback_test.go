// 保留积分（reserve）在**全冷却兜底路径**上同样生效。
//
// 为什么单独补这组：既有的 TestReserveCreditsParksAccount 里那句「全池停牌时不该再
// 选出账号」是**假绿**——两个号都没进冷却，pickEarliestExpiryLocked 靠 expiry()==0
// 把它们跳过才返回 nil，压根没走到 reserve 判据。真实场景是「健康候选已被 reserve
// 拦净 → cands 空 → 兜底」，此时账号多半处于冷却/熔断（expiry 非零）才需要兜底。
// 故本组用 Cooldown 造出非零 expiry，再断言 reserve 拦得住。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReserveBlocksFallbackPathPaidModel 停牌号不得被兜底捞回来接付费模型：
// health 候选被 reserve 拦净后，兜底是最后一道路径，这里不拦就等于没拦。
func TestReserveBlocksFallbackPathPaidModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(100)
	// 目录倍率 1.62 → 收费模型（目录未覆盖时 modelFreeOf 亦返回 false，同样当付费）。
	p.SetModelRateOf(rates(map[string]map[string]string{"global": {"kimi-k3": "1.62"}}))

	e := &auth.Auth{UID: "poor"}
	auth.BackfillRealmFor(e, "global")
	p.Add(e)
	p.SetCredits("poor", 90, 500) // 余额 90 <= reserve 100 且已知 → 停牌
	// 置入软冷却：让 healthy 候选为空，强制走兜底路径（expiry 非零）。
	p.Cooldown("poor", CoolSoft, 2*time.Minute, "test")

	if a := p.PickExcludingForRealm(nil, "kimi-k3", "global"); a != nil {
		t.Fatalf("兜底路径应受 reserve 约束：停牌号 credits=90 <= 100 不得被捞出，got %v", a)
	}
}

// TestReserveFallbackAllowsFreeModel 兜底路径对**免费档**照常放行：reserve 拦的是
// 付费/未知倍率，不得让见底号连 x0.00 免费模型也接不到（那等于变相禁用）。
func TestReserveFallbackAllowsFreeModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(100)
	p.SetModelRateOf(rates(map[string]map[string]string{"global": {"hy3": "0.00"}}))

	e := &auth.Auth{UID: "poor"}
	auth.BackfillRealmFor(e, "global")
	p.Add(e)
	p.SetCredits("poor", 90, 500)
	p.Cooldown("poor", CoolSoft, 2*time.Minute, "test")

	if a := p.PickExcludingForRealm(nil, "hy3", "global"); a == nil {
		t.Fatal("兜底路径对免费档应放行停牌号，got nil")
	}
}

// TestReserveFallbackControlNoReserve 对照组：同一场景不设 reserve 时**必须**能选出。
// 缺了这条，上两条可能因为「兜底压根没走到」而假绿（正是既有测试的毛病）。
func TestReserveFallbackControlNoReserve(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(0) // 关闭
	p.SetModelRateOf(rates(map[string]map[string]string{"global": {"kimi-k3": "1.62"}}))

	e := &auth.Auth{UID: "poor"}
	auth.BackfillRealmFor(e, "global")
	p.Add(e)
	p.SetCredits("poor", 90, 500)
	p.Cooldown("poor", CoolSoft, 2*time.Minute, "test")

	if a := p.PickExcludingForRealm(nil, "kimi-k3", "global"); a == nil {
		t.Fatal("对照组：reserve 关闭时兜底路径应把该冷却号交出来")
	}
}
