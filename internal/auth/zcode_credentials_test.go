package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// 手抄 ZCode 凭据这件事能不能消灭，全看这里：
// 用户粘过来的可能是凭据本体、整份 config.json、provider_config.json、
// 甚至带 key=value 的日志行。任一种认不出来，用户就得回去手工抄一遍。
const goodCred = "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"

func TestExtractSignCredentialFromText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"裸凭据", goodCred, goodCred},
		{"带引号", `"` + goodCred + `"`, goodCred},
		{"带空白", "  " + goodCred + "\n", goodCred},
		{"config.json", `{
  "provider": {
    "builtin:bigmodel-coding-plan": {
      "options": { "apiKey": "` + goodCred + `" }
    }
  }
}`, goodCred},
		{"provider_config", `{"access":{"type":"api-key","apiKey":"` + goodCred + `"}}`, goodCred},
		{"下划线写法", `{"api_key":"` + goodCred + `"}`, goodCred},
		{"key=value 行", "apiKey=" + goodCred, goodCred},
		{"带引号冒号行", `  "apiKey": "` + goodCred + `",`, goodCred},
		{"内嵌在日志里", "loading config ... credential=" + goodCred + " done", goodCred},
		{"空", "", ""},
		{"无关文本", "hello world, nothing here", ""},
		{"keyId 太短", "abc.def", ""},
		{"keyId 非十六进制", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.secret", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractSignCredentialFromText(c.in); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// enc:v1: 是 ZCode 客户端落盘时的加密格式，解错密钥就等于取不到 token，
// 「自动获取」当场失效。这里用同一套算法现场加密再解，钉死密钥派生与分段顺序。
func TestDecryptZCodeCredentialRoundTrip(t *testing.T) {
	secrets := ZCodeCredentialSecrets()
	if len(secrets) == 0 {
		t.Skip("no USERNAME/USER in env, cannot derive key")
	}
	key := sha256.Sum256([]byte(secrets[0]))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, gcm.NonceSize())
	for i := range iv {
		iv[i] = byte(i + 1)
	}
	want := "eyJhbGciOiJIUzI1NiJ9.payload.sig"
	sealed := gcm.Seal(nil, iv, []byte(want), nil)
	ct, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	enc := "enc:v1:" + base64.RawURLEncoding.EncodeToString(iv) + "." +
		base64.RawURLEncoding.EncodeToString(tag) + "." +
		base64.RawURLEncoding.EncodeToString(ct)

	got, err := DecryptZCodeCredential(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := DecryptZCodeCredential("enc:v1:not-a-blob"); err == nil {
		t.Fatal("bad blob should error")
	}
}

// credentials.json 里的 token 是加密存的；ParseZCodeCredentials 必须自己解开，
// 并优先挑 bigmodel 那条（实测只有它是活的）。
func TestParseZCodeCredentialsPrefersBigModel(t *testing.T) {
	secrets := ZCodeCredentialSecrets()
	if len(secrets) == 0 {
		t.Skip("no USERNAME/USER in env")
	}
	key := sha256.Sum256([]byte(secrets[0]))
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, gcm.NonceSize())
	sealed := gcm.Seal(nil, iv, []byte("aaa.bbb.ccc"), nil)
	ct, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	enc := "enc:v1:" + base64.RawURLEncoding.EncodeToString(iv) + "." +
		base64.RawURLEncoding.EncodeToString(tag) + "." +
		base64.RawURLEncoding.EncodeToString(ct)

	raw, _ := json.Marshal(map[string]string{
		"oauth:zai:access_token":      "x.y.z",
		"oauth:bigmodel:access_token": enc,
	})
	prov, tok := ParseZCodeCredentials(raw)
	if prov != "bigmodel" || tok != "aaa.bbb.ccc" {
		t.Fatalf("got (%q,%q) want (bigmodel, aaa.bbb.ccc)", prov, tok)
	}

	// 明文 zai-only 也要能出（降级路径）。
	raw2, _ := json.Marshal(map[string]string{"oauth:zai:access_token": "p.q.r"})
	if prov, tok := ParseZCodeCredentials(raw2); prov != "zai" || tok != "p.q.r" {
		t.Fatalf("plain zai: got (%q,%q)", prov, tok)
	}
	if _, tok := ParseZCodeCredentials([]byte("not json")); tok != "" {
		t.Fatal("garbage should yield empty")
	}
}

func TestZCodeCredentialsPathsNonEmpty(t *testing.T) {
	paths := ZCodeCredentialsPaths()
	if len(paths) == 0 {
		t.Fatal("expected at least the user-home .zcode/v2/credentials.json candidate")
	}
	if _, err := os.UserHomeDir(); err == nil {
		found := false
		for _, p := range paths {
			if len(p) > 0 {
				found = true
			}
		}
		if !found {
			t.Fatal("empty path entries")
		}
	}
}
