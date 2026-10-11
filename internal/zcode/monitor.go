package zcode

// monitor.go —— ZCode「真实积分/额度」契约（2026-10-08 逆向还原，逐条线上实测）。
//
// 背景：交接文档 §3.1 记「Trae/ZCode 的积分无法计量，只能估算」。对 ZCode 而言
// 这条结论是**错的** —— 它的积分真值一直有公开接口，只是不在 zcode-plan 命名空间下，
// 而在 BigModel/Z.AI 的 monitor 命名空间里。官方桌面端 asar：
//
//	_out_host_index.js @1921808  Sm()  createBigModelUsageHeaders
//	_out_host_index.js @1897123  C5e  = "/api/biz/subscription/list"
//	_out_host_index.js @1927502  nwe() buildCreditUsageMonitorUrl
//	_out_host_index.js @1867503  G5e  = "/api/monitor/usage/quota/limit"
//
// 端点（base = https://open.bigmodel.cn 或 https://api.z.ai，两者同一后端）：
//
//	GET /api/monitor/usage/quota/limit?type=1
//	     → data.limits[] = {type,unit,number,usage,currentValue,remaining,
//	                        percentage,nextResetTime,usageDetails[]}
//	       这就是「每周 10000 积分」的那 10000：type=TIME_LIMIT 那条是周期额度，
//	       usageDetails[] 给出每个模型各自吃了多少。
//	       type=2 是团队维度（需 bigmodel-organization/project 头）。
//	GET /api/monitor/credit-usage/activity?type=1&startTime=&endTime=
//	     → data.series[].totalCredits = 当天真实消耗积分（字符串小数，4 位）
//	       这是**积分真值**，逐日给出，可直接替代估算。
//	GET /api/monitor/credit-usage/usage-detail?...&usageType=MODEL|MCP
//	     → data.summary.{totalCredits,cacheHitRate,offPeakUsageRate,averageDailyCredits}
//	       都是 {value,trend} 形态，值同样是字符串小数。
//
// 鉴权（实测 2026-10-08）：
//
//	Authorization: <id>.<secret>          ← coding-plan 的 API key，单头即可
//	Authorization: Bearer <id>.<secret>    ← 亦可
//	X-Bigmodel-Authorization: <同值>        ← 客户端会带，可省
//
// 反例（同一把 key）：
//
//	type=3 无参数      → 空体
//	/api/v1/zcode-plan/billing/balance   → 400 code 3001 parameter error（该命名空间只认官方客户端签名）
//	用错 host           → {"code":401,"msg":"token expired or incorrect"}
//
// 无套餐账号的合法回包（**不是错误**，是「该号没买 Coding Plan」）：
//
//	{"code":500,"msg":"当前用户不存在coding plan","success":false}
//
// 所以调用方必须区分：code 500 + msg 含「不存在 coding plan」= 无套餐（返回空快照，
// 不报错、不把账号标记异常）；其他才是真失败。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// monitor 命名空间端点。
const (
	// PathMonitorQuotaLimit 周期额度上限（「每周 10000 积分」的那条）。
	PathMonitorQuotaLimit = "/api/monitor/usage/quota/limit"
	// PathMonitorCreditActivity 逐日积分消耗（积分真值）。
	PathMonitorCreditActivity = "/api/monitor/credit-usage/activity"
	// PathMonitorCreditDetail 积分总览（按模型 / MCP 拆分）。
	PathMonitorCreditDetail = "/api/monitor/credit-usage/usage-detail"
	// PathMonitorSubscriptionList 订阅列表（拿方案名/产品 ID）。
	PathMonitorSubscriptionList = "/api/biz/subscription/list"
)

// MonitorHostBigModel / MonitorHostZAI 是 monitor 命名空间的两个等价宿主。
// 同一把 key 在两边都能过（后端同一套），但账号属地不同时只有一边有数据，
// 所以调用方按「先属地、后另一个」的顺序回落。
const (
	MonitorHostBigModel = "https://open.bigmodel.cn"
	MonitorHostZAI      = "https://api.z.ai"
)

