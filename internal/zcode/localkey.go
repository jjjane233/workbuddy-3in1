package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// 本文件实现「本机 ZCode 客户端签名凭据」的全自动获取。
//
// 背景（2026-10-06 曾判定「换 key 无法全自动」，本文件推翻该结论）：
//
//	上游 /api/biz/customer/getCustomerInfo 对本机 OAuth token 回 200 + 空体，
//	但空体**不是服务端**造成的 —— 是 ZCode 客户端自己的请求拦截器：
//	  app.asar -> isCurrentOAuthCredentialRequest()
//	当请求命中该路径、且 Authorization 就是本机 oauth:<family>:access_token 时，
//	由**客户端进程**直接回填响应体。网关在机房直连没有这层拦截器，所以看到空体。
//
//	同时，客户端内部逻辑（zcode.cjs -> createCodingPlanApiKeyResolver）每次刷新
//	都会跑：getCustomerInfo -> api_keys -> api_keys/copy/<key> -> "<key>.<secret>"，
//	并把它写进 %USERPROFILE%\.zcode\v2\config.json 的
//	provider["builtin:bigmodel-coding-plan"].options.apiKey。
//
//	→ 因此「全自动」有两条互补通道，二者都无需人工手抄：
//	   A. 服务端换 key（ResolveAPIKeyAt）：不经客户端拦截器，直连上游；
//	      bigmodel 家族可能拿不到（受限），zai 家族通常可拿。
//	   B. 客户端成品读取（auth.ReadSignCredentialFromLocalClient）：
//	      读客户端已经解好的 config.json，永远可用（前提是客户端登录过且解出过）。
//
//	EnsureLocalClientAPIKey 把两者按 A→B 顺序串起来。

// ErrNoLocalCredential 表示本机既换不到、也读不到 ZCode 签名凭据。
var ErrNoLocalCredential = errors.New("zcode: 本机无可用 ZCode 签名凭据（客户端未登录或未解出）")

// BizHosts 是「换 key」链路可用的宿主。
//
// 客户端 createCodingPlanApiKeyResolver 里：
//
//	bigmodel 家族 host = https://bigmodel.cn（生产默认，zcode.cjs 的 Yor）
//	zai     家族 host = https://api.z.ai   （zcode.cjs 的 UZr）
var (
	BizHostBigModel = "https://bigmodel.cn"
	BizHostZAI      = "https://api.z.ai"
)

// bizKeyName 是换 key 链路里要复用的 API Key 名称（客户端常量 jZr）。
const bizKeyName = "zcode-api-key"

// ---- 服务端换 key（通道 A）----

// bizEnvelope 是 BigModel/Z.AI 业务接口的统一信封。
type bizEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// bizOK 判定信封是否成功（客户端 isSuccessfulBigModelBizEnvelope 同口径）。
func bizOK(env *bizEnvelope) bool {
	if env.Success != nil && !*env.Success {
		return false
	}
	if env.Code == 0 || env.Code == 200 {
		return true
	}
	return false
}

// bizGet 发一次业务请求；空体返回 ErrBizEmptyBody（沿用 login.go 的语义）。
func bizGet(ctx context.Context, hc *http.Client, method, url, authorization string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s %s", ErrBizEmptyBody, method, url)
	}
	var env bizEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode: %w (%s)", err, truncate(string(raw), 200))
	}
	if !bizOK(&env) {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, nil
}

