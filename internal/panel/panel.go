// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/httpauth"
	"github.com/jjjane233/workbuddy-3in1/internal/livecfg"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
	"github.com/jjjane233/workbuddy-3in1/internal/reqlog"
	"github.com/jjjane233/workbuddy-3in1/internal/scheduler"
	"github.com/jjjane233/workbuddy-3in1/internal/trae"
	"github.com/jjjane233/workbuddy-3in1/internal/upstream"
	"github.com/jjjane233/workbuddy-3in1/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder
	// RequestLog 请求指标与归档（nil = 对应接口返回 501）。
	RequestLog *reqlog.Recorder

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// TraeRelayPort 常驻回环中继的固定端口（Trae 授权页只认 127.0.0.1:<port>）。
	// 0 / 越界时回落 panel.defaultTraeRelayPort（17654）。见 trae_relay.go。
	TraeRelayPort int

	// PublicBaseURL 面板对外可达基址（如 http://your-gateway.example.com）。
	// 仅 Trae 授权码登录的回调地址需要它——回调由**用户浏览器**发起，必须写成
	// 公网可达地址而不是网关看到的 Host。留空时按请求头（X-Forwarded-Proto/Host
	// 或 r.Host）推导，nginx 反代场景下通常也能得到正确值。
	PublicBaseURL string
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState

	// upstream 三合一的 Trae / ZCode 操作（见 upstream_ops.go）。
	// nil = 未注入，相关按钮返回 501 而不是调用 CodeBuddy 接口打错上游。
	upstream UpstreamOps

	// relayMu/relay 常驻回环中继的最近心跳（见 trae_relay.go）。
	// 中继每 15s 上报一次；面板据此把 Trae 的回调端口固定为 17654，
	// 并在添加账号弹窗显示"常驻中继：在线/离线"。
	relayMu sync.Mutex
	relay   *traeRelayState

	// ulogins 进行中的 Trae / ZCode 浏览器登录会话（state → 会话）。
	// 与 logins（CodeBuddy 设备授权）分开存放：两者 state 语义、回调载体、
	// 轮询节奏都不同，混放会让 loginTTL 回收逻辑两边打架。
	// 见 login_upstream.go。
	ulogins map[string]*upstreamLogin
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
		ulogins: map[string]*upstreamLogin{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	// 三合一：Trae / ZCode 的浏览器登录（见 login_upstream.go）。
	p.mux.HandleFunc("POST /panel/api/login/trae/start", p.withAuth(p.loginTraeStart))
	p.mux.HandleFunc("GET /panel/api/login/trae/poll", p.withAuth(p.loginTraePoll))
	// 回调端点**不走 withAuth**：请求来自用户浏览器（Trae 服务端 307），
	// 携带的是授权码而非面板密钥；它写不进任何数据，只是把 query 暂存到
	// 该 state 的会话里，真正的换票在带鉴权的 poll 端做。
	// 回调同时收 GET（浏览器 307 直接落地）与 POST（本机中继把原始 query 原样转发，
	// 避免对含 JSON 的 authCodeInfo 做二次 URL 拼装而转义出错）。
	p.mux.HandleFunc("GET /panel/api/login/trae/callback", p.loginTraeCallback)
	p.mux.HandleFunc("POST /panel/api/login/trae/callback", p.loginTraeCallback)
	// 手工投递回调 URL（授权页只认回环回调，本机不跑中继时用粘贴兜底）。走鉴权。
	p.mux.HandleFunc("POST /panel/api/login/trae/submit", p.withAuth(p.loginTraeSubmit))
	// 常驻回环中继：心跳（中继带 api_key 调用）+ 状态/安装命令 + 脚本下载。
	// 见 trae_relay.go —— 授权页硬校验 http://127.0.0.1:<port>/authorize，
	// 装一次常驻中继后用户登录 Trae 不再需要任何额外步骤。
	p.mux.HandleFunc("POST /panel/api/login/trae/relay/ping", p.withAuth(p.traeRelayPing))
	p.mux.HandleFunc("GET /panel/api/login/trae/relay/status", p.withAuth(p.traeRelayStatusHandler))
	p.mux.HandleFunc("GET /panel/api/login/trae/relay/script", p.withAuth(p.traeRelayScriptHandler))
	p.mux.HandleFunc("POST /panel/api/login/zcode/start", p.withAuth(p.loginZCodeStart))
	p.mux.HandleFunc("GET /panel/api/login/zcode/poll", p.withAuth(p.loginZCodePoll))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.importCockpit))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	// 手填 ZCode 客户端签名凭据（"<keyId>.<secret>"）：线上没有本机客户端配置时
	// 用它把签名/回落链路接通。见 upstream_dispatch.go 的 upstreamCredential。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/credential", p.withAuth(p.upstreamCredential))
	// ZCode 凭据自动获取 / 批量补齐（消灭「每加一个号手抄一次 keyId.secret」）。
	// 见 zcode_credential.go 的 zcodeCredentialAuto。
	p.mux.HandleFunc("POST /panel/api/zcode/credential/auto", p.withAuth(p.zcodeCredentialAuto))
	p.mux.HandleFunc("GET /panel/api/zcode/credential/status", p.withAuth(p.zcodeCredentialStatus))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("POST /panel/api/upstream/activity_scan", p.withAuth(p.upstreamActivityScan))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 server 包同口径的 Bearer 鉴权（经 httpauth 常量时间比较）；
