package zcode

import (
	"encoding/json"
	"testing"
)

// TestCLIReadyBigModelTokenSet 钉住「bigmodel 家族 ready 载荷可解析」这一修复。
//
// 背景：官方桌面端（app.asar -> out/host/index.js）在 ready 分支里同时读
// data.zai 与 data.bigmodel。旧实现只声明了 zai，于是 bigmodel 家族的号
// 永远拿不到业务 token，只能回退去读本机客户端 config.json —— 这正是
// 「电脑不开机就用不了」的根因。这个测试保证它不再退化。
func TestCLIReadyBigModelTokenSet(t *testing.T) {
	raw := []byte(`{
	  "code": 0,
	  "msg": "",
	  "data": {
	    "status": "ready",
	    "token": "zcode-jwt-token",
	    "user": {"user_id": "10000000000000001", "name": "demo-user"},
	    "bigmodel": {"access_token": "bm-access", "refresh_token": "bm-refresh"}
	  }
	}`)

	var env struct {
		Code int       `json:"code"`
		Msg  string    `json:"msg"`
		Data *CLIReady `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data == nil {
		t.Fatal("data is nil")
	}
	if env.Data.Token != "zcode-jwt-token" {
		t.Fatalf("token = %q", env.Data.Token)
	}

	at, rt := env.Data.ProviderTokenSet("bigmodel")
	if at != "bm-access" {
		t.Fatalf("bigmodel access_token = %q, want bm-access", at)
	}
	if rt != "bm-refresh" {
		t.Fatalf("bigmodel refresh_token = %q, want bm-refresh", rt)
	}

	// zai 家族在本次载荷里没有数据，必须返回空而不是 panic。
	if at, rt := env.Data.ProviderTokenSet("zai"); at != "" || rt != "" {
		t.Fatalf("zai = (%q,%q), want empty", at, rt)
	}
}

// TestCLIReadyZAITokenSet 保持 zai 家族既有行为不回退。
func TestCLIReadyZAITokenSet(t *testing.T) {
	raw := []byte(`{"status":"ready","token":"j","user":{"user_id":"1"},
	  "zai":{"access_token":"zai-access","refresh_token":"zai-refresh"}}`)
	var r CLIReady
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	at, rt := r.ProviderTokenSet("zai")
	if at != "zai-access" || rt != "zai-refresh" {
		t.Fatalf("zai = (%q,%q)", at, rt)
	}
	// realm 为空时按 zai 处理（面板默认 realm 是 "zai"）。
	at2, _ := r.ProviderTokenSet("")
	if at2 != "zai-access" {
		t.Fatalf("empty realm access_token = %q", at2)
	}
}

// TestCLITokenSetCamelCase 兼容桌面端 normalizePolledTokenSet 的 camelCase 变体。
func TestCLITokenSetCamelCase(t *testing.T) {
	var ts CLITokenSet
	if err := json.Unmarshal([]byte(`{"accessToken":"a","refreshToken":"r"}`), &ts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ts.AccessToken != "a" || ts.RefreshToken != "r" {
		t.Fatalf("camelCase = (%q,%q)", ts.AccessToken, ts.RefreshToken)
	}
}
