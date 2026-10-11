package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
)

// 「模型与档位」页必须拿**同 provider** 的账号去查上游。
//
// 事故现场：原实现写死 AvailableUIDsForRealm("cn")[0]。那个函数内部固定
// provider=workbuddy（见 pool.AvailableUIDsForRealm），只要池里有一个 CN 的
// CodeBuddy 号，它就会排在最前。于是查 ZCode 模型时递出去的是一份 CodeBuddy
// 凭据，ZCode 的 billing 收到别人家的 Bearer 只回 401 空体，面板上就显示
// "account_error: balance http 401"，套餐授权的 GLM-5.3-Flash 永远并不进列表
// ——用户看到的正是「我领了 5.3-Flash，模型列表里却没有它」。
func TestPickModelsAccountUsesSameProvider(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	wb := &auth.Auth{UID: "wb-cn", Provider: auth.ProviderWorkbuddy, AccessToken: "cb-token"}
	zc := &auth.Auth{UID: "zc-cn", Provider: auth.ProviderZCode, AccessToken: "jwt", Region: "cn"}
	p.cfg.Pool.Add(wb)
	p.cfg.Pool.Add(zc)

	if got := p.pickModelsAccount(auth.ProviderZCode); got == nil || got.UID != "zc-cn" {
		t.Fatalf("pickModelsAccount(zcode)=%v want zc-cn", got)
	}
	if got := p.pickModelsAccount(auth.ProviderWorkbuddy); got == nil || got.UID != "wb-cn" {
		t.Fatalf("pickModelsAccount(workbuddy)=%v want wb-cn", got)
	}
}

// 没有该 provider 的账号时必须返回 nil（退化为公开目录），
// 绝不能「顺手」拿别的 provider 的号去凑。
func TestPickModelsAccountReturnsNilWhenProviderAbsent(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	p.cfg.Pool.Add(&auth.Auth{UID: "wb-cn", Provider: auth.ProviderWorkbuddy, AccessToken: "cb-token"})
	if got := p.pickModelsAccount(auth.ProviderZCode); got != nil {
		t.Fatalf("pickModelsAccount(zcode)=%v want nil", got)
	}
	if got := p.pickModelsAccount(auth.ProviderTrae); got != nil {
		t.Fatalf("pickModelsAccount(trae)=%v want nil", got)
	}
	if got := p.pickModelsAccount(""); got != nil {
		t.Fatalf("pickModelsAccount(\"\")=%v want nil", got)
	}
}

// fakeUpstreamOps 是最小 UpstreamOps 实现，只为 modelsAll 供 Trae/ZCode 目录。
type fakeUpstreamOps struct {
	UpstreamOps // 未实现的方法保持 nil；modelsAll 只调 ModelsFor
	models      map[string][]map[string]any
	errs        map[string]error
	calls       []string
}

func (f *fakeUpstreamOps) ModelsFor(_ context.Context, provider string, _ *auth.Auth) ([]map[string]any, error) {
	f.calls = append(f.calls, provider)
	if err := f.errs[provider]; err != nil {
		return nil, err
	}
	return f.models[provider], nil
}

