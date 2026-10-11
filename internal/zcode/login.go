package zcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---- ZCode CLI OAuth 设备流（服务端可驱动，浏览器只负责授权）----
//
// 逆向自 ZCode 桌面端 resources/glm/zcode.cjs（createZaiCliOAuthClient）：
//
//	POST {base}/oauth/cli/init                 Authorization: Bearer <pollToken>
//	     body {"provider":"zai"|"bigmodel"}
//	  →  {code:0,data:{flow_id,poll_token,authorize_url,expires_at,poll_interval_sec}}
//	GET  {base}/oauth/cli/poll/<flow_id>        Authorization: Bearer <pollToken>
//	  →  {code:0,data:{status:"pending"}}
//	  →  {code:0,data:{status:"failed"}}
//	  →  {code:0,data:{status:"ready",token:<zcodeJwtToken>,
//	                   user:{user_id,name,avatar,email},zai:{access_token:<zaiToken>}}}
//
// 【pollToken 用哪一个】官方实现里 pollToken 由客户端本地随机生成，
// init 时作为 Bearer 送出，轮询时继续用同一个本地值（响应里的 data.poll_token
// 是服务端回执，官方代码并未用它轮询）。网关沿用同一口径。
//
// 【为什么 base 是 https://zcode.z.ai/api/v1】官方常量 F7s="https://zcode.z.ai/api/v1"。
//
// 【拿到了什么】ready 里的 data.token 就是 auth 文件里的 access_token（zcodejwt），
// 也就是调用 /api/v1/zcode-plan/anthropic/v1/messages 时 Bearer 所需的凭证；
// data.user.user_id 即 auth 文件里的 uid。api_key 是可选增强（请求签名用），
// 由 ResolveAPIKey 走 BigModel/Z.AI 业务接口换取。

// CLIInitData 是 /oauth/cli/init 的响应数据。
type CLIInitData struct {
	FlowID          string `json:"flow_id"`
	PollToken       string `json:"poll_token"`
	AuthorizeURL    string `json:"authorize_url"`
	ExpiresAt       int64  `json:"expires_at"`
	PollIntervalSec int    `json:"poll_interval_sec"`
}

// CLIUser 是 ready 响应里的用户信息。
type CLIUser struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
	Email  string `json:"email"`
}

// CLIReady 是 ready 状态轮询结果。
type CLIReady struct {
	Status string       `json:"status"`
	Token  string       `json:"token"`
	User   CLIUser      `json:"user"`
	ZAI    *CLITokenSet `json:"zai"`
	// BigModel 是 bigmodel 家族的 ready 载荷。
	//
	// 官方桌面端（app.asar -> out/host/index.js，pollPendingOAuth 的 ready 分支）同时读
	// data.zai 与 data.bigmodel：zai 家族取 zai.access_token，bigmodel 家族取
	// bigmodel.access_token（并带 refresh_token）。
	//
	// 旧实现只解析 zai，于是 bigmodel 家族的号拿不到业务 token，只能回退去读
	// 「本机已登录客户端」的 config.json —— 这就是「电脑不开机就用不了」的根因。
	// 补上 bigmodel 分支后，服务端凭一次设备授权即可自助换出 <keyId>.<secret>。
	BigModel *CLITokenSet `json:"bigmodel"`
}

// CLITokenSet 是 ready 载荷里单个 provider（zai / bigmodel）的 token 集合。
type CLITokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// UnmarshalJSON 兼容 access_token/accessToken、refresh_token/refreshToken 两种写法。
func (t *CLITokenSet) UnmarshalJSON(b []byte) error {
	var raw struct {
		AccessToken  string `json:"access_token"`
		AccessToken2 string `json:"accessToken"`
		RefreshToken string `json:"refresh_token"`
		RefreshTok2  string `json:"refreshToken"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	t.AccessToken = firstNonEmptyTrim(raw.AccessToken, raw.AccessToken2)
	t.RefreshToken = firstNonEmptyTrim(raw.RefreshToken, raw.RefreshTok2)
	return nil
}

// ProviderTokenSet 按 realm 返回该家族的业务 access_token / refresh_token。
//
// bigmodel 家族用 data.bigmodel.*，其余（zai）用 data.zai.*。
// 两者皆无时返回空串，调用方据此判断「上游本轮授权没下发业务 token」。
func (r *CLIReady) ProviderTokenSet(realm string) (accessToken, refreshToken string) {
	if r == nil {
		return "", ""
	}
	if realm == "bigmodel" {
		if r.BigModel != nil {
			return strings.TrimSpace(r.BigModel.AccessToken), strings.TrimSpace(r.BigModel.RefreshToken)
		}
		return "", ""
	}
	if r.ZAI != nil {
		return strings.TrimSpace(r.ZAI.AccessToken), strings.TrimSpace(r.ZAI.RefreshToken)
	}
	return "", ""
}

// firstNonEmptyTrim 返回第一个 trim 后非空的字符串。
func firstNonEmptyTrim(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// CLIInit 发起设备授权，返回授权 URL 与 flow_id。
func CLIInit(ctx context.Context, hc *http.Client, pollToken, provider string) (*CLIInitData, error) {
	if provider == "" {
		provider = "zai"
	}
	body, _ := json.Marshal(map[string]string{"provider": provider})
	req, err := http.NewRequestWithContext(ctx, "POST", BaseURL+"/api/v1/oauth/cli/init", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int          `json:"code"`
		Msg  string       `json:"msg"`
		Data *CLIInitData `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("zcode cli init decode: %w (%s)", err, truncate(string(raw), 200))
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("zcode cli init code=%d msg=%s", env.Code, env.Msg)
	}
	if env.Data == nil || env.Data.FlowID == "" || env.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("zcode cli init: incomplete data (%s)", truncate(string(raw), 200))
	}
	if env.Data.PollIntervalSec <= 0 {
		env.Data.PollIntervalSec = 2
	}
	return env.Data, nil
}