// MonitorLimit 一条额度限制（官方 quota/limit 的 limits[] 元素）。
//
// 数值语义（实测）：number=周期总额度，usage=已用，remaining=剩余，
// percentage=已用比例（0~100）。type=TIME_LIMIT 的那条即"本周期积分额度"。
type MonitorLimit struct {
	Type          string  `json:"type"`
	Unit          int     `json:"unit"`
	Number        int64   `json:"number"`
	Usage         int64   `json:"usage"`
	CurrentValue  int64   `json:"currentValue"`
	Remaining     int64   `json:"remaining"`
	Percentage    float64 `json:"percentage"`
	NextResetTime int64   `json:"nextResetTime"`
	// UsageDetails 本限制下每个模型的用量明细（modelCode/displayName/usage）。
	UsageDetails []MonitorUsageDetail `json:"usageDetails"`
}

// MonitorUsageDetail 单模型用量明细。
type MonitorUsageDetail struct {
	ModelCode   string `json:"modelCode"`
	DisplayName string `json:"displayName"`
	Usage       int64  `json:"usage"`
}

// QuotaLimit quota/limit 的 data 段。
type QuotaLimit struct {
	Limits []MonitorLimit `json:"limits"`
	Level  string         `json:"level"`
}

// CreditActivityDay 一天的积分消耗（credit-usage/activity 的 series[] 元素）。
type CreditActivityDay struct {
	Date         string `json:"date"`
	TotalCredits string `json:"totalCredits"` // 字符串小数（4 位），真积分
	TotalTokens  int64  `json:"totalTokens"`
	McpCalls     int64  `json:"mcpCalls"`
}

// CreditsFloat 把 totalCredits 字符串转 float64（解析失败回 0）。
func (d CreditActivityDay) CreditsFloat() float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(d.TotalCredits), 64)
	if err != nil {
		return 0
	}
	return f
}

// CreditActivity credit-usage/activity 的 data 段。
type CreditActivity struct {
	Granularity string `json:"granularity"`
	Timezone    string `json:"timezone"`
	Summary     struct {
		TotalTokens          int64 `json:"totalTokens"`
		PeakDailyTokens      int64 `json:"peakDailyTokens"`
		TotalUsageDurationMs int64 `json:"totalUsageDurationMs"`
		CurrentStreakDays    int64 `json:"currentStreakDays"`
		LongestStreakDays    int64 `json:"longestStreakDays"`
	} `json:"summary"`
	Series []CreditActivityDay `json:"series"`
}

// TotalCredits 区间内消耗积分合计（= series 求和，真值）。
func (a *CreditActivity) TotalCredits() float64 {
	if a == nil {
		return 0
	}
	var sum float64
	for _, d := range a.Series {
		sum += d.CreditsFloat()
	}
	return sum
}

// TodayCredits 返回 series 里指定日期（YYYY-MM-DD）的积分；缺失回 0。
func (a *CreditActivity) DayCredits(date string) float64 {
	if a == nil {
		return 0
	}
	for _, d := range a.Series {
		if d.Date == date {
			return d.CreditsFloat()
		}
	}
	return 0
}

// MonitorMetric 上游 {value,trend} 形态的指标。
type MonitorMetric struct {
	Value string `json:"value"`
	Trend string `json:"trend"`
}

// Float 解析指标数值（失败回 0）。
func (m MonitorMetric) Float() float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(m.Value), 64)
	if err != nil {
		return 0
	}
	return f
}

// CreditDetail credit-usage/usage-detail 的 data 段。
type CreditDetail struct {
	Granularity string `json:"granularity"`
	Timezone    string `json:"timezone"`
	Summary     struct {
		CacheHitRate        MonitorMetric `json:"cacheHitRate"`
		OffPeakUsageRate    MonitorMetric `json:"offPeakUsageRate"`
		TotalCredits        MonitorMetric `json:"totalCredits"`
		AverageDailyCredits MonitorMetric `json:"averageDailyCredits"`
	} `json:"summary"`
	ModelUsage json.RawMessage `json:"modelUsage"`
}

