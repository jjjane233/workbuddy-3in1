// Package zcode 实现 ZCode（zcode.z.ai）上游协议：
//
//	对话        POST /api/v1/zcode-plan/anthropic/v1/messages   （Anthropic Messages 协议）
//	方案        GET  /api/v1/zcode-plan/billing/current
//	余额        GET  /api/v1/zcode-plan/billing/balance?app_version=<ver>
//	领取预览    GET  /api/v1/zcode-plan/billing/preview?app_version=<ver>&platform=<p>
//	手动领取    POST /api/v1/zcode-plan/billing/claim             （需无痕验证）
//	积分总览    GET  /api/v1/incentive/referral/summary
//	邀请列表    GET  /api/v1/incentive/referral/invitees?limit=N
//	奖励记录    GET  /api/v1/incentive/reward-records?limit=N
//	绑定邀请码  POST /zcode-api/incentive/referral/bind-code
//
// JWT 模式下对话需携带阿里云无痕验证参数（X-Aliyun-Captcha-Verify-Param）；
// 若配置了客户端签名凭据（"<keyId>.<secret>"），则叠加 Ed25519 请求签名（见 sign.go）。
package zcode

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	BaseURL  = "https://zcode.z.ai"
	PathChat = "/api/v1/zcode-plan/anthropic/v1/messages"
	// BigModel Anthropic-compatible chat endpoint (official client uses this for chat).
	BaseURLBigModel      = "https://open.bigmodel.cn"
	PathChatBigModel     = "/api/anthropic/v1/messages"
	PathBillingCur       = "/api/v1/zcode-plan/billing/current"
	PathBillingBal       = "/api/v1/zcode-plan/billing/balance"
	PathBillingPrev      = "/api/v1/zcode-plan/billing/preview"
	PathBillingClaim     = "/api/v1/zcode-plan/billing/claim"
	PathClientCfg        = "/api/v1/client/configs"
	PathAgentCfg         = "/api/v1/agent/configs"
	PathRewardSummary    = "/api/v1/incentive/referral/summary"
	PathReferralInvitees = "/api/v1/incentive/referral/invitees"
	PathRewardRecords    = "/api/v1/incentive/reward-records"
	PathBindCode         = "/zcode-api/incentive/referral/bind-code"
)

// DefaultAppVersion 出站客户端版本（对齐官方桌面端 3.14.x）。
const DefaultAppVersion = "3.14.3"

// Client 一个账号的上游客户端。
type Client struct {
	a       *auth.Auth
	hc      *http.Client
	appVer  string
	baseURL string
	signer  *Signer
	// monitorBase 是 monitor 命名空间（quota/limit、credit-usage）的宿主：
	// 账号属地决定首选（open.bigmodel.cn / api.z.ai），另一个自动兜底。
	// 见 monitor.go 的 monitorHosts()。
	monitorBase string
}

// Options 构造参数。
type Options struct {
	AppVersion string
	Timeout    time.Duration
	HTTPClient *http.Client
	BaseURL    string
	// MonitorHost 指定 monitor 命名空间（额度/积分真值）的首选宿主，
	// 取 MonitorHostBigModel / MonitorHostZAI；空 = BigModel。
	MonitorHost string
}

// New 构造账号客户端（签名器惰性创建）。
func New(a *auth.Auth, o Options) *Client {
	hc := o.HTTPClient
	if hc == nil {
		to := o.Timeout
		if to <= 0 {
			to = 120 * time.Second
		}
		hc = &http.Client{Timeout: to}
	}
	av := o.AppVersion
	if av == "" {
		av = DefaultAppVersion
	}
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = BaseURL
	}
	return &Client{a: a, hc: hc, appVer: av, baseURL: base, monitorBase: o.MonitorHost}
}

var _ = io.Discard

// readAllLimited 读满 limit 字节（供 monitor.go 复用；与 doJSON 同口径）。
func readAllLimited(rc io.ReadCloser, limit int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(rc, limit))
	return b
}