// CLIPoll 轮询一次授权状态。status 为 pending / failed 时 ready 为 nil。
func CLIPoll(ctx context.Context, hc *http.Client, pollToken, flowID string) (string, *CLIReady, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", BaseURL+"/api/v1/oauth/cli/poll/"+flowID, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int       `json:"code"`
		Msg  string    `json:"msg"`
		Data *CLIReady `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", nil, fmt.Errorf("zcode cli poll decode: %w (%s)", err, truncate(string(raw), 200))
	}
	if env.Code != 0 {
		return "", nil, fmt.Errorf("zcode cli poll code=%d msg=%s", env.Code, env.Msg)
	}
	if env.Data == nil {
		return "pending", nil, nil
	}
	switch env.Data.Status {
	case "ready":
		if env.Data.Token == "" {
			return "", nil, fmt.Errorf("zcode cli poll: ready 但缺少 token")
		}
		return "ready", env.Data, nil
	case "failed":
		return "failed", nil, nil
	default:
		return "pending", nil, nil
	}
}

// NewPollToken 生成本地轮询令牌（32 字节 hex，对齐官方 HZr()）。
func NewPollToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- 业务 API Key 换取（可选增强：请求签名的 sign_key_id / sign_secret）----
//
// 逆向自 zcode.cjs 的 createCodingPlanApiKeyResolver / resolveBizApiKey：
//
//	zai      : POST https://api.z.ai/api/auth/z/login {"token": <zaiAccessToken>}
//	           → {access_token}（业务 token）
//	bigmodel : 直接拿 BigModel OAuth 的 access_token 当业务 token
//	两者随后 GET  {bigModelHost}/api/biz/customer/getCustomerInfo
//	           Authorization: <bizToken>   → {organizations:[{organizationId,projects:[{projectId}]}]}
//	GET  .../organization/{oid}/projects/{pid}/api_keys               → 找 name=="zcode-api-key"
//	POST 同 URL {"name":"zcode-api-key"}                              → 不存在则创建
//	GET  .../api_keys/copy/<apiKey>                                    → {secretKey}
//	→ "{apiKey}.{secretKey}"
//
// 该链路失败不阻断登录：网关的 JWT 模式（Bearer zcodejwt）不依赖签名，
// 签名只在带了 sign_key_id 时才叠加。

// BigModelHost 是 BigModel 生产域。
const BigModelHost = "https://bigmodel.cn"

// ZAIBizHost 是 Z.AI 业务域。
const ZAIBizHost = "https://api.z.ai"

const zcodeAPIKeyName = "zcode-api-key"

// bizRequest 发一次业务 JSON 请求并解 {code,data} 信封。
func bizRequest(ctx context.Context, hc *http.Client, method, url, auth string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	// 2026-10-06 实测：bigmodel.cn / api.z.ai 的鉴权业务接口在「凭证有效」时
	// 会返回 200 + Content-Length: 0（空体、无 Content-Type），而无效凭证反而能拿到
	// 标准 JSON 错误。这不是该端点不存在（不存在会回 404 HTML），
	// 也不是鉴权失败，而是网关/缓存层对鉴权成功的业务响应做了置空。
	// 把它翻译成可读错误，避免只报 "unexpected end of JSON input"。
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s 返回 200 但响应体为空（鉴权已通过，业务响应被网关置空）", ErrBizEmptyBody, req.URL.Path)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode: %w (%s)", err, truncate(string(raw), 200))
	}
	if env.Code != 0 && env.Code != 200 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, nil
}

// ErrBizEmptyBody 表示上游鉴权成功但业务响应被置空（宿主网关层）。
var ErrBizEmptyBody = errors.New("zcode biz: empty response body")

// bizCustomer 是 getCustomerInfo 的响应结构（只取机构/项目）。
type bizCustomer struct {
	Organizations []struct {
		OrganizationID   string `json:"organizationId"`
		OrganizationName string `json:"organizationName"`
		Projects         []struct {
			ProjectID   string `json:"projectId"`
			ProjectName string `json:"projectName"`
		} `json:"projects"`
	} `json:"organizations"`
}

// ResolveAPIKey 换取编码方案 API Key（"<keyId>.<secret>"）。
// provider: "zai" / "bigmodel"；oauthAccessToken 是对应 provider 的 OAuth access_token。
func ResolveAPIKey(ctx context.Context, hc *http.Client, provider, oauthAccessToken string) (string, error) {
	if strings.TrimSpace(oauthAccessToken) == "" {
		return "", fmt.Errorf("zcode api key: empty oauth access token")
	}
	bizToken := oauthAccessToken
	host := BigModelHost
	if provider == "zai" {
		data, err := bizRequest(ctx, hc, "POST", ZAIBizHost+"/api/auth/z/login", oauthAccessToken,
			map[string]string{"token": oauthAccessToken})
		if err != nil {
			return "", fmt.Errorf("zai biz login: %w", err)
		}
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(data, &tok)
		if tok.AccessToken == "" {
			return "", fmt.Errorf("zai biz login: missing access_token")
		}
		bizToken = tok.AccessToken
	}
	// getCustomerInfo 取默认机构/项目。
	data, err := bizRequest(ctx, hc, "GET", host+"/api/biz/customer/getCustomerInfo", bizToken, nil)
	if err != nil {
		return "", fmt.Errorf("getCustomerInfo: %w", err)
	}
	var cust bizCustomer
	if err := json.Unmarshal(data, &cust); err != nil {
		return "", fmt.Errorf("getCustomerInfo decode: %w", err)
	}
	orgID, projID := pickOrgProject(cust)
	if orgID == "" || projID == "" {
		return "", fmt.Errorf("getCustomerInfo: 无可用机构/项目")
	}
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", host, orgID, projID)
	var keyID string
	if kd, err := bizRequest(ctx, hc, "GET", keysURL, bizToken, nil); err == nil {
		var keys []struct {
			Name   string `json:"name"`
			APIKey string `json:"apiKey"`
		}
		if json.Unmarshal(kd, &keys) == nil {
			for _, k := range keys {
				if k.Name == zcodeAPIKeyName {
					keyID = k.APIKey
					break
				}
			}
		}
	}
	if keyID == "" {
		cd, err := bizRequest(ctx, hc, "POST", keysURL, bizToken, map[string]string{"name": zcodeAPIKeyName})
		if err != nil {
			return "", fmt.Errorf("create api key: %w", err)
		}
		var created struct {
			APIKey string `json:"apiKey"`
		}
		_ = json.Unmarshal(cd, &created)
		keyID = created.APIKey
	}
	if keyID == "" {
		return "", fmt.Errorf("api key: empty apiKey")
	}
	sd, err := bizRequest(ctx, hc, "GET", keysURL+"/copy/"+keyID, bizToken, nil)
	if err != nil {
		return "", fmt.Errorf("copy api key: %w", err)
	}
	var secret struct {
		SecretKey string `json:"secretKey"`
	}
	_ = json.Unmarshal(sd, &secret)
	if secret.SecretKey == "" {
		return "", fmt.Errorf("copy api key: empty secretKey")
	}
	return keyID + "." + secret.SecretKey, nil
}

// pickOrgProject 选默认机构/项目：优先名字含「默认机构」「默认项目」，否则取第一个。
func pickOrgProject(cust bizCustomer) (string, string) {
	if len(cust.Organizations) == 0 {
		return "", ""
	}
	org := cust.Organizations[0]
	for _, o := range cust.Organizations {
		if strings.Contains(o.OrganizationName, "默认机构") {
			org = o
			break
		}
	}
	if len(org.Projects) == 0 {
		return "", ""
	}
	proj := org.Projects[0]
	for _, p := range org.Projects {
		if strings.Contains(p.ProjectName, "默认项目") {
			proj = p
			break
		}
	}
	return org.OrganizationID, proj.ProjectID
}
