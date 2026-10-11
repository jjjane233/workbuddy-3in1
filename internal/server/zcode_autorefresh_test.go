package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

func newTestHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	return NewHandler(Config{Pool: pool.New(""), AuthDir: dir}), dir
}

// TestZCodeApplyLocalCredentialFillsAndUpdates 自动同步要同时做到：
//   - 客户端 uid 匹配的号：用新 secret 覆盖（处理上游轮换）
//   - 缺凭据的号：补上
//   - 不属于本客户端、且已有凭据的号：不动（多号防串号）
func TestZCodeApplyLocalCredentialFillsAndUpdates(t *testing.T) {
	h, dir := newTestHandler(t)

	rotated := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "me",
		FilePath: filepath.Join(dir, "me.json")}
	rotated.SetSignCredential("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.oldsecret")
	rotated.SetAPIKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.oldsecret")

	emptyMe := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "me2-empty",
		FilePath: filepath.Join(dir, "me2.json")}

	other := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "other",
		FilePath: filepath.Join(dir, "other.json")}
	other.SetSignCredential("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.othersecret")
	other.SetAPIKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.othersecret")

	for _, a := range []*auth.Auth{rotated, emptyMe, other} {
		h.cfg.Pool.Add(a)
	}

	const fresh = "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"
	res := &zcode.LocalKeyResult{
		Credential: fresh,
		Source:     "client-config",
		Info:       auth.LocalClientInfo{UID: "me", Provider: "bigmodel", JWToken: "jwt"},
	}
	updated, filled, adopted := h.zcodeApplyLocalCredential(res)

	if updated != 1 {
		t.Fatalf("updated=%d, want 1", updated)
	}
	if filled != 1 {
		t.Fatalf("filled=%d, want 1", filled)
	}
	if adopted != 0 {
		t.Fatalf("adopted=%d, want 0", adopted)
	}
	if got := rotated.SignCredential(); got != fresh {
		t.Fatalf("rotated not overwritten: %q", got)
	}
	if got := emptyMe.SignCredential(); got != fresh {
		t.Fatalf("empty not filled: %q", got)
	}
	if got := other.SignCredential(); got != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.othersecret" {
		t.Fatalf("other account was clobbered: %q", got)
	}
	// 轮换后的凭据要落盘
	raw, err := os.ReadFile(rotated.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	back, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.SignCredential() != fresh {
		t.Fatalf("rotation not persisted: %q", back.SignCredential())
	}
}

// TestZCodeApplyLocalCredentialAdoptsNewAccount 客户端上未入池的 uid 自动建号。
func TestZCodeApplyLocalCredentialAdoptsNewAccount(t *testing.T) {
	h, _ := newTestHandler(t)
	const fresh = "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"

	res := &zcode.LocalKeyResult{
		Credential: fresh,
		Source:     "exchange:bigmodel",
		Info:       auth.LocalClientInfo{UID: "brand-new", Provider: "bigmodel", JWToken: "jwt"},
	}
	updated, filled, adopted := h.zcodeApplyLocalCredential(res)
	if updated != 0 || filled != 0 || adopted != 1 {
		t.Fatalf("got %d/%d/%d, want 0/0/1", updated, filled, adopted)
	}
	a := h.cfg.Pool.AuthByUID("brand-new")
	if a == nil {
		t.Fatal("new account not adopted")
	}
	if a.ProviderOf() != auth.ProviderZCode || a.SignCredential() != fresh {
		t.Fatalf("bad adopted account: provider=%q cred=%q", a.ProviderOf(), a.SignCredential())
	}
	if _, err := os.Stat(a.FilePath); err != nil {
		t.Fatalf("adopted account not persisted: %v", err)
	}
	// 幂等：再来一次不重复建号
	if _, _, again := h.zcodeApplyLocalCredential(res); again != 0 {
		t.Fatalf("adopt not idempotent: %d", again)
	}
}

// TestZCodeAutoSyncDisabled 开关关闭时不读盘、不返回动作。
func TestZCodeAutoSyncDisabled(t *testing.T) {
	h, _ := newTestHandler(t)
	if msg := h.zcodeAutoSync(context.Background(), false); msg != "" {
		t.Fatalf("disabled sync produced %q", msg)
	}
}

// TestZCodeApplyLocalCredentialNoUIDOnlyFills 客户端 uid 未知时只补空缺、不动已有。
func TestZCodeApplyLocalCredentialNoUIDOnlyFills(t *testing.T) {
	h, dir := newTestHandler(t)
	has := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z1", FilePath: filepath.Join(dir, "z1.json")}
	has.SetSignCredential("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.keep")
	has.SetAPIKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.keep")
	none := &auth.Auth{Provider: auth.ProviderZCode, AccessToken: "j", UID: "z2", FilePath: filepath.Join(dir, "z2.json")}
	h.cfg.Pool.Add(has)
	h.cfg.Pool.Add(none)

	const fresh = "f1e2d3c4b5a697887766554433221100.AbCdEfGh12345678"
	res := &zcode.LocalKeyResult{Credential: fresh, Info: auth.LocalClientInfo{}}
	updated, filled, adopted := h.zcodeApplyLocalCredential(res)
	if updated != 0 || filled != 1 || adopted != 0 {
		t.Fatalf("got %d/%d/%d, want 0/1/0", updated, filled, adopted)
	}
	if has.SignCredential() != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.keep" {
		t.Fatal("existing credential without uid match must be untouched")
	}
	if none.SignCredential() != fresh {
		t.Fatal("empty credential must be filled")
	}
}
