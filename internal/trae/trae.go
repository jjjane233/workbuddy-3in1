// Package trae 实现 TraeCode（Trae CN，api.trae.cn / trae-api-cn.mchost.guru）上游协议。
//
// 逆向自 Trae CN 3.3.96（resources/app/out/main.js 与 extensions/ai-completion）。
//
//	对话        POST https://trae-api-cn.mchost.guru/api/ide/v1/chat   （SSE 流式）
//	连通性      GET  https://trae-api-cn.mchost.guru/api/ide/v1/ping   → {"message":"pong"}
//	签到状态    POST https://api.trae.cn/trae/api/v2/ug/checkin_credits/status
//	签到领取    POST https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim
//	活动信息    POST https://api.trae.cn/trae/api/v2/ug/activity/info     body {req_source:1}
//	活动操作    POST https://api.trae.cn/trae/api/v2/ug/activity/action   body {activity_id,req_source}
//	裂变领取    POST https://api.trae.cn/trae/api/v2/ug/work_fission/grant
//	付费状态    POST https://api.trae.cn/trae/api/v2/pay/ide_user_pay_status
//	额度用量    POST https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage   body {require_usage:true,req_source:1}
//	用户信息    POST https://api.trae.cn/cloudide/api/v3/trae/GetUserInfo
//	登录校验    POST https://api.trae.cn/cloudide/api/v3/trae/CheckLogin
//
// 鉴权：Authorization: Cloud-IDE-JWT <token>（对话/签到/活动），
//
//	x-cloudide-token: <token>（GetUserInfo / CheckLogin / 第三方 token）。
package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// 端点常量。
const (
	// AgentDomain 承载对话 / ping / 远程文件。
	AgentDomain = "https://trae-api-cn.mchost.guru"
	// UGBase 承载签到 / 活动 / 付费 / 用户信息。
	UGBase = "https://api.trae.cn"
	// AppID 是 Trae CN 客户端在 product.json 中写死的 appId。
	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// DefaultIDEVersion 出站 IDE 版本号（对齐 Trae CN 3.3.96）。
	DefaultIDEVersion = "3.3.96"

	PathPing           = "/api/ide/v1/ping"
	PathChat           = "/api/ide/v1/chat"
	PathSoloChat       = "/api/agent/v3/llm_utils_chat"
	PathDetailParam    = "/api/ide/v1/get_detail_param"
	PathModelList      = "/api/ide/v1/model_list"
	PathLLMRawChat     = "/api/ide/v1/llm_raw_chat"
	PathCheckinStatus  = "/trae/api/v2/ug/checkin_credits/status"
	PathCheckinClaim   = "/trae/api/v2/ug/checkin_credits/claim"
	PathActivityInfo   = "/trae/api/v2/ug/activity/info"
	PathActivityAction = "/trae/api/v2/ug/activity/action"
	PathFissionGrant   = "/trae/api/v2/ug/work_fission/grant"
	// PathGrantWorkBenefit 通用权益领取（签到/新人/发消息/学生认证等所有
	// activity_type != 103 的活动）。无 activity_id 参数：服务端按账号当前
	// 可领清单自行发放，body 只需 trae_client + device_id。
	//
	// 来源：Trae CN 3.3.96 客户端 out/main.js 的 grantWorkBenefit()。
	// 注意既不是 /ug/work_benefit/grant（实测 404），也不要传 activity_id。
	PathGrantWorkBenefit = "/trae/api/v2/pay/grant_work_benefit"
	PathPayStatus        = "/trae/api/v2/pay/ide_user_pay_status"
	PathEntUsage         = "/trae/api/v2/pay/ide_user_ent_usage"
	PathGetUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	PathCheckLogin       = "/cloudide/api/v3/trae/CheckLogin"
)

// SOLO/DLL 模型链使用独立的协议版本与 function；它承载 DLL 内置模型池
// （glm-5.3-flashx / deepseek-v4.1-flash / step-5-preview 等）。
// 头矩阵与协议版本来自 Trae SOLO 客户端 0.1.52/20260811 抓包（docs/TRAECODE_BUILTIN_MODELS_OPENAI_PROXY.md）。
const (
	SoloIDEVersion     = "0.1.52"
	SoloIDEVersionCode = "20260811"
	SoloFunction       = "solo_work_lite"
)

