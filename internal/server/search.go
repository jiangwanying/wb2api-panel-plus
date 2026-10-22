// search.go POST /v1/search：官方 agenttool 联网检索的网关入口（issue #195 的 A 形态）。
//
// 非对话端点，与 images 同一套取号与失败处置：单次取号、失败换号上限 3、
// 请求级错误（参数/内容/模型）fail-fast 不烧全池。回程把上游原文原样写出
// （{query,type,provider,results,total_results,response_time_ms}），不做二次包装。
//
// 选号域：请求可带 "model":"cn:x" 只用其 realm 前缀，或直接 "realm":"global"；都没有 → cn。
//
//	POST /v1/search
//	{"query": "玉林 电梯 维保 新规", "max_results": 5}
//	{"query": "...", "deep": true}   ← 综述式（agentic_search），单次 40~66s
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchAttempts 失败换号上限（与 images 同量级：人工/低频触发，3 次足够绕开坏号）。
const searchAttempts = 3

type searchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	Model      string `json:"model"` // 可选，只取 realm 前缀
	Realm      string `json:"realm"` // 可选，优先于 model 前缀
	// Deep 可选：走官方综述式检索（agentic_search），回 done 帧原文
	// （synthesis.content 带引用的综述 + results 整页正文）。实测单次 40~66s，
	// 默认 false 保持 snippet 快路径（~1s）。
	Deep bool `json:"deep"`
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req searchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`query is required（例：{"query":"玉林 电梯 维保 新规","max_results":5}）`)
		return
	}
	realm, _ := resolveModel(req.Model)
	if req.Realm != "" {
		realm = req.Realm
	}
	if realm != "global" {
		realm = "cn"
	}

	res := h.doSearch(r.Context(), query, req.MaxResults, realm, req.Deep)
	if res.status == 0 {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(res.data)
		return
	}
	writeOpenAIError(w, res.status, res.code, res.msg)
}

// searchOutcome doSearch 的结果：成功时 status=0、data 为上游原文；
// 失败时 status/code/msg 直接可写给客户端（OpenAI 或 Anthropic 形态由调用方决定）。
type searchOutcome struct {
	data   json.RawMessage
	status int
	code   string
	msg    string
}

// doSearch 执行一次联网检索：单次取号、失败换号上限 searchAttempts、请求级错误
// fail-fast 不烧全池。抽成独立函数供 /v1/search（OpenAI 透传）与
// /v1/search/messages（Anthropic 搜索壳）共用，两处失败处置保持同一口径。
func (h *Handler) doSearch(ctx context.Context, query string, maxResults int, realm string, deep bool) searchOutcome {
	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < searchAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		var data json.RawMessage
		var err error
		if deep {
			data, err = h.cfg.Upstream.AgenticSearch(ctx, a, query)
		} else {
			data, err = h.cfg.Upstream.Search(ctx, a, query, maxResults)
		}
		if err == nil {
			return searchOutcome{data: data}
		}
		lastErr = err
		var ue *upstream.Error
		if errors.As(err, &ue) {
			// 与 chat/images 同一套失败处置（冷却/熔断/模型负缓存/12153 计数）。
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			// 请求级错误：共享清单（参数/内容/模型）之外，search 的未知 400 也不换号——
			// 检索没有「模型权限」维度，400 是请求字段的属性（实测 15003 unsupported
			// provider / 15013 url is required 都走 400），换号必然同样被拒；
			// 而 429/402/401（限流/见底/会话失效）换号可能有效，故只收 400+ErrClient。
			if requestLevelFailFastKind(ue.Kind) ||
				(ue.Status == http.StatusBadRequest && ue.Kind == upstream.ErrClient) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				log.Printf("search realm=%s: 请求级错误 kind=%v，不再换号", realm, ue.Kind)
				return searchOutcome{status: status, code: "upstream_error", msg: ue.Msg}
			}
		}
		log.Printf("search %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, err)
	}

	status, code := http.StatusBadGateway, "upstream_error"
	var ue *upstream.Error
	switch {
	case len(tried) == 0:
		status, code = http.StatusServiceUnavailable, "no_account"
		lastErr = fmt.Errorf("没有可用的 %s 账号", realm)
	case errors.As(lastErr, &ue) && ue.Status >= 400:
		status = ue.Status
	}
	log.Printf("search realm=%s: 全部尝试失败（%d 个账号）：%v", realm, len(tried), lastErr)
	msg := "search failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	return searchOutcome{status: status, code: code, msg: msg}
}