// MonitorUnavailableError 表示「账号本身没问题，只是没有 Coding Plan 套餐」。
//
// 为什么单独建类型：调用方（面板「余额」、调度器）需要把这两种情况分开——
// 无套餐时额度就是 0，不该把账号标成异常、也不该刷错误日志。
type MonitorUnavailableError struct {
	Host string
	Msg  string
}

func (e *MonitorUnavailableError) Error() string {
	return fmt.Sprintf("zcode monitor %s: %s", e.Host, e.Msg)
}

// IsNoCodingPlan 判定上游 msg 是否表示「该号没有 Coding Plan」。
//
// 实测原文：`当前用户不存在coding plan`（英文端可能回 no coding plan）。
func IsNoCodingPlan(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "不存在coding plan") ||
		strings.Contains(m, "no coding plan") ||
		strings.Contains(m, "coding plan not") ||
		strings.Contains(m, "not subscribed")
}

// monitorHosts 返回本次要尝试的宿主顺序：属地优先，另一个兜底。
//
// 账号属地来自 Options.MonitorHost（server 层按凭据来源填 open.bigmodel.cn /
// api.z.ai）；未填时默认 BigModel，再补 ZAI。
func (c *Client) monitorHosts() []string {
	first := strings.TrimRight(c.monitorBase, "/")
	if first == "" {
		first = MonitorHostBigModel
	}
	alt := MonitorHostZAI
	if first == MonitorHostZAI {
		alt = MonitorHostBigModel
	}
	return []string{first, alt}
}

// monitorHeaders 写入 monitor 命名空间的鉴权头。
//
// 单 Authorization 即可（实测 bearer / raw 都过）；X-Bigmodel-Authorization 与
// 团队维度头按需带。故意**不带** X-Device-Mid / X-Platform 这类客户端头——
// monitor 端点不校验它们，带上反而可能被新网关策略拒。
func (c *Client) monitorHeaders(h http.Header, cred string) {
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "ZCode/"+c.appVer)
	h.Set("X-ZCode-App-Version", c.appVer)
	h.Set("Authorization", cred)
	h.Set("X-Bigmodel-Authorization", cred)
}

// monitorCredential 取本账号用于 monitor 的凭据。
//
// 优先级：
//  1. APIKey（api_key 模式的主凭证，"<id>.<secret>" 或 "sk-..."）
//  2. AccessToken（JWT / OAuth token，做成 "Bearer <tok>"）
//
// 为什么不是 SignKeyID/SignSecret：monitor 端点走的是**平台 API key 鉴权**，
// 与对话端点的 Ed25519 客户端签名是两套东西（实测签名头在这里完全不被识别）。
func (c *Client) monitorCredential() string {
	if k := strings.TrimSpace(c.a.APIKeyValue()); k != "" {
		return k
	}
	if t := strings.TrimSpace(c.a.TokenValue()); t != "" {
		return "Bearer " + t
	}
	return ""
}

