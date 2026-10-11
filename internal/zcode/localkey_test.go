package zcode

import (
	"encoding/json"
	"testing"
)

// TestPickOrgProjectFromData 默认机构/项目优先，否则取第一个（客户端同口径）。
func TestPickOrgProjectFromData(t *testing.T) {
	data := json.RawMessage(`{"organizations":[
		{"organizationId":"o1","organizationName":"公司A","projects":[{"projectId":"p1","projectName":"项目A"}]},
		{"organizationId":"o2","organizationName":"默认机构","projects":[
			{"projectId":"p2","projectName":"别的"},
			{"projectId":"p3","projectName":"默认项目"}]}
	]}`)
	org, proj := pickOrgProjectFromData(data)
	if org != "o2" || proj != "p3" {
		t.Fatalf("got org=%q proj=%q, want o2/p3", org, proj)
	}
}

// TestPickOrgProjectFromDataFallsBack 没有默认名时取第一组。
func TestPickOrgProjectFromDataFallsBack(t *testing.T) {
	data := json.RawMessage(`{"organizations":[{"organizationId":"x","projects":[{"projectId":"y"}]}]}`)
	org, proj := pickOrgProjectFromData(data)
	if org != "x" || proj != "y" {
		t.Fatalf("got %q/%q", org, proj)
	}
}

// TestPickOrgProjectFromDataEmpty 空/坏数据返回空。
func TestPickOrgProjectFromDataEmpty(t *testing.T) {
	for _, raw := range []string{"", "{}", `{"organizations":[]}`, "not json"} {
		if org, proj := pickOrgProjectFromData(json.RawMessage(raw)); org != "" || proj != "" {
			t.Fatalf("raw=%q got %q/%q", raw, org, proj)
		}
	}
}

// TestBizOK 判定信封成功（容忍 code 0/200 与 success 字段）。
func TestBizOK(t *testing.T) {
	f := false
	tr := true
	cases := []struct {
		env  bizEnvelope
		want bool
	}{
		{bizEnvelope{Code: 0}, true},
		{bizEnvelope{Code: 200}, true},
		{bizEnvelope{Code: 401}, false},
		{bizEnvelope{Success: &tr}, true},
		{bizEnvelope{Success: &f}, false},
		{bizEnvelope{Code: 200, Success: &f}, false},
	}
	for i, c := range cases {
		if got := bizOK(&c.env); got != c.want {
			t.Fatalf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

// TestErrBizEmptyBodyKept 语义保留：空体用可读错误。
func TestErrBizEmptyBodyKept(t *testing.T) {
	if ErrBizEmptyBody == nil {
		t.Fatal("ErrBizEmptyBody must stay")
	}
}
