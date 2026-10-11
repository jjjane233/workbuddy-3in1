package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 本文件回答面板上反复被问到的那个问题：
//
//	「每加一个 ZCode 号都要手抄一次 <keyId>.<secret>，能不能自动化？」
//
// 结论：能。ZCode 官方客户端本来就是自动取得的（反编译 zcode.cjs 的
// createCodingPlanApiKeyResolver：每次刷新都跑
// getCustomerInfo -> api_keys -> api_keys/copy/<key> -> "<key>.<secret>"）。
// 服务端的换 Key 逻辑在 internal/zcode/login.go 的 ResolveAPIKey。
//
// 本文件负责「取料」：从本机 ZCode 客户端落盘的文件里把能拿去换 Key 的
// OAuth access_token 抠出来，或者从任意文本里认出已经成型的
// "<keyId>.<secret>"。

// ZCodeCredentialsPaths 返回 credentials.json 可能的位置。
//
//	%USERPROFILE%\.zcode\v2\credentials.json   ZCode 3.14.x 主落盘位置
//	%APPDATA%\ZCode\credentials.json           安装器级配置（历史版本）
func ZCodeCredentialsPaths() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".zcode", "v2", "credentials.json"))
	}
	if app := os.Getenv("APPDATA"); app != "" {
		out = append(out, filepath.Join(app, "ZCode", "credentials.json"))
	}
	return out
}

// DiscoverZCodeCredentialsFile 从本机 ZCode 客户端的 credentials.json 里
// 解出 (provider, accessToken)，provider 为 "bigmodel" 或 "zai"。
//
// accessToken 是 OAuth access_token（JWT），不是 <keyId>.<secret>；
// 调用方拿到后应该再走 zcode.ResolveAPIKey 去换真正的业务 Key。
//
// 值可能是 "enc:v1:<iv>.<tag>.<ct>"（AES-256-GCM），解不开就跳过，不报错。
func DiscoverZCodeCredentialsFile() (provider, accessToken string) {
	for _, p := range ZCodeCredentialsPaths() {
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 {
			continue
		}
		if prov, tok := ParseZCodeCredentials(raw); tok != "" {
			return prov, tok
		}
	}
	return "", ""
}

// ParseZCodeCredentials 从 credentials.json 原文里解出可用的 OAuth access_token。
//
// 优先顺序：oauth:bigmodel:access_token > oauth:zai:access_token > 任一 oauth:*:access_token。
//
// 为什么优先 bigmodel：本机实测（2026-10-06）只有 bigmodel 那条是活的；
// zai 分支去 api.z.ai 换 Key 会回 "Z.ai user information is invalid"。
func ParseZCodeCredentials(raw []byte) (provider, token string) {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return "", ""
	}
	pick := func(key, expProv string) (string, string) {
		v, ok := doc[key].(string)
		if !ok || v == "" {
			return "", ""
		}
		if strings.HasPrefix(v, "enc:v1:") {
			plain, err := DecryptZCodeCredential(v)
			if err != nil {
				return "", ""
			}
			v = plain
		}
		if strings.Count(v, ".") != 2 {
			return "", ""
		}
		return expProv, strings.TrimSpace(v)
	}
	if p, t := pick("oauth:bigmodel:access_token", "bigmodel"); t != "" {
		return p, t
	}
	if p, t := pick("oauth:zai:access_token", "zai"); t != "" {
		return p, t
	}
	for k, v := range doc {
		if !strings.HasPrefix(k, "oauth:") || !strings.HasSuffix(k, ":access_token") {
			continue
		}
		s, _ := v.(string)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "enc:v1:") {
			plain, err := DecryptZCodeCredential(s)
			if err != nil {
				continue
			}
			s = plain
		}
		if strings.Count(s, ".") != 2 {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(k, "oauth:"), ":access_token")
		return name, strings.TrimSpace(s)
	}
	return "", ""
}

// DecryptZCodeCredential 解 "enc:v1:<iv_b64url>.<tag_b64url>.<ct_b64url>"。
//
// 算法（对照 ZCode 客户端实现）：
//
//	key = sha256(<fallbackSecret>)            // 32 字节，直接当 AES-256 密钥
//	AES-256-GCM(key, iv=<iv>, aad=nil, ct=<ct>, tag=<tag>)
//
// fallbackSecret 见 ZCodeCredentialSecrets。
func DecryptZCodeCredential(enc string) (string, error) {
	body := strings.TrimPrefix(enc, "enc:v1:")
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("auth: bad zcode credential blob")
	}
	dec := func(s string) ([]byte, error) {
		if m := len(s) % 4; m != 0 {
			s += strings.Repeat("=", 4-m)
		}
		return base64.URLEncoding.DecodeString(s)
	}
	iv, err := dec(parts[0])
	if err != nil {
		return "", err
	}
	tag, err := dec(parts[1])
	if err != nil {
		return "", err
	}
	ct, err := dec(parts[2])
	if err != nil {
		return "", err
	}
	for _, sec := range ZCodeCredentialSecrets() {
		key := sha256.Sum256([]byte(sec))
		block, err := aes.NewCipher(key[:])
		if err != nil {
			continue
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			continue
		}
		sealed := make([]byte, 0, len(ct)+len(tag))
		sealed = append(sealed, ct...)
		sealed = append(sealed, tag...)
		plain, err := gcm.Open(nil, iv, sealed, nil)
		if err == nil {
			return string(plain), nil
		}
	}
	return "", fmt.Errorf("auth: zcode credential decrypt failed")
}