// ---- 出站请求头 ----

func (c *Client) deviceMid() string {
	// ZCode 服务端要求 X-Device-Mid / device_mid 为合法 UUID（zod .uuid()），
	// 否则返回 code 3001 parameter error。
	if IsUUID(c.a.DeviceID) {
		return c.a.DeviceID
	}
	return ""
}

// effectiveDeviceMid 返回可安全放进 query / 头的 device_mid：
// 优先账号里真实的 UUID；没有就按 UID 派生一个稳定的。
func (c *Client) effectiveDeviceMid() string {
	if dm := c.deviceMid(); dm != "" {
		return dm
	}
	return derivedDeviceMid(c.a.UID)
}

// IsUUID 判断 s 是否是标准 8-4-4-4-12 十六进制 UUID。
//
// 为什么单独判定而不用正则库：这里只需要长度/分隔/字符集三条硬规则，
// 用不着引 regexp；而且这个函数在每次 billing 查询都会走，越轻越好。
//
// 为什么必须判定：auth 里的 DeviceID 在手工写 / 老文件导入时可能是空、
// 可能被塞成 UID（纯数字），线上实测这两种值都会让 billing/* 回
// 400 parameter error——比不传更糟，因为不传时至少 current 兜得住。
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// derivedDeviceMid 在账号没有合法 UUID 时给出一个稳定的替代值。
//
// 为什么用 UID 派生而不是随机：device_mid 只是「同一客户端别每次变脸」的
// 标识，上游不拿它做鉴权（实测任意合法 UUID 都 200）。派生保证同一账号
// 每次请求值一致，避免被当成异常客户端；同时不需要落盘新字段。
// 非 UUID 的 UID 会走 fd00::/8 里的一种稳定映射（下面手工拼 128bit）。
func derivedDeviceMid(seed string) string {
	if seed == "" {
		return ""
	}
	h := sha256.Sum256([]byte("zcode-device-mid:" + seed))
	// 直接按 UUID v4 形态摆放前 16 字节；version/variant 位按 RFC 4122 摆好。
	var b [16]byte
	copy(b[:], h[:16])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexd[v>>4], hexd[v&0x0f])
	}
	return string(out)
}

// commonHeaders 写入 ZCode 客户端通用头。
func (c *Client) commonHeaders(h http.Header) {
	h.Set("Accept", "application/json")
	h.Set("X-Client-Language", "zh-CN")
	h.Set("X-Client-Timezone", "Asia/Shanghai")
	h.Set("HTTP-Referer", c.baseURL+"/")
	h.Set("User-Agent", "ZCode/"+c.appVer)
	h.Set("X-ZCode-App-Version", c.appVer)
	h.Set("X-Platform", "win32-x64")
	h.Set("X-Os-Category", "windows")
	h.Set("X-Title", "Z Code@win32-x64")
	h.Set("X-Device-Mid", c.deviceMid())
}

// bigModelHeaders writes headers for the BigModel Anthropic-compatible endpoint.
func (c *Client) bigModelHeaders(h http.Header) {
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "ZCode/"+c.appVer)
	h.Set("X-ZCode-App-Version", c.appVer)
	if k := c.a.APIKeyValue(); k != "" {
		h.Set("Authorization", "Bearer "+k)
	}
}

func (c *Client) authHeader(h http.Header) {
	if c.a.Mode == auth.ModeAPIKey {
		if k := c.a.APIKeyValue(); k != "" {
			h.Set("x-api-key", k)
		}
		return
	}
	if t := c.a.TokenValue(); t != "" {
		h.Set("Authorization", "Bearer "+t)
	}
}

// ---- 通用请求 ----

