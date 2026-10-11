// Command zcmint 在服务器上自助换出 ZCode 的「<keyId>.<secret>」业务凭据。
//
// 为什么需要它：网关部署在云服务器，服务器上没有 ZCode 桌面客户端。旧实现里
// 面板只解析 OAuth ready 载荷里的 data.zai，bigmodel 家族的号因此拿不到业务
// token，只能回退去读「本机客户端 config.json」——于是「电脑不开机就用不了」。
//
// 本工具把整条链路做成服务端可跑，不需要任何本机客户端：
//
//  1. POST {zcode}/api/v1/oauth/cli/init  {provider:"bigmodel"|"zai"}
//     得到 {flow_id, poll_token, authorize_url, expires_at, poll_interval_sec}
//  2. 打印 authorize_url：账号主人用任意浏览器（手机也行）完成一次授权。
//     这一步是账号身份证明，无法也不应被绕过——但它与本机是否开机无关。
//  3. GET {zcode}/api/v1/oauth/cli/poll/<flow_id>  (Bearer <poll_token>)
//     得到 status:"ready" {token:<zcodejwt>, user:{...},
//     zai:{access_token}, bigmodel:{access_token,refresh_token}}
//  4. 取本家族业务 token ->
//     bigmodel: GET  https://bigmodel.cn/api/biz/customer/getCustomerInfo  (裸 token)
//     zai:      POST https://api.z.ai/api/auth/z/login {token} -> Bearer <access_token>
//     再 GET  .../organization/<oid>/projects/<pid>/api_keys
//     POST 同址 {"name":"zcode-api-key"}（不存在则创建）
//     GET  .../api_keys/copy/<apiKey>  -> {secretKey}
//     拼成「<keyId>.<secret>」。
//
// 用法：
//
//	zcmint -provider bigmodel -out /tmp/zc.key
//	zcmint -provider bigmodel -gateway http://127.0.0.1:7888 -api-key sk-xxx -uid 10000000000000001
//
// 产物默认写到 -out（0600）。-gateway/-api-key 存在时同时推送进面板池。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

func main() {
	var (
		provider = flag.String("provider", "bigmodel", "bigmodel | zai")
		out      = flag.String("out", "", "把 <keyId>.<secret> 写到该文件（0600）")
		gateway  = flag.String("gateway", "", "面板基址，如 http://127.0.0.1:7888")
		apiKey   = flag.String("api-key", "", "面板 API key（Bearer）")
		uid      = flag.String("uid", "", "归属 uid，推送进面板时用")
		timeout  = flag.Duration("timeout", 10*time.Minute, "授权等待上限")
		interval = flag.Duration("interval", 2*time.Second, "轮询间隔下限")
		quiet    = flag.Bool("quiet", false, "只输出最终结果")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	hc := &http.Client{Timeout: 30 * time.Second}

	pollToken := zcode.NewPollToken()
	init, err := zcode.CLIInit(ctx, hc, pollToken, *provider)
	if err != nil {
		fatal("init 失败: %v", err)
	}

	if !*quiet {
		fmt.Printf("provider   = %s\n", *provider)
		fmt.Printf("flow_id    = %s\n", init.FlowID)
		fmt.Printf("expires_at = %s\n", time.Unix(init.ExpiresAt, 0).Format(time.RFC3339))
		fmt.Printf("interval   = %ds\n\n", init.PollIntervalSec)
	}
	fmt.Println("请在浏览器打开下面的地址完成一次授权（手机亦可，与本机是否开机无关）：")
	fmt.Println()
	fmt.Println("  " + init.AuthorizeURL)
	fmt.Println()

	iv := *interval
	if init.PollIntervalSec > 0 {
		if d := time.Duration(init.PollIntervalSec) * time.Second; d > iv {
			iv = d
		}
	}
	var ready *zcode.CLIReady
	for ready == nil {
		status, r, err := zcode.CLIPoll(ctx, hc, pollToken, init.FlowID)
		if err != nil {
			fatal("poll 失败: %v", err)
		}
		switch status {
		case "ready":
			ready = r
		case "failed":
			fatal("授权被拒绝或已失效")
		}
		if ready != nil {
			break
		}
		if !*quiet {
			fmt.Print(".")
		}
		select {
		case <-ctx.Done():
			fmt.Println()
			fatal("等待授权超时（%s）", *timeout)
		case <-time.After(iv):
		}
	}
	if !*quiet {
		fmt.Println()
	}

	accessTok, refreshTok := ready.ProviderTokenSet(*provider)
	if accessTok == "" {
		fatal("ready 载荷里没有 %s 家族业务 token（服务器未下发）", *provider)
	}

	cred, err := zcode.ResolveAPIKey(ctx, hc, *provider, accessTok)
	if err != nil {
		fatal("换 key 失败: %v", err)
	}
	if err := zcode.ValidateAPIKey(ctx, hc, cred, "glm-5.3-flash"); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 换出的 key 连通性校验未通过: %v\n", err)
	}

	uidOut := pick(ready.User.UserID, *uid)
	fmt.Printf("\nuid        = %s\n", uidOut)
	fmt.Printf("nickname   = %s\n", ready.User.Name)
	fmt.Printf("keyId      = %s\n", keyIDOf(cred))
	fmt.Printf("credential = %s\n", cred)
	if refreshTok != "" {
		fmt.Println("refresh_token 已下发（可用于后续免交互续期）")
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(cred+"\n"), 0o600); err != nil {
			fatal("写 %s 失败: %v", *out, err)
		}
		fmt.Printf("已写入 %s\n", *out)
	}

	if *gateway != "" && *apiKey != "" {
		if err := pushCredential(*gateway, *apiKey, cred, uidOut); err != nil {
			fatal("推送面板失败: %v", err)
		}
		fmt.Println("已推送进面板池")
	}
}

func pushCredential(gateway, apiKey, cred, uid string) error {
	base := strings.TrimRight(gateway, "/")
	payload := map[string]any{"text": cred, "force": true, "validate": true}
	if uid != "" {
		payload["uids"] = []string{uid}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", base+"/panel/api/zcode/credential/auto", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var d struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &d)
	if resp.StatusCode != 200 || !d.OK {
		return fmt.Errorf("面板返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func keyIDOf(cred string) string {
	if i := strings.IndexByte(cred, '.'); i > 0 {
		return cred[:i]
	}
	return ""
}

func pick(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(1)
}
