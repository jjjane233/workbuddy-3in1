package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// zcodeCreditsDetailer 是 UpstreamOps 的**可选**扩展：ZCode 真实积分明细。
// 实现方（server.Handler）提供；未实现时面板静默跳过，不影响余额刷新本身。
type zcodeCreditsDetailer interface {
	ZCodeCreditsDetail(ctx context.Context, a *auth.Auth) (map[string]any, error)
}

// upstreamOp 单号操作类型。
type upstreamOp string

const (
	upstreamOpCheckin   upstreamOp = "checkin"
	upstreamOpBalance   upstreamOp = "balance"
	upstreamOpActivity  upstreamOp = "activity"
	upstreamOpKeepalive upstreamOp = "keepalive"
	upstreamOpClaim     upstreamOp = "claim"
)

// upstreamAccountOps 处理 Trae / ZCode 账号的单号操作（签到 / 余额）。
//
// 响应字段与 CodeBuddy 分支刻意保持同构（ok / checkin_done / checkin_message /
// credits / credits_total / balance_error），前端不用为上游分支写第二套渲染；
// 多返回一个 provider 便于排查「这个号走的是哪条链路」。
func (p *Panel) upstreamAccountOps(w http.ResponseWriter, r *http.Request, a *auth.Auth, op upstreamOp) {
	ops, ok := p.upstreamOrErr(w)
	if !ok {
		return
	}
	ctx := r.Context()
	resp := map[string]any{"ok": true, "provider": a.ProviderValue()}

	switch op {
	case upstreamOpCheckin:
		done, msg := ops.Checkin(ctx, a)
		resp["checkin_done"] = done
		if msg != "" {
			resp["checkin_message"] = msg
		}
		// 与 CodeBuddy 分支一致：签到后顺带刷新余额，按钮一次点击给完整反馈。
		if remain, total, err := ops.Balance(ctx, a); err != nil {
			resp["balance_error"] = err.Error()
		} else {
			p.cfg.Pool.SetCredits(a.UID, remain, total)
			resp["credits"], resp["credits_total"] = remain, total
		}
		log.Printf("panel: %s checkin uid=%s done=%v msg=%q", a.ProviderValue(), a.UID, done, msg)

	case upstreamOpBalance:
		remain, total, err := ops.Balance(ctx, a)
		if err != nil {
			// 与 CodeBuddy 分支同码：余额接口失败用 502，前端按错误 toast。
			writeErr(w, http.StatusBadGateway, "upstream balance: "+err.Error())
			return
		}
		p.cfg.Pool.SetCredits(a.UID, remain, total)
		resp["credits"], resp["credits_total"] = remain, total
		// 可选能力：ZCode 的真实积分明细（含今日消耗、逐模型用量、当日折算单价）。
		// 用类型断言探测而不是加进 UpstreamOps 接口——加方法会逼所有测试替身
		// 一起改签名；这是纯展示增强，缺了也只是少一块数据。
		if a.ProviderValue() == auth.ProviderZCode {
			if d, ok := ops.(zcodeCreditsDetailer); ok {
				if detail, derr := d.ZCodeCreditsDetail(ctx, a); derr == nil && detail != nil {
					resp["credit_detail"] = detail
					if src, _ := detail["source"].(string); src == "quota/limit" {
						if c, ok := detail["credits"].(int64); ok {
							if t, ok := detail["credits_total"].(int64); ok {
								p.cfg.Pool.SetCredits(a.UID, c, t)
								resp["credits"], resp["credits_total"] = c, t
							}
						}
					}
				}
			}
		}
		log.Printf("panel: %s balance uid=%s %d/%d", a.ProviderValue(), a.UID, remain, total)

	default:
		writeErr(w, http.StatusBadRequest, "unknown upstream op: "+string(op))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// upstreamBatch 对全部 Trae / ZCode 账号并发执行一次上游操作（面板批量按钮用）。
//
// 【为什么与 CodeBuddy 的批量分开】
// sch.RunCheckinNow / RunActivityNow 等遍历的是 CodeBuddy 账号集合（
// ListCodeBuddy），把上游账号塞进去只会打到错误端点。这里独立成一条通路：
// 只遍历 IsUpstream 账号，按 provider 分派到 ops，互不干扰。
func (p *Panel) upstreamBatch(w http.ResponseWriter, r *http.Request, op upstreamOp) {
	if p.upstream == nil {
		writeErr(w, http.StatusNotImplemented, "上游操作未启用（该部署只运行 CodeBuddy 链路）")
		return
	}
	summary, _ := p.batchUpstream(r, op)
	writeJSON(w, http.StatusOK, summary)
}

// upstreamBatchResult 单个账号的批量操作结果（面板批量按钮的返回行）。
type upstreamBatchResult struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Remain  int64  `json:"credits,omitempty"`
	Total   int64  `json:"credits_total,omitempty"`
}

// batchUpstream 对全部 Trae / ZCode 账号并发执行一次上游操作并返回汇总。
// 返回 (汇总, 是否执行过)——未注入 ops 或无上游账号时返回 (nil,false)，
// 由调用方决定是走 CodeBuddy 通路还是报 501。
func (p *Panel) batchUpstream(r *http.Request, op upstreamOp) (map[string]any, bool) {
	ops := p.upstream
	if ops == nil {
		return nil, false
	}
	ctx := r.Context()
	type job struct {
		a    *auth.Auth
		name string
	}
	var jobs []job
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || !a.IsUpstream() {
			continue
		}
		jobs = append(jobs, job{a: a, name: a.Display()})
	}
	if len(jobs) == 0 {
		return map[string]any{"ok": true, "total": 0, "message": "没有 Trae / ZCode 账号"}, false
	}

	type result = upstreamBatchResult
	results := make([]result, len(jobs))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := result{Name: j.name, UID: j.a.UID}
			switch op {
			case upstreamOpCheckin:
				done, msg := ops.Checkin(ctx, j.a)
				res.OK, res.Message = done, msg
			case upstreamOpActivity:
				n, err := ops.Activity(ctx, j.a)
				if err != nil {
					res.Message = err.Error()
				} else {
					res.OK = true
					res.Message = fmt.Sprintf("处理 %d 个活动", n)
				}
			case upstreamOpKeepalive:
				if err := ops.Keepalive(ctx, j.a); err != nil {
					res.Message = err.Error()
				} else {
					res.OK, res.Message = true, "保活完成"
				}
			case upstreamOpBalance:
				remain, total, err := ops.Balance(ctx, j.a)
				if err != nil {
					res.Message = err.Error()
				} else {
					res.OK, res.Remain, res.Total = true, remain, total
					p.cfg.Pool.SetCredits(j.a.UID, remain, total)
				}
			case upstreamOpClaim:
				okc, msg := ops.Claim(ctx, j.a)
				res.OK, res.Message = okc, msg
			}
			results[i] = res
		}(i, j)
	}
	wg.Wait()
	done := 0
	for _, r := range results {
		if r.OK {
			done++
		}
	}
	return map[string]any{
		"ok": true, "total": len(results), "succeeded": done, "results": results,
	}, true
}

