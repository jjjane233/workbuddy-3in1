// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/logfmt"
)

// Provider 标识凭证所属上游。三合一网关的账号池按此分流，
// 空值一律归一化为 ProviderWorkbuddy（存量 auth 文件零回归）。
const (
	ProviderWorkbuddy = "workbuddy" // 腾讯 CodeBuddy（copilot.tencent.com / workbuddy.ai）
	ProviderTrae      = "trae"      // Trae CN / Trae Code
	ProviderZCode     = "zcode"     // ZCode / BigModel GLM
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	// Provider 标识上游；空 = workbuddy（历史文件未写该键）。
	// 字段名与 traecode2api / zcode2api / gw2api 的 Auth 保持一致，
	// 便于把三家已跑通的上游实现平移进来时直接复用取值语义。
	Provider string

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// ---- Trae 专属（provider=trae）----
	// 字段与 traecode2api/internal/auth 同名，便于平移 ExchangeToken / DeviceProof 逻辑。
	// 序列化不走 struct tag：Parse 读、SaveAtomic 写，均为显式键映射（与本文件其余字段一致）。
	DevicePrivateKeyPEM string // PEM 私钥（DeviceProof ECDSA-P256 签名用）
	DevicePublicKeyPEM  string // PEM 公钥（缺省时由私钥派生）
	MachineID           string
	DeviceID            string
	TokenExpireAt       int64 // 过期时刻；Trae 侧单位为**毫秒**（勿与 CodeBuddy 的 ExpiresAt 秒混用）
	RefreshExpireAt     int64 // 过期时刻；Trae 侧单位为**毫秒**

	// ---- ZCode 专属（provider=zcode）----
	// APIKey 为 BigModel GLM Key（"<id>.<secret>" 或 "sk-..."，api_key 模式主凭证）；
	// SignKeyID/SignSecret 为对话接口的 Ed25519 客户端签名凭据。
	APIKey     string
	SignKeyID  string
	SignSecret string

	// ---- 跨 provider 共享 ----
	// Source 账号来源标注（ZCode 的 zai / bigmodel；Trae 留空）。
	//
	// 为什么要单独存一列：ZCode 同一个 uid 在 Z.AI 与 BigModel 两套账号体系下
	// 是**互相独立的额度**，面板不标出来会被误读成"额度凭空消失"。它也不能复用
	// Region——Region 是阿里云无痕验证的地域（cn），两者语义完全不同。
	Source      string
	Mode        string // "jwt" / "api_key"（ZCode 双模式；workbuddy 恒空）
	Username    string
	Region      string
	SessionID   string
	Disabled    bool
	Remark      string
	LastRefresh int64 // Unix 秒
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// ZCodeSignCredentialPaths 返回本机 ZCode 客户端可能存放签名凭据的配置文件路径。
//
// 背景：ZCode 的对话端点（/api/v1/zcode-plan/anthropic）在签名门开启时需要
// 客户端 Ed25519 签名（Client Request Signing v4）；而该凭据不在登录接口返回里，
// 只存在客户端本地配置中。同一台机器上跑网关时直接读它即可，避免手填。
func ZCodeSignCredentialPaths() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".zcode", "v2", "config.json"))
	}
	if app := os.Getenv("APPDATA"); app != "" {
		out = append(out, filepath.Join(app, "ZCode", "config.json"))
	}
	return out
}

// DiscoverZCodeSignCredential 从本机 ZCode 客户端配置里找出签名凭据。
//
// 挑选规则（实测校准 2026-10-06）：
//
//	ZCode 把它存在 provider.options.apiKey 里，且形如 "<32位hex>.<密钥>"。
//	同一份配置里还有很多 "sk-..." 的第三方中转密钥，必须用
//	「恰好一个点 + 左侧是 32 位小写 hex」过滤掉，
//	否则会把 "sk-xxx" 当成凭据发出去。
func DiscoverZCodeSignCredential() string {
	for _, p := range ZCodeSignCredentialPaths() {
		if c := readSignCredentialFrom(p); c != "" {
			return c
		}
	}
	return ""
}

