package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// 本文件是「全自动」的核心：把本机 ZCode 客户端已经握有的签名凭据
// <keyId>.<secret> 直接取出来落到网关池里，不需要用户手抄、也不需要
// 「上游换 key 接口」可用。
//
// ── 逆向结论（ZCode 3.14.4 实测，2026-10-07）────────────────────────────
// 证据链：
//
//  1. resources/glm/zcode.cjs -> createCodingPlanApiKeyResolver($Zr)：
//     bigmodel 分支 = getCustomerInfo -> api_keys -> api_keys/copy/<key> -> "apiKey.secretKey"
//     该逻辑**在客户端内部**完成，写成 <keyId>.<secret> 后持久化。
//
//  2. resources/app.asar -> AccountProviderApiKeyResolver.resolveBizApiKey：
//     同一条链；结果进 provider 配置。app.asar 里还有一个
//     isCurrentOAuthCredentialRequest()：当请求命中 /api/biz/customer/getCustomerInfo
//     且 Authorization 就是本机 OAuth token 时，由 **客户端进程自己** 直接回填响应体
//     —— 这正是「服务端 200 + Content-Length: 0」的真相：空体是本地拦截器造成的，
//     不是机房。所以换 key 链路在客户端里永远是通的。
//
//  3. 结果落在 %USERPROFILE%\.zcode\v2\config.json 的
//     provider["builtin:bigmodel-coding-plan"].options.apiKey。
//
//  → 网关只要读这个文件，就等于拿到了「换 key」的最终产物，完全绕开
//    「getCustomerInfo 被置空」的死路。这是本条自动化的正确注入点。
//
// ── 另一处真实偏差 ────────────────────────────────────────────────────
// 客户端加密口令里的平台串用 os.platform()（Windows = "win32"），
// 不是 Go 的 runtime.GOOS（Windows = "windows"）。见 ZCodeCredentialSecrets。

// ZCodeClientConfigPaths 返回 ZCode 客户端主配置 config.json 的候选位置。
//
// config.json 里 provider.*.options.apiKey 就是客户端解出的 <keyId>.<secret>。
func ZCodeClientConfigPaths() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".zcode", "v2", "config.json"))
	}
	if app := os.Getenv("APPDATA"); app != "" {
		out = append(out, filepath.Join(app, "ZCode", "config.json"))
	}
	return out
}

// DiscoverZCodeJWToken 从本机 ZCode 客户端的 credentials.json 解出 zcodejwt。
//
// 用途：给池里的 ZCode 账号补 access_token（zcode-plan / billing / 积分口径）。
func DiscoverZCodeJWToken() string {
	for _, p := range ZCodeCredentialsPaths() {
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 {
			continue
		}
		if t := parseZCodeJWToken(raw); t != "" {
			return t
		}
	}
	return ""
}

// parseZCodeJWToken 解 credentials.json 里的 "zcodejwttoken"（可能是 enc:v1: 加密值）。
func parseZCodeJWToken(raw []byte) string {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	v, _ := doc["zcodejwttoken"].(string)
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "enc:v1:") {
		if plain, err := DecryptZCodeCredential(v); err == nil {
			return strings.TrimSpace(plain)
		}
		return ""
	}
	return v
}

// DiscoverZCodeLocalUID 从本机 ZCode 客户端解出登录账号 uid（user_id / customer_id）。
//
// 顺序：credentials.json 的 user_info.id 优先（它就是 uid），
// 其次 user_info.customer_id，最后回落到 zcodejwt 的 payload。
func DiscoverZCodeLocalUID() string {
	for _, p := range ZCodeCredentialsPaths() {
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 {
			continue
		}
		if uid := parseZCodeUID(raw); uid != "" {
			return uid
		}
	}
	return ""
}

func parseZCodeUID(raw []byte) string {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	for k, v := range doc {
		if !strings.HasSuffix(k, ":user_info") {
			continue
		}
		s, _ := v.(string)
		if strings.TrimSpace(s) == "" {
			continue
		}
		if strings.HasPrefix(s, "enc:v1:") {
			plain, err := DecryptZCodeCredential(s)
			if err != nil {
				continue
			}
			s = plain
		}
		var info struct {
			ID         string `json:"id"`
			CustomerID string `json:"customer_id"`
			UserID     any    `json:"user_id"`
		}
		if json.Unmarshal([]byte(s), &info) != nil {
			continue
		}
		if u := strings.TrimSpace(info.ID); u != "" {
			return u
		}
		if u := strings.TrimSpace(info.CustomerID); u != "" {
			return u
		}
	}
	if tok := parseZCodeJWToken(raw); tok != "" {
		return uidFromJWTPayload(tok)
	}
	return ""
}

