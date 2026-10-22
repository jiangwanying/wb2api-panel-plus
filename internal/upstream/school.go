// school.go 开学季活动域（/portal/activity/school）。
//
// 目前只用到「我的券码」一项：券码是开学季抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），
// 活动已于 2026-09 结束、不再发放，但券的有效期最长到 2027-09，故保留只读查询入口。
package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// schoolBase 开学季活动域基础路径（拼在 billingBase 之后）。
const schoolBase = "/portal/activity/school"

// schoolJSON 向开学季活动域发一次 JSON 请求；鉴权与头族同 billing 域
// （Bearer access token + X-User-Id），出站走 c.doJSON（HTTP 客户端 120s 超时）。
func (c *Client) schoolJSON(a *auth.Auth, method, path string, body map[string]any, out any) error {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+schoolBase+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// SchoolVoucher 开学季抽奖抽中的第三方券（KFC/瑞幸/酷狗等）。
// 字段结构按真实响应样本：GET /vouchers 单次拉全（无分页），data.items[]。
type SchoolVoucher struct {
	GrantID   int64  `json:"grant_id"`
	DrawUUID  string `json:"draw_uuid,omitempty"`
	SKUCode   string `json:"sku_code,omitempty"`   // kfc_ice_cream / voucher_luckin / voucher_kugou …
	PrizeName string `json:"prize_name,omitempty"` // 肯德基冰淇淋
	Code      string `json:"code"`                 // 券码本体（复制给店员核销）
	ValidFrom string `json:"valid_from,omitempty"` // 上游常为空
	ValidTo   string `json:"valid_to,omitempty"`   // "2026-10-24"
	GrantedAt string `json:"granted_at,omitempty"` // RFC3339
}

// SchoolVouchers 查询账号的开学季券码列表（只读）。
// 抽到积分的记录不在此端点（那是 /rewards 的 type=credit 条目）。
func (c *Client) SchoolVouchers(a *auth.Auth) ([]SchoolVoucher, error) {
	var out struct {
		Items []SchoolVoucher `json:"items"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/vouchers", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