// TestModelsAllEndpoint provider=all 与 /v1/models 同口径：id 带 provider 前缀。
//
// 这条路径的存在理由：面板原来只能按 provider 分页看，改口径后要能一眼看全
// CodeBuddy / Trae / ZCode 三份，且 id 形态与 /v1/models 完全一致（客户端复制即用）。
func TestModelsAllEndpoint(t *testing.T) {
	up := &fakeUpstreamOps{
		models: map[string][]map[string]any{
			auth.ProviderTrae:  {{"id": "glm-5.3-flash", "credits": "0.06"}},
			auth.ProviderZCode: {{"id": "glm-5.2", "context_length": int64(1048576)}},
		},
		errs: map[string]error{},
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	p.cfg.Pool.Add(&auth.Auth{UID: "tr1", Provider: auth.ProviderTrae, AccessToken: "tk"})
	p.cfg.Pool.Add(&auth.Auth{UID: "zc1", Provider: auth.ProviderZCode, AccessToken: "tk"})
	p.SetUpstreamOps(up)

	req := httptest.NewRequest("GET", "/panel/api/models?provider=all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK       bool             `json:"ok"`
		Provider string           `json:"provider"`
		Models   []map[string]any `json:"models"`
		Notes    []string         `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Provider != "all" {
		t.Fatalf("ok=%v provider=%q", got.OK, got.Provider)
	}
	ids := map[string]map[string]any{}
	for _, m := range got.Models {
		ids[m["id"].(string)] = m
	}
	if _, ok := ids["trae:glm-5.3-flash"]; !ok {
		t.Errorf("缺 trae 前缀条目: %v", ids)
	}
	if e, ok := ids["zcode:glm-5.2"]; !ok {
		t.Errorf("缺 zcode 前缀条目: %v", ids)
	} else if e["provider"] != auth.ProviderZCode {
		t.Errorf("provider 字段=%v", e["provider"])
	}
	// 无 CodeBuddy 号 → 该段进 notes，不影响 Trae/ZCode 段返回
	joined := strings.Join(got.Notes, " ")
	if !strings.Contains(joined, "codebuddy") {
		t.Errorf("CodeBuddy 无号应进 notes: %v", got.Notes)
	}
}

// TestModelsAllIsolatesUpstreamFailure 一个上游抓失败不阻塞其余（notes 记录）。
func TestModelsAllIsolatesUpstreamFailure(t *testing.T) {
	up := &fakeUpstreamOps{
		models: map[string][]map[string]any{
			auth.ProviderTrae: {{"id": "glm-5.3-flash"}},
		},
		errs: map[string]error{auth.ProviderZCode: errors.New("boom")},
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	p.cfg.Pool.Add(&auth.Auth{UID: "tr1", Provider: auth.ProviderTrae, AccessToken: "tk"})
	p.cfg.Pool.Add(&auth.Auth{UID: "zc1", Provider: auth.ProviderZCode, AccessToken: "tk"})
	p.SetUpstreamOps(up)

	req := httptest.NewRequest("GET", "/panel/api/models?provider=all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("单段失败不该 502: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models []map[string]any `json:"models"`
		Notes  []string         `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	foundTrae := false
	for _, m := range got.Models {
		if m["id"] == "trae:glm-5.3-flash" {
			foundTrae = true
		}
		if strings.HasPrefix(m["id"].(string), "zcode:") {
			t.Errorf("失败的段不该有条目: %v", m["id"])
		}
	}
	if !foundTrae {
		t.Errorf("Trae 段应照常返回: %v", got.Models)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "boom") {
		t.Errorf("失败原因应进 notes: %v", got.Notes)
	}
}

// TestModelsAllNoAccountNoUpstreamCall 无号的上游不该被调用（列表即「能调」）。
func TestModelsAllNoAccountNoUpstreamCall(t *testing.T) {
	up := &fakeUpstreamOps{models: map[string][]map[string]any{}, errs: map[string]error{}}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	p.cfg.Pool.Add(&auth.Auth{UID: "tr1", Provider: auth.ProviderTrae, AccessToken: "tk"})
	p.SetUpstreamOps(up)

	req := httptest.NewRequest("GET", "/panel/api/models?provider=all", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	for _, c := range up.calls {
		if c == auth.ProviderZCode {
			t.Errorf("ZCode 无号却调用了上游: %v", up.calls)
		}
	}
}

// TestModelsProviderPrefixIsColon 分页视图的 id 前缀必须是冒号，与 /v1/models 一致。
//
// 旧实现用 "trae/" 斜杠前缀，导致同一个模型在「面板分页」是 trae/xxx、
// 在「全部上游」与 /v1/models 是 trae:xxx，用户得记两套写法。两条路由都认，
// 但展示口径必须统一——这个用例钉住它。
func TestModelsProviderPrefixIsColon(t *testing.T) {
	up := &fakeUpstreamOps{
		models: map[string][]map[string]any{
			auth.ProviderTrae: {{"id": "seed_m8"}},
		},
		errs: map[string]error{},
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	p.cfg.Pool.Add(&auth.Auth{UID: "tr1", Provider: auth.ProviderTrae, AccessToken: "tk"})
	p.SetUpstreamOps(up)

	req := httptest.NewRequest("GET", "/panel/api/models?provider=trae", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("want 1 model, got %v", got.Models)
	}
	if id := got.Models[0]["id"]; id != "trae:seed_m8" {
		t.Errorf("id = %v, want trae:seed_m8（冒号前缀，与 /v1/models 一致）", id)
	}
	if got.Models[0]["provider"] != auth.ProviderTrae {
		t.Errorf("provider 字段 = %v", got.Models[0]["provider"])
	}
	// 该 id 必须能被网关路由解析回 Trae（粘贴即用）
	prov, _, bare := resolveRouteForTest(got.Models[0]["id"].(string))
	if prov != auth.ProviderTrae || bare != "seed_m8" {
		t.Errorf("id 不能被路由解析: prov=%q bare=%q", prov, bare)
	}
}

// resolveRouteForTest 复刻 server.resolveRoute 的前缀剥离语义（panel 包不 import server）。
// 只覆盖 provider 段，足够验证「面板给出的 id 粘到 /v1/chat/completions 能命中正确上游」。
func resolveRouteForTest(model string) (provider, realm, bare string) {
	provider, rest := "", model
	for _, sep := range []byte{'/', ':'} {
		i := strings.IndexByte(rest, sep)
		if i <= 0 {
			continue
		}
		head := strings.ToLower(rest[:i])
		switch head {
		case "workbuddy":
			return "workbuddy", "cn", rest[i+1:]
		case "zcode", "zai", "bigmodel":
			return "zcode", "cn", rest[i+1:]
		case "trae", "traecode":
			return "trae", "cn", rest[i+1:]
		}
	}
	return "workbuddy", "cn", rest
}
