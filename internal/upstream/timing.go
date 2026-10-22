// timing.go 上游调用的分段耗时观测（纯观测：不参与任何判定、重试或账号处置）。
//
// 为什么需要：日志里的 TTFB 语义是「首个 SSE 帧 − 请求进 handler」，把选号、请求体准备、
// 建连、上游首字节、SSE 解析揉成一个数，归因不了——直接拿它对比 curl 的
// time_starttransfer 会系统性偏高（实测差 0.5s 量级）。本文件把那段时间拆成：
//
//	Prep 本地准备（脱敏 / effort 注入 / 缓存键 / 建请求对象）
//	Do   上游 Do 往返（含建连 + 首字节）；global 的路径 fallback 逐次累加
//
// 各字段零值 = 未测量（不是 0ms）；nil *ChatTiming 调用方零开销、零行为变化。
package upstream

import "time"

// ChatTiming 单次上游 chat 调用的分段耗时。
type ChatTiming struct {
	Prep time.Duration // 本地请求体准备
	Do   time.Duration // 上游 Do（含建连/首字节；路径 fallback 累加）
	// RequestedEffort 客户端原始请求的思考档位（reasoning_effort / reasoningEffort
	// 原值，snake 优先）；未携带 → 空。不含网关补的默认档（injectThinking 之后
	// 两者就分不开了，故在注入前抓取）。
	RequestedEffort string
	// Effort 本次实际生效的思考档位（客户端携带过 reasoning_effort 才有值；未携带/未知
	// 模型/未知档位 → 空）。改写可能把它换成模型支持的最近档，行里显示的就是生效值。
	Effort string
}

// Total 返回已测量的分段合计（未测量的段为零值，不计入）。
func (t ChatTiming) Total() time.Duration { return t.Prep + t.Do }
