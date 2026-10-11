// ZCode 客户端请求签名（Client Request Signing v4）。
//
// 逆向自 ZCode 3.14.x（resources/glm/zcode.cjs 的 ClientRequestSigningV4Signer）。
// 关键常量与流程：
//
//	handshake path : /api/paas/c1f3a7e2/v2/client
//	KDF salt       : "WD_CLIENT_SIGN_KDF_SALT"
//	KDF infos      : "getSignKey_hmac"（握手 HMAC） / "ed25519_priv"（私钥解密）
//	appId          : "zcode"
//	nonce 长度     : 16 字节（hex 32 字符）
//	PoW 难度       : 8 位前导零
//	签名载荷       : "<apiKeyId>\n<ts>\n<clientVersion>\n<sessionId>\n<nonce>"
//
// 凭据为 "<apiKeyId>.<apiKeySecret>"。握手返回 AES-GCM 加密的 Ed25519 私钥。
package zcode

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	signPath      = "/api/paas/c1f3a7e2/v2/client"
	signKDFSalt   = "WD_CLIENT_SIGN_KDF_SALT"
	signKDFHMAC   = "getSignKey_hmac"
	signKDFPriv   = "ed25519_priv"
	signAppID     = "zcode"
	signNonceLen  = 16
	signHandshake = "get_sign_key"
	// signPoWBits 缺省 PoW 难度（前导零位数）。上游可在握手里下发 powBits，
	// 真实客户端用的是**服务端下发值**（zcode.cjs ClientRequestSigning 的
	// e.powBits），8 只是实战观测到的常见值，故当缺省而非定值。
	signPoWBits = 8
)

// Signer 绑定单个账号的签名状态（私钥在内存中缓存，失败即作废重握手）。
type Signer struct {
	keyID  string
	secret string
	origin string // 业务 origin（握手 URL 的 origin）
	appVer string
	httpc  *http.Client

	mu   sync.Mutex
	priv ed25519.PrivateKey
	// powBits 上游握手下发的 PoW 难度；0 = 用缺省 signPoWBits。
	powBits int
}

// SignHandshakeOrigin 是客户端签名握手的真实宿主。
//
// 2026-10-06 实测矩阵（本机真实请求）：
//
//	POST https://open.bigmodel.cn/api/paas/c1f3a7e2/v2/client -> 200 privateCipher
//	POST https://api.z.ai/api/paas/c1f3a7e2/v2/client        -> 200 privateCipher
//	POST https://bigmodel.cn/api/paas/c1f3a7e2/v2/client     -> 200 privateCipher
//	POST https://zcode.z.ai/api/paas/c1f3a7e2/v2/client      -> 404 page not found
//
// 握手端点不在 zcode.z.ai。之前拿业务 baseURL 当 origin，
// 永远 404，这是线上一直握手失败的根因。
const SignHandshakeOrigin = "https://open.bigmodel.cn"

// SignHandshakeOrigins 是握手可用宿主列表，按优先级排序。
var SignHandshakeOrigins = []string{
	"https://open.bigmodel.cn",
	"https://api.z.ai",
	"https://bigmodel.cn",
}

// NewSigner 由 "<keyId>.<secret>" 凭据构造签名器。
// origin 为空时使用 SignHandshakeOrigin（注意：不是业务 baseURL）。
func NewSigner(credential, origin, appVer string, hc *http.Client) (*Signer, error) {
	credential = strings.TrimSpace(credential)
	i := strings.IndexByte(credential, '.')
	if i <= 0 || i == len(credential)-1 {
		return nil, fmt.Errorf("zcode: signing credential must be \"<keyId>.<secret>\"")
	}
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		origin = SignHandshakeOrigin
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Signer{keyID: credential[:i], secret: credential[i+1:], origin: origin, appVer: appVer, httpc: hc}, nil
}

// ---- KDF / 编解码 ----