// api_key 为空时放行。密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求
// 即用新值（无需重启）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// expiringSoonWindow 返回调度器当前生效的快过期路由窗口；测试面板无调度器时返回 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	accounts := p.cfg.Pool.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"accounts":        accounts,
		// 三合一：按上游分组的账号数。三个 key 恒存在（缺失时前端显示 0 而非空白），
		// 面板顶部的 CodeBuddy / Trae / ZCode 三张卡片直接读这里。
		"providers": providerCounts(accounts),
		// 三合一：按上游分组的积分。与 providers 同 key 恒存在，
		// 面板把「积分剩余 / 总额」拆成三份独立显示——三个上游的额度口径不同
		// （CodeBuddy 积分 / ZCode token / Trae 积分），加在一起没有意义。
		"provider_credits": providerCredits(accounts),
	})
}

// providerCounts 按 provider 统计账号数。
// healthy 口径与 Cards 的「可用」一致：未禁用且未冷却；已禁用优先计入 disabled。
// 空 provider 归一化为 workbuddy（历史 state 未写该字段）。
func providerCounts(accounts []pool.Status) map[string]map[string]int {
	keys := []string{auth.ProviderWorkbuddy, auth.ProviderTrae, auth.ProviderZCode}
	out := make(map[string]map[string]int, len(keys))
	for _, k := range keys {
		out[k] = map[string]int{"total": 0, "healthy": 0, "cooling": 0, "disabled": 0}
	}
	for _, a := range accounts {
		k := auth.NormalizeProvider(a.Provider)
		c := out[k]
		if c == nil {
			// 未知 provider（未来新增）也计数，不丢数据。
			c = map[string]int{"total": 0, "healthy": 0, "cooling": 0, "disabled": 0}
			out[k] = c
		}
		c["total"]++
		switch {
		case a.Disabled:
			c["disabled"]++
		case a.Cooling:
			c["cooling"]++
		default:
			c["healthy"]++
		}
	}
	return out
}

// providerCredits 按 provider 聚合积分。返回结构：
//
//	{"workbuddy":{"credits":N,"total":M,"expiring":E,"accounts":K},...}
//
// 三个上游的额度口径互相独立（CodeBuddy/Trae 是积分，ZCode 是 token），
// 跨 provider 求和没有业务含义，因此面板分开展示而不是给一个合计。
// 总额为 0 的账号（旧 state / 查询失败）不计入 total，但仍计入 credits 与 accounts，
// 这样卡片上的「剩余」永远等于该上游各账号剩余之和，不会因未知总额而丢数。
func providerCredits(accounts []pool.Status) map[string]map[string]int64 {
	out := make(map[string]map[string]int64, 3)
	for _, k := range []string{auth.ProviderWorkbuddy, auth.ProviderTrae, auth.ProviderZCode} {
		out[k] = map[string]int64{"credits": 0, "total": 0, "expiring": 0, "accounts": 0}
	}
	for _, a := range accounts {
		k := auth.NormalizeProvider(a.Provider)
		c := out[k]
		if c == nil {
			c = map[string]int64{"credits": 0, "total": 0, "expiring": 0, "accounts": 0}
			out[k] = c
		}
		c["accounts"]++
		if a.Credits > 0 {
			c["credits"] += a.Credits
		}
		if a.CreditsTotal > 0 {
			c["total"] += a.CreditsTotal
		}
		if a.CreditsExpiring > 0 {
			c["expiring"] += a.CreditsExpiring
		}
	}
	return out
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// requestMetrics 返回进程内请求指标、最近 100 条与归档状态。
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.RequestLog.Snapshot())
}

// requestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
// 支持按 outcome/account/model/client_ip/user_agent 过滤（字符串字段为包含匹配）
// 与 from/to 时间区间（闭区间，unix 秒或 RFC3339）——面板「运行日志」的筛选框、
// 来源查询与「今天 / 自定义区间」都走这里。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	q := r.URL.Query()
	rows, err := p.cfg.RequestLog.ReadArchive(limit, reqlog.Filter{
		Outcome:   q.Get("outcome"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 空结果回 []（而不是 JSON null）：前端把 null 与"归档关闭"混在一起会走错分支，
	// 显示成不满足筛选条件的最近请求。
	if rows == nil {
		rows = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	var fetchErrs []string

	// 三合一：?provider=trae|zcode 走各自上游的模型目录（CodeBuddy 为默认/缺省）。
	//
	// id 前缀统一用 `<provider>:`（冒号），与 /v1/models 统一目录、与「全部上游」视图
	// 完全同一形态：前缀既是**显示值**也是**调用值**，用户剪贴到
	// /v1/chat/completions 的 model 字段即可命中正确上游（resolve_provider.go 对 "/" 与 ":"
	// 两种分隔符都认，这里用 ":" 保持一致）。
	//
	// 为什么不再用 "/" 作前缀：同一个模型在不同视图里写法不一样，
	// 用户得记「面板里是斜杠、API 里是冒号」，很容易拷错。
	//
	// provider=all 「全部上游」视图：与 /v1/models 统一目录同口径（CodeBuddy + Trae + ZCode）。
	// 放在最前面，因为下面的代码块只处理「单一非 CodeBuddy 上游」。
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("provider")), "all") {
		p.modelsAll(w, r)
		return
	}

	if prov := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider"))); prov != "" && prov != "workbuddy" && prov != "codebuddy" {
		if p.upstream == nil {
			writeErr(w, http.StatusNotImplemented, "上游操作未启用（该部署只运行 CodeBuddy 链路）")
			return
		}
		// 尽量把账号一并传给上游接口：ZCode 的「套餐可用模型」
		// 不在匿名目录里，只有拿到账号才能从 billing 拉到。
		// 没有可用账号时传 nil，退化为纯公开目录（不因此报错）。
		acct := p.pickModelsAccount(normProvider(prov))
		ms, err := p.upstream.ModelsFor(r.Context(), normProvider(prov), acct)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "fetch models ("+prov+"): "+err.Error())
			return
		}
		norm := normProvider(prov)
		for _, m := range ms {
			if id, ok := m["id"].(string); ok && id != "" {
				m["id"] = norm + ":" + id
			}
			if _, has := m["provider"]; !has {
				m["provider"] = norm
			}
			out = append(out, m)
		}
		if len(out) == 0 {
			writeErr(w, http.StatusServiceUnavailable, upstreamEmptyHint(norm))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out, "provider": normProvider(prov)})
		return
	}

	// CN 域：有可用 CN 账号才查（此前无条件 Pool.Pick()+FetchModels——选中 global
	// 账号时打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸）。
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				fetchErrs = append(fetchErrs, "cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
	}

	// global 域：路由开关开且有可用 global 账号才查（独立目录端点，FetchGlobalModelInfos；
	// Upstream.GlobalEnabled 是探测侧同一道闸，与 main 装配的 config global.enabled 一致）。
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					fetchErrs = append(fetchErrs, "global: 上游未返回可用模型")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}

	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// pickModelsAccount 为「模型与档位」页挑一个**同 provider** 的账号。
