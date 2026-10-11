package trae

import "testing"

// 上游 REST 层对所有业务结果都回 HTTP 200，真正的成败在 body.code。
// 这组用例锁住"不要只看状态码"这条铁律——旧实现就是因为它把
// 9090「活动暂不可用」记成了领取成功。
func TestActivityCode(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		code   int
		ok     bool
		msgHas string
	}{
		{"空对象=受理成功", `{}`, 0, true, ""},
		{"无 code 字段", `{"data":null}`, 0, true, ""},
		{"code 0 = 成功", `{"code":0,"message":"ok"}`, 0, true, "ok"},
		{"9090 活动暂不可用", `{"code":9090,"message":"活动暂不可用"}`, 9090, true, "活动暂不可用"},
		{"9070 暂无领取权限", `{"code":9070,"message":"暂无领取权限"}`, 9070, true, "暂无领取权限"},
		{"9069 活动暂不可用(权益)", `{"code":9069,"message":"活动暂不可用"}`, 9069, true, "活动暂不可用"},
		{"4001 参数错误", `{"code":4001,"message":"param is invalid"}`, 4001, true, "param is invalid"},
	}
	for _, c := range cases {
		code, msg, ok := ActivityCode([]byte(c.raw))
		if !ok {
			t.Fatalf("%s: ok=false，应能解析", c.name)
		}
		if code != c.code {
			t.Fatalf("%s: code=%d want %d", c.name, code, c.code)
		}
		if c.msgHas != "" && msg != c.msgHas {
			t.Fatalf("%s: msg=%q want %q", c.name, msg, c.msgHas)
		}
	}
}

func TestActivityCodeEmptyAndGarbage(t *testing.T) {
	if _, _, ok := ActivityCode(nil); ok {
		t.Fatal("空响应应判为无法解析")
	}
	if _, _, ok := ActivityCode([]byte("<html>502</html>")); ok {
		t.Fatal("非 JSON 应判为无法解析")
	}
}

// 只有 9090/9070 属于"下轮再试"，其余业务码都是真失败。
func TestIsPendingCode(t *testing.T) {
	for _, c := range []int{9069, 9090, 9070} {
		if !IsPendingCode(c) {
			t.Fatalf("code %d 应为 pending", c)
		}
	}
	for _, c := range []int{0, 4001, 1001, 500} {
		if IsPendingCode(c) {
			t.Fatalf("code %d 不应为 pending", c)
		}
	}
}