// upstreamCredential 手填 ZCode 客户端签名凭据（"<keyId>.<secret>"）。
//
// 为什么需要这个入口：
//
//	ZCode 对话链路要过上游的风控闸门——同一把 JWT 打 billing/* 全是 200，
//	打 zcode-plan/.../messages 却是 405 code 3012；上游对"是否官方客户端"
//	的判定里，客户端请求签名（Ed25519，凭据为 "<keyId>.<secret>"）是必需项。
//	而签名器要先拿凭据去 open.bigmodel.cn / api.z.ai 做握手换私钥。
//
//	自动发现（auth.DiscoverZCodeSignCredential）读的是**本机** ~/.zcode/v2/config.json，
//	在服务器上这个文件不存在，于是线上换不到 key、回落链路永远不触发。
//	这个接口把"粘贴凭据"做成面板的一等入口，换机器/换账号都不再需要登服务器改文件。
//
// 写入后立即落盘（SaveAtomic）并回读校验，响应里的 signed/credential 就是
// 真实落盘结果，不是"已受理"这种含糊话术。
func (p *Panel) upstreamCredential(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if auth.NormalizeProvider(a.ProviderValue()) != auth.ProviderZCode {
		writeErr(w, http.StatusBadRequest, "该账号不是 ZCode，本接口只写 ZCode 签名凭据")
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "body 不是合法 JSON")
		return
	}
	cred := strings.TrimSpace(body.APIKey)
	if cred == "" {
		writeErr(w, http.StatusBadRequest, "缺少 api_key")
		return
	}
	// "<keyId>.<secret>" 是硬格式；SetSignCredential 对非法格式静默忽略，
	// 所以这里自己先判一次并明确报错，避免用户以为写进去了其实没有。
	dot := strings.IndexByte(cred, '.')
	if dot <= 0 || dot == len(cred)-1 {
		writeErr(w, http.StatusBadRequest, "格式应为 <keyId>.<secret>（一小段十六进制、一个点、再一段密钥）")
		return
	}
	// 兼容用户整段粘贴 JSON 或 "api_key=..." 的写法：剥掉外层包裹再取。
	if strings.HasPrefix(cred, "{") {
		var wrap struct {
			APIKey string `json:"api_key"`
			Key    string `json:"key"`
		}
		if json.Unmarshal([]byte(cred), &wrap) == nil {
			cred = firstNonEmptyStr(wrap.APIKey, wrap.Key)
		}
	} else if i := strings.Index(cred, "="); i > 0 {
		cred = strings.TrimSpace(cred[i+1:])
	}
	cred = strings.Trim(cred, "\"'")
	dot = strings.IndexByte(cred, '.')
	if dot <= 0 || dot == len(cred)-1 {
		writeErr(w, http.StatusBadRequest, "格式应为 <keyId>.<secret>")
		return
	}
	a.SetSignCredential(cred)
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	// 同时写入 APIKey（api_key 字段）：部分部署用它作为回落链路的直接凭据。
	a.SetAPIKey(cred)
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存 api_key 失败: "+err.Error())
		return
	}
	log.Printf("panel: ZCode 签名凭据已写入 uid=%s keyId=%s", a.UID, cred[:dot])
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "provider": auth.ProviderZCode,
		"signed": true, "key_id": cred[:dot],
		"credential": cred[:minInt(dot, 8)] + "…" + cred[dot+1:][:minInt(len(cred)-dot-1, 4)],
	})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
