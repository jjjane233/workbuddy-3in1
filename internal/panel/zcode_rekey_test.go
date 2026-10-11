package panel

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRekeyWithRetrySucceedsAfterTransientFailures 校准「登录后自动换钥」的重试语义：
// 前两次上游抖动，第三次成功时必须把 key 带回来（这是「不再手工粘贴」的关键路径）。
func TestRekeyWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	key, err := rekeyWithRetry(context.Background(), 4, 0, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("upstream 502")
		}
		return "aabbccddeeff00112233445566778899.sEcReT", nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if key != "aabbccddeeff00112233445566778899.sEcReT" {
		t.Fatalf("key = %q", key)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

// TestRekeyWithRetryExhaustsAndReportsLastErr 次数用尽时返回最后一次错误，调用方据此
// 在前端提示「这个号还缺 key，请兜底粘贴」。
func TestRekeyWithRetryExhaustsAndReportsLastErr(t *testing.T) {
	calls := 0
	key, err := rekeyWithRetry(context.Background(), 3, 0, func(context.Context) (string, error) {
		calls++
		return "", errors.New("boom")
	})
	if key != "" {
		t.Fatalf("key = %q, want empty", key)
	}
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

// TestRekeyWithRetryEmptyKeyIsFailure 空 key + nil error 也必须算失败：
// 上游「业务成功但没给 apiKey」的形态不能当成换钥成功写进池子。
func TestRekeyWithRetryEmptyKeyIsFailure(t *testing.T) {
	calls := 0
	key, _ := rekeyWithRetry(context.Background(), 2, 0, func(context.Context) (string, error) {
		calls++
		return "", nil
	})
	if key != "" || calls != 2 {
		t.Fatalf("key=%q calls=%d, want empty/2", key, calls)
	}
}

// TestRekeyWithRetryHonorsContextCancel ctx 取消后必须立即停手并返回 ctx 错误，
// 避免登录会话都失效了后台还在打上游。
func TestRekeyWithRetryHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	key, err := rekeyWithRetry(ctx, 5, 50*time.Millisecond, func(context.Context) (string, error) {
		calls++
		cancel()
		return "", errors.New("nope")
	})
	if key != "" {
		t.Fatalf("key = %q", key)
	}
	if err == nil {
		t.Fatalf("err = nil, want cancellation")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (ctx 取消后不得再打上游)", calls)
	}
}