// doJSON 发起一次 JSON 请求并解包 {code,msg,data} 信封；返回 data 原始字节。
func (c *Client) doJSON(ctx context.Context, method, path string, body any, query map[string]string, headers map[string]string) (int, []byte, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		var b strings.Builder
		b.WriteString("?")
		first := true
		for k, v := range query {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(k + "=" + v)
		}
		u += b.String()
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	c.commonHeaders(req.Header)
	c.authHeader(req.Header)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// envelope 解 {code,msg,data}。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func parseEnvelope(raw []byte) (*envelope, error) {
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ---- 方案 / 余额 ----

// Plan 一个已激活方案。
type Plan struct {
	PlanID       string        `json:"plan_id"`
	Name         string        `json:"name"`
	Status       string        `json:"status"`
	StartsAt     int64         `json:"starts_at"`
	EndsAt       int64         `json:"ends_at"`
	Entitlements []Entitlement `json:"entitlements"`
}

// Entitlement 方案内的一项权益。
type Entitlement struct {
	EntitlementID string `json:"entitlement_id"`
	ShowName      string `json:"show_name"`
	Meter         string `json:"meter"`
	UnitType      string `json:"unit_type"`
	GrantUnits    int64  `json:"grant_units"`
	Period        string `json:"period"`
	// Capabilities 该权益可用的模型能力标识，形如 ["model:glm-5.3-flash"]。
	//
	// 上游实测（2026-10-06，zcode-v3-start-plan-trust-1006）：
	// 一次领取的 1 亿 token 只在 capabilities 列出的模型上生效，
	// 若不把它透出到面板，用户会拿到"100000000 / 100000000 却调不动任何模型"。
	Capabilities []string `json:"capabilities"`
	// Priority 权益优先级（同一 meter 下多份权益的取用顺序，越小越先）。
	Priority int `json:"priority"`
	// EffectiveAt 权益生效时间（unix 秒，0 = 立即生效）。
	EffectiveAt int64 `json:"effective_at"`
}

// Balance 一个模型额度桶。
type Balance struct {
	ShowName       string `json:"show_name"`
	Model          string `json:"model"`
	TotalUnits     int64  `json:"total_units"`
	UsedUnits      int64  `json:"used_units"`
	RemainingUnits int64  `json:"remaining_units"`
	ExpiresAt      int64  `json:"expires_at"`
	// Capabilities 本桶可用的模型能力标识（形如 ["model:glm-5.3-flash"]）。
	// 这是"这 1 亿到底能喂给哪些模型"的唯一权威来源——面板必须显示它，
	// 否则用户只看到余额，不知道能用在谁身上。
	Capabilities []string `json:"capabilities"`
	// UnitType 计量单位："token" / "credit"（面板据此决定是否叫"积分"）。
	UnitType string `json:"unit_type"`
	// Meter 计量维度："model_usage" 等。
	Meter string `json:"meter"`
	// PlanID 本桶归属的方案 ID（与 plans[].plan_id 对应）。
	PlanID string `json:"plan_id"`
	// PeriodStart / PeriodEnd 本桶的周期（unix 秒）。one_time 包两者相同。
	PeriodStart int64 `json:"period_start"`
	PeriodEnd   int64 `json:"period_end"`
	// AvailableUnits 上游给出的"当前可动用"额度（含未过期判定后的结果）。
	AvailableUnits int64 `json:"available_units"`
}

// Billing 余额快照。
type Billing struct {
	Plans      []Plan    `json:"plans"`
	Balances   []Balance `json:"balances"`
	ServerTime int64     `json:"server_time"`
}

// Balance 查询余额（含方案）。返回解析后的 Billing。
func (c *Client) Balance(ctx context.Context) (*Billing, int, error) {
	q := c.billingQuery()
	st, raw, err := c.doJSON(ctx, "GET", PathBillingBal, nil, q, nil)
	if err != nil {
		return nil, 0, err
	}
	if st == 200 {
		if b, perr := parseBilling(raw); perr == nil {
			return b, st, nil
		}
	}
	// fallback 1: drop device_mid (some edges only accept app_version).
	if st == 400 {
		if st2, raw2, err2 := c.doJSON(ctx, "GET", PathBillingBal, nil,
			map[string]string{"app_version": c.appVer}, nil); err2 == nil && st2 == 200 {
			if b, perr := parseBilling(raw2); perr == nil {
				return b, st2, nil
			}
		}
	}
	// fallback 2: billing/current (plans only, no device_mid required).
	if plans, cerr := c.Current(ctx); cerr == nil && len(plans) > 0 {
		return &Billing{Plans: plans, ServerTime: time.Now().Unix()}, 200, nil
	}
	return nil, st, fmt.Errorf("balance http %d: %s", st, truncate(string(raw), 200))
}

// billingQuery 组装 billing/* 端点的查询参数。
//
// 为什么必须有 device_mid：2026-10-06 实测，zcode.z.ai 的
// billing/balance、billing/preview 缺了 device_mid 一律回
// 400 {"code":3001,"msg":"parameter error"}；billing/current 不校验它
// （所以线上旧代码只有 balance 挂、current 却能过）。device_mid 与
// 账号绑定无关，任意合法 UUID 都能通过，但客户端真实值在 auth.DeviceID 里。
func (c *Client) billingQuery() map[string]string {
	q := map[string]string{"app_version": c.appVer, "platform": "win32-x64"}
	if dm := c.effectiveDeviceMid(); dm != "" {
		q["device_mid"] = dm
	}
	return q
}

func parseBilling(raw []byte) (*Billing, error) {
	e, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var data struct {
		Plans    []Plan    `json:"plans"`
		Balances []Balance `json:"balances"`
	}
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &data)
	}
	return &Billing{Plans: data.Plans, Balances: data.Balances, ServerTime: time.Now().Unix()}, nil
}