// monitorGet 打一次 monitor GET，返回 (状态码, 信封, 原始体, 错误)。
// 先属地后另一宿主依次尝试；每次都把「无套餐」识别出来单独回。
func (c *Client) monitorGet(ctx context.Context, path string, q url.Values) (int, *envelope, []byte, error) {
	cred := c.monitorCredential()
	if cred == "" {
		return 0, nil, nil, fmt.Errorf("zcode monitor: 账号缺少 APIKey/Token 凭据")
	}
	var lastErr error
	for _, host := range c.monitorHosts() {
		u := host + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		c.monitorHeaders(req.Header, cred)
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw := readAllLimited(resp.Body, 8<<20)
		_ = resp.Body.Close()
		env, perr := parseEnvelope(raw)
		if perr != nil {
			lastErr = fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
			continue
		}
		// 无套餐：合法回包，直接返回（不再换宿主——另一个宿主一样没套餐）。
		if IsNoCodingPlan(env.Msg) {
			return resp.StatusCode, env, raw, &MonitorUnavailableError{Host: host, Msg: env.Msg}
		}
		// 鉴权失败类：换宿主再试。
		if env.Code == 401 || env.Code == 403 || strings.Contains(strings.ToLower(env.Msg), "token expired") ||
			strings.Contains(env.Msg, "令牌已过期") {
			lastErr = fmt.Errorf("monitor %s code=%d msg=%s", host, env.Code, env.Msg)
			continue
		}
		return resp.StatusCode, env, raw, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("zcode monitor: 所有宿主均失败")
	}
	return 0, nil, nil, lastErr
}

// QuotaLimit 拉周期额度上限（「每周 N 积分」的那条）。
//
// teamContext 为真时带 type=2（团队维度），否则 type=1（个人）。
func (c *Client) QuotaLimit(ctx context.Context, teamContext bool) (*QuotaLimit, error) {
	q := url.Values{}
	if teamContext {
		q.Set("type", "2")
	} else {
		q.Set("type", "1")
	}
	_, env, _, err := c.monitorGet(ctx, PathMonitorQuotaLimit, q)
	if err != nil {
		return nil, err
	}
	if env.Code != 200 && env.Code != 0 {
		return nil, fmt.Errorf("quota/limit code=%d msg=%s", env.Code, env.Msg)
	}
	var data QuotaLimit
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("quota/limit decode: %w", err)
		}
	}
	return &data, nil
}

// CreditActivity 拉「最近 days 天」的逐日积分消耗（真值）。
//
//	startTime/endTime 用 "YYYY-MM-DD HH:MM:SS"（空格必须转义，url.Values 会做）。
//
// 返回的 Series 逐日给 totalCredits；这是 ZCode 唯一可信的积分口径，
// 网关不再需要估算。
func (c *Client) CreditActivity(ctx context.Context, days int) (*CreditActivity, error) {
	if days <= 0 {
		days = 1
	}
	now := time.Now()
	start := now.AddDate(0, 0, -(days-1)).Format("2006-01-02") + " 00:00:00"
	end := now.Format("2006-01-02") + " 23:59:59"
	q := url.Values{}
	q.Set("type", "1")
	q.Set("startTime", start)
	q.Set("endTime", end)
	_, env, _, err := c.monitorGet(ctx, PathMonitorCreditActivity, q)
	if err != nil {
		return nil, err
	}
	if env.Code != 200 && env.Code != 0 {
		return nil, fmt.Errorf("credit-usage/activity code=%d msg=%s", env.Code, env.Msg)
	}
	var data CreditActivity
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("credit-usage/activity decode: %w", err)
		}
	}
	return &data, nil
}

// CreditDetail 拉积分总览（按 usageType=MODEL|MCP 拆分）。
func (c *Client) CreditDetail(ctx context.Context, days int, usageType string) (*CreditDetail, error) {
	if days <= 0 {
		days = 1
	}
	if usageType == "" {
		usageType = "MODEL"
	}
	now := time.Now()
	start := now.AddDate(0, 0, -(days-1)).Format("2006-01-02") + " 00:00:00"
	end := now.Format("2006-01-02") + " 23:59:59"
	q := url.Values{}
	q.Set("type", "1")
	q.Set("startTime", start)
	q.Set("endTime", end)
	q.Set("usageType", usageType)
	_, env, _, err := c.monitorGet(ctx, PathMonitorCreditDetail, q)
	if err != nil {
		return nil, err
	}
	if env.Code != 200 && env.Code != 0 {
		return nil, fmt.Errorf("credit-usage/usage-detail code=%d msg=%s", env.Code, env.Msg)
	}
	var data CreditDetail
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("credit-usage/usage-detail decode: %w", err)
		}
	}
	return &data, nil
}
