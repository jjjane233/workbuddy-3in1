package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
)

// TestZCodeCredentialAdoptCreatesAccount 「自动接入」：本机客户端的 uid 尚未入池时建号。
func TestZCodeCredentialAdoptCreatesAccount(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})

	info := auth.LocalClientInfo{
		APIKey:   "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678",
		JWToken:  "eyJhbGciOiJIUzI1NiJ9.aaa.bbb",
		UID:      "10000000000000001",
		Provider: "bigmodel",
	}
	a, created := p.zcodeCredentialAdopt(info, info.APIKey, keyIDOf(info.APIKey))
	if !created || a == nil {
		t.Fatal("expected a new account")
	}
	if a.UID != "10000000000000001" || a.Provider != auth.ProviderZCode {
		t.Fatalf("bad account: %+v", a)
	}
	if a.SignCredential() != info.APIKey {
		t.Fatalf("sign credential not set: %q", a.SignCredential())
	}
	// 落盘可回读
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	back, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.SignCredential() != info.APIKey {
		t.Fatalf("not persisted: %q", back.SignCredential())
	}
	// 已在池里就不再建
	if _, again := p.zcodeCredentialAdopt(info, info.APIKey, "x"); again {
		t.Fatal("adopt must be idempotent")
	}
}

// TestZCodeCredentialTargetsFiltersZCode 目标账号只含 ZCode，不碰 Trae。
func TestZCodeCredentialTargetsFiltersZCode(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	p.cfg.Pool.Add(&auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z1", FilePath: filepath.Join(dir, "z1.json")})
	p.cfg.Pool.Add(&auth.Auth{Provider: auth.ProviderTrae, AccessToken: "j", UID: "t1", FilePath: filepath.Join(dir, "t1.json")})

	all := p.zcodeCredentialTargets(nil)
	if len(all) != 1 || all[0].UID != "z1" {
		t.Fatalf("targets=%v", uidList(all))
	}
	none := p.zcodeCredentialTargets([]string{"t1"})
	if len(none) != 0 {
		t.Fatalf("trae uid must be filtered, got %v", uidList(none))
	}
}

// TestZCodeCredentialWriteSkipsExisting 已有凭据的号不被覆盖（多号防串号）。
func TestZCodeCredentialWriteSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	a := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z1", FilePath: filepath.Join(dir, "z1.json")}
	a.SetSignCredential("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.old")
	a.SetAPIKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.old")
	p.cfg.Pool.Add(a)

	res := p.zcodeCredentialWrite([]*auth.Auth{a}, "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678", "f1e2d3c4b5a697887766554433221100", true, false)
	if len(res) != 1 || !res[0].OK || res[0].Wrote {
		t.Fatalf("expected skip, got %+v", res)
	}
	if a.SignCredential() != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.old" {
		t.Fatal("existing credential was overwritten")
	}
}

// TestZCodeCredentialStatusReportsLocalClient 状态端点带本机客户端诊断字段。
func TestZCodeCredentialStatusReportsLocalClient(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	p.cfg.Pool.Add(&auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z1", FilePath: filepath.Join(dir, "z1.json")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/zcode/credential/status", nil)
	p.zcodeCredentialStatus(rec, req)

	var d struct {
		OK          bool           `json:"ok"`
		MissingKey  int            `json:"missing_key"`
		LocalClient map[string]any `json:"local_client"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.OK || d.MissingKey != 1 {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if _, ok := d.LocalClient["available"]; !ok {
		t.Fatalf("local_client.available missing: %s", rec.Body.String())
	}
	// 状态响应绝不泄露 secret
	if strings.Contains(rec.Body.String(), "AbCdEfGh12345678") {
		t.Fatal("secret leaked")
	}
}

// TestZCodeCredentialAutoAdoptAdoptsFromInjectedLocalClient 干跑不改盘、带 auto 诊断字段。
func TestZCodeCredentialAutoDryRunIncludesAuto(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	p.cfg.Pool.Add(&auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z1", FilePath: filepath.Join(dir, "z1.json")})

	body := `{"text":"f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678","dry_run":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/zcode/credential/auto", strings.NewReader(body))
	p.zcodeCredentialAuto(rec, req)

	var d struct {
		DryRun  bool   `json:"dry_run"`
		Parsed  bool   `json:"parsed"`
		Origin  string `json:"origin"`
		Targets int    `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.DryRun || !d.Parsed || d.Origin != "text" || d.Targets != 1 {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// TestZCodeCredentialWriteForceUpdatesSecret secret 轮换场景：force=true 覆盖已有凭据。
func TestZCodeCredentialWriteForceUpdatesSecret(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	a := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z1", FilePath: filepath.Join(dir, "z1.json")}
	a.SetSignCredential("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.old")
	a.SetAPIKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.old")
	p.cfg.Pool.Add(a)

	newCred := "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"
	res := p.zcodeCredentialWrite([]*auth.Auth{a}, newCred, "f1e2d3c4b5a697887766554433221100", false, true)
	if len(res) != 1 || !res[0].OK || !res[0].Wrote || !res[0].HadCred {
		t.Fatalf("expected forced update, got %+v", res)
	}
	if a.SignCredential() != newCred {
		t.Fatalf("credential not rotated: %q", a.SignCredential())
	}
	res = p.zcodeCredentialWrite([]*auth.Auth{a}, newCred, "f1e2d3c4b5a697887766554433221100", false, true)
	if res[0].Wrote {
		t.Fatal("identical credential must not rewrite")
	}
}

// TestZCodeCredentialTargetsSingleUIDScopesToOne 显式单 uid 只动该号（防串号守门）。
func TestZCodeCredentialTargetsSingleUIDScopesToOne(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})
	p.cfg.Pool.Add(&auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z1", FilePath: filepath.Join(dir, "z1.json")})
	p.cfg.Pool.Add(&auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z2", FilePath: filepath.Join(dir, "z2.json")})

	body := "{\"text\":\"f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678\",\"uids\":[\"z2\"]}"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/zcode/credential/auto", strings.NewReader(body))
	p.zcodeCredentialAuto(rec, req)

	var d struct {
		OK    bool `json:"ok"`
		Total int  `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.OK || d.Total != 1 {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if a := p.cfg.Pool.AuthByUID("z1"); a.SignCredential() != "" {
		t.Fatalf("z1 must not receive another account key: %q", a.SignCredential())
	}
	if a := p.cfg.Pool.AuthByUID("z2"); a.SignCredential() == "" {
		t.Fatal("z2 should have the credential")
	}
}