func readSignCredentialFrom(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return ""
	}
	var doc struct {
		Provider map[string]struct {
			Options struct {
				APIKey string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	best := ""
	for _, v := range doc.Provider {
		k := strings.TrimSpace(v.Options.APIKey)
		if !LooksLikeSignCredential(k) {
			continue
		}
		if best == "" {
			best = k
		}
	}
	return best
}

// LooksLikeSignCredential 校验 "<hex32>.<secret>" 形态。
func LooksLikeSignCredential(s string) bool {
	i := strings.IndexByte(s, '.')
	if i <= 0 || i != strings.LastIndexByte(s, '.') {
		return false
	}
	left := s[:i]
	if len(left) != 32 {
		return false
	}
	for _, c := range left {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return len(s)-i-1 > 0
}

// 以下 TokenValue / APIKeyValue / SignCredential / SetSignCredential 是**移植适配层**：
// traecode2api 的 internal/trae 与 gw2api 的 internal/zcode 以这组名字访问凭证，
// 把它们平移进本仓库时无需改写调用点，直接复用本包已有的加锁读取实现。
//
// 命名分歧说明：本包历史名为 AccessTokenValue（CodeBuddy 语境），移植代码用 TokenValue
// （Trae/ZCode 语境指同一字段）。两者语义一致，保留双名以免改动存量调用方。

// TokenValue 加锁读取 AccessToken。等价于 AccessTokenValue，为移植代码提供短名。
func (a *Auth) TokenValue() string { return a.AccessTokenValue() }

// APIKeyValue 加锁读取 APIKey（ZCode api_key 模式的主凭证）。
func (a *Auth) APIKeyValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.APIKey
}

// SignCredential 返回 "<keyId>.<secret>" 形式的 Ed25519 客户端签名凭据（ZCode 对话用）。
// 任一分量为空即返回空串，由调用方决定降级行为。
func (a *Auth) SignCredential() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.SignKeyID == "" || a.SignSecret == "" {
		return ""
	}
	return a.SignKeyID + "." + a.SignSecret
}

// SetAPIKey 写入 ZCode 的 api_key（BigModel GLM Key，形如 "<id>.<secret>" 或 "sk-..."）。
//
// 为什么单独一个 setter 而不是直接赋值：APIKeyValue / Chat 的 useBigModel 分支
// 都在 a.mu 下读它，直接赋值会与并发读竞态。手填凭据入口是运行时写路径，
// 必须走锁。
func (a *Auth) SetAPIKey(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.APIKey = key
}

// SetSignCredential 写入 "<keyId>.<secret>" 形式的签名凭据（面板「写入」操作）。
// 格式非法（缺点、点在首尾）时静默忽略，不破坏已有凭据。
// 持 a.mu 写入，与 SignCredential 的加锁读取配对。
func (a *Auth) SetSignCredential(cred string) {
	cred = strings.TrimSpace(cred)
	if cred == "" {
		return
	}
	i := strings.IndexByte(cred, '.')
	if i <= 0 || i == len(cred)-1 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.SignKeyID = cred[:i]
	a.SignSecret = cred[i+1:]
}

// 以下为移植适配层（traecode2api/internal/trae 与 gw2api 所用），签名与语义原样照搬，
// 避免平移上游实现时改动其调用点。

// DevicePrivateKeyValue 加锁读取设备私钥 PEM（Trae DeviceProof 签名用）。
func (a *Auth) DevicePrivateKeyValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DevicePrivateKeyPEM
}

// DevicePublicKeyValue 加锁读取设备公钥 PEM。
func (a *Auth) DevicePublicKeyValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DevicePublicKeyPEM
}