// uidFromJWTPayload 从 JWT payload 里取 user_id（ZCode 的 zcodejwt 用它）。
func uidFromJWTPayload(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	seg := parts[1]
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return ""
	}
	var payload struct {
		UserID any `json:"user_id"`
		Sub    any `json:"sub"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	for _, v := range []any{payload.UserID, payload.Sub} {
		switch x := v.(type) {
		case string:
			if s := strings.TrimSpace(x); s != "" {
				return s
			}
		case float64:
			return strconvI64(int64(x))
		}
	}
	return ""
}

func strconvI64(v int64) string {
	if v == 0 {
		return ""
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ReadSignCredentialFromLocalClient 从本机 ZCode 客户端读出 <keyId>.<secret>。
//
// 两条来源，任一命中即返回：
//
//  1. config.json 的 provider.*.options.apiKey（客户端解出来的成品，最可靠）
//  2. config.json 里任意层级内嵌的 "<hex32>.<secret>" 文本
//
// 找不到返回空串（调用方按「本机无客户端」处理，不视为错误）。
func ReadSignCredentialFromLocalClient() string {
	for _, p := range ZCodeClientConfigPaths() {
		if c := readSignCredentialFrom(p); c != "" {
			return c
		}
	}
	return ""
}

// ZCodeClientInfo 汇总本机 ZCode 客户端当前可用的全部素材（一次读盘）。
//
// 网关侧「全自动接入」只要这一个入口：拿到 apikey 即可写入池，
// jwt/uid 用于补 access_token 与对齐 uid，raw 供面板展示来源。
type LocalClientInfo struct {
	// APIKey 是 <keyId>.<secret>；空串表示本机客户端没解出来（未登录/未开套餐）。
	APIKey string
	// JWToken 是 zcodejwt（可能为空）。
	JWToken string
	// UID 是登录账号 uid（可能为空）。
	UID string
	// Provider 是 "bigmodel" / "zai"（来自 credentials.json 的 active_provider）。
	Provider string
	// Sources 记录各字段来自哪个文件，供面板排查。
	Sources []string
}

// DiscoverZCodeLocalClient 一次读盘汇总本机 ZCode 客户端素材。
func DiscoverZCodeLocalClient() LocalClientInfo {
	var info LocalClientInfo
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			info.Sources = append(info.Sources, p)
		}
	}

	// 1) config.json：签名凭据（成品）
	for _, p := range ZCodeClientConfigPaths() {
		c := readSignCredentialFrom(p)
		if c == "" {
			continue
		}
		info.APIKey = c
		add(p)
		break
	}
	// 2) config.json：任意层级内嵌凭据（老版本/自定义 provider）
	if info.APIKey == "" {
		for _, p := range ZCodeClientConfigPaths() {
			raw, err := os.ReadFile(p)
			if err != nil || len(raw) == 0 {
				continue
			}
			if c := ExtractSignCredentialFromText(string(raw)); c != "" {
				info.APIKey = c
				add(p)
				break
			}
		}
	}

	// 3) credentials.json：jwt / uid / provider
	for _, p := range ZCodeCredentialsPaths() {
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 {
			continue
		}
		if info.JWToken == "" {
			info.JWToken = parseZCodeJWToken(raw)
		}
		if info.UID == "" {
			info.UID = parseZCodeUID(raw)
		}
		if info.Provider == "" {
			info.Provider = parseZCodeActiveProvider(raw)
		}
		if info.JWToken != "" || info.UID != "" {
			add(p)
		}
	}
	if info.Provider == "" {
		if _, tok := DiscoverZCodeCredentialsFile(); tok != "" {
			info.Provider = "bigmodel"
		}
	}
	return info
}

// parseZCodeActiveProvider 解 credentials.json 的 oauth:active_provider（可能是 enc:v1:）。
func parseZCodeActiveProvider(raw []byte) string {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	v, _ := doc["oauth:active_provider"].(string)
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "enc:v1:") {
		if plain, err := DecryptZCodeCredential(v); err == nil {
			v = strings.TrimSpace(plain)
		} else {
			return ""
		}
	}
	return v
}

// ZCodeLocalClientAvailable 报告本机是否装了 ZCode 客户端（有任一素材文件即算）。
func ZCodeLocalClientAvailable() bool {
	for _, p := range append(ZCodeClientConfigPaths(), ZCodeCredentialsPaths()...) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}