// Current 查询当前方案。
func (c *Client) Current(ctx context.Context) ([]Plan, error) {
	_, raw, err := c.doJSON(ctx, "GET", PathBillingCur, nil, map[string]string{"app_version": c.appVer}, nil)
	if err != nil {
		return nil, err
	}
	e, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var data struct {
		Plans []Plan `json:"plans"`
	}
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &data)
	}
	return data.Plans, nil
}

// Remaining 汇总余额：返回剩余总量、总额度、最早到期（unix 秒）。
//
// 【口径警告】这里是把各桶的 remaining_units / total_units **算术相加**，不区分
// unit_type——一个账号同时持有积分桶与 token 桶时，得到的和没有任何物理意义。
// 调用方必须用 SoleUnitType 确认口径后再解释这个数字（池内据此标注 creditsUnit）。
func (b *Billing) Remaining() (remain, total int64, earliest int64) {
	for _, x := range b.Balances {
		remain += x.RemainingUnits
		total += x.TotalUnits
		if x.RemainingUnits > 0 && x.ExpiresAt > 0 {
			if earliest == 0 || x.ExpiresAt < earliest {
				earliest = x.ExpiresAt
			}
		}
	}
	return
}

// SoleUnitType 返回账单里唯一出现的 unit_type（"token" / "credit"）。
//
// 桶为空、或多种单位混用（同一账号既有积分桶又有 token 桶）时返回空串。返回空串
// 是**有意的**：调用方据此退回保守口径，而不是拿一个混了两种单位的和去冒充某一种
// 单位（见 Remaining 的口径警告）。上游新增未知单位时同样返回该原值，由
// pool.CreditsUnitFromUpstream 决定如何归类（未识别 → 积分口径，即历史行为）。
func (b *Billing) SoleUnitType() string {
	unit := ""
	for _, x := range b.Balances {
		u := strings.TrimSpace(x.UnitType)
		if u == "" {
			continue
		}
		if unit == "" {
			unit = u
			continue
		}
		if !strings.EqualFold(unit, u) {
			return "" // 混用：不表态
		}
	}
	return unit
}

// ---- 上游拒绝诊断 ----

