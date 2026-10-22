// compensation.go 官方补偿活动的状态查询（POST /billing/meter/compensation-status）。
//
// 背景（上游 PR #196 / issue #189，2026-10-16 cn 域实测）：官方会在「套餐与积分」页挂限时
// 补偿横幅（例：「DeepSeek 模型服务异常额外补偿 500 积分」，带领取截止日）。网关此前是
// **盲领**——ClaimCompensation 只在每日签到后顺带调一次，写法是
// `if credit, err := ...; err == nil { log }`；而实测「无补偿 / 已领过」时上游仍返回
// code 0 且 body 无 credit 字段，于是 ClaimCompensation 返回 (0, nil)：日志里每天多出一行
// 假的「🎊 补偿领取 +0c」（线上 2026-09-16~10-16 共 20 行，真到账 0 行），真正的失败
// （401/5xx/网络）反倒无痕——global 号在 CN billing 端点恒 401，同样悄无声息。
// 补偿带截止日，签到停摆（被禁用 / 关机 / checkin 关）就会静默过期。本文件补上
// 「先查状态再决定是否领」所缺的那一半。
//
// 上游口径（实测，CLI 域与 Web 域返回一致）：
//
//	{"code":0,"msg":"OK","data":{"claimed":false,"active":true,
//	   "credit_num":500,"validity_period":1,"end_time":"2026-10-22 23:59:59"}}
//
// active=false 是「当前无补偿活动」的**正常 200**，不是错误——故本接口只在传输失败 /
// 非 2xx / 业务 code 非 0 时报错，否则没活动的账号会把日志刷成一片错误。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 官方「福利」类活动共用同一响应形态（补偿 / 新手礼包实测同构）：
//
//	POST /billing/meter/compensation-status  补偿（有无活动看 active）
//	POST /billing/meter/check-gift-claimed   新手礼包状态（**无 /v2 前缀**，带 /v2 是 404）
const (
	compensationStatusPath = "/billing/meter/compensation-status"
	giftStatusPath         = "/billing/meter/check-gift-claimed"
)

// CompensationStatus 单账号的补偿 / 礼包活动状态（两类同构，故共用一个结构）。
type CompensationStatus struct {
	Active    bool  `json:"active"`     // 当前是否有生效中的补偿活动（false = 无活动，正常返回）
	Claimed   bool  `json:"claimed"`    // 本次活动是否已领取
	CreditNum int64 `json:"credit_num"` // 可领额度（积分）
	// ValidityPeriod 发放积分的有效期倍数（上游未明示单位，前端也不渲染）：
	// 只透传不换算，避免臆造语义。
	ValidityPeriod int64 `json:"validity_period"`
	// EndTime 领取截止（上游为 "2006-01-02 15:04:05" 无时区字符串）。
	EndTime string `json:"end_time"`
}

// Pending 是否存在「可领且未领」的补偿——调 ClaimCompensation 的前置判据。
// 三与门：active=false 是没有活动；claimed=true 是已领完（再调只拿业务错误）；
// credit_num<=0 是活动在但无额度。三者都归「无需领取」。
// 排程与面板共用本判据（唯一权威实现，避免同一个与门散落成多份拷贝）。
func (s CompensationStatus) Pending() bool {
	return s.Active && !s.Claimed && s.CreditNum > 0
}

// GetCompensationStatus 查补偿活动状态。走 billingJSON（billingBase），与既有
// ClaimCompensation / ClaimGift 同域同头；实测该端点 CLI 域与 Web 域返回一致，
// 不必像 growth 领奖那样切 webBase。
func (c *Client) GetCompensationStatus(a *auth.Auth) (CompensationStatus, error) {
	return c.benefitStatus(a, compensationStatusPath)
}

// GetGiftStatus 查新手礼包状态（每号一次、带有效期）。同域同头，字段与补偿同构：
//
//	{"code":0,"data":{"claimed":true,"claimed_at":"2026-09-24 18:15:15","active":true,
//	   "credit_num":1500,"validity_period":1,"start_time":"2026-04-01 00:00:00",
//	   "end_time":"2026-10-31 23:59:59"}}
//
// 用途与补偿完全一致：ClaimGift 对「已领过」也只返回 (0, nil)，盲领会在日志里打出
// 假的「🎊 新手礼包 +0c」成功行。
func (c *Client) GetGiftStatus(a *auth.Auth) (CompensationStatus, error) {
	return c.benefitStatus(a, giftStatusPath)
}

// benefitStatus 福利状态查询（补偿 / 礼包共用）：走 billingJSON（billingBase），与
// 两者的领取端点同域同头。
func (c *Client) benefitStatus(a *auth.Auth, path string) (CompensationStatus, error) {
	data, err := c.billingJSON(a, http.MethodPost, path, map[string]any{})
	if err != nil {
		return CompensationStatus{}, err
	}
	var st CompensationStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return CompensationStatus{}, err
	}
	return st, nil
}