//
// 为什么必须按 provider 挑：
//
//	原实现写死 AvailableUIDsForRealm("cn")[0] —— 那个函数内部固定
//	provider=workbuddy（见 pool.AvailableUIDsForRealm）。池子里只要有任意
//	一个 CN 的 CodeBuddy 账号，它就会排在最前，于是「查 ZCode 模型」时
//	拿到的是一份 CodeBuddy 凭据。ZCode 的 billing 端点收到别人家的
//	Bearer 只会回 401 空体，面板于是显示
//	"account_error: balance http 401"，套餐授权模型（GLM-5.3-Flash）
//	永远并不进列表。
//
// 回落顺序：同 provider + 同 realm 的可用号 → 同 provider 任意可用号 →
// 同 provider 任意号（含冷却/降级，只为读一份目录，不消耗额度）→ nil
// （此时退化为纯公开目录，不报错）。
// modelsAll 汇总「全部上游」的模型目录（/panel/api/models?provider=all）。
//
// 与 /v1/models 同一口径、同一 id 形态：
//
//	cn:glm-5.2 / global:xxx   CodeBuddy
//	<provider>:<model>        Trae / ZCode
//
// 为什么要这个视图：CodeBuddy 的模型带 effort 档位与促销价，Trae/ZCode 的条目形状
// 不同（credits 倍率、套餐授权），按 provider 分页看会漏掉「同名模型在不同上游各自
// 什么价、各自扣哪份额度」。这里合成一份，前端只按 id 区分，不用先切下拉框。
//
// 单段失败不阻塞其余：一个上游挂掉不该让整页空白，失败的段以 notes 回报。
func (p *Panel) modelsAll(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, 64)
	notes := make([]string, 0, 4)

	// CodeBuddy 段依赖 cfg.Upstream（上游 upstream.Client）。
	// 命令行与生产总是注入，但单元测试/裸构造 Panel 时可能为 nil，
	// 这里显式跳过而不是 panic（本函数的语义是「能拿多少拿多少」）。
	if p.cfg.Upstream == nil {
		notes = append(notes, "codebuddy: 上游未配置")
		out, notes = p.modelsAllUpstreams(w, r, out, notes)
		if out == nil {
			return // 已写错误响应
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out, "provider": "all", "notes": notes})
		return
	}

	// --- CodeBuddy：CN ---
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			if infos, err := p.cfg.Upstream.FetchModels(acct); err != nil {
				notes = append(notes, "codebuddy-cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
	} else {
		notes = append(notes, "codebuddy-cn: 无可用账号")
	}

	// --- CodeBuddy：global ---
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
				for _, mi := range infos {
					out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
				}
			}
		}
	}

	// --- Trae / ZCode ---
	out, notes = p.modelsAllUpstreams(w, r, out, notes)
	if out == nil {
		return // 已写错误响应
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out, "provider": "all", "notes": notes})
}

// modelsAllUpstreams 把 Trae / ZCode 的目录并入 out：与 /v1/models 一样，
// **无号的上游整段跳过**（列表即「能调」），拉不到就记 notes 而不报 502。
//
// 返回 nil 表示已写错误响应（整份目录一条都没有）。
func (p *Panel) modelsAllUpstreams(w http.ResponseWriter, r *http.Request, out []map[string]any, notes []string) ([]map[string]any, []string) {
	if p.upstream != nil {
		for _, prov := range []string{auth.ProviderTrae, auth.ProviderZCode} {
			acct := p.pickModelsAccount(prov)
			if acct == nil {
				notes = append(notes, prov+": 无可用账号")
				continue
			}
			ms, err := p.upstream.ModelsFor(r.Context(), prov, acct)
			if err != nil {
				notes = append(notes, prov+": "+err.Error())
				continue
			}
			// 「全部上游」视图与 /v1/models 同口径：**滤掉 client-only 条目**。
			//
			// Trae 单 provider 视图会展示「仅客户端可用」的模型（让用户看见
			// GLM-5.3 / Kimi-K3 确实存在，只是上游 API 不认），那是展示页；
			// 而这里是横跨所有上游的选型视图，列进去会引导用户去调一个
			// 100% 回 code 4023 的模型。筛选与对外目录必须同口径。
			ms = trae.CallableOnly(ms)
			for _, m := range ms {
				id, _ := m["id"].(string)
				if strings.TrimSpace(id) == "" {
					continue
				}
				e := make(map[string]any, len(m)+2)
				for k, v := range m {
					e[k] = v
				}
				e["id"] = prov + ":" + id
				e["provider"] = prov
				if _, has := e["owned_by"]; !has {
					e["owned_by"] = prov
				}
				out = append(out, e)
			}
		}
	}

	if len(out) == 0 {
		msg := "没有可用模型"
		if len(notes) > 0 {
			msg += "（" + strings.Join(notes, "; ") + "）"
		}
		writeErr(w, http.StatusServiceUnavailable, msg)
		return nil, notes
	}
	return out, notes
}

