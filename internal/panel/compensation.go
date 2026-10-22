// compensation.go 面板侧「官方补偿」入口（上游 PR #196 / issue #189）。
//
// 补偿是带截止日的限时活动，而网关此前只有排程里的盲领：账号侧看得见横幅，面板上
// 什么都看不到，失败也无痕。这里给出三个入口，取数与呈现分离：
//   - GET  /panel/api/compensation              只读查全部账号状态（不领取）
//   - POST /panel/api/accounts/{uid}/compensation  单号「查状态 → 待领才领」
//   - POST /panel/api/compensation_all          全量（串行；同一活动每号仅一次）
//
// 取数只在进页面 / 点「检查」/ 点领取时发生（补偿状态没有 overview 快照可吃，挂进 5s 的
// refreshVisible 就是每 5 秒轰炸上游）；倒计时走动由前端对缓存做本地重渲染。
package panel

import (
	"log"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

// compensation GET /panel/api/compensation：只读查全部账号的补偿状态。
func (p *Panel) compensation(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	rows := p.cfg.Scheduler.CompensationRows()
	var pending int
	var credit int64
	var errored int
	for _, row := range rows {
		if row.Error != "" {
			errored++
		}
		// 待领 = 查到了活动、未领、有额度（与 scheduler 侧 Pending 同判据）；
		// 被门控跳过的不计入待领（项目上没有它的事）。
		if !row.Skip && row.Error == "" && row.Active && !row.Claimed && row.Credit > 0 {
			pending++
			credit += row.Credit
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": rows,
		"pending":  pending,
		"credit":   credit,
		"errored":  errored,
	})
}

// accountCompensation POST /panel/api/accounts/{uid}/compensation：单号领取。
func (p *Panel) accountCompensation(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	uid := r.PathValue("uid")
	row := p.cfg.Scheduler.CompensateOne(uid)
	if row.Error == "account not found" {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: 补偿领取 uid=%s ok=%v credit=%d skip=%v err=%q", uid, row.Claimed, row.Credit, row.Skip, row.Error)
	writeJSON(w, http.StatusOK, row)
}

// compensationAll POST /panel/api/compensation_all：全量「查状态 → 待领才领」。
// 串行执行（同一活动每号仅一次，串行不会丢单，也不会把上游打热）；逐号结果全部返回，
// summary 里 errored 与 claimed 分开——**面板必须先看 errored 再看 claimed**，
// 否则「上游全挂」会被读成「没有待领补偿」。
func (p *Panel) compensationAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	stats := p.cfg.Pool.List()
	results := make([]scheduler.CompensationRow, 0, len(stats))
	var claimed, skipped, errored int
	var credit int64
	for _, st := range stats {
		row := p.cfg.Scheduler.CompensateOne(st.UID)
		results = append(results, row)
		switch {
		case row.Error != "":
			errored++
		case row.Claimed:
			claimed++
			credit += row.Credit
		default:
			skipped++ // 无待领 / 被门控跳过：都不是失败
		}
	}
	log.Printf("panel: 补偿全量完成 claimed=%d skipped=%d errored=%d credit=%d", claimed, skipped, errored, credit)
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": results,
		"summary": map[string]any{
			"claimed": claimed, "skipped": skipped, "errored": errored, "credit": credit,
		},
	})
}
