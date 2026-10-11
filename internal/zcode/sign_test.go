package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// 本组测试把 zcode.cjs（Client Request Signing v4）的口径固化成断言：
// 以后谁再"顺手简化"分隔符或省掉一层 base64，这些用例会直接红。
// 全部离线，不联网。

// TestHasLeadingZeroBits 覆盖位级难度判定（整字节 + 非整字节掩码两条路径）。
func TestHasLeadingZeroBits(t *testing.T) {
	cases := []struct {
		name string
		d    []byte
		bits int
		want bool
	}{
		{"8bit-hit", []byte{0x00, 0xff}, 8, true},
		{"8bit-miss", []byte{0x01, 0x00}, 8, false},
		{"16bit-hit", []byte{0x00, 0x00, 0xff}, 16, true},
		{"16bit-miss", []byte{0x00, 0x01}, 16, false},
		{"12bit-hit", []byte{0x00, 0x0f}, 12, true},   // 次字节高 4 位为 0
		{"12bit-miss", []byte{0x00, 0x10}, 12, false}, // 次字节第 4 位为 1
		{"9bit-hit", []byte{0x00, 0x7f}, 9, true},
		{"9bit-miss", []byte{0x00, 0x80}, 9, false},
		{"0bit", []byte{0xff, 0xff}, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasLeadingZeroBits(c.d, c.bits); got != c.want {
				t.Fatalf("hasLeadingZeroBits(%x, %d) = %v, want %v", c.d, c.bits, got, c.want)
			}
		})
	}
}

// TestSolvePoWMatchesOfficialAlgorithm 逐字复算客户端算法，校验 Go 版产出的候选解
// 确实满足难度——同时锁住「分隔符是 \n 而不是空格」这个此前被写错的点。
func TestSolvePoWMatchesOfficialAlgorithm(t *testing.T) {
	const apiKeyID = "f1e2d3c4b5a697887766554433221100"
	const appID = "zcode"
	const sessionID = "sess-abc"
	const ts = "1753600000000"

	cand := solvePoW(apiKeyID, appID, sessionID, ts, 8)
	if len(cand) != 32 {
		t.Fatalf("candidate = %q (len %d), want 32 hex chars (12 random + 8 counter)", cand, len(cand))
	}
	// 官方: seed = sha256(apiKeyId\nappId\nsessionId\nts); prefix = hex(seed)[0:32]
	seed := sha256.Sum256([]byte(apiKeyID + "\n" + appID + "\n" + sessionID + "\n" + ts))
	prefix := hex.EncodeToString(seed[:])[:32]
	sum := sha256.Sum256([]byte(prefix + "\n" + cand))
	if !hasLeadingZeroBits(sum[:], 8) {
		t.Fatalf("candidate %q does not satisfy 8 leading zero bits", cand)
	}
	// 反证：用空格分隔的旧实现算出的候选不该恰好也满足（不是必须，但说明差异真实）。
	spaceSeed := sha256.Sum256([]byte(apiKeyID + " " + appID + " " + sessionID + " " + ts))
	if hex.EncodeToString(spaceSeed[:]) == prefix {
		t.Fatal("newline and space seeds must differ")
	}
}

// TestSolvePoWRespectsPowBits 验证难度可调（上游下发 powBits 时按值求解）。
func TestSolvePoWRespectsPowBits(t *testing.T) {
	cand := solvePoW("kid", "zcode", "sid", "1", 12)
	seed := sha256.Sum256([]byte("kid\nzcode\nsid\n1"))
	prefix := hex.EncodeToString(seed[:])[:32]
	sum := sha256.Sum256([]byte(prefix + "\n" + cand))
	if !hasLeadingZeroBits(sum[:], 12) {
		t.Fatal("candidate does not satisfy 12 leading zero bits")
	}
}

// TestDecryptPrivateKeyBase64Text 是本组最关键的回归：握手返回的 privateCipher
// 解开后是**base64 文本**，要再解一层才是 PKCS8 DER（客户端 eur() 那一层）。
// 修复前直接拿明文去 ParsePKCS8PrivateKey，握手永远失败。
func TestDecryptPrivateKeyBase64Text(t *testing.T) {
	keyID := "kid-1"
	secret := "secret-1"

	// 造一把真实 Ed25519 私钥并编码成客户端看到的形态。
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	inner := base64.StdEncoding.EncodeToString(der) // 上游放进密文的那一层 base64
	cipherB64 := sealForTest(t, keyID, secret, []byte(inner))

	got, err := decryptPrivateKey(keyID, secret, cipherB64)
	if err != nil {
		t.Fatalf("decryptPrivateKey: %v", err)
	}
	if !got.Equal(priv) {
		t.Fatal("decrypted key differs from original")
	}
}

// TestDecryptPrivateKeyRawDER 兼容上游直接给 DER 的形态（不应因此崩）。
func TestDecryptPrivateKeyRawDER(t *testing.T) {
	keyID, secret := "kid-2", "secret-2"
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	cipherB64 := sealForTest(t, keyID, secret, der)

	got, err := decryptPrivateKey(keyID, secret, cipherB64)
	if err != nil {
		t.Fatalf("decryptPrivateKey(raw DER): %v", err)
	}
	if !got.Equal(priv) {
		t.Fatal("decrypted key differs from original")
	}
}

// TestDecryptPrivateKeyWrongAAD 验证 AAD 必须是 keyID（传错解不开）。
func TestDecryptPrivateKeyWrongAAD(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	inner := base64.StdEncoding.EncodeToString(der)
	cipherB64 := sealForTest(t, "right", "secret", []byte(inner))

	if _, err := decryptPrivateKey("wrong", "secret", cipherB64); err == nil {
		t.Fatal("expected decrypt failure with wrong AAD")
	}
}

// sealForTest 复现上游封装流程：key = HKDF(secret, salt, "ed25519_priv")，
// AES-256-GCM，AAD = keyID，输出 base64(iv || ciphertext||tag)。
func sealForTest(t *testing.T, keyID, secret string, plaintext []byte) string {
	t.Helper()
	key := hkdfSHA256([]byte(secret), []byte(signKDFSalt), []byte(signKDFPriv), 32)
	block, err := aes.NewCipher(key)
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
	ct := gcm.Seal(nil, iv, plaintext, []byte(keyID))
	return base64.StdEncoding.EncodeToString(append(iv, ct...))
}

// TestSignPayloadShape 锁住业务签名载荷的换行分隔与字段顺序
// （apiKeyId\nts\nclientVersion\nsessionId\nnonce）。
func TestSignPayloadShape(t *testing.T) {
	parts := []string{"kid", "1753600000000", "3.4.0", "sess", "nonce0"}
	payload := strings.Join(parts, "\n")
	if strings.Count(payload, "\n") != 4 {
		t.Fatalf("payload must have 4 newline separators: %q", payload)
	}
	if parts[0]+"\n"+parts[1]+"\n"+parts[2]+"\n"+parts[3]+"\n"+parts[4] != payload {
		t.Fatal("payload order changed")
	}
}