// Client 一个 Trae 账号的上游客户端。
type Client struct {
	a   *auth.Auth
	hc  *http.Client
	ver string
}

// Options 构造参数。
type Options struct {
	IDEVersion string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// New 构造账号客户端。
func New(a *auth.Auth, o Options) *Client {
	hc := o.HTTPClient
	if hc == nil {
		to := o.Timeout
		if to <= 0 {
			to = 300 * time.Second
		}
		// 对话为长连接流式，读超时由 context 控制，此处不设整体超时。
		hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			ForceAttemptHTTP2:     true,
		}}
	}
	ver := o.IDEVersion
	if ver == "" {
		ver = DefaultIDEVersion
	}
	return &Client{a: a, hc: hc, ver: ver}
}

// DeviceID 返回该账号的稳定设备 id。
func (c *Client) DeviceID() string {
	if c.a.DeviceID != "" {
		return c.a.DeviceID
	}
	return c.a.UID
}

// commonHeaders 写入 Trae 客户端通用头（对齐 fb() 头构造器与 webRequest 拦截器）。
func (c *Client) commonHeaders(h http.Header) {
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "Trae/"+c.ver+" (win32; x64)")
	h.Set("x-ide-version-code", c.ver)
	h.Set("x-app-id", AppID)
	h.Set("x-device-id", c.DeviceID())
	h.Set("x-device-brand", "pc")
	h.Set("x-device-type", "windows")
	h.Set("x-os-version", "10.0.19045")
}

// authHeader 写入鉴权头。
func (c *Client) authHeader(h http.Header) {
	if t := c.a.TokenValue(); t != "" {
		h.Set("Authorization", "Cloud-IDE-JWT "+t)
		h.Set("x-cloudide-token", t)
	}
}

// Token 返回账号 token。
func (c *Client) Token() string { return c.a.TokenValue() }

// ---- 通用请求 ----

func (c *Client) do(ctx context.Context, method, url string, body any, headers map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req.Header)
	c.authHeader(req.Header)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.hc.Do(req)
}

func (c *Client) doJSON(ctx context.Context, method, url string, body any, headers map[string]string) (int, []byte, error) {
	resp, err := c.do(ctx, method, url, body, headers)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// ---- 连通性 / 用户信息 ----

// Ping 探测 agent 域可用性。
func (c *Client) Ping(ctx context.Context) (bool, error) {
	st, raw, err := c.doJSON(ctx, "GET", AgentDomain+PathPing, nil, nil)
	if err != nil {
		return false, err
	}
	return st == 200 && strings.Contains(string(raw), "pong"), nil
}

// UserInfo 返回账号用户信息（GetUserInfo）。
type UserInfo struct {
	UserID   string          `json:"UserID"`
	NickName string          `json:"NickName"`
	Region   string          `json:"Region"`
	IsLogin  bool            `json:"IsLogin"`
	Email    string          `json:"Email"`
	Raw      json.RawMessage `json:"-"`
}

// GetUserInfo 拉取用户信息。
func (c *Client) GetUserInfo(ctx context.Context) (*UserInfo, error) {
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathGetUserInfo,
		map[string]any{"ReqSource": "IDE", "IDEVersion": c.ver}, nil)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, fmt.Errorf("trae getuserinfo http %d: %s", st, truncate(string(raw), 200))
	}
	var env struct {
		Result *UserInfo `json:"Result"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Result == nil {
		return nil, fmt.Errorf("trae getuserinfo: empty result")
	}
	env.Result.Raw = json.RawMessage(raw)
	return env.Result, nil
}

// ---- 签到 ----

// CheckinStatus 签到状态。
type CheckinStatus struct {
	Enable    bool   `json:"enable"`
	CheckedIn bool   `json:"checked_in"`
	Credits   int64  `json:"credits"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(ctx context.Context) (*CheckinStatus, error) {
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathCheckinStatus, map[string]any{}, nil)
	if err != nil {
		return nil, err
	}
	var s CheckinStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("trae checkin status http %d: %s", st, truncate(string(raw), 200))
	}
	if s.Code != 0 && s.Code != 1001 {
		return &s, fmt.Errorf("trae checkin status code %d: %s", s.Code, s.Message)
	}
	if s.Code == 1001 {
		return &s, ErrUnauthorized
	}
	return &s, nil
}

