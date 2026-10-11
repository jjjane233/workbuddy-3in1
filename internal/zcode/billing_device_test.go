package zcode

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// 事实（2026-10-06 线上逐条实测，不是推测）：
//
//	GET billing/balance?app_version=X                          -> 400 code 3001 parameter error
//	GET billing/balance?app_version=X&platform=Y               -> 400 code 3001 parameter error
//	GET billing/balance?app_version=X&device_mid=<uuid>        -> 200
//	同请求把 device_mid 换成非 UUID（"abc" / 纯数字 UID）        -> 400 code 3001
//	billing/preview 同样必须有 device_mid；billing/current 不校验
//
// 所以「凭据没问题、余额却是 0 / 报 400」的真实原因就是少了这个参数，
// 或者把它填成了非 UUID。下面两个用例把这条契约钉死。

func billingServer(t *testing.T, requireDeviceMid bool, wantDeviceMid string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dm := r.URL.Query().Get("device_mid")
		if !IsUUID(dm) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":3001,"msg":"parameter error"}`))
			return
		}
		if wantDeviceMid != "" && dm != wantDeviceMid {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[],"balances":[{"show_name":"GLM-5.3-Flash","unit_type":"token","capabilities":["model:glm-5.3-flash"],"total_units":100000000,"used_units":0,"remaining_units":100000000,"available_units":100000000,"expires_at":1791302400}]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBillingAlwaysSendsUUIDDeviceMid(t *testing.T) {
	srv := billingServer(t, false, "")
	a := &auth.Auth{Provider: auth.ProviderZCode, UID: "10000000000000001",
		AccessToken: "jwt", DeviceID: "0ee0a9d7-5595-4aaf-bb59-ea8d52240783"}
	c := New(a, Options{BaseURL: srv.URL, AppVersion: "3.14.3", Timeout: 5 * time.Second})
	b, st, err := c.Balance(t.Context())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if st != 200 {
		t.Fatalf("status=%d want 200", st)
	}
	if len(b.Balances) != 1 || b.Balances[0].RemainingUnits != 100000000 {
		t.Fatalf("balances=%+v", b.Balances)
	}
}

// 账号没有合法 UUID（老文件 / 手写 / 只填了 UID）时要派生出稳定 UUID，
// 而不是直接把 UID 发出去——线上实测那会 400。
func TestBillingDerivesUUIDWhenDeviceIDMissing(t *testing.T) {
	srv := billingServer(t, true, "")
	a := &auth.Auth{Provider: auth.ProviderZCode, UID: "10000000000000001", AccessToken: "jwt"}
	c := New(a, Options{BaseURL: srv.URL, AppVersion: "3.14.3", Timeout: 5 * time.Second})
	if _, st, err := c.Balance(t.Context()); err != nil || st != 200 {
		t.Fatalf("Balance: st=%d err=%v", st, err)
	}
	// 派生的 device_mid 必须稳定（两次相同），否则每次请求换脸。
	first := derivedDeviceMid(a.UID)
	if first != derivedDeviceMid(a.UID) {
		t.Fatal("derivedDeviceMid not stable")
	}
	if !IsUUID(first) {
		t.Fatalf("derivedDeviceMid=%q not a UUID", first)
	}
}

func TestBillingFallsBackToCurrentWhenBalanceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/balance") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[{"plan_id":"p1","name":"ZCode Trust Build","status":"active","ends_at":1791302400,"entitlements":[{"show_name":"GLM-5.3-Flash","unit_type":"token","capabilities":["model:glm-5.3-flash"],"grant_units":100000000}]}]}}`))
	}))
	defer srv.Close()
	a := &auth.Auth{Provider: auth.ProviderZCode, UID: "u1", AccessToken: "jwt"}
	c := New(a, Options{BaseURL: srv.URL, AppVersion: "3.14.3", Timeout: 5 * time.Second})
	b, st, err := c.Balance(t.Context())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if st != 200 || len(b.Plans) != 1 {
		t.Fatalf("st=%d plans=%+v", st, b.Plans)
	}
	// current 兜回来的方案也必须能推出授权模型，面板才不会空。
	if got := b.ModelNames(); len(got) != 1 || got[0] != "glm-5.3-flash" {
		t.Fatalf("ModelNames=%v", got)
	}
}

func TestPreviewAndClaimSendDeviceMid(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		if !IsUUID(r.URL.Query().Get("device_mid")) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":3001,"msg":"parameter error"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[]}}`))
	}))
	defer srv.Close()
	a := &auth.Auth{Provider: auth.ProviderZCode, UID: "u1", AccessToken: "jwt",
		DeviceID: "0ee0a9d7-5595-4aaf-bb59-ea8d52240783"}
	c := New(a, Options{BaseURL: srv.URL, AppVersion: "3.14.3", Timeout: 5 * time.Second})
	if _, err := c.Preview(t.Context()); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if _, err := c.Claim(t.Context(), "zcode-v3-start-plan", "", "cn"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("requests=%v", seen)
	}
	for _, s := range seen {
		if !strings.Contains(s, "device_mid=0ee0a9d7-5595-4aaf-bb59-ea8d52240783") {
			t.Errorf("missing device_mid: %s", s)
		}
	}
}

func TestIsUUID(t *testing.T) {
	good := []string{
		"0ee0a9d7-5595-4aaf-bb59-ea8d52240783",
		"0EE0A9D7-5595-4AAF-BB59-EA8D52240783",
		"00000000-0000-0000-0000-000000000000",
	}
	bad := []string{"", "abc", "10000000000000001", "0ee0a9d7-5595-4aaf-bb59-ea8d5224078",
		"0ee0a9d7_5595_4aaf_bb59_ea8d52240783", "zzzzzzzz-5595-4aaf-bb59-ea8d52240783"}
	for _, s := range good {
		if !IsUUID(s) {
			t.Errorf("IsUUID(%q)=false want true", s)
		}
	}
	for _, s := range bad {
		if IsUUID(s) {
			t.Errorf("IsUUID(%q)=true want false", s)
		}
	}
}

// 显示层：Claim 与 Balance 共用同一套 query 构造，别再各写一份。
func TestBillingQueryIncludesDeviceMid(t *testing.T) {
	a := &auth.Auth{Provider: auth.ProviderZCode, UID: "u1", DeviceID: "0ee0a9d7-5595-4aaf-bb59-ea8d52240783"}
	c := New(a, Options{AppVersion: "3.14.3"})
	q := c.billingQuery()
	if q["device_mid"] != a.DeviceID {
		t.Fatalf("billingQuery=%v", q)
	}
	if q["app_version"] != "3.14.3" || q["platform"] != "win32-x64" {
		t.Fatalf("billingQuery=%v", q)
	}
}
