package trae

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// ---- Trae 长登录（ExchangeToken） ----
//
// 逆向自 Trae CN 3.3.96 的 byteCrypto / oauth 模块：
//   POST https://api.trae.cn/trae/api/v3/oauth/ExchangeToken
//   用 refreshToken + 设备私钥签名（DeviceProof）换取新的 Token / RefreshToken。
//   DeviceProof 原文：
//       "POST\n/trae/api/v3/oauth/ExchangeToken\n<ClientID>\n<RefreshToken>\n<Timestamp>\n<Nonce>"
//   算法：ECDSA-P256 + SHA256，签名结果 base64。
//   Token / RefreshToken 通过响应 Result.Token / Result.RefreshToken 返回，过期时间为毫秒。

const (
	// PathExchangeToken 是刷新换票端点。
	PathExchangeToken = "/trae/api/v3/oauth/ExchangeToken"
	// OAuthClientID 是 Trae CN 客户端写死的 ClientID。
	OAuthClientID = "ono9krqynydwx5"
)

// ExchangeResult 是 ExchangeToken 的响应载荷。
type ExchangeResult struct {
	Token            string `json:"Token"`
	UserJwt          string `json:"UserJwt"`
	RefreshToken     string `json:"RefreshToken"`
	TokenExpireAt    int64  `json:"TokenExpireAt"`
	RefreshExpireAt  int64  `json:"RefreshExpireAt"`
	BoundDeviceID    string `json:"BoundDeviceID"`
	DeviceBindStatus string `json:"DeviceBindStatus"`
}

type exchangeEnvelope struct {
	Result *ExchangeResult `json:"Result"`
}

// ExchangeToken 用 refreshToken 换一组新的 token。不修改 a，调用方负责回写。
func (c *Client) ExchangeToken(ctx context.Context, refresh string) (*ExchangeResult, error) {
	if refresh == "" {
		return nil, fmt.Errorf("trae exchange: empty refresh token")
	}
	privPEM := c.a.DevicePrivateKeyValue()
	if privPEM == "" {
		return nil, fmt.Errorf("trae exchange: missing device private key (need device_private_key in auth file)")
	}
	ts := time.Now().Unix()
	nonce := randomHex(16)
	payload := strings.Join([]string{"POST", PathExchangeToken, OAuthClientID, refresh, strconv.FormatInt(ts, 10), nonce}, "\n")
	sig, err := signDeviceProof(privPEM, payload)
	if err != nil {
		return nil, err
	}

	deviceID := c.DeviceID()
	machineID := c.a.MachineIDValue()
	if machineID == "" {
		machineID = c.a.UID
	}
	body := map[string]any{
		"ClientID":     OAuthClientID,
		"ClientSecret": "",
		"RefreshToken": refresh,
		"DeviceInfo": map[string]any{
			"DeviceID":        deviceID,
			"MachineID":       machineID,
			"PlatformCode":    "IDE_PC",
			"DeviceType":      "PC",
			"DeviceName":      "Trae",
			"DeviceModel":     "PC",
			"ClientVersion":   c.ver,
			"DevicePublicKey": c.a.DevicePublicKeyValue(),
			"DeviceBrand":     "pc",
			"OSInfo":          "windows",
			"OSVersion":       "Windows 11",
		},
		"DeviceProof": map[string]any{
			"Signature": sig,
			"Timestamp": ts,
			"Nonce":     nonce,
		},
		"IDEVersion": c.ver,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", UGBase+PathExchangeToken, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req.Header)
	req.Header.Set("Authorization", "Bearer "+refresh)
	req.Header.Set("User-Agent", "Trae/"+c.ver+" (win32; x64)")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae exchange http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var env exchangeEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("trae exchange decode: %w (%s)", err, truncate(string(data), 200))
	}
	if env.Result == nil || env.Result.Token == "" {
		return nil, fmt.Errorf("trae exchange: empty result (%s)", truncate(string(data), 200))
	}
	return env.Result, nil
}

// RefreshAccessToken 就地刷新账号 token 并原子回写 auth 文件。
// 返回 (changed, err)：changed 为 true 表示发生了实际换票。
func (c *Client) RefreshAccessToken(ctx context.Context, a *auth.Auth) (bool, error) {
	rt := a.RefreshTokenValue()
	if rt == "" {
		return false, fmt.Errorf("trae refresh: no refresh_token")
	}
	res, err := c.ExchangeToken(ctx, rt)
	if err != nil {
		return false, err
	}
	a.SetTokens(res.Token, res.RefreshToken, res.TokenExpireAt, res.RefreshExpireAt)
	if err := a.SaveAtomic(); err != nil {
		// 换票成功但落盘失败：token 已在内存生效，返回错误让调用方知晓。
		return true, fmt.Errorf("trae refresh: persist: %w", err)
	}
	return true, nil
}

// EnsureFreshToken 若 AccessToken 距过期不足 margin 则先换票；否则直接返回。
func (c *Client) EnsureFreshToken(ctx context.Context, a *auth.Auth, margin time.Duration) error {
	exp := a.TokenExpireAtValue()
	if exp > 0 && a.TokenValue() != "" {
		if time.Until(time.UnixMilli(exp)) > margin {
			return nil
		}
	}
	if a.RefreshTokenValue() == "" {
		return nil // 无 refreshToken，无法刷新；交由上游判定
	}
	_, err := c.RefreshAccessToken(ctx, a)
	return err
}

// signDeviceProof 用设备私钥对 payload 做 ECDSA-P256/SHA256 签名并 base64。
func signDeviceProof(privPEM, payload string) (string, error) {
	key, err := parseECPrivateKey(privPEM)
	if err != nil {
		return "", fmt.Errorf("trae device key: %w", err)
	}
	h := sha256.Sum256([]byte(payload))
	r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
	if err != nil {
		return "", err
	}
	// 与 Node 一致：IEEE-P1363 (r||s) 而非 ASN.1。
	curveBytes := (key.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, curveBytes*2)
	r.FillBytes(sig[:curveBytes])
	s.FillBytes(sig[curveBytes:])
	return base64.StdEncoding.EncodeToString(sig), nil
}

func parseECPrivateKey(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemStr)))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok {
			return ec, nil
		}
		return nil, fmt.Errorf("not an EC key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// PublicKeyPEMFromPrivate 从私钥 PEM 派生 SPKI 公钥 PEM（导入多账号时用）。
func PublicKeyPEMFromPrivate(privPEM string) (string, error) {
	k, err := parseECPrivateKey(privPEM)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LoadDeviceKeysFromDir 从目录内的 icube-dc 凭证文件读取设备密钥对（可选辅助）。
func LoadDeviceKeysFromDir(path string) (priv, pub string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return "", "", fmt.Errorf("bad device key file")
	}
	return m["privateKeyPEM"], m["publicKeyPEM"], nil
}
