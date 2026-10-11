package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestZCodeCredentialSecretsUsesNodePlatform 钉死本机凭据解密口令的平台串。
//
// 客户端用 os.platform()（Windows="win32"），Go 的 runtime.GOOS 是 "windows"。
// 之前只给 runtime.GOOS，导致 Windows 上 auto 解密**永远**失败。
func TestZCodeCredentialSecretsUsesNodePlatform(t *testing.T) {
	home, _ := os.UserHomeDir()
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		t.Skip("no USERNAME/USER env")
	}
	wantNode := "zcode-credential-fallback:" + nodePlatform() + ":" + home + ":" + user
	got := ZCodeCredentialSecrets()
	if !contains(got, wantNode) {
		t.Fatalf("secrets %v missing node-platform variant %q", got, wantNode)
	}
	if runtime.GOOS == "windows" && nodePlatform() != "win32" {
		t.Fatalf("nodePlatform()=%q, want win32 on windows", nodePlatform())
	}
}

// TestDecryptZCodeCredentialNodePlatformRoundTrip 用 node 平台串加密，必须能解开。
func TestDecryptZCodeCredentialNodePlatformRoundTrip(t *testing.T) {
	home, _ := os.UserHomeDir()
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		t.Skip("no USERNAME/USER env")
	}
	secret := "zcode-credential-fallback:" + nodePlatform() + ":" + home + ":" + user
	blob := mustEncrypt(t, secret, "hello-zcode-credential")
	got, err := DecryptZCodeCredential(blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "hello-zcode-credential" {
		t.Fatalf("got %q", got)
	}
}

// TestParseZCodeUIDFromUserInfo user_info.id 就是 uid。
func TestParseZCodeUIDFromUserInfo(t *testing.T) {
	home, _ := os.UserHomeDir()
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		t.Skip("no USERNAME/USER env")
	}
	secret := "zcode-credential-fallback:" + nodePlatform() + ":" + home + ":" + user
	info := `{"id":"10000000000000001","username":"demo-user","customer_id":"999"}`
	enc := mustEncrypt(t, secret, info)
	raw := []byte(`{"oauth:bigmodel:user_info":"` + enc + `"}`)
	if got := parseZCodeUID(raw); got != "10000000000000001" {
		t.Fatalf("parseZCodeUID=%q", got)
	}
}

// TestParseZCodeUIDFallsBackToCustomerID id 缺失时用 customer_id。
func TestParseZCodeUIDFallsBackToCustomerID(t *testing.T) {
	var doc map[string]any
	raw := []byte(`{"oauth:bigmodel:user_info":"{\"customer_id\":\"4711\"}"}`)
	if i := len(raw); i > 0 {
		_ = doc
	}
	if got := parseZCodeUID(raw); got != "4711" {
		t.Fatalf("parseZCodeUID=%q", got)
	}
}

// TestReadSignCredentialFromLocalClientConfig 客户端 config.json 的成品凭据要能读出。
func TestReadSignCredentialFromLocalClientConfig(t *testing.T) {
	dir := t.TempDir()
	// 直接复用 readSignCredentialFrom（ZCodeSignCredentialPaths 是固定路径，
	// 这里验证的是「解析口径」，与 DiscoverZCodeSignCredential 同源）。
	p := filepath.Join(dir, "config.json")
	body := `{"provider":{"builtin:bigmodel-coding-plan":{"options":{"apiKey":"f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"}}}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readSignCredentialFrom(p); got != "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678" {
		t.Fatalf("readSignCredentialFrom=%q", got)
	}
}

// TestZCodeClientConfigPathsNonEmpty 至少给出一个候选路径。
func TestZCodeClientConfigPathsNonEmpty(t *testing.T) {
	if len(ZCodeClientConfigPaths()) == 0 {
		t.Fatal("expected at least one client config path")
	}
}

// TestExtractSignCredentialFromNestedConfig config.json 里内嵌凭据也能认（不止 apiKey 字段）。
func TestExtractSignCredentialFromNestedConfig(t *testing.T) {
	text := `{"a":{"b":["x","f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"]}}`
	if got := ExtractSignCredentialFromText(text); got != "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678" {
		t.Fatalf("got %q", got)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func mustEncrypt(t *testing.T, secret, plain string) string {
	t.Helper()
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	ct, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	if !strings.HasPrefix("enc:v1:", "enc:v1:") {
		t.Fatal("sanity")
	}
	return "enc:v1:" + enc(iv) + "." + enc(tag) + "." + enc(ct)
}
