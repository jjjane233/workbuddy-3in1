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

// 「每加一个号都要手抄一次 <keyId>.<secret>」是面板上被问最多的痛点。
// 这个端点把「粘一次 -> 写满池里所有 ZCode 号」变成一步，所以必须钉死：
//
//  1. 从整份 config.json 里认出凭据（用户不会只粘 keyId.secret）
//  2. 一次写进所有 ZCode 号，且同时写 SignCredential 与 APIKey
//  3. 落盘（重启后还在）
//  4. 不碰非 ZCode 账号
//  5. dry_run 不写
func TestZCodeCredentialAutoWritesAllZCodeAccounts(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})

	mk := func(uid string, prov string) *auth.Auth {
		a := &auth.Auth{
			Provider:    prov,
			AccessToken: "jwt",
			UID:         uid,
			Mode:        auth.ModeJWT,
			FilePath:    filepath.Join(dir, prov+"-"+uid+".json"),
		}
		p.cfg.Pool.Add(a)
		return a
	}
	zc1 := mk("z1", auth.ProviderZCode)
	zc2 := mk("z2", auth.ProviderZCode)
	tr := mk("t1", auth.ProviderTrae)

	const cred = "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"
	raw := `{"provider":{"builtin:bigmodel-coding-plan":{"options":{"apiKey":"` + cred + `"}}}}`

	body, _ := json.Marshal(map[string]any{"text": raw})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/zcode/credential/auto", bytes.NewReader(body))
	p.zcodeCredentialAuto(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK        bool `json:"ok"`
		Total     int  `json:"total"`
		Succeeded int  `json:"succeeded"`
		KeyID     string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Total != 2 || resp.Succeeded != 2 {
		t.Fatalf("got ok=%v total=%d succ=%d body=%s", resp.OK, resp.Total, resp.Succeeded, rec.Body.String())
	}
	for _, a := range []*auth.Auth{zc1, zc2} {
		if got := a.SignCredential(); got != cred {
			t.Fatalf("%s SignCredential()=%q", a.UID, got)
		}
		if got := a.APIKeyValue(); got != cred {
			t.Fatalf("%s APIKeyValue()=%q", a.UID, got)
		}
		// 落盘校验：重启后靠文件恢复。
		rawFile, err := os.ReadFile(a.FilePath)
		if err != nil {
			t.Fatalf("read %s: %v", a.UID, err)
		}
		reloaded, err := auth.Parse(rawFile)
		if err != nil {
			t.Fatalf("reload %s: %v", a.UID, err)
		}
		if reloaded.SignCredential() != cred {
			t.Fatalf("%s not persisted", a.UID)
		}
	}
	if tr.SignCredential() != "" || tr.APIKeyValue() != "" {
		t.Fatal("Trae account must not be touched")
	}
}

// dry_run 只识别不写入：用户先确认 keyId 对不对，再落库。
func TestZCodeCredentialAutoDryRunDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	a := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "jwt", UID: "z1",
		FilePath: filepath.Join(dir, "z1.json")}
	p.cfg.Pool.Add(a)

	const cred = "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"
	body, _ := json.Marshal(map[string]any{"text": cred, "dry_run": true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/zcode/credential/auto", bytes.NewReader(body))
	p.zcodeCredentialAuto(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var d struct {
		DryRun  bool   `json:"dry_run"`
		Parsed  bool   `json:"parsed"`
		KeyID   string `json:"key_id"`
		Targets int    `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.DryRun || !d.Parsed || d.KeyID == "" || d.Targets != 1 {
		t.Fatalf("bad dry_run resp: %s", rec.Body.String())
	}
	if a.SignCredential() != "" {
		t.Fatal("dry_run must not write")
	}
}

// 认不出凭据时必须给出可操作提示，而不是 200 空转。
func TestZCodeCredentialAutoUnrecognizedText(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	p.cfg.Pool.Add(&auth.Auth{Provider: auth.ProviderZCode, AccessToken: "jwt", UID: "z1",
		FilePath: filepath.Join(dir, "z1.json")})

	body, _ := json.Marshal(map[string]any{"text": "hello world"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/zcode/credential/auto", bytes.NewReader(body))
	p.zcodeCredentialAuto(rec, req)

	var d struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.OK || d.Error == "" || d.Hint == "" {
		t.Fatalf("want ok=false with error+hint, got %s", rec.Body.String())
	}
}

// 状态端点要能一眼看出「哪个号缺 key」。缺 key 的 ZCode 号实测完全不可用
// （zcode.z.ai 的 zcode-plan 对非官方客户端一律 405/3012），
// 所以这是唯一能让用户知道该补哪个号的界面依据。
func TestZCodeCredentialStatusReportsMissingKey(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})

	withKey := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "jwt", UID: "z-has",
		Source: "bigmodel", Nickname: "有key",
		FilePath: filepath.Join(dir, "z-has.json")}
	withKey.SetSignCredential("f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678")
	noKey := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "jwt", UID: "z-none",
		Source: "zai", Nickname: "没key",
		FilePath: filepath.Join(dir, "z-none.json")}
	trae := &auth.Auth{Provider: auth.ProviderTrae, AccessToken: "jwt", UID: "t1",
		FilePath: filepath.Join(dir, "t1.json")}
	p.cfg.Pool.Add(withKey)
	p.cfg.Pool.Add(noKey)
	p.cfg.Pool.Add(trae)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/zcode/credential/status", nil)
	p.zcodeCredentialStatus(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var d struct {
		OK         bool `json:"ok"`
		Total      int  `json:"total"`
		MissingKey int  `json:"missing_key"`
		Accounts   []struct {
			UID     string `json:"uid"`
			HasKey  bool   `json:"has_key"`
			KeyID   string `json:"key_id"`
			Preview string `json:"preview"`
			HasJWT  bool   `json:"has_jwt"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	// Trae 不能被算进来（它没有 ZCode 凭据概念）。
	if !d.OK || d.Total != 2 || d.MissingKey != 1 {
		t.Fatalf("total=%d missing=%d body=%s", d.Total, d.MissingKey, rec.Body.String())
	}
	for _, a := range d.Accounts {
		switch a.UID {
		case "z-has":
			if !a.HasKey || a.KeyID != "f1e2d3c4b5a697887766554433221100" || a.Preview == "" {
				t.Fatalf("z-has row bad: %+v", a)
			}
			// secret 绝不能出网关。
			if strings.Contains(rec.Body.String(), "AbCdEfGh12345678") {
				t.Fatal("secret leaked in status response")
			}
		case "z-none":
			if a.HasKey || !a.HasJWT {
				t.Fatalf("z-none row bad: %+v", a)
			}
		default:
			t.Fatalf("unexpected account %q", a.UID)
		}
	}
}