// pickOrgProjectFromData 从 getCustomerInfo 的 data 里挑默认机构/项目。
// 客户端 pickOrgAndProject 同口径：优先名字含「默认机构」「默认项目」，否则取第一个。
func pickOrgProjectFromData(data json.RawMessage) (orgID, projID string) {
	var cust struct {
		Organizations []struct {
			OrganizationID   string `json:"organizationId"`
			OrganizationName string `json:"organizationName"`
			Projects         []struct {
				ProjectID   string `json:"projectId"`
				ProjectName string `json:"projectName"`
			} `json:"projects"`
		} `json:"organizations"`
	}
	if json.Unmarshal(data, &cust) != nil || len(cust.Organizations) == 0 {
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

// ResolveAPIKeyAt 在指定宿主上跑完整换 key 链路，返回 "<keyId>.<secret>"。
//
//	GET  {host}/api/biz/customer/getCustomerInfo
//	GET  {host}/api/biz/v1/organization/{org}/projects/{proj}/api_keys
//	     -> 找不到名为 zcode-api-key 的就 POST 建一个
//	GET  {host}/api/biz/v1/organization/{org}/projects/{proj}/api_keys/copy/{apiKey}
//	     -> secretKey
//
// requireSecret=false 时（bigmodel 分支）secret 拿不到只返回 keyId。
func ResolveAPIKeyAt(ctx context.Context, hc *http.Client, host, authorization string, requireSecret bool) (string, error) {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		return "", fmt.Errorf("zcode: empty biz host")
	}
	if strings.TrimSpace(authorization) == "" {
		return "", fmt.Errorf("zcode: empty authorization")
	}
	data, err := bizGet(ctx, hc, "GET", host+"/api/biz/customer/getCustomerInfo", authorization, nil)
	if err != nil {
		return "", fmt.Errorf("getCustomerInfo: %w", err)
	}
	orgID, projID := pickOrgProjectFromData(data)
	if orgID == "" || projID == "" {
		return "", fmt.Errorf("getCustomerInfo: 无可解析的机构/项目")
	}
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", host, orgID, projID)

	keyID := ""
	if kd, err := bizGet(ctx, hc, "GET", keysURL, authorization, nil); err == nil {
		var keys []struct {
			Name   string `json:"name"`
			APIKey string `json:"apiKey"`
		}
		if json.Unmarshal(kd, &keys) == nil {
			for _, k := range keys {
				if k.Name == bizKeyName {
					keyID = strings.TrimSpace(k.APIKey)
					break
				}
			}
		}
	}
	if keyID == "" {
		cd, err := bizGet(ctx, hc, "POST", keysURL, authorization, map[string]string{"name": bizKeyName})
		if err != nil {
			return "", fmt.Errorf("create api key: %w", err)
		}
		var created struct {
			APIKey string `json:"apiKey"`
		}
		_ = json.Unmarshal(cd, &created)
		keyID = strings.TrimSpace(created.APIKey)
	}
	if keyID == "" {
		return "", fmt.Errorf("api key: empty apiKey")
	}
	sd, err := bizGet(ctx, hc, "GET", keysURL+"/copy/"+keyID, authorization, nil)
	if err != nil {
		if requireSecret {
			return "", fmt.Errorf("copy api key: %w", err)
		}
		return keyID, nil
	}
	var secret struct {
		SecretKey string `json:"secretKey"`
	}
	_ = json.Unmarshal(sd, &secret)
	if sk := strings.TrimSpace(secret.SecretKey); sk != "" {
		return keyID + "." + sk, nil
	}
	if requireSecret {
		return "", fmt.Errorf("copy api key: empty secretKey")
	}
	return keyID, nil
}

// ResolveAPIKeyFromLocalOAuth 用本机客户端 OAuth token 走通道 A。
//
// provider=="zai" 时先 POST https://api.z.ai/api/auth/z/login 换 biz token
// （客户端 resolveZaiBizToken 同口径），再跑换 key。
func ResolveAPIKeyFromLocalOAuth(ctx context.Context, hc *http.Client) (cred, provider string, err error) {
	prov, tok := auth.DiscoverZCodeCredentialsFile()
	if tok == "" {
		return "", "", ErrNoLocalCredential
	}
	if prov == "zai" {
		zb, err := bizGet(ctx, hc, "POST", BizHostZAI+"/api/auth/z/login", tok, map[string]string{"token": tok})
		if err != nil {
			return "", prov, fmt.Errorf("zai biz login: %w", err)
		}
		var t struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(zb, &t)
		if strings.TrimSpace(t.AccessToken) == "" {
			return "", prov, fmt.Errorf("zai biz login: missing access_token")
		}
		c, err := ResolveAPIKeyAt(ctx, hc, BizHostZAI, "Bearer "+t.AccessToken, true)
		return c, prov, err
	}
	c, err := ResolveAPIKeyAt(ctx, hc, BizHostBigModel, tok, false)
	return c, "bigmodel", err
}

// ---- 统一入口：通道 A → 通道 B ----

// LocalKeyResult 是一次「本机凭据解析」的结果。
type LocalKeyResult struct {
	// Credential 是 "<keyId>.<secret>"。
	Credential string
	// Source 说明来源："exchange:bigmodel" / "exchange:zai" / "client-config"。
	Source string
	// ChannelErr 记录通道 A 的失败原因（即便最终由通道 B 成功），便于面板排查。
	ChannelErr string
	// Info 是客户端素材（uid / jwt / provider）。
	Info auth.LocalClientInfo
}

// EnsureLocalClientAPIKey 按「服务端换 key → 客户端成品」顺序拿签名凭据。
//
// 任一通道成功即返回；都失败返回 ErrNoLocalCredential。
// 无论成功与否，结果里的 Info 都带上本机客户端能提供的 uid/jwt/provider。
func EnsureLocalClientAPIKey(ctx context.Context, hc *http.Client) (*LocalKeyResult, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res := &LocalKeyResult{Info: auth.DiscoverZCodeLocalClient()}

	// 通道 A：服务端换 key
	cred, prov, err := ResolveAPIKeyFromLocalOAuth(ctx, hc)
	if err != nil {
		res.ChannelErr = err.Error()
	} else if cred != "" {
		res.Credential = cred
		res.Source = "exchange:" + prov
		if res.Info.Provider == "" {
			res.Info.Provider = prov
		}
		return res, nil
	}

	// 通道 B：客户端已解出的成品
	if c := auth.ReadSignCredentialFromLocalClient(); c != "" {
		res.Credential = c
		res.Source = "client-config"
		return res, nil
	}
	if res.Credential == "" {
		return res, ErrNoLocalCredential
	}
	return res, nil
}

// ValidateAPIKey 用一把 "<keyId>.<secret>" 打一次真实对话，确认可用。
//
// 打的是 anthropic 协议（与网关对外口径一致）：
//
//	POST https://open.bigmodel.cn/api/anthropic/v1/messages
//
// model 默认 glm-5.3-flash（1 亿 token 套餐的授权模型；普通额度也能过）。
func ValidateAPIKey(ctx context.Context, hc *http.Client, cred, model string) error {
	cred = strings.TrimSpace(cred)
	if cred == "" {
		return fmt.Errorf("zcode: empty credential")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	if strings.TrimSpace(model) == "" {
		model = "glm-5.3-flash"
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 8,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://open.bigmodel.cn/api/anthropic/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("x-api-key", cred)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}
