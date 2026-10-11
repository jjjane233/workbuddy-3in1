// Command zcsync 把本机 ZCode 客户端解出的签名凭据推送到远端网关。
//
// 解决的真实问题：网关部署在云服务器，服务器上没有 ZCode 客户端；
// 通道A（服务端直连上游换 key）被上游对「带有效凭据的请求」定向短路
// （实测 200 + 空体，见 docs/ZCODE_CREDENTIAL_AND_ACTIVITY.md §三 3.1），
// 所以服务器自己换不到 key；而 ZCode 客户端所在的本机能解出成品 <keyId>.<secret>。
//
// 正确架构是「本机(有客户端) → 远端网关(无客户端)」单向推送：
//
//	zcsync -gateway http://your-gateway.example.com -api-key sk-xxx [-once] [-interval 5m]
//
// 它做三件事：
//  1. 读本机客户端凭据（config.json 成品，附带 credentials.json 的 jwt/uid）
//  2. POST /panel/api/zcode/credential/auto，带 force=true（secret 轮换也能覆盖）
//  3. 循环执行 + 去重日志（凭据没变就静默）
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

func main() {
	var (
		gateway  = flag.String("gateway", "", "远端网关基址，例如 http://your-gateway.example.com")
		apiKey   = flag.String("api-key", "", "远端网关 API key（Bearer）")
		once     = flag.Bool("once", false, "只同步一次后退出")
		interval = flag.Duration("interval", 5*time.Minute, "同步周期")
		uids     = flag.String("uids", "", "逗号分隔，只同步这些 uid（默认本机客户端 uid）")
		timeout  = flag.Duration("timeout", 60*time.Second, "单次请求超时")
		dry      = flag.Bool("dry-run", false, "只打印将要推送的内容，不实际请求")
	)
	flag.Parse()

	if *gateway == "" || *apiKey == "" {
		fmt.Fprintln(os.Stderr, "用法: zcsync -gateway <url> -api-key <key> [-once] [-interval 5m]")
		os.Exit(2)
	}
	base := strings.TrimRight(*gateway, "/")
	hc := &http.Client{Timeout: *timeout}

	last := ""
	run := func() {
		info := auth.DiscoverZCodeLocalClient()
		cred := info.APIKey
		if cred == "" {
			cred = auth.ReadSignCredentialFromLocalClient()
		}
		if cred == "" {
			log.Printf("本机未解出 ZCode 签名凭据（客户端未登录/未开套餐），跳过")
			return
		}
		payload := map[string]any{"text": cred, "force": true, "validate": true}
		if *uids != "" {
			payload["uids"] = strings.Split(*uids, ",")
		} else if info.UID != "" {
			payload["uids"] = []string{info.UID}
		}
		if *dry {
			b, _ := json.Marshal(map[string]any{
				"gateway": base, "uid": info.UID, "provider": info.Provider,
				"key_id": keyIDOf(cred), "preview": preview(cred), "payload_uids": payload["uids"],
			})
			fmt.Println(string(b))
			return
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", base+"/panel/api/zcode/credential/auto", bytes.NewReader(body))
		if err != nil {
			log.Printf("构造请求失败: %v", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+*apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			log.Printf("推送失败: %v", err)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var d struct {
			OK        bool   `json:"ok"`
			Total     int    `json:"total"`
			Succeeded int    `json:"succeeded"`
			Wrote     int    `json:"wrote"`
			Updated   int    `json:"updated"`
			Validated string `json:"validated"`
			Error     string `json:"error"`
		}
		_ = json.Unmarshal(raw, &d)
		if resp.StatusCode != 200 || !d.OK {
			log.Printf("网关返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
			return
		}
		sig := fmt.Sprintf("%d/%d/%d/%s", d.Succeeded, d.Wrote, d.Updated, d.Validated)
		if sig == last {
			return
		}
		last = sig
		log.Printf("已同步 uid=%s keyId=%s → 目标 %d 个号，写入 %d 更新 %d 校验 %s",
			info.UID, keyIDOf(cred), d.Total, d.Wrote, d.Updated, d.Validated)
	}

	run()
	if *once {
		return
	}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for range t.C {
		run()
	}
}

func keyIDOf(cred string) string {
	if i := strings.IndexByte(cred, '.'); i > 0 {
		return cred[:i]
	}
	return ""
}

func preview(cred string) string {
	i := strings.IndexByte(cred, '.')
	if i <= 0 {
		return ""
	}
	h, t := cred[:i], cred[i+1:]
	if len(h) > 8 {
		h = h[:8]
	}
	if len(t) > 4 {
		t = t[:4]
	}
	return h + "..." + t
}
