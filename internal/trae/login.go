package trae

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---- Trae 浏览器登录（授权码 + PKCE 换票）----
//
// 逆向自 Trae CN 3.3.99 的登录链路（resources/app/out/main.js）：
//
//  1. 客户端起本地 HTTP server（127.0.0.1:随机端口），回调路径 /authorize；
//  2. 打开 {loginHost}/authorization?login_version=1&auth_from=trae&login_channel=native_ide
//     &plugin_version=<ver>&auth_type=local&client_id=<clientId>&redirect=0
//     &login_trace_id=<uuid>&auth_callback_url=http://127.0.0.1:<port>/authorize
//     &machine_id=<mid>&device_id=<did>&x_device_id=...&code_challenge=<challenge>
//     &code_challenge_method=S256
//  3. 用户登录后服务端 307 到 auth_callback_url，带回 authCodeInfo(JSON,含 AuthCode)
//     与（客户端本地注入的）codeVerifier；
//  4. POST {apiHost}/trae/api/v3/oauth/ExchangeToken，body 为
//     {ClientID, AuthCode, CodeVerifier, DeviceInfo, IDEVersion}（**无 DeviceProof**，
//     与 refresh 换票的唯一差异），响应与原 refresh 换票同形（Result 包裹）。
//
// 网关在浏览器方案里把回环 server 换成面板自身的一个 HTTP 入口，因此
// auth_callback_url 指向线上可达地址（如 https://host/panel/api/login/trae/callback）。

// ExchangeTokenByAuthCode 用授权码 + PKCE verifier 换取首组 token。
// devicePublicKeyPEM 为空时不影响请求（服务端仅记录设备信息）。
func (c *Client) ExchangeTokenByAuthCode(ctx context.Context, authCode, codeVerifier, devicePublicKeyPEM string) (*ExchangeResult, error) {
	if authCode == "" {
		return nil, fmt.Errorf("trae authcode exchange: empty auth code")
	}
	if codeVerifier == "" {
		return nil, fmt.Errorf("trae authcode exchange: empty code verifier")
	}
	deviceID := c.DeviceID()
	machineID := c.a.MachineIDValue()
	if machineID == "" {
		machineID = c.a.UID
	}
	deviceInfo := map[string]any{
		"DeviceID":        deviceID,
		"MachineID":       machineID,
		"PlatformCode":    "IDE_PC",
		"DeviceType":      "PC",
		"DeviceName":      "Trae",
		"DeviceModel":     "PC",
		"ClientVersion":   c.ver,
		"DevicePublicKey": devicePublicKeyPEM,
		"DeviceBrand":     "pc",
		"DeviceCPU":       "",
		"OSInfo":          "windows",
		"OSVersion":       "Windows 11",
	}
	body := map[string]any{
		"ClientID":     OAuthClientID,
		"AuthCode":     authCode,
		"CodeVerifier": codeVerifier,
		"DeviceInfo":   deviceInfo,
		"IDEVersion":   c.ver,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", UGBase+PathExchangeToken, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req.Header)
	req.Header.Set("User-Agent", "Trae/"+c.ver+" (win32; x64)")
	// 授权码换票不携带 Authorization（getHeaders("") 口径）。
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae authcode exchange http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var env exchangeEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("trae authcode exchange decode: %w (%s)", err, truncate(string(data), 200))
	}
	if env.Result == nil || env.Result.Token == "" {
		return nil, fmt.Errorf("trae authcode exchange: empty result (%s)", truncate(string(data), 200))
	}
	return env.Result, nil
}

// DeviceKeyPair 设备密钥对（PEM）。
type DeviceKeyPair struct {
	PrivateKeyPEM string
	PublicKeyPEM  string
}

// GenerateDeviceKeyPair 生成 ECDSA P-256 设备密钥对（PKCS8 私钥 + SPKI 公钥）。
// 与 Trae 客户端 crypto.generateKeyPairSync("ec",{namedCurve:"P-256"}) 同口径，
// 用于后续 refresh 换票时的 DeviceProof 签名。
func GenerateDeviceKeyPair() (*DeviceKeyPair, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}))
	return &DeviceKeyPair{PrivateKeyPEM: privPEM, PublicKeyPEM: pubPEM}, nil
}

// ---- PKCE ----

// PKCEPair 是一次 PKCE 校验对：verifier 本地留存，challenge 随授权 URL 出站。
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE 生成 S256 PKCE 对。
//
// 与 Trae 客户端 QAe() 完全同口径：
//
//	codeVerifier  = base64url(randomBytes(48))     （无 padding）
//	codeChallenge = base64url(sha256(codeVerifier))（对 verifier **字符串** 求哈希）
func GeneratePKCE() (*PKCEPair, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return &PKCEPair{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// MachineIDFromHost 派生本机 machine_id。
//
// 与 Trae 客户端一致：sha256(hostname).hex 取前 32 字符。
// 该值只是设备标识，服务端不校验；这里由调用方传入稳定种子（线上用服务器主机名）。
func MachineIDFromHost(hostname string) string {
	sum := sha256.Sum256([]byte(hostname))
	h := fmt.Sprintf("%x", sum)
	if len(h) > 32 {
		h = h[:32]
	}
	return h
}

var _ = strings.TrimSpace
var _ = context.Background
var _ = time.Now
