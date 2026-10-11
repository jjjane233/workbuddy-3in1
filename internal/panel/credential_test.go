package panel

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
)

// 手填 ZCode 签名凭据：写入后必须真的落到 auth 文件里，并且回读一致。
//
// 为什么必须钉死：
//
//	ZCode 对话端点对非官方客户端回 405/3012，网关靠"JWT + 客户端签名 + 回落
//	BigModel"三件套打通。而签名器要的 "<keyId>.<secret>" 在服务器上无从自动发现
//	（auth.DiscoverZCodeSignCredential 读的是本机 ~/.zcode/v2/config.json），
//	只能靠这个入口手填。如果写入静默失败，用户看到的是"面板说成功了，但对话
//	还是 3012"，排查成本极高。
func TestUpstreamCredentialWritesSignCredential(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	a := &auth.Auth{
		Provider:    auth.ProviderZCode,
		AccessToken: "jwt-token",
		UID:         "10000000000000001",
		Mode:        auth.ModeJWT,
		FilePath:    filepath.Join(dir, "zcode-10000000000000001.json"),
	}
	p.cfg.Pool.Add(a)

	const cred = "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"
	body, _ := json.Marshal(map[string]string{"api_key": cred})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/10000000000000001/credential", bytes.NewReader(body))
	req.SetPathValue("uid", "10000000000000001")
	p.upstreamCredential(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := a.SignCredential(); got != cred {
		t.Fatalf("SignCredential()=%q want %q", got, cred)
	}
	if got := a.APIKeyValue(); got != cred {
		t.Fatalf("APIKeyValue()=%q want %q", got, cred)
	}
	// 落盘校验：服务器重启后靠文件恢复，只写内存等于没写。
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	reloaded, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.SignCredential(); got != cred {
		t.Fatalf("persisted SignCredential()=%q want %q", got, cred)
	}
	if got := reloaded.APIKeyValue(); got != cred {
		t.Fatalf("persisted APIKeyValue()=%q want %q", got, cred)
	}
}

// 非法格式必须明确报错而不是静默忽略：SetSignCredential 对缺点/端点的值
// 是静默返回的，接口层若不先校验，用户会拿到 200 却什么也没写进去。
func TestUpstreamCredentialRejectsBadFormat(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	a := &auth.Auth{
		Provider: auth.ProviderZCode, AccessToken: "jwt", UID: "u1",
		FilePath: filepath.Join(dir, "zcode-u1.json"),
	}
	p.cfg.Pool.Add(a)
	for _, bad := range []string{"", "no-dot-here", ".leading", "trailing.", "   "} {
		body, _ := json.Marshal(map[string]string{"api_key": bad})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/panel/api/accounts/u1/credential", bytes.NewReader(body))
		req.SetPathValue("uid", "u1")
		p.upstreamCredential(rec, req)
		if rec.Code == 200 {
			t.Fatalf("bad credential %q accepted (want 4xx)", bad)
		}
		if a.SignCredential() != "" {
			t.Fatalf("bad credential %q mutated account", bad)
		}
	}
}

// 非 ZCode 账号不能写这个凭据（Trae / CodeBuddy 没有这套签名）。
func TestUpstreamCredentialRejectsOtherProviders(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	a := &auth.Auth{Provider: auth.ProviderTrae, AccessToken: "tok", UID: "t1"}
	p.cfg.Pool.Add(a)
	body, _ := json.Marshal(map[string]string{"api_key": "a.b"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/t1/credential", bytes.NewReader(body))
	req.SetPathValue("uid", "t1")
	p.upstreamCredential(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ZCode") {
		t.Fatalf("error should mention ZCode: %s", rec.Body.String())
	}
}