// hkdfSHA256 实现 RFC 5869 HKDF-SHA256（extract + expand，输出 n 字节）。
func hkdfSHA256(ikm, salt, info []byte, n int) []byte {
	// extract
	ext := hmac.New(sha256.New, salt)
	ext.Write(ikm)
	prk := ext.Sum(nil)
	// expand
	out := make([]byte, 0, n)
	var t []byte
	for i := 1; len(out) < n; i++ {
		h := hmac.New(sha256.New, prk)
		h.Write(t)
		h.Write(info)
		h.Write([]byte{byte(i)})
		t = h.Sum(nil)
		out = append(out, t...)
	}
	return out[:n]
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ---- 握手 ----

// ensureKey 惰性完成握手，返回 Ed25519 私钥。
func (s *Signer) ensureKey(ctx context.Context) (ed25519.PrivateKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priv != nil {
		return s.priv, nil
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := randomHex(signNonceLen)
	hk := hkdfSHA256([]byte(s.secret), []byte(signKDFSalt), []byte(signKDFHMAC), 32)
	mac := hmac.New(sha256.New, hk)
	mac.Write([]byte(signHandshake + "\n" + s.keyID + "\n" + ts + "\n" + nonce))
	sig := b64(mac.Sum(nil))

	credential := s.keyID + "." + s.secret
	body, _ := json.Marshal(map[string]string{"apiKey": credential, "nonce": nonce, "sig": sig, "ts": ts})
	// 握手逐个宿主尝试（s.handshakeOrigins），首个成功即返回。
	// 三个宿主都能签发私钥，但限流/容量状态不同，单宿主写死会在它拖库时整账号不可用。
	var raw []byte
	var lastErr error
	for _, og := range s.handshakeOrigins() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, og+signPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", credential)
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := s.httpc.Do(req.WithContext(rctx))
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("zcode: sign handshake %s: %w", og, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("zcode: sign handshake %s http %d: %s", og, resp.StatusCode, truncate(string(b), 160))
			continue
		}
		raw = b
		break
	}
	if raw == nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("zcode: sign handshake: no origin attempted")
		}
		return nil, lastErr
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			PrivateCipher string `json:"privateCipher"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("zcode: sign handshake json: %w", err)
	}
	if env.Code != 200 || env.Data.PrivateCipher == "" {
		return nil, fmt.Errorf("zcode: sign handshake rejected code=%d msg=%s", env.Code, env.Msg)
	}
	priv, err := decryptPrivateKey(s.keyID, s.secret, env.Data.PrivateCipher)
	if err != nil {
		return nil, err
	}
	s.priv = priv
	return priv, nil
}

// decryptPrivateKey AES-256-GCM 解密握手返回的 PKCS8 Ed25519 私钥。
func decryptPrivateKey(keyID, secret, cipherB64 string) (ed25519.PrivateKey, error) {
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cipherB64))
	if err != nil {
		return nil, fmt.Errorf("zcode: privateCipher base64: %w", err)
	}
	if len(blob) <= 12+16 {
		return nil, fmt.Errorf("zcode: privateCipher too short")
	}
	key := hkdfSHA256([]byte(secret), []byte(signKDFSalt), []byte(signKDFPriv), 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv := blob[:12]
	ct := blob[12:]
	// AAD 是 apiKeyId（客户端 nur 的 additionalData: RV(e)）。
	plain, err := gcm.Open(nil, iv, ct, []byte(keyID))
	if err != nil {
		return nil, fmt.Errorf("zcode: privateCipher decrypt: %w", err)
	}
	// 逆向校准：解密出来的**不是** DER，而是一段 base64 文本；客户端还要再
	// base64 解一次才得到 PKCS8（zcode.cjs: `a = eur(new TextDecoder().decode(s))`
	// 再 `importKey("pkcs8", a, "Ed25519")`）。少这一步会得到
	// "parse pkcs8" 失败，握手永远拿不到可用私钥。
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(plain)))
	if err != nil || len(der) == 0 {
		// 兼容上游直接返回 DER 的形态（严格按客户端口径，但不必因此崩掉）。
		der = plain
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("zcode: parse pkcs8: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("zcode: signing key is not Ed25519")
	}
	return priv, nil
}

// invalidate 作废缓存的私钥（签名被上游拒绝时调用）。
func (s *Signer) invalidate() { s.mu.Lock(); s.priv = nil; s.mu.Unlock() }

// ---- PoW + 业务签名 ----

// solvePoW 求解客户端请求工作量证明。
//
// 逆向校准（zcode.cjs solveClientRequestProofOfWork，已逐字核对）：
//
//	seed   = sha256(apiKeyId + "\n" + appId + "\n" + sessionId + "\n" + ts)
//	prefix = hex(seed)[0:32]
//	nonce0 = 12 随机字节 → hex（24 字符）
//	候选   = nonce0 + hex(counter) 左补零到 8 位
//	命中   = sha256(prefix + "\n" + 候选) 的前 powBits 位为 0
//
// 注意两点（此前实现有偏差，已修正）：
//   - 分隔符是 "\n" 而非空格。空格版能被上游接受只是运气（难度 8 位下
//     两版都能凑出解），但与客户端不一致会让服务端校验在严格模式下失配。
//   - 难度取上游下发的 powBits（powBits<=0 时回落到 signPoWBits）。
func solvePoW(apiKeyID, appID, sessionID, ts string, powBits int) string {
	if powBits <= 0 {
		powBits = signPoWBits
	}
	sum := sha256.Sum256([]byte(apiKeyID + "\n" + appID + "\n" + sessionID + "\n" + ts))
	prefix := hex.EncodeToString(sum[:])[:32]
	nonce := randomHex(12)
	for i := uint32(0); ; i++ {
		cand := nonce + fmt.Sprintf("%08x", i)
		d := sha256.Sum256([]byte(prefix + "\n" + cand))
		if hasLeadingZeroBits(d[:], powBits) {
			return cand
		}
	}
}

// hasLeadingZeroBits 判定摘要前 n 位（bit）是否全零。
// 与客户端 e7i(msg, bits) 同口径：先整字节比较，剩余位按掩码判。
func hasLeadingZeroBits(d []byte, bits int) bool {
	full := bits / 8
	for i := 0; i < full && i < len(d); i++ {
		if d[i] != 0 {
			return false
		}
	}
	rem := bits % 8
	if rem == 0 || full >= len(d) {
		return true
	}
	mask := byte(255 << (8 - rem))
	return d[full]&mask == 0
}

// handshakeOrigins 返回依次尝试的握手宿主列表：显式指定的 origin 最先，
// 其次为全局可用宿主。去重。
func (s *Signer) handshakeOrigins() []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimRight(strings.TrimSpace(v), "/")
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(s.origin)
	for _, og := range SignHandshakeOrigins {
		add(og)
	}
	return out
}

// SignRequest 就地为即将出站的请求注入签名头：
//
//	X-Client-Ts / X-Client-Version / X-Client-Sig / X-Client-Nonce / X-App-Id / X-Client-Pow
//
// sessionID 必须非空（X-Session-Id）。失败返回错误，调用方可回退为未签名请求。
func (s *Signer) SignRequest(ctx context.Context, req *http.Request, sessionID, clientVersion string) error {
	if s == nil || s.keyID == "" {
		return fmt.Errorf("zcode: no signing credential")
	}
	if sessionID == "" {
		return fmt.Errorf("zcode: signing requires X-Session-Id")
	}
	priv, err := s.ensureKey(ctx)
	if err != nil {
		return err
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := randomHex(signNonceLen)
	payload := s.keyID + "\n" + ts + "\n" + clientVersion + "\n" + sessionID + "\n" + nonce
	sig := b64(ed25519.Sign(priv, []byte(payload)))
	pow := solvePoW(s.keyID, signAppID, sessionID, ts, s.powBits)
	h := req.Header
	h.Set("X-Client-Ts", ts)
	h.Set("X-Client-Version", clientVersion)
	h.Set("X-Client-Sig", sig)
	h.Set("X-Session-Id", sessionID)
	h.Set("X-Client-Nonce", nonce)
	h.Set("X-App-Id", signAppID)
	h.Set("X-Client-Pow", pow)
	return nil
}

// VerifyRejected 判断上游响应体是否表示签名被拒（需重握手）。
func VerifyRejected(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "VERIFY_SIGNATURE_INVALID") || strings.Contains(s, "VERIFY_APIKEY_EXPIRED")
}
