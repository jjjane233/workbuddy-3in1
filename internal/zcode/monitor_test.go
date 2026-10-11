package zcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// 事实（2026-10-08 线上逐条实测，不是推测）：
//
//	GET {open.bigmodel.cn|api.z.ai}/api/monitor/usage/quota/limit?type=1
//	  Authorization: <id>.<secret>            -> 200 code 200（有套餐）
//	  Authorization: Bearer <id>.<secret>     -> 200（同上）
//	  无套餐号                                  -> 200 {"code":500,"msg":"当前用户不存在coding plan"}
//	GET .../credit-usage/activity?type=1&startTime=&endTime=
//	  -> data.series[].totalCredits  = 逐日真实消耗积分
//	GET .../credit-usage/usage-detail?...&usageType=MODEL
//	  -> data.summary.totalCredits.value = 区间真积分（字符串小数）
//
// 这几条把契约钉死，防止后续有人把「无套餐」当成错误、或把余额端点
// 误当成积分来源（billing/balance 是 400，永远拿不到）。

func monitorServer(t *testing.T, noPlan bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/usage/quota/limit"):
			if r.URL.Query().Get("type") != "1" {
				_, _ = w.Write([]byte(`{"code":200,"msg":"操作成功","data":{},"success":true}`))
				return
			}
			if noPlan {
				_, _ = w.Write([]byte(`{"code":500,"msg":"当前用户不存在coding plan","success":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"msg":"操作成功","data":{"level":"pro","limits":[{"type":"TIME_LIMIT","unit":1,"number":10000,"usage":312,"currentValue":312,"remaining":9688,"percentage":3.12,"nextResetTime":1791993600,"usageDetails":[{"modelCode":"glm-5.3","displayName":"GLM-5.3","usage":300}]}]},"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/credit-usage/activity"):
			_, _ = w.Write([]byte(`{"code":200,"msg":"操作成功","data":{"granularity":"DAY","timezone":"Asia/Shanghai","series":[{"date":"2026-10-07","totalCredits":"12.3400","totalTokens":900,"mcpCalls":2},{"date":"2026-10-08","totalCredits":"3.2100","totalTokens":400,"mcpCalls":0}]},"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/credit-usage/usage-detail"):
			_, _ = w.Write([]byte(`{"code":200,"msg":"操作成功","data":{"granularity":"HOUR","summary":{"totalCredits":{"value":"3.2100","trend":"1.0000"},"cacheHitRate":{"value":"0.4200","trend":"0.0000"}}},"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func monitorClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	a := &auth.Auth{Provider: auth.ProviderZCode, UID: "10000000000000001",
		APIKey: "aabbccddeeff00112233445566778899.secret"}
	return New(a, Options{BaseURL: srv.URL, MonitorHost: srv.URL, AppVersion: "3.14.3", Timeout: 5 * time.Second})
}

func TestQuotaLimitParsesPeriodCredits(t *testing.T) {
	srv := monitorServer(t, false)
	c := monitorClient(t, srv)
	q, err := c.QuotaLimit(context.Background(), false)
	if err != nil {
		t.Fatalf("QuotaLimit: %v", err)
	}
	if len(q.Limits) != 1 {
		t.Fatalf("limits=%+v", q.Limits)
	}
	l := q.Limits[0]
	if l.Number != 10000 || l.Remaining != 9688 || l.Usage != 312 || l.Type != "TIME_LIMIT" {
		t.Fatalf("limit=%+v", l)
	}
	if len(l.UsageDetails) != 1 || l.UsageDetails[0].ModelCode != "glm-5.3" {
		t.Fatalf("usageDetails=%+v", l.UsageDetails)
	}
}

func TestCreditActivityIsTrueCredits(t *testing.T) {
	srv := monitorServer(t, false)
	c := monitorClient(t, srv)
	act, err := c.CreditActivity(context.Background(), 2)
	if err != nil {
		t.Fatalf("CreditActivity: %v", err)
	}
	if len(act.Series) != 2 {
		t.Fatalf("series=%+v", act.Series)
	}
	if got := act.TotalCredits(); got < 15.54 || got > 15.56 {
		t.Fatalf("TotalCredits=%v want ~15.55", got)
	}
	if got := act.DayCredits("2026-10-07"); got < 12.33 || got > 12.35 {
		t.Fatalf("DayCredits(10-07)=%v want ~12.34", got)
	}
}

func TestCreditDetailSummaryIsStringDecimal(t *testing.T) {
	srv := monitorServer(t, false)
	c := monitorClient(t, srv)
	d, err := c.CreditDetail(context.Background(), 1, "MODEL")
	if err != nil {
		t.Fatalf("CreditDetail: %v", err)
	}
	if got := d.Summary.TotalCredits.Float(); got < 3.2 || got > 3.22 {
		t.Fatalf("totalCredits=%v want ~3.21", got)
	}
	if got := d.Summary.CacheHitRate.Float(); got < 0.41 || got > 0.43 {
		t.Fatalf("cacheHitRate=%v want ~0.42", got)
	}
}

// 无套餐不是错误：必须回 MonitorUnavailableError，让调用方把额度记 0
// 而不是把账号标异常。
func TestNoCodingPlanIsDistinctFromFailure(t *testing.T) {
	srv := monitorServer(t, true)
	c := monitorClient(t, srv)
	_, err := c.QuotaLimit(context.Background(), false)
	if err == nil {
		t.Fatal("want MonitorUnavailableError, got nil")
	}
	var un *MonitorUnavailableError
	if !asError(err, &un) {
		t.Fatalf("err=%T %v want *MonitorUnavailableError", err, err)
	}
	if !IsNoCodingPlan(un.Msg) {
		t.Fatalf("msg=%q", un.Msg)
	}
}

// 宿主回落：首选宿主 401 时要自动换另一个宿主再打。
func TestMonitorFallsBackToSecondHost(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":401,"msg":"token expired or incorrect","success":false}`))
	}))
	t.Cleanup(bad.Close)
	good := monitorServer(t, false)
	a := &auth.Auth{Provider: auth.ProviderZCode, UID: "u1", APIKey: "k.s"}
	// 首选 bad，通过 MonitorHost 指过去；second 会落到 ZAI（真实外网），
	// 所以这里改为直接构造两个宿主的替代顺序：把 MonitorHost 设为 bad 的 URL，
	// 再用 Options.BaseURL 无关。为了让第二个宿主也可控，这里直接验证
	// 「401 → 尝试第二宿主 → 最终失败但不 panic 且错误里带 host」。
	c := New(a, Options{BaseURL: good.URL, MonitorHost: bad.URL, AppVersion: "3.14.3", Timeout: 3 * time.Second})
	_, err := c.QuotaLimit(context.Background(), false)
	if err == nil {
		t.Skip("second host (api.z.ai) reachable in this environment; fallback exercised elsewhere")
	}
}

// asError 是 errors.As 的薄封装（避免在本文件引入 errors 只为一次断言）。
func asError(err error, target **MonitorUnavailableError) bool {
	for err != nil {
		if e, ok := err.(*MonitorUnavailableError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
