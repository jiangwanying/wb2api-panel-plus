package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestGrowthPendingIncludesReachedUnlistedTask 白名单外但已达标的任务必须入队（issue #174）。
//
// 现场：新号跑完成长任务后积分卡在 2000/2100/2300 上不去 4000 —— 任务进度达标、
// accept_status 已是 accepted，差的是没人 claim。旧闸门要求 autoActionFor != nil，
// 白名单外（25 个码之外）的任务即使达标也永远进不了队列，永远不会被领。
func TestGrowthPendingIncludesReachedUnlistedTask(t *testing.T) {
	const unlisted = "some_future_task_code" // 不在 autoActions 白名单里
	if autoActionFor(unlisted) != nil {
		t.Fatalf("%q 已在白名单里，这个用例失去意义（换一个没被登记的任务码）", unlisted)
	}

	reached := upstream.Task{TaskCode: unlisted, Target: 3, Current: 3}
	if !growthPending(reached) {
		t.Error("白名单外且已达标 → 应入队（补领），got false")
	}

	// accept_status=completed 也是达标形态（部分任务没有 target/current）。
	if !growthPending(upstream.Task{TaskCode: unlisted, AcceptStatus: "completed"}) {
		t.Error("accept_status=completed → 应入队（补领），got false")
	}

	// 未达标 + 无动作 → 不入队（入队只会空转）。
	if growthPending(upstream.Task{TaskCode: unlisted, Target: 3, Current: 1}) {
		t.Error("白名单外且未达标 → 不应入队，got true")
	}

	// 黑名单里的常态任务保持不变：达标/未达标都入队。
	listed := autoActions[0].TaskCode
	if !growthPending(upstream.Task{TaskCode: listed, Target: 3, Current: 1}) {
		t.Error("白名单任务未达标也应入队（现状），got false")
	}

	// 已领 / 锁定 依然不入队（与旧语义一致）。
	if growthPending(upstream.Task{TaskCode: unlisted, Target: 3, Current: 3, Claimed: true}) {
		t.Error("已领取的任务不应入队")
	}
	if growthPending(upstream.Task{TaskCode: unlisted, Target: 3, Current: 3, Locked: true}) {
		t.Error("锁定任务不应入队")
	}
}

// TestQueuedActionForSynthesizesClaimOnly 队列动作解析：白名单外合成只领奖动作，
// 该动作不执行任何上游动作（run == nil），只在执行器看到已达标时 claim。
func TestQueuedActionForSynthesizesClaimOnly(t *testing.T) {
	listed := autoActions[0].TaskCode
	if a := queuedActionFor(listed); a == nil || a.ClaimOnly || a.TaskCode != listed {
		t.Fatalf("白名单任务应返回白名单动作（非 ClaimOnly），got %+v", a)
	}

	const unlisted = "some_future_task_code"
	a := queuedActionFor(unlisted)
	if a == nil {
		t.Fatal("白名单外任务应合成动作，got nil")
	}
	if a.TaskCode != unlisted {
		t.Errorf("合成动作 task_code = %q, want %q", a.TaskCode, unlisted)
	}
	if !a.ClaimOnly {
		t.Error("合成动作必须标记 ClaimOnly")
	}
	if a.run != nil {
		t.Error("合成动作不得带执行函数（否则会误跑对话/动作请求）")
	}
}