// ZCodeCredentialSecrets 返回本机 ZCode 凭据加密密钥的候选。
//
// 客户端实现（app.asar -> createZCodeCredentialCipher / resolveCredentialSecret）：
//
//	secret = ZCODE_CREDENTIAL_SECRET 环境变量，否则
//	         "zcode-credential-fallback:" + os.platform() + ":" + os.homedir() + ":" + os.userInfo().username
//	key    = sha256(secret)
//
// 注意 os.platform() 与 Go 的 runtime.GOOS **不同名**：
//
//	Node os.platform() : win32 / darwin / linux
//	Go runtime.GOOS   : windows / darwin / linux
//
// Windows 上客户端写的是 "win32"，而 runtime.GOOS 是 "windows" —— 这正是
// 3.14.4 本机凭据 auto 解密失败的根因。这里同时给出两套命名。
func ZCodeCredentialSecrets() []string {
	home, _ := os.UserHomeDir()
	plats := []string{nodePlatform(), runtime.GOOS}
	var out []string
	seen := map[string]bool{}
	for _, pl := range plats {
		if pl == "" {
			continue
		}
		for _, u := range []string{os.Getenv("USERNAME"), os.Getenv("USER")} {
			if u == "" {
				continue
			}
			s := fmt.Sprintf("zcode-credential-fallback:%s:%s:%s", pl, home, u)
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// nodePlatform 返回客户端 os.platform() 的取值（win32 / darwin / linux）。
func nodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}

// ExtractSignCredentialFromText 从任意文本里认出 "<keyId>.<secret>"。
//
// 面板「自动获取 / 一键补齐」要能吃下用户直接粘贴的：
//
//  1. 凭据本体                c7c12f...416.AbCdEfGh12345678
//  2. 整份 config.json        任意层级下的 "apiKey" / "api_key" / "key" / "credential"
//  3. provider_config.json    "access": {"type":"api-key","apiKey":"..."}
//  4. key=value / key: value  行文本
//  5. 上述任一的 JSON 字符串（内嵌在更大文本里）
//
// 找不到合法凭据返回空串。
func ExtractSignCredentialFromText(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	if c := strings.Trim(t, "'\""); LooksLikeSignCredential(c) {
		return c
	}
	if c := scanJSONForCredential([]byte(t)); c != "" {
		return c
	}
	for _, line := range strings.Split(t, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "'\"")
		line = strings.TrimSuffix(line, ",")
		low := strings.ToLower(line)
		for _, k := range []string{"apikey", "api_key", "api-key", "credential", "key"} {
			for _, sep := range []string{"\":", "\" :", "\": ", "\" =", "=", ":"} {
				if strings.HasPrefix(low, k+sep) || strings.HasPrefix(low, "\""+k+sep) {
					rest := line
					if i := strings.IndexAny(rest, "=:"); i >= 0 {
						rest = rest[i+1:]
					}
					v := strings.Trim(strings.TrimSpace(rest), "\"', ")
					if LooksLikeSignCredential(v) {
						return v
					}
				}
			}
		}
		if c := findCredentialSubstring(line); c != "" {
			return c
		}
	}
	return ""
}

// scanJSONForCredential 解析 JSON 后遍历所有字符串叶子。
func scanJSONForCredential(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(node any) string {
		switch x := node.(type) {
		case string:
			if LooksLikeSignCredential(x) {
				return x
			}
		case map[string]any:
			for k, val := range x {
				lk := strings.ToLower(k)
				if s, ok := val.(string); ok && LooksLikeSignCredential(s) {
					if strings.Contains(lk, "apikey") || strings.Contains(lk, "api_key") ||
						strings.Contains(lk, "api-key") || lk == "key" ||
						strings.Contains(lk, "credential") {
						return s
					}
				}
			}
			for _, val := range x {
				if c := walk(val); c != "" {
					return c
				}
			}
		case []any:
			for _, val := range x {
				if c := walk(val); c != "" {
					return c
				}
			}
		}
		return ""
	}
	return walk(v)
}

// findCredentialSubstring 在一行文本里找 "<hex32>.<secret>" 子串。
func findCredentialSubstring(line string) string {
	for i := 0; i+34 <= len(line); i++ {
		if !isHexLower(line[i]) {
			continue
		}
		if line[i+32] != '.' {
			continue
		}
		ok := true
		for j := i; j < i+32; j++ {
			if !isHexLower(line[j]) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		end := i + 33
		for end < len(line) && isSecretChar(line[end]) {
			end++
		}
		if c := line[i:end]; LooksLikeSignCredential(c) {
			return c
		}
	}
	return ""
}

func isHexLower(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

func isSecretChar(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		c == '_' || c == '-'
}