// blockedCode 上游「风控拦截」的业务码。
//
// 实测（2026-10-06，zcode.z.ai /api/v1/zcode-plan/anthropic/v1/messages）：
//
//	账号 JWT 合法、billing/* 全部 200，
//	但对话端点 不论带不带签名都回 405 + 该码。
//
// 区分它有意义：让调用方知道「账号没问题，是端点被拦」。
const blockedCode = 3012

// IsBlocked 判定错误/响应文本是否为上游风控拦截（3012 / unusual activity）。
func IsBlocked(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, "\"code\":3012") || strings.Contains(s, "code=3012") {
		return true
	}
	return strings.Contains(s, "unusual activity") || strings.Contains(s, "request has been blocked")
}

// IsModelNotEntitled 判定是否为「套餐不含该模型」。
func IsModelNotEntitled(s string) bool {
	if s == "" {
		return false
	}
	for _, k := range []string{"not entitled", "not_entitled", "coding_plan_not_entitled", "no permission", "3008"} {
		if strings.Contains(strings.ToLower(s), k) {
			return true
		}
	}
	return false
}

// ---- 套餐可用模型 ----

// EntitledModel 一个"由额度包授权"的模型。
type EntitledModel struct {
	// Model 能力标识去前缀后的模型名，如 "glm-5.3-flash"。
	Model string
	// ShowName 上游给的展示名，如 "GLM-5.3-Flash"。
	ShowName string
	// Source 这份授权来自哪一类载体："balance"（已到手的额度桶）
	// 或 "plan"（生效中的方案权益）。
	Source string
	// Remain / Total 该桶的剩余与总额度（Source=plan 时为 0）。
	Remain int64
	Total  int64
	// ExpiresAt unix 秒；0 = 未知。
	ExpiresAt int64
	// UnitType "token" / "credit"。
	UnitType string
}

// modelFromCapability 从 "model:glm-5.3-flash" 这类能力标识里取出模型名。
//
// 上游 capabilities 的取值域实测只有 "model:<id>"，但也留出裸模型名的兼容分支，
// 免得上游哪天把前缀去掉就整条链路失聪。
func modelFromCapability(cap string) string {
	c := strings.TrimSpace(cap)
	if c == "" {
		return ""
	}
	if i := strings.LastIndex(c, ":"); i >= 0 && strings.HasPrefix(c, "model:") {
		return strings.TrimSpace(c[i+1:])
	}
	return c
}

// EntitledModels 汇总本账号当前**真正有权调用**的模型，去重后按首次出现顺序返回。
//
// 为什么必须有这个函数：
//
//	GET /api/v1/client/configs 是**匿名公共目录**，与账号、套餐无关。上游那份表里
//	只有 GLM-5.2 / GLM-5-Turbo（2026-10-06 实测），而 Start Plan 送的
//	GLM-5.3-Flash 根本不在其中。于是面板「模型与档位」页面会出现
//	"我有 1 亿 GLM-5.3-Flash 额度，模型列表里却找不到 GLM-5.3-Flash"。
//
//	真正决定"能不能调"的是 billing 返回的 capabilities，所以把它单独成表，
//	与公共目录分开呈现，两者都显示、都标来源。
func (b *Billing) EntitledModels() []EntitledModel {
	seen := map[string]int{}
	out := make([]EntitledModel, 0, 8)
	push := func(m EntitledModel) {
		key := strings.ToLower(m.Model)
		if key == "" {
			return
		}
		if i, ok := seen[key]; ok {
			// 已存在：补全信息，不重复出行。
			if out[i].ShowName == "" {
				out[i].ShowName = m.ShowName
			}
			if out[i].Remain == 0 {
				out[i].Remain = m.Remain
			}
			if out[i].Total == 0 {
				out[i].Total = m.Total
			}
			if out[i].ExpiresAt == 0 {
				out[i].ExpiresAt = m.ExpiresAt
			}
			return
		}
		seen[key] = len(out)
		out = append(out, m)
	}

	for _, x := range b.Balances {
		for _, c := range x.Capabilities {
			push(EntitledModel{
				Model: modelFromCapability(c), ShowName: x.ShowName, Source: "balance",
				Remain: x.RemainingUnits, Total: x.TotalUnits, ExpiresAt: x.ExpiresAt,
				UnitType: x.UnitType,
			})
		}
	}
	for _, p := range b.Plans {
		for _, e := range p.Entitlements {
			for _, c := range e.Capabilities {
				push(EntitledModel{
					Model: modelFromCapability(c), ShowName: e.ShowName, Source: "plan",
					ExpiresAt: p.EndsAt, UnitType: e.UnitType,
				})
			}
		}
	}
	return out
}

