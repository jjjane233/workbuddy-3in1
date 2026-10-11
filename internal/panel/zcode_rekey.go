package panel

import (
	"context"
	"time"
)

// rekeyWithRetry 反复调用 fetch 直到拿到非空凭据、尝试次数用尽或 ctx 结束。
//
// 为什么需要它：ZCode 的「登录后自动换出 <keyId>.<secret>」是一次外部依赖
// （getCustomerInfo -> api_keys -> api_keys/copy）。上游偶发 5xx/超时不该让
// 账号以「缺 key」状态落库——那种状态只能靠用户手工粘贴 config.json 补救，
// 正是「登录 ZCode 还要手动获取凭证」这条抱怨的最后一环。
//
// 失败语义：fetch 返回 (空串, err) 时继续重试；返回非空串即成功；ctx 结束立即返回。
// attempts<=0 视为 1；sleep<=0 时不等待（测试注入）。
func rekeyWithRetry(ctx context.Context, attempts int, sleep time.Duration, fetch func(context.Context) (string, error)) (string, error) {
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return "", lastErr
		}
		key, err := fetch(ctx)
		if err == nil && key != "" {
			return key, nil
		}
		lastErr = err
		if i == attempts-1 {
			break
		}
		if sleep > 0 {
			select {
			case <-ctx.Done():
				return "", lastErr
			case <-time.After(sleep):
			}
		}
	}
	return "", lastErr
}
