package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLooksLikeSignCredential 过滤规则必须严格：
// ZCode 客户端配置里混着大量 "sk-..." 第三方密钥，一旦漏过就会把
// 中转密钥当成 Ed25519 签名凭据发给上游。
func TestLooksLikeSignCredential(t *testing.T) {
	ok := []string{
		"f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678",
		"0123456789abcdef0123456789abcdef.x",
	}
	for _, s := range ok {
		if !LooksLikeSignCredential(s) {
			t.Fatalf("LooksLikeSignCredential(%q) = false, want true", s)
		}
	}
	bad := []string{
		"",
		"sk-EXAMPLE0000000000000000000000000000000000000000000000",
		"f1e2d3c4b5a697887766554433221100",           // 没有点
		"f1e2d3c4b5a697887766554433221100.a.b",       // 两个点
		".secret",                                    // 点在最前
		"f1e2d3c4b5a697887766554433221100.",          // 点在最后
		"F1E2D3C4B5A697887766554433221100.Secret",    // 大写 hex：上游 keyId 是小写
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.zzzzz", // JWT 三个点其实两个点，也不该过
	}
	for _, s := range bad {
		if LooksLikeSignCredential(s) {
			t.Fatalf("LooksLikeSignCredential(%q) = true, want false", s)
		}
	}
}

// TestReadSignCredentialFromConfig 用真实结构的 config.json 片段验证抽取逻辑。
func TestReadSignCredentialFromConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{
	  "provider": {
	    "builtin:bigmodel": {"options": {"apiKey": ""}},
	    "builtin:bigmodel-coding-plan": {
	      "name": "BigModel - Coding Plan",
	      "options": {
	        "apiKey": "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678",
	        "baseURL": "https://open.bigmodel.cn/api/anthropic"
	      }
	    },
	    "2e2d6e39-3bec-4d46-a2a9-b467c2db2703": {
	      "name": "第三方中转",
	      "options": {"apiKey": "sk-EXAMPLE1111111111111111111111111111111111111111111111"}
	    }
	  }
	}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readSignCredentialFrom(p)
	if got != "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678" {
		t.Fatalf("readSignCredentialFrom = %q", got)
	}
}

func TestReadSignCredentialFromMissingFile(t *testing.T) {
	if got := readSignCredentialFrom(filepath.Join(t.TempDir(), "nope.json")); got != "" {
		t.Fatalf("missing file should give empty, got %q", got)
	}
}

func TestZCodeSignCredentialPathsNonEmpty(t *testing.T) {
	if len(ZCodeSignCredentialPaths()) == 0 {
		t.Fatal("expected at least one candidate path")
	}
}