// MachineIDValue 加锁读取机器指纹。
func (a *Auth) MachineIDValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.MachineID
}

// TokenExpireAtValue 加锁读取 AccessToken 过期时间（Trae 侧单位为**毫秒**，
// 与 CodeBuddy 的 ExpiresAt（秒）不同，勿混用）。
func (a *Auth) TokenExpireAtValue() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.TokenExpireAt
}

// SetTokens 一次性更新 token / refreshToken / 过期时间（Trae ExchangeToken 换票回写）。
// 空值表示「该项不变」，避免部分字段刷新失败时把已有凭证冲掉。
// LastRefresh 记录本次变更时刻，供面板与调度器判断换票新旧。
func (a *Auth) SetTokens(token, refresh string, tokenExpMs, refreshExpMs int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if token != "" {
		a.AccessToken = token
	}
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if tokenExpMs > 0 {
		a.TokenExpireAt = tokenExpMs
	}
	if refreshExpMs > 0 {
		a.RefreshExpireAt = refreshExpMs
	}
	a.LastRefresh = time.Now().Unix()
}

// SetToken 更新 AccessToken（非空时同步刷新 LastRefresh）。
func (a *Auth) SetToken(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AccessToken = tok
	if tok != "" {
		a.LastRefresh = time.Now().Unix()
	}
}

// ProviderOf 加锁读取 Provider。
// 与无锁的 ProviderValue 的区别：面板与调度器在运行期读它，而 SetProvider 类写入
// 会持同一把锁，故跨包运行期读取一律走本方法。
func (a *Auth) ProviderOf() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return NormalizeProvider(a.Provider)
}

// IsJWT 报告账号是否 jwt 模式（ZCode 双模式判别）。
func (a *Auth) IsJWT() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Mode == ModeJWT
}

// IsDisabled 报告账号是否被禁用（面板灰显与池子跳过共用）。
func (a *Auth) IsDisabled() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Disabled
}

// SetDisabled 写入禁用状态。
func (a *Auth) SetDisabled(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Disabled = v
}

// Display 返回用于日志/面板的展示名：昵称 > 用户名 > UID > 文件名。
func (a *Auth) Display() string {
	if a == nil {
		return "?"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Nickname != "" {
		return a.Nickname
	}
	if a.Username != "" {
		return a.Username
	}
	if a.UID != "" {
		return a.UID
	}
	return filepath.Base(a.FilePath)
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
//
// NormalizeProvider 归一化 provider 标识（去空白 + 小写）；空值回落 workbuddy。
// 所有跨包比较一律经此归一化，避免 "Trae " / "TRAE" 之类大小写差异分流失败。
func NormalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return ProviderWorkbuddy
	}
	return p
}

// ProviderValue 返回归一化后的 provider（a 为 nil 时返回空串）。
// 该字段仅在解析期写入、运行期只读，故不持 a.mu。
func (a *Auth) ProviderValue() string {
	if a == nil {
		return ""
	}
	return NormalizeProvider(a.Provider)
}

// IsUpstream 报告该账号是否走 Trae / ZCode 上游（而非 CodeBuddy）。
// 池子挑号、调度器分流、面板渲染都以此判别。
func (a *Auth) IsUpstream() bool {
	switch a.ProviderValue() {
	case ProviderTrae, ProviderZCode:
		return true
	}
	return false
}

