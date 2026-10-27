// export.go 账号导出（备份）：把 AuthDir 下全部账号凭证打包成 JSON 快照。
//
// 格式与「添加账号 → 导入 JSON」（importCockpit）兼容：cockpit 账号字段
// （access_token / refresh_token / uid / nickname / expires_at / domain），
// 因此导出文件可直接回导；另附 realm / enterprise_id / device_token 等本仓扩展，
// 回导时被 import 忽略（未知字段不报错）。
//
// 注意 expires_at 单位：cockpit 口径是毫秒（importCockpit 会 /1000），而内部
// Auth.ExpiresAt 是秒——导出时 ×1000 转换，保证「导出 → 导入」到期时间无损。
//
// ⚠️ 响应包含明文凭证（access_token / refresh_token），与 /panel/api/* 同口径
// 面板鉴权；前端下载前会二次确认。不要把导出文件提交进仓库或分享给他人。
package panel

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jiangwanying/wb2api-panel-plus/internal/auth"
)

// exportedAccount 导出快照中的单个账号（cockpit 兼容字段 + 本仓扩展）。
type exportedAccount struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	DeviceToken  string `json:"device_token,omitempty"`
	Source       string `json:"source,omitempty"`
}

// accountsExport 处理 GET /panel/api/accounts/export。
// 遍历 AuthDir 下 *.json（跳过 .tmp 原子写残留），逐个 Parse 后导出；
// 单文件读失败/解析失败只记日志并跳过，不中断整份导出。
func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.AuthDir == "" {
		writeErr(w, http.StatusNotImplemented, "auth dir not configured")
		return
	}
	entries, err := os.ReadDir(p.cfg.AuthDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read auth dir: "+err.Error())
		return
	}
	accounts := make([]exportedAccount, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".tmp") ||
			!strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(p.cfg.AuthDir, name))
		if err != nil {
			log.Printf("export: read %s: %v", name, err)
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			log.Printf("export: parse %s: %v", name, err)
			continue
		}
		accounts = append(accounts, exportedAccount{
			UID:          a.UID,
			Nickname:     a.Nickname,
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
			ExpiresAt:    a.ExpiresAt * 1000, // cockpit 口径毫秒；import 会 /1000 还原
			Domain:       a.Domain,
			Realm:        a.Realm(),
			EnterpriseID: a.EnterpriseID,
			DeviceToken:  a.DeviceToken,
			Source:       name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     1,
		"format":      "cockpit",
		"exported_at": time.Now().Format(time.RFC3339),
		"count":       len(accounts),
		"accounts":    accounts,
	})
}