// ModelNames 返回去重后的模型标识列表。
func (b *Billing) ModelNames() []string {
	ms := b.EntitledModels()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Model)
	}
	return out
}

// ---- 领取积分 ----

// Preview 返回可领取的方案列表（需已登录）。
func (c *Client) Preview(ctx context.Context) ([]Plan, error) {
	_, raw, err := c.doJSON(ctx, "GET", PathBillingPrev, nil, c.billingQuery(), nil)
	if err != nil {
		return nil, err
	}
	e, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var data struct {
		Plans []Plan `json:"plans"`
	}
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &data)
	}
	return data.Plans, nil
}

// ClaimResult 领取结果。
type ClaimResult struct {
	OK     bool   `json:"ok"`
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
	PlanID string `json:"plan_id,omitempty"`
	EndsAt int64  `json:"ends_at,omitempty"`
	Raw    string `json:"raw,omitempty"`
}

// Claim 手动领取指定方案（需无痕验证参数 verifyParam）。
func (c *Client) Claim(ctx context.Context, planID, verifyParam, region string) (*ClaimResult, error) {
	hdr := map[string]string{
		"X-ZCode-App-Version": c.appVer,
		"X-Platform":          "win32-x64",
	}
	if verifyParam != "" {
		hdr["X-Aliyun-Captcha-Verify-Param"] = verifyParam
	}
	if region != "" {
		hdr["X-Aliyun-Captcha-Verify-Region"] = region
	}
	// 同样要带 device_mid（见 billingQuery 注释）；漏了就是 400 parameter error。
	st, raw, err := c.doJSON(ctx, "POST", PathBillingClaim, map[string]string{"plan_id": planID},
		map[string]string{"device_mid": c.effectiveDeviceMid()}, hdr)
	if err != nil {
		return nil, err
	}
	e, err := parseEnvelope(raw)
	if err != nil {
		return nil, fmt.Errorf("claim http %d: %s", st, truncate(string(raw), 200))
	}
	res := &ClaimResult{Code: e.Code, Msg: e.Msg, Raw: truncate(string(raw), 400)}
	if e.Code == 0 {
		res.OK = true
		var data struct {
			Plan struct {
				PlanID string `json:"plan_id"`
				EndsAt int64  `json:"ends_at"`
			} `json:"plan"`
		}
		if len(e.Data) > 0 {
			_ = json.Unmarshal(e.Data, &data)
		}
		res.PlanID = data.Plan.PlanID
		res.EndsAt = data.Plan.EndsAt
	}
	return res, nil
}

// ---- 积分总览 / 邀请 ----

// Reward 一条奖励任务。
type Reward struct {
	RewardType string `json:"reward_type"`
	Rule       string `json:"rule"`
	State      string `json:"state"`
	Title      string `json:"title"`
	Progress   struct {
		Current int `json:"current"`
		Target  int `json:"target"`
	} `json:"progress"`
	GrantUnits int64  `json:"grant_units"`
	UnitType   string `json:"unit_type"`
	Period     string `json:"period"`
}

// ReferralSide 邀请关系一侧（invitee / inviter）。
type ReferralSide struct {
	Visible    bool     `json:"visible"`
	Rewards    []Reward `json:"rewards"`
	InviteCode string   `json:"invite_code"`
	State      string   `json:"state"`
}