// parseUpstreamAuth 解析 Trae / ZCode 的扁平形 auth 文件。
// 字段键与 traecode2api / zcode2api / gw2api 的落盘格式完全一致，
// 因此这三家导出的 auth 文件可直接投喂进来，无需转换。
func parseUpstreamAuth(raw []byte, provider string) (*Auth, error) {
	var f struct {
		Provider            string `json:"provider"`
		Mode                string `json:"mode"`
		AccessToken         string `json:"access_token"`
		RefreshToken        string `json:"refresh_token"`
		APIKey              string `json:"api_key"`
		SignKeyID           string `json:"sign_key_id"`
		SignSecret          string `json:"sign_secret"`
		UID                 string `json:"uid"`
		Username            string `json:"username"`
		Nickname            string `json:"nickname"`
		Region              string `json:"region"`
		Source              string `json:"source"`
		DeviceID            string `json:"device_id"`
		SessionID           string `json:"session_id"`
		DevicePrivateKeyPEM string `json:"device_private_key"`
		DevicePublicKeyPEM  string `json:"device_public_key"`
		MachineID           string `json:"machine_id"`
		TokenExpireAt       int64  `json:"token_expire_at"`
		RefreshExpireAt     int64  `json:"refresh_expire_at"`
		ExpiresAt           int64  `json:"expires_at"`
		Disabled            bool   `json:"disabled"`
		Remark              string `json:"remark"`
		LastRefresh         int64  `json:"last_refresh"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	// ZCode 支持纯 api_key 模式（无 access_token），故两者至少有其一即可。
	if strings.TrimSpace(f.AccessToken) == "" && strings.TrimSpace(f.APIKey) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken/apiKey (provider=%s)", provider)
	}
	return &Auth{
		Provider:     provider,
		Mode:         NormalizeMode(f.Mode),
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
		ExpiresAt:    f.ExpiresAt,
		APIKey:       f.APIKey,
		SignKeyID:    f.SignKeyID,
		SignSecret:   f.SignSecret,
		UID:          f.UID,
		Username:     f.Username,
		Nickname:     f.Nickname,
		Region:       f.Region,
		// 旧凭证兼容：Source 是后加的字段，老文件里没有。
		//
		// 为什么在解析时补而不是在界面上兜底：界面兜底会把
		// "真的没有来源"和"老格式文件"混为一谈，写进结构体后
		// 下次保存就自动补齐了，误差不会累积。
		//
		// 默认值取 zai：与 loginZCodeStart 的 CLI 默认 provider 一致（zcode.CLIInit 的
		// 缺省参数就是 "zai"），而 bigmodel 只能由用户在登录时显式选。
		Source:              firstNonBlank(f.Source, defaultSourceFor(NormalizeProvider(f.Provider), f.Region)),
		DeviceID:            f.DeviceID,
		SessionID:           f.SessionID,
		DevicePrivateKeyPEM: f.DevicePrivateKeyPEM,
		DevicePublicKeyPEM:  f.DevicePublicKeyPEM,
		MachineID:           f.MachineID,
		TokenExpireAt:       f.TokenExpireAt,
		RefreshExpireAt:     f.RefreshExpireAt,
		Disabled:            f.Disabled,
		Remark:              f.Remark,
		LastRefresh:         f.LastRefresh,
	}, nil
}

// firstNonBlank 返回第一个去空白后非空的字符串。
//
// auth 包里已有一个同名意图的小工具叫 firstNonEmpty，
// 但它只判 nil/空串不去空白；这里单独定义一个避免改动既有语义。
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// defaultSourceFor 给缺省 source 的老凭证推一个来源标注。
//
// 只有 ZCode 有双账号体系（zai / bigmodel），其他 provider 一律空。
// 推不出来就空着，界面会省掉该标签，不会显示假信息。
func defaultSourceFor(provider, region string) string {
	if provider != ProviderZCode {
		return ""
	}
	return "zai"
}

// NormalizeMode 归一化凭证模式；空值按凭证形态推断（有 api_key 无 token → api_key）。
func NormalizeMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return ""
	}
	switch mode {
	case ModeJWT, ModeAPIKey:
		return mode
	}
	return mode
}

// 凭证模式（ZCode 双模式；workbuddy / Trae 不使用）。
const (
	ModeJWT    = "jwt"
	ModeAPIKey = "api_key"
)

func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	// 三合一：显式 provider 键 → 走 Trae/ZCode 各自的扁平形解析。
	// 存量 workbuddy auth 文件没有该键，恒落入下方原逻辑（零回归）。
	if pv, ok := probe["provider"]; ok {
		var p string
		if err := json.Unmarshal(pv, &p); err == nil {
			switch NormalizeProvider(p) {
			case ProviderTrae, ProviderZCode:
				return parseUpstreamAuth(raw, NormalizeProvider(p))
			}
		}
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
		}
	}
	// 嵌套形 / 扁平形都是 CodeBuddy 形态；显式 provider 分支已在上方提前返回。
	if a.Provider == "" {
		a.Provider = ProviderWorkbuddy
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Trae / ZCode 走各自的扁平形：必须先于「accessToken 非空」守卫分流，
	// 否则 ZCode 纯 api_key 模式（无 access_token）会被误拒。
	if a.IsUpstream() {
		return a.saveUpstreamLocked()
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// saveUpstreamLocked 写回 Trae / ZCode 的扁平形 auth 文件（调用方须已持 a.mu）。
// 格式与 traecode2api / zcode2api / gw2api 的落盘一致，三家导出的文件可互读。
// 空值一律省略键：避免在文件里堆砌空字段，也便于与上游原始导出比对。
func (a *Auth) saveUpstreamLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	if strings.TrimSpace(a.AccessToken) == "" && strings.TrimSpace(a.APIKey) == "" {
		return fmt.Errorf("save refused: empty accessToken/apiKey (uid=%s provider=%s)", a.UID, a.ProviderValue())
	}
	doc := map[string]any{"provider": a.ProviderValue()}
	putStr := func(k, v string) {
		if v != "" {
			doc[k] = v
		}
	}
	putI64 := func(k string, v int64) {
		if v != 0 {
			doc[k] = v
		}
	}
	putStr("mode", a.Mode)
	putStr("access_token", a.AccessToken)
	putStr("refresh_token", a.RefreshToken)
	putStr("api_key", a.APIKey)
	putStr("sign_key_id", a.SignKeyID)
	putStr("sign_secret", a.SignSecret)
	putStr("uid", a.UID)
	putStr("username", a.Username)
	putStr("nickname", a.Nickname)
	putStr("region", a.Region)
	putStr("source", a.Source)
	putStr("device_id", a.DeviceID)
	putStr("session_id", a.SessionID)
	putStr("device_private_key", a.DevicePrivateKeyPEM)
	putStr("device_public_key", a.DevicePublicKeyPEM)
	putStr("machine_id", a.MachineID)
	putStr("remark", a.Remark)
	putI64("expires_at", a.ExpiresAt)
	putI64("token_expire_at", a.TokenExpireAt)
	putI64("refresh_expire_at", a.RefreshExpireAt)
	putI64("last_refresh", a.LastRefresh)
	if a.Disabled {
		doc["disabled"] = true
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	// 三合一：三类凭证文件共存于同一 auths 目录，按前缀分别 glob 后合并。
	// 命名约定：workbuddy-<uid>.json / trae-<uid>.json / zcode-<uid>.json。
	var files []string
	for _, pat := range []string{"workbuddy*.json", "trae*.json", "zcode*.json"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	// seenKey 重复检测：以 provider|uid 联合为键 —— 三合一后不同 provider 恰好同 UID
	// 是合法情况，只用 UID 会误报；同 provider 同 UID 才是真冲突（行为与旧版一致）。
	seenKey := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		key := a.ProviderValue() + "|" + a.UID
		if prev, ok := seenKey[key]; ok {
			log.Printf("WARN: uid %s (%s) duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), a.ProviderValue(), prev, f)
		}
		seenKey[key] = f
		// realm 只对 CodeBuddy 有意义：Trae/ZCode 无 domain/realm 概念，
		// 若不跳过会被误判为「待迁移」而触发一次无意义的写回。
		if !a.IsUpstream() && a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