// upstreamEmptyHint 该上游目录为空时的提示文案。
//
// Trae 这边容易让人误会：TraeCode 客户端界面列了三十多个模型，
// 但那是**客户端本地表**，不是上游当前账号能调的集合。
// 实测：上游 model_list 对免费档只下发 6 条，其中 2 条是客户端自带自定义模型
// （带 ak，服务端不认），真正能调的只有 4 个。档位越高下发越多。
func upstreamEmptyHint(prov string) string {
	if prov != auth.ProviderTrae {
		return "该上游未返回可用模型（请先添加上游账号）"
	}
	return "Trae 上游未下发可用模型：本列表只列「当前账号真能调」的模型，" +
		"不列 TraeCode 客户端本地能力表里那些服务端不认的名字。" +
		"请检查账号是否过期、档位是否过低（高档位下发更多模型）"
}

func (p *Panel) pickModelsAccount(prov string) *auth.Auth {
	if prov == "" {
		return nil
	}
	for _, realm := range []string{"cn", "global", ""} {
		if uids := p.cfg.Pool.AvailableUIDsForScope(realm, prov); len(uids) > 0 {
			if a := p.cfg.Pool.AuthByUID(uids[0]); a != nil {
				return a
			}
		}
	}
	for _, st := range p.cfg.Pool.List() {
		if auth.NormalizeProvider(st.Provider) == prov {
			return p.cfg.Pool.AuthByUID(st.UID)
		}
	}
	return nil
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id":                   realm + ":" + mi.ID,
		"name":                 mi.Name,
		"default_effort":       mi.DefaultEffort,
		"supported_efforts":    mi.Efforts,
		"can_disable_thinking": mi.CanDisableThinking,
		"supports_reasoning":   mi.SupportsReasoning,
		"supports_images":      mi.SupportsImages,
		"credits":              mi.Credits,
		"description":          mi.Description,
		"tags":                 mi.Tags,
		"vendor":               mi.Vendor,
		"is_default":           mi.IsDefault,
		"supports_tool_call":   mi.SupportsToolCall,
		"only_reasoning":       mi.OnlyReasoning,
		"reasoning_effort":     mi.ReasoningEffort,
		"reasoning_summary":    mi.ReasoningSummary,
	}
	// 限时优惠（modelPromotions）：credits 是牌价，promo_* 是当前生效折扣
	//（WorkBuddy 客户端显示的就是这个生效价）。前端据此显示「生效价+标签+划线牌价」。
	if mi.PromoFactor != nil {
		entry["promo_factor"] = *mi.PromoFactor
		entry["promo_credits"] = mi.PromoCredits
	}
	if mi.PromoLabel != "" {
		entry["promo_label"] = mi.PromoLabel
	}
	if mi.PromoNote != "" {
		entry["promo_note"] = mi.PromoNote
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// 三合一：Trae / ZCode 账号绝不能走 CodeBuddy 的 DailyCheckin——两边协议无关，
	// 打过去只会 401/404 并把账号误判为异常。分流到各自的上游实现。
	if a.IsUpstream() {
		p.upstreamAccountOps(w, r, a, upstreamOpCheckin)
		return
	}
	checkinMsg := ""
	checkinDone := false
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
		// 幂等拒绝同样是「今日已签」，标记后按钮显示「已签」。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
		}
	} else {
		p.cfg.Pool.NoteCheckinDone(uid)
		checkinDone = true
	}
	resp := map[string]any{"ok": true, "checkin_done": checkinDone}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：更新余额与到期快照，不触碰冷却状态。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// 三合一：上游账号走各自的余额/额度实现（同 accountCheckin 的分流理由）。
	if a.IsUpstream() {
		p.upstreamAccountOps(w, r, a, upstreamOpBalance)
		return
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到。
//
// 三合一后要覆盖两类账号：CodeBuddy 走 scheduler（含猫猫旅行），
// Trae / ZCode 走 upstreamBatch（各自的签到/余额口径）。
// 上游账号同步执行（账号数有限，直接给结果更确定）；CodeBuddy 仍异步。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	upstream, hasUpstream := p.batchUpstream(r, upstreamOpCheckin)
	if p.cfg.Scheduler == nil && !hasUpstream {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	started := false
	if p.cfg.Scheduler != nil {
		go p.cfg.Scheduler.RunCheckinNow()
		started = true
	}
	log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")
	resp := map[string]any{"ok": true, "started": started}
	if hasUpstream {
		resp["upstream"] = upstream
		resp["started"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunTravelNow()
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（点亮连登 + 解锁领养前置），
// 顺带对 Trae 账号执行「活动领取」（同一语义：把可领的商业活动领掉）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	upstream, hasUpstream := p.batchUpstream(r, upstreamOpActivity)
	started := false
	if p.cfg.Scheduler != nil {
		go p.cfg.Scheduler.RunActivityNow()
		started = true
	}
	if !started && !hasUpstream {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	log.Printf("panel: 手动全量活跃上报已触发")
	resp := map[string]any{"ok": true, "started": started}
	if hasUpstream {
		resp["upstream"] = upstream
		resp["started"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// keepaliveAll 手动触发全量 token 保活；Trae 的保活走 upstreamBatch。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	upstream, hasUpstream := p.batchUpstream(r, upstreamOpKeepalive)
	started := false
	if p.cfg.Scheduler != nil {
		go p.cfg.Scheduler.RunKeepaliveNow()
		started = true
	}
	if !started && !hasUpstream {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	log.Printf("panel: 手动全量保活已触发")
	resp := map[string]any{"ok": true, "started": started}
	if hasUpstream {
		resp["upstream"] = upstream
		resp["started"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	upstream, hasUpstream := p.batchUpstream(r, upstreamOpBalance)
	if p.cfg.Scheduler != nil {
		p.cfg.Scheduler.RunBalanceRefreshNow()
	}
	p.syncNicknames()
	log.Printf("panel: 手动全量余额刷新完成")
	resp := map[string]any{"ok": true, "accounts": p.cfg.Pool.List()}
	if hasUpstream {
		resp["upstream"] = upstream
	}
	writeJSON(w, http.StatusOK, resp)
}

// syncNicknames 手动刷新时的昵称同步（issue #94：上游改名免重登）。
// 只在面板手动「刷新」路径调用——后台余额定时器不触发（用户明确要求资料接口
// 仅手动触达）。逐号拉 /console/account，只取 nickname（手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）；单号失败静默跳过，不打断余额刷新
// 的既有结果。
func (p *Panel) syncNicknames() {
	type job struct {
		uid string
		a   *auth.Auth
	}
	var jobs []job
	// 三合一：昵称同步只对 CodeBuddy 账号有意义（拉的是 /console/account）。
	// Trae/ZCode 的昵称来自各自 auth 文件，不该被打到 CodeBuddy 资料接口。
	for _, st := range p.cfg.Pool.ListCodeBuddy() {
		if st.Disabled {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.AccessTokenValue() != "" {
			jobs = append(jobs, job{uid: st.UID, a: a})
		}
	}
	if len(jobs) == 0 {
		return
	}
	var (
		mu      sync.Mutex
		updated int
		failed  int
		sem     = make(chan struct{}, 3)
		wg      sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nick, err := p.cfg.Upstream.FetchAccountProfile(j.a)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			if p.cfg.Pool.SetNickname(j.uid, nick) {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	if updated > 0 || failed > 0 {
		log.Printf("panel: 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。统计窗口三选一：
//   - from/to（unix 秒）：显式区间，用于「今天」与「自定义」——区间由浏览器按
//     本地时区算好再发，服务端时区与浏览器不一致时「今天」才不会被算错；
//   - hours：滚动窗口（默认 72，上限 1440=60 天），卡片汇总/按域/按账号/按模型/
//     时序**全部**按该窗口统计；显式 hours=0 表示全部历史（含 90 天前折叠出的日桶）；
//   - 都不给：等同于 hours=72（保持旧调用方行为）。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	q := r.URL.Query()
	win := usage.Window{}
	win.From = parseTimeParam(q.Get("from"))
	win.To = parseTimeParam(q.Get("to"))
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if v := q.Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				win.Hours = n
			}
		}
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	var currentRate func(realm, model string) string
	if p.cfg.Upstream != nil {
		currentRate = p.cfg.Upstream.ModelRate
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.SnapshotWindow(win, nicks, currentRate))
}

// parseTimeParam 解析时间查询参数：unix 秒（前端默认）或 RFC3339（便于手工调
// 接口/写脚本）。空串与非法值都返回零值 = 该侧不设界，不报错——区间参数是可选
// 增强，拼错一个 from 不该让整页用量打不开。
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		// 兼容秒与毫秒（前端可能直接把 Date.now() 传上来）。
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Provider string                   `json:"provider"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
		// Source 账号来源（ZCode 的 zai / bigmodel）；PendingTotal 可领取额度合计；
		// Note 上游给的补充说明（如"未激活编码方案，可在任务里领取"）。
		Source       string `json:"source,omitempty"`
		PendingTotal int64  `json:"pending_total,omitempty"`
		Note         string `json:"note,omitempty"`
	}
	out := make([]row, len(accts))

	ctx := r.Context()
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm, Provider: auth.NormalizeProvider(s.Provider)}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			// 三合一：Trae / ZCode 的「积分构成」来自各自上游（Trae 权益包、
			// ZCode billing 额度桶），字段压平成与 CodeBuddy 同形的 CreditPackage
			// 行，前端一套渲染通吃。
			if a.IsUpstream() {
				if p.upstream == nil {
					it.Error = "上游操作未启用"
					out[i] = it
					return
				}
				v, err := p.upstream.Packages(ctx, a)
				if err != nil {
					it.Error = err.Error()
					out[i] = it
					return
				}
				it.Provider = v.Provider
				it.Remain, it.Size = v.Remain, v.Size
				it.Packages = upstreamPacksToCredit(v.Packages)
				it.Source, it.PendingTotal, it.Note = v.Source, v.PendingTotal, v.Note
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// normProvider 把面板/查询串里的 provider 别名归一为上游枚举。
func normProvider(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "trae", "traecode":
		return auth.ProviderTrae
	case "zcode", "zai", "bigmodel":
		return auth.ProviderZCode
	default:
		return auth.ProviderWorkbuddy
	}
}

// upstreamActivityScan 立刻对全部 Trae / ZCode 账号跑一轮活动巡检并领取。
//
// 与后台排程共用 ops.ActivityScan，只是把"等 30 分钟"变成"现在就跑"，
// 方便运维在活动刚上线时手动催一次。并发上限 2，避免同时拉起多个
// zcode 验证码求解（每个要起一个 Chrome）。
func (p *Panel) upstreamActivityScan(w http.ResponseWriter, r *http.Request) {
	ops, ok := p.upstreamOrErr(w)
	if !ok {
		return
	}
	ctx := r.Context()
	type result struct {
		UID      string `json:"uid"`
		Provider string `json:"provider"`
		Name     string `json:"name,omitempty"`
		Message  string `json:"message,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	var (
		mu   sync.Mutex
		out  []result
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 2)
		list = p.cfg.Pool.List()
	)
	for _, st := range list {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.IsDisabled() || !a.IsUpstream() {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			msg, err := ops.ActivityScan(ctx, a)
			mu.Lock()
			defer mu.Unlock()
			it := result{UID: a.UID, Provider: auth.NormalizeProvider(a.Provider), Name: a.Nickname}
			if err != nil {
				it.Error = err.Error()
			} else {
				it.Message = msg
			}
			out = append(out, it)
		}(a)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// upstreamPacksToCredit 把上游的积分/额度包压平成 CodeBuddy 同形的 CreditPackage，
// 让「积分构成」页不必区分 provider 就能渲染（颜色分组、到期分布、明细表全部复用）。
func upstreamPacksToCredit(in []UpstreamPackage) []upstream.CreditPackage {
	out := make([]upstream.CreditPackage, 0, len(in))
	for _, p := range in {
		// 到期时间双写：ExpiresAt 走精确毫秒供聚合图，EndTime 走 "YYYY-MM-DD"
		// 供明细表的「到期」列。只填前者的话 Trae/ZCode 的包在明细表里会显示 "—"
		// （该列读的是 end_time 字符串，而这两个上游不带这个字段）。
		endTime := ""
		if p.ExpiresAt > 0 {
			endTime = time.Unix(p.ExpiresAt, 0).Format("2006-01-02")
		}
		out = append(out, upstream.CreditPackage{
			Name:      p.Name,
			Remain:    p.Remain,
			Used:      p.Used,
			Size:      p.Size,
			EndTime:   endTime,
			ExpiresAt: p.ExpiresAt * 1000, // CreditPackage.ExpiresAt 是毫秒
			Pending:   p.Pending,
			Note:      p.Note,
			PlanID:    p.PlanID,
			GrantSize: p.GrantSize,
		})
	}
	return out
}