// ErrUnauthorized 表示上游判定未登录/凭证无效。
var ErrUnauthorized = fmt.Errorf("trae: unauthorized (auth expired or not a MARSCODE account)")

// CheckinClaimResult 签到领取结果。
type CheckinClaimResult struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Credits int64  `json:"credits"`
	Raw     string `json:"raw,omitempty"`
	OK      bool   `json:"ok"`
}

// ClaimCheckin 执行签到领取。
func (c *Client) ClaimCheckin(ctx context.Context) (*CheckinClaimResult, error) {
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathCheckinClaim, map[string]any{}, nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Credits int64  `json:"credits"`
		Data    struct {
			Credits int64 `json:"credits"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &r)
	out := &CheckinClaimResult{Code: r.Code, Message: r.Message, Credits: r.Credits, Raw: truncate(string(raw), 400)}
	if out.Credits == 0 && r.Data.Credits != 0 {
		out.Credits = r.Data.Credits
	}
	out.OK = r.Code == 0
	if st != 200 && r.Code == 0 {
		return out, fmt.Errorf("trae checkin claim http %d", st)
	}
	if r.Code == 1001 {
		return out, ErrUnauthorized
	}
	return out, nil
}

// ---- 活动 / 裂变 ----

// ActivityInfo 商业活动信息。
type ActivityInfo struct {
	Code       int              `json:"code"`
	Message    string           `json:"message"`
	Activities []map[string]any `json:"commercial_activities"`
	Raw        json.RawMessage  `json:"-"`
}

// QueryActivityInfo 拉取活动信息。
func (c *Client) QueryActivityInfo(ctx context.Context, reqSource int) (*ActivityInfo, error) {
	if reqSource == 0 {
		reqSource = 1
	}
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathActivityInfo, map[string]any{"req_source": reqSource}, nil)
	if err != nil {
		return nil, err
	}
	var a ActivityInfo
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("trae activity info http %d: %s", st, truncate(string(raw), 200))
	}
	a.Raw = json.RawMessage(raw)
	if a.Code == 1001 {
		return &a, ErrUnauthorized
	}
	return &a, nil
}

// HandleActivity 执行活动动作（activity_id 为具体活动）。
//
// 语义澄清（2026-10-06 实测）：这条不是通用领取入口。它对部分活动回
// code=9090「活动暂不可用」、对 student_auth_reward 回空对象（受理但无动作）。
// 真正的领取链路是：
//
//	activity_type == 103 → FissionGrant(activity_id)
//	其余                 → BenefitGrant(device_id)
//
// 保留本方法仅作兼容，新代码请勿用它当"领取"。
func (c *Client) HandleActivity(ctx context.Context, activityID string, reqSource int) (int, json.RawMessage, error) {
	if reqSource == 0 {
		reqSource = 1
	}
	body := map[string]any{"activity_id": activityID, "req_source": reqSource}
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathActivityAction, body, nil)
	if err != nil {
		return st, nil, err
	}
	return st, json.RawMessage(raw), nil
}

// FissionGrant 领取裂变奖励（activity_type=103 的活动，如 work_fission_*）。
//
// payload 只带 activity_id。为什么不是 activity_type：实测传 activity_type
// （int 103 / 字符串 "103" / "code_fission_202609"）一律回
// 4001 param is invalid；传 activity_id 才过参数校验进业务层。
// eligible 仅用于日志/可观测，不参与请求体（客户端同样只发 activity_id）。
func (c *Client) FissionGrant(ctx context.Context, activityID string, eligible bool, reqSource int) (int, json.RawMessage, error) {
	if reqSource == 0 {
		reqSource = 1
	}
	_ = eligible
	body := map[string]any{"activity_id": activityID}
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathFissionGrant, body, nil)
	if err != nil {
		return st, nil, err
	}
	return st, json.RawMessage(raw), nil
}

// BenefitGrant 领取通用权益（签到/新人/发消息/学生认证等）。
//
// 无 activity_id：服务端按账号当前的权益清单一次性发放可领项，重复调用是
// 幂等的（没有可领项时回 9069）。trae_client=Lite 与客户端一致；device_id
// 必带，否则 4001。
func (c *Client) BenefitGrant(ctx context.Context, deviceID string, reqSource int) (int, json.RawMessage, error) {
	if reqSource == 0 {
		reqSource = 1
	}
	if deviceID == "" {
		deviceID = c.DeviceID()
	}
	body := map[string]any{"trae_client": "Lite", "device_id": deviceID}
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathGrantWorkBenefit, body, nil)
	if err != nil {
		return st, nil, err
	}
	return st, json.RawMessage(raw), nil
}

// ActivityCode 从活动接口响应里取业务码。
//
// 约定（与 Trae 客户端 tb() 判定一致）：
//   - HTTP 2xx + code 缺失或 0  → 成功
//   - HTTP 2xx + code != 0      → 业务失败（活动暂不可用 9090 / 暂无领取权限 9070 等）
//
// 这个区分是必须的：上游 REST 层永远回 HTTP 200，只看状态码会把
// 「活动暂不可用」当成领取成功——网关此前正是如此，日志谎报已领取。
func IsPendingCode(code int) bool {
	switch code {
	case 9069, 9090, 9070:
		// 9069 活动暂不可用（pay/grant_work_benefit 权益路径）
		// 9090 活动暂不可用（ug/work_fission/grant 裂变路径）
		// 9070 暂无领取权限
		return true
	}
	return false
}

func ActivityCode(raw []byte) (code int, msg string, ok bool) {
	if len(raw) == 0 {
		return 0, "", false
	}
	var e struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return 0, "", false
	}
	if e.Code == nil {
		// 无 code 字段（如 student_auth_reward 回空对象 {}）= 受理成功。
		return 0, e.Message, true
	}
	return *e.Code, e.Message, true
}

// ---- 付费 / 额度 ----

// PayStatus 付费状态。
type PayStatus struct {
	Code              int             `json:"code"`
	Message           string          `json:"message"`
	EnableFission     bool            `json:"enable_fission"`
	EnableSoloBuilder bool            `json:"enable_solo_builder"`
	EnableSoloCoder   bool            `json:"enable_solo_coder"`
	EnableSoloLite    bool            `json:"enable_solo_lite"`
	IsPayFreshman     bool            `json:"is_pay_freshman"`
	SupportsCommerce  bool            `json:"is_support_commercialization"`
	Raw               json.RawMessage `json:"-"`
}

// QueryPayStatus 查询付费状态。
func (c *Client) QueryPayStatus(ctx context.Context) (*PayStatus, error) {
	body := map[string]any{"trae_client": "IDE", "device_id": c.DeviceID(), "req_source": 1}
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathPayStatus, body, nil)
	if err != nil {
		return nil, err
	}
	var p PayStatus
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("trae pay status http %d: %s", st, truncate(string(raw), 200))
	}
	p.Raw = json.RawMessage(raw)
	if p.Code == 1001 {
		return &p, ErrUnauthorized
	}
	return &p, nil
}

// EntitlementPack 权益包。
type EntitlementPack struct {
	Name      string          `json:"name"`
	Total     int64           `json:"total"`
	Used      int64           `json:"used"`
	Remaining int64           `json:"remaining"`
	ExpiresAt int64           `json:"expires_at"`
	Raw       json.RawMessage `json:"-"`
}

// Usage 额度用量快照。
type Usage struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Summary UsageSummary      `json:"usage_summary"`
	Packs   []json.RawMessage `json:"user_entitlement_pack_list"`
	// Remaining / Total 为聚合后的额度（以积分计，取整）。
	Remaining int64           `json:"-"`
	Total     int64           `json:"-"`
	Raw       json.RawMessage `json:"-"`
}

// UsageSummary 额度概览（Trae 以金额/积分计量）。
type UsageSummary struct {
	ConsumedAmount   float64 `json:"consumed_amount"`
	TotalAmount      float64 `json:"total_amount"`
	ConsumptionRatio float64 `json:"consumption_ratio"`
}

// QueryUsage 查询额度用量。
func (c *Client) QueryUsage(ctx context.Context, reqSource int) (*Usage, error) {
	if reqSource == 0 {
		reqSource = 1
	}
	body := map[string]any{"require_usage": true, "req_source": reqSource}
	st, raw, err := c.doJSON(ctx, "POST", UGBase+PathEntUsage, body, nil)
	if err != nil {
		return nil, err
	}
	var u Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("trae ent usage http %d: %s", st, truncate(string(raw), 200))
	}
	u.Raw = json.RawMessage(raw)
	u.aggregate()
	if u.Code == 1001 {
		return &u, ErrUnauthorized
	}
	return &u, nil
}

// aggregate 汇总剩余/总额度。优先用 usage_summary（真实口径），
// 缺失时回退到权益包字段累加（兼容多版本）。
func (u *Usage) aggregate() {
	total := int64(u.Summary.TotalAmount)
	rem := int64(u.Summary.TotalAmount - u.Summary.ConsumedAmount)
	if rem < 0 {
		rem = 0
	}
	if total <= 0 {
		// 回退路径：usage_summary 缺席时按权益包累加。额度在
		// entitlement_base_info.quota.credits_limit，已用在顶层 usage.credits_amount
		// （线上实测结构，2026-10-06），顶层 capacity/total 只是旧版兼容。
		for _, p := range u.Packs {
			var m map[string]any
			if json.Unmarshal(p, &m) != nil {
				continue
			}
			limit := int64(0)
			if bi, ok := m["entitlement_base_info"].(map[string]any); ok {
				if q, ok := bi["quota"].(map[string]any); ok {
					limit = numOf(q, "credits_limit", "total", "capacity")
				}
			}
			if limit == 0 {
				limit = numOf(m, "total", "total_credits", "credits_total", "capacity")
			}
			used := int64(0)
			if us, ok := m["usage"].(map[string]any); ok {
				used = numOf(us, "credits_amount", "used", "used_credits")
			}
			if used == 0 {
				used = numOf(m, "used", "used_credits", "credits_used")
			}
			total += limit
			r := numOf(m, "remaining", "remaining_credits", "credits_remaining", "left")
			if r == 0 {
				r = limit - used
			}
			if r > 0 {
				rem += r
			}
		}
	}
	u.Total = total
	u.Remaining = rem
}

func numOf(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		case string:
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// ---- 对话（SSE 流式） ----

// ChatStream 发起流式对话，返回 SSE 事件通道。
// body 为客户端原始请求体（可选做过字段归一化），
// 返回的 channel 逐条送出上游 SSE 的 `data:` 载荷；errCh 在上游报错时给出错误。
func (c *Client) ChatStream(ctx context.Context, body []byte, extraHeaders map[string]string) (<-chan string, <-chan error, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", AgentDomain+PathChat, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	c.commonHeaders(req.Header)
	c.authHeader(req.Header)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return nil, nil, fmt.Errorf("trae chat http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	ct := resp.Header.Get("Content-Type")
	dataCh := make(chan string, 64)
	errCh := make(chan error, 1)

	// SSE 事件流。
	if strings.Contains(ct, "text/event-stream") {
		go func() {
			defer close(dataCh)
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				// 同时透传 event: 与 data: 两行，保持顺序让上层能按事件类型解析。
				if strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "event:") {
					dataCh <- line
				}
				if ctx.Err() != nil {
					return
				}
			}
			if err := sc.Err(); err != nil && ctx.Err() == nil {
				errCh <- err
			}
		}()
		return dataCh, errCh, nil
	}

	// 非 SSE：整体读回后单条送出（上游降级为 JSON 响应时）。
	go func() {
		defer close(dataCh)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if len(raw) > 0 {
			dataCh <- string(raw)
		}
	}()
	return dataCh, errCh, nil
}

// ModelList 拉取上游模型清单。typ 为 "chat" 或 "llm_raw_chat"。
func (c *Client) ModelList(ctx context.Context, typ string) (json.RawMessage, error) {
	if typ == "" {
		typ = "chat"
	}
	st, raw, err := c.doJSON(ctx, "POST", AgentDomain+PathModelList, map[string]any{"type": typ}, nil)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, fmt.Errorf("trae model_list http %d: %s", st, truncate(string(raw), 200))
	}
	return json.RawMessage(raw), nil
}

// ---- 助手 ----

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