// Summary 积分总览。
type Summary struct {
	Gameplay struct {
		Status string `json:"status"`
	} `json:"gameplay"`
	Invitee *ReferralSide `json:"invitee"`
	Inviter *ReferralSide `json:"inviter"`
}

// RewardSummary 拉取积分总览。
func (c *Client) RewardSummary(ctx context.Context) (*Summary, error) {
	_, raw, err := c.doJSON(ctx, "GET", PathRewardSummary, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	e, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var s Summary
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &s)
	}
	return &s, nil
}

// ClaimableRewards 返回总览中所有「进行中且未达上限」的奖励任务（供面板展示/定时领取提示）。
func (s *Summary) ClaimableRewards() []Reward {
	var out []Reward
	add := func(side *ReferralSide) {
		if side == nil || !side.Visible {
			return
		}
		for _, r := range side.Rewards {
			if r.State == "in_progress" {
				out = append(out, r)
			}
		}
	}
	add(s.Invitee)
	add(s.Inviter)
	return out
}

// Invitees / Records 分页拉取（面板用）。
func (c *Client) Invitees(ctx context.Context, limit int) (json.RawMessage, error) {
	if limit <= 0 {
		limit = 5
	}
	_, raw, err := c.doJSON(ctx, "GET", PathReferralInvitees, nil, map[string]string{"limit": fmt.Sprintf("%d", limit)}, nil)
	if err != nil {
		return nil, err
	}
	e, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	return e.Data, nil
}

// Records 奖励记录。
func (c *Client) Records(ctx context.Context, limit int) (json.RawMessage, error) {
	if limit <= 0 {
		limit = 5
	}
	_, raw, err := c.doJSON(ctx, "GET", PathRewardRecords, nil, map[string]string{"limit": fmt.Sprintf("%d", limit)}, nil)
	if err != nil {
		return nil, err
	}
	e, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	return e.Data, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- 客户端配置（模型目录）----
//
// GET /api/v1/client/configs?app_version=<ver>&platform=<platform>
//
// 该端点**匿名可访问**（不需要账号、不需要签名）——它是客户端启动时用来发现
// 可用 provider/模型/baseURL 的清单。面板「模型与档位」在 ZCode 分支直接读它。
type ClientConfigs struct {
	Providers []ProviderConfig `json:"providers"`
}

// ProviderConfig 一个 provider 及其模型清单。
type ProviderConfig struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Schema       string        `json:"schema"`
	BaseURL      string        `json:"baseUrl"`
	APIKeyURL    string        `json:"apiKeyUrl"`
	DefaultModel string        `json:"defaultModel"`
	Priority     int           `json:"priority"`
	Models       []ClientModel `json:"models"`
}

// ClientModel 模型条目。
type ClientModel struct {
	ModelID             string `json:"modelId"`
	Name                string `json:"name"`
	ContextWindow       int64  `json:"contextWindow"`
	MaxCompletionTokens int64  `json:"maxCompletionTokens"`
	Priority            int    `json:"priority"`
}

// FetchClientConfigs 拉取客户端配置（匿名端点）。
// hc 为 nil 时用默认 client；appVer 为空时用 DefaultAppVersion。
func FetchClientConfigs(ctx context.Context, hc *http.Client, appVer string) (*ClientConfigs, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if appVer == "" {
		appVer = DefaultAppVersion
	}
	req, err := http.NewRequestWithContext(ctx, "GET", BaseURL+PathClientCfg+"?app_version="+appVer+"&platform=win32-x64", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+appVer)
	req.Header.Set("X-ZCode-App-Version", appVer)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("client configs http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data *ClientConfigs `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("client configs decode: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("client configs code=%d msg=%s", env.Code, env.Msg)
	}
	if env.Data == nil {
		return nil, fmt.Errorf("client configs: empty data")
	}
	return env.Data, nil
}
