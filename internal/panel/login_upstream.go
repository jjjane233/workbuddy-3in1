// login_upstream.go 面板内嵌的 Trae / ZCode 浏览器登录（三合一账号添加）。
//
// 与 login.go（CodeBuddy 设备授权）并列，按 provider 分流：
//
//	Trae  : 授权码 + PKCE。面板给出 www.trae.cn/authorization 链接，用户登录后
//	        服务端 307 重定向到本网关的 /panel/api/login/trae/callback，query 里带
//	        authCodeInfo(JSON,内含 AuthCode)/userInfo；面板轮询到回调后拿
//	        AuthCode + 本地 codeVerifier 去 api.trae.cn 换票（ExchangeTokenByAuthCode）。
//	ZCode : CLI OAuth 设备流（服务端可驱动）。POST /api/v1/oauth/cli/init 拿
//	        authorize_url + flow_id，用户浏览器登录后服务端完成绑定；面板轮询
//	        GET /api/v1/oauth/cli/poll/<flow_id>，ready 时拿到 zcodejwt + user_id。
//
// 为什么要 callback 走网关自身而不是回环：
//
//	面板部署在公网（your-gateway.example.com），用户浏览器在任意机器上——回环
//	127.0.0.1 的授权码永远回不到网关。改成把 auth_callback_url 指向网关的公开
//	地址后，授权码经浏览器 → 网关 /callback 入口 → 进程内会话表 → 轮询换票。
package panel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/trae"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// upstreamLoginTTL 上游登录会话有效期（与 loginTTL 同口径）。
const upstreamLoginTTL = 15 * time.Minute

// upstreamLogin 是一次进行中的 Trae / ZCode 登录会话。
type upstreamLogin struct {
	provider string // "trae" / "zcode"
	created  time.Time
	realm    string // 仅 ZCode 用：cn（Z.AI）/ bigmodel
	// Trae
	verifier  string
	deviceKey *trae.DeviceKeyPair
	// deviceID/machineID 在 start 时生成并固定，poll 换票必须复用同一组值。
	//
	// 为什么不能各生成一次：Trae 把 DeviceInfo 写进授权码与 token 的绑定关系里，
	// start（决定授权页展示哪台设备）与 poll（ExchangeToken 上报 DeviceInfo）用的
	// device_id 不一致时，换票会被判成"设备不匹配"而失败。逆向确认：客户端
	// main.js 只在启动时算一次 device_id，整个登录流程复用同一个值。
	deviceID  string
	machineID string
	// callbackPort 是本机回环中继监听的端口（授权页白名单要求 127.0.0.1:<port>）。
	callbackPort int
	// Trae 回调投递（浏览器 → 本机中继 → /callback）
	callback   string // 原始 query
	callbackAt time.Time
	// ZCode
	pollToken string
	flowID    string
	interval  int
	expires   time.Time
}

// publicBase 推导对外可达的面板基址：显式配置优先，其次按请求头。
// 挂反代时 X-Forwarded-Proto 决定 scheme（nginx 默认带 X-Forwarded-Proto）。
func (p *Panel) publicBase(r *http.Request) string {
	if b := strings.TrimRight(strings.TrimSpace(p.cfg.PublicBaseURL), "/"); b != "" {
		return b
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = strings.TrimSpace(strings.Split(xf, ",")[0])
	}
	host := r.Host
	if xf := r.Header.Get("X-Forwarded-Host"); xf != "" {
		host = strings.TrimSpace(strings.Split(xf, ",")[0])
	}
	return scheme + "://" + host
}

// ---- 会话表 ----

func (p *Panel) putUpstreamLogin(state string, s *upstreamLogin) {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	if p.ulogins == nil {
		p.ulogins = map[string]*upstreamLogin{}
	}
	for k, v := range p.ulogins {
		if time.Since(v.created) > upstreamLoginTTL {
			delete(p.ulogins, k)
		}
	}
	p.ulogins[state] = s
}

func (p *Panel) getUpstreamLogin(state string) *upstreamLogin {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	return p.ulogins[state]
}

func (p *Panel) delUpstreamLogin(state string) {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	delete(p.ulogins, state)
}

// findUpstreamLoginByCallback 按回调投递找到对应会话（回调 URL 里带 state）。
func (p *Panel) findUpstreamLoginByCallback(state string) *upstreamLogin {
	return p.getUpstreamLogin(state)
}

// ---- Trae ----

// loginTraeStart 发起 Trae 授权码登录，返回授权 URL 与 state。
func (p *Panel) loginTraeStart(w http.ResponseWriter, r *http.Request) {
	state := randomHexToken(16)
	pk, err := trae.GeneratePKCE()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成 PKCE 失败: "+err.Error())
		return
	}
	key, err := trae.GenerateDeviceKeyPair()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成设备密钥失败: "+err.Error())
		return
	}
	// body 可带 {"callback_port":N}：用户本机中继要监听哪个端口（默认随机）。
	// 授权页对 auth_callback_url 做**前端硬校验**（正则 ^http://127\.0\.0\.1:(\d+)/authorize$），
	// 公网回调地址一定被拒；只能把回调指回用户本机，再由中继把 query 转发进来。
	// 端口优先级：请求显式指定 > 常驻中继在线（用它的固定端口，用户零操作）
	//            > 随机高位端口（老的一次性中继 / 手工粘贴通道）。
	//
	// 为什么把"常驻中继在线"排在随机端口之前：中继常驻在 17654，授权页会跳到
	// 127.0.0.1:17654；此时若仍按下发随机端口，中继根本收不到，用户又会看到
	// ERR_CONNECTION_REFUSED —— 装了常驻中继却还要手工粘贴，比没装更让人困惑。
	port := 0
	if r.Body != nil {
		var body struct {
			CallbackPort int `json:"callback_port"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err == nil {
			if body.CallbackPort >= 1024 && body.CallbackPort <= 65535 {
				port = body.CallbackPort
			}
		}
	}
	relayOn := false
	if port == 0 {
		if st, on := p.relayStatus(); on && st.port >= 1024 && st.port <= 65535 {
			port, relayOn = st.port, true
		}
	}
	if port == 0 {
		port = 30000 + int(randomUint32()%30000)
	}
	deviceID := randomDigits(15)
	machineID := trae.MachineIDFromHost(hostname())
	callback := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)
	base := p.publicBase(r)

	u := buildTraeAuthURL(callback, state, pk.Challenge, deviceID, machineID)
	p.putUpstreamLogin(state, &upstreamLogin{
		provider:     auth.ProviderTrae,
		created:      time.Now(),
		verifier:     pk.Verifier,
		deviceKey:    key,
		deviceID:     deviceID,
		machineID:    machineID,
		callbackPort: port,
	})
	log.Printf("panel: 发起 Trae 登录 state=%s 回环回调=%s 中继目标=%s", state, callback, base)
	// 一次性中继命令只在"常驻中继没在线"时才给：常驻中继在线意味着端口已经有人
	// 接住，再塞一条命令给用户只是噪音。
	var relayPS, relaySH string
	if !relayOn {
		relayPS = traeRelayPowerShell(port, base+"/panel/api/login/trae/callback")
		relaySH = traeRelayShell(port, base+"/panel/api/login/trae/callback")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": u, "state": state,
		"callback":      callback,
		"callback_port": port,
		"relay_target":  base + "/panel/api/login/trae/callback",
		"relay_ps":      relayPS,
		"relay_sh":      relaySH,
		// relay_online=true 表示浏览器会被 127.0.0.1:<port> 上的常驻中继接住，
		// 用户登录完什么都不用做（前端据此隐藏"方式一/方式二"的说明）。
		"relay_online": relayOn,
	})
}

// traeRelayPowerShell 生成 Windows 侧一行中继命令（复制到 PowerShell 执行）。
//
// 用户本机监听 127.0.0.1:<port>/authorize，收到 Trae 授权页 307 过来的
// ?authCodeInfo=... 后原样转发给网关的公开回调端点。转发成功后回一个
// "可以关闭本页"的响应，与 Trae 官方客户端本地 server 的行为一致。
func traeRelayPowerShell(port int, target string) string {
	// 把回调的**原始 URL 串**原样 POST 给网关（服务端从 body 里解析 query）。
	// 这样 authCodeInfo 里的 JSON / %XX / '+' 不经过任何二次拼装，零转义风险。
	return fmt.Sprintf(
		`$p=%d;$t='%s';$l=New-Object Net.HttpListener;$l.Prefixes.Add("http://127.0.0.1:$p/");$l.Start();`+
			`Write-Host "中继已就绪（端口 $p），请回到面板打开授权链接…";$c=$l.GetContext();`+
			`$raw=$c.Request.RawUrl;$i=$raw.IndexOf('?');if($i -ge 0){$q=$raw.Substring($i+1)}else{$q=''};`+
			`try{Invoke-WebRequest -UseBasicParsing -Method POST -Uri $t -Body $q -ContentType 'text/plain' | Out-Null}catch{};`+
			`$b=[Text.Encoding]::UTF8.GetBytes('<meta charset=utf-8><h2>授权成功</h2><p>可以关闭本页，返回面板等待账号载入。</p>');`+
			`$c.Response.ContentType='text/html; charset=utf-8';$c.Response.OutputStream.Write($b,0,$b.Length);$c.Response.Close();$l.Stop()`,
		port, target)
}

// traeRelayShell 生成 Linux / macOS 侧一行中继命令（bash）。
func traeRelayShell(port int, target string) string {
	return fmt.Sprintf(
		`p=%d;t='%s';{ printf 'HTTP/1.1 200 OK
Content-Type: text/html; charset=utf-8

<meta charset=utf-8><h2>授权成功</h2><p>可以关闭本页。</p>'; } | `+
			`{ q=$(nc -l 127.0.0.1 $p | head -1 | sed 's/[^?]*//;s/ HTTP.*//'); `+
			`curl -s -X POST -H 'Content-Type: text/plain' --data "$q" "$t" >/dev/null; }`,
		port, target)
}

// buildTraeAuthURL 构造授权 URL（字段顺序与 Trae CN 客户端一致）。
func buildTraeAuthURL(callback, state, challenge, deviceID, machineID string) string {
	q := []string{
		"login_version=1",
		"auth_from=trae",
		"login_channel=native_ide",
		"plugin_version=" + trae.DefaultIDEVersion,
		"auth_type=local",
		"client_id=" + trae.OAuthClientID,
		"redirect=0",
		"login_trace_id=" + state,
		"auth_callback_url=" + url.QueryEscape(callback),
		"machine_id=" + machineID,
		"device_id=" + deviceID,
		"x_device_id=" + deviceID,
		"x_machine_id=" + machineID,
		"x_device_brand=pc",
		"x_device_type=windows",
		"x_os_version=10.0.19045",
		"x_app_version=" + trae.DefaultIDEVersion,
		"x_app_type=IDE_PC",
		"code_challenge=" + challenge,
		"code_challenge_method=S256",
	}
	return "https://www.trae.cn/authorization?" + strings.Join(q, "&")
}

// loginTraeCallback 接收 Trae 授权回调（浏览器 307 落地）。
//
// 不在这里换票：回调可能在**用户的浏览器**里发起而网关尚未就绪于该请求；
// 统一由轮询端消费，回调只做「原样记下 query」这一件事。
func (p *Panel) loginTraeCallback(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.RawQuery
	// 中继优先走 POST，把**原始 query 串**放进 body 原样转发。
	//
	// 为什么不让中继拼 URL 再 GET：Trae 回调里的 authCodeInfo 是一整段
	// percent-encoded 的 JSON（含 {}""、+ 等字符），经 PowerShell / curl 再拼一次
	// URL 时极易被二次转义或吃字符（曾观测到 + 被解成空格 → JSON 解析失败）。
	// POST 原样投递则零转义风险，服务端拿到什么就是什么。
	if r.Method == http.MethodPost {
		if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
			if q := strings.TrimSpace(string(body)); q != "" {
				raw = strings.TrimPrefix(q, "?")
				if i := strings.IndexByte(raw, '?'); i >= 0 && strings.HasPrefix(strings.TrimSpace(string(body)), "http") {
					raw = raw[i+1:]
				}
			}
		}
	}
	state := r.URL.Query().Get("state")
	if state == "" {
		state = r.URL.Query().Get("login_trace_id")
	}
	if state == "" {
		// Trae 授权页 307 时回带的参数名是 loginTraceID（驼峰），
		// 与授权 URL 里我们写入的 login_trace_id 不同名——两种都要认。
		state = r.URL.Query().Get("loginTraceID")
	}
	if state == "" && raw != "" {
		// POST 转发路径：query 全在 body 里，URL 上没有 state，从头再解析一遍。
		if vals, err := url.ParseQuery(raw); err == nil {
			state = firstNonEmptyStr(vals.Get("state"), vals.Get("login_trace_id"), vals.Get("loginTraceID"))
		}
	}
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	sess := p.getUpstreamLogin(state)
	if sess == nil {
		http.Error(w, "unknown or expired state", http.StatusNotFound)
		return
	}
	sess.callback = raw
	sess.callbackAt = time.Now()
	log.Printf("panel: Trae 回调已收到 state=%s（%d 字节 query）", state, len(raw))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>登录完成</title>
<body style="font:15px system-ui;padding:48px;text-align:center">
<h2>授权成功</h2><p>可以关闭本页面，返回面板等待账号自动载入。</p></body>`)
}

// loginTraeSubmit 手工投递回调 URL（兜底通道，零本机依赖）。
//
//	POST /panel/api/login/trae/submit  {"state":"...","callback_url":"http://127.0.0.1:PORT/authorize?...authCodeInfo=..."}
//
// 【为什么需要它】授权页对 auth_callback_url 做前端硬校验，只认回环地址，所以
// 用户浏览器登录完必然被 307 到 http://127.0.0.1:<port>/authorize?...，若本机没跑
// 中继就会显示"无法访问此网站"。但**地址栏里那串 URL 带着完整的 authCodeInfo**，
// 把它整段粘贴回来即可完成换票——不依赖 PowerShell / nc / 端口占用，任何环境都能用。
func (p *Panel) loginTraeSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	if body.State == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	sess := p.getUpstreamLogin(body.State)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	url := strings.TrimSpace(body.CallbackURL)
	if url == "" {
		writeErr(w, http.StatusBadRequest, "回调 URL 为空")
		return
	}
	// 兼容三种输入：完整 URL、只有 query 段、以及被 shell/终端转义过的整串。
	q := url
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		q = url[i+1:]
	} else if !strings.Contains(url, "authCodeInfo") && !strings.Contains(url, "code=") {
		writeErr(w, http.StatusBadRequest, "这串内容里没有 authCodeInfo / code，可能复制少了（请从浏览器地址栏整行复制）")
		return
	}
	if _, err := extractTraeAuthCode(q); err != nil {
		writeErr(w, http.StatusBadRequest, "解析回调失败: "+err.Error())
		return
	}
	sess.callback = q
	sess.callbackAt = time.Now()
	log.Printf("panel: Trae 手工回调已接收 state=%s（%d 字节）", body.State, len(q))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "回调已接收，正在换票…"})
}

// loginTraePoll 轮询 Trae 登录：回调到达后换票、落盘、入池。
func (p *Panel) loginTraePoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	sess := p.getUpstreamLogin(state)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	if sess.callback == "" {
		if time.Since(sess.created) > upstreamLoginTTL {
			p.delUpstreamLogin(state)
			writeErr(w, http.StatusGone, "授权超时（15 分钟），请重新发起")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}

	authCode, err := extractTraeAuthCode(sess.callback)
	if err != nil {
		p.delUpstreamLogin(state)
		writeErr(w, http.StatusBadGateway, "解析回调失败: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tmp := &auth.Auth{
		Provider:            auth.ProviderTrae,
		DevicePrivateKeyPEM: sess.deviceKey.PrivateKeyPEM,
		DevicePublicKeyPEM:  sess.deviceKey.PublicKeyPEM,
		// 必须与 start 时送给授权页的 device_id / machine_id 完全一致，
		// 否则 ExchangeToken 上报的 DeviceInfo 与授权码绑定信息对不上。
		MachineID: firstNonEmptyStr(sess.machineID, trae.MachineIDFromHost(hostname())),
		DeviceID:  firstNonEmptyStr(sess.deviceID, randomDigits(15)),
	}
	cli := trae.New(tmp, trae.Options{})
	res, err := cli.ExchangeTokenByAuthCode(ctx, authCode, sess.verifier, sess.deviceKey.PublicKeyPEM)
	if err != nil {
		p.delUpstreamLogin(state)
		writeErr(w, http.StatusBadGateway, "换取 token 失败: "+err.Error())
		return
	}
	tmp.AccessToken = res.Token
	ui, err := cli.GetUserInfo(ctx)
	if err != nil {
		// GetUserInfo 失败不阻断：token 已到手，用户名可后续补。
		log.Printf("panel: Trae GetUserInfo 失败（token 已获取）: %v", err)
	}
	uid := ""
	nick := ""
	if ui != nil {
		uid, nick = ui.UserID, ui.NickName
	}
	if uid == "" {
		uid = deviceUIDFromToken(res.Token)
	}
	if uid == "" || !validUID(uid) {
		p.delUpstreamLogin(state)
		writeErr(w, http.StatusBadGateway, "上游未返回可用 uid（token 已获取但账号标识缺失）")
		return
	}
	if nick == "" {
		nick = uid
	}

	a := &auth.Auth{
		Provider:            auth.ProviderTrae,
		Mode:                auth.ModeJWT,
		AccessToken:         res.Token,
		RefreshToken:        res.RefreshToken,
		TokenExpireAt:       res.TokenExpireAt,
		RefreshExpireAt:     res.RefreshExpireAt,
		UID:                 uid,
		Username:            nick,
		Nickname:            nick,
		Region:              "cn",
		DeviceID:            tmp.DeviceID,
		DevicePrivateKeyPEM: sess.deviceKey.PrivateKeyPEM,
		DevicePublicKeyPEM:  sess.deviceKey.PublicKeyPEM,
		MachineID:           tmp.MachineID,
		Remark:              "panel-login",
		LastRefresh:         time.Now().Unix(),
		FilePath:            filepath.Join(p.cfg.AuthDir, fmt.Sprintf("trae-%s.json", uid)),
	}
	if err := p.persistUpstream(a); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	p.delUpstreamLogin(state)
	log.Printf("panel: Trae 新账号已热加载 uid=%s nickname=%q", uid, nick)

	credits, total, checkinMsg := p.postLoginRefresh(ctx, a)
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "uid": uid, "nickname": nick, "provider": auth.ProviderTrae,
		"credits": credits, "credits_total": total, "checkin_message": checkinMsg,
	})
}

// ---- ZCode ----

// loginZCodeStart 发起 ZCode 设备流登录，返回授权 URL。
func (p *Panel) loginZCodeStart(w http.ResponseWriter, r *http.Request) {
	provider := "zai"
	if r.Body != nil {
		var body struct {
			Provider string `json:"provider"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err == nil {
			if body.Provider == "bigmodel" {
				provider = "bigmodel"
			}
		}
	}
	pollToken := zcode.NewPollToken()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	hc := &http.Client{Timeout: 30 * time.Second}
	init, err := zcode.CLIInit(ctx, hc, pollToken, provider)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "发起 ZCode 授权失败: "+err.Error())
		return
	}
	state := randomHexToken(16)
	exp := time.Now().Add(upstreamLoginTTL)
	if init.ExpiresAt > 0 {
		exp = time.Unix(init.ExpiresAt, 0)
	}
	p.putUpstreamLogin(state, &upstreamLogin{
		provider:  auth.ProviderZCode,
		created:   time.Now(),
		realm:     provider,
		pollToken: pollToken,
		flowID:    init.FlowID,
		interval:  init.PollIntervalSec,
		expires:   exp,
	})
	log.Printf("panel: 发起 ZCode 登录 provider=%s flow=%s", provider, init.FlowID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": init.AuthorizeURL, "state": state,
		"interval": init.PollIntervalSec, "expires_at": exp.Unix(),
	})
}

// loginZCodePoll 轮询 ZCode 设备流；ready 时落盘并入池。
func (p *Panel) loginZCodePoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	sess := p.getUpstreamLogin(state)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	if time.Now().After(sess.expires) {
		p.delUpstreamLogin(state)
		writeErr(w, http.StatusGone, "授权超时，请重新发起")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	hc := &http.Client{Timeout: 30 * time.Second}
	status, ready, err := zcode.CLIPoll(ctx, hc, sess.pollToken, sess.flowID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "轮询失败: "+err.Error())
		return
	}
	switch status {
	case "pending":
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	case "failed":
		p.delUpstreamLogin(state)
		writeErr(w, http.StatusBadGateway, "授权被拒绝或已失效，请重新发起")
		return
	}

	uid := ready.User.UserID
	nick := ready.User.Name
	if uid == "" || !validUID(uid) {
		p.delUpstreamLogin(state)
		writeErr(w, http.StatusBadGateway, "上游未返回可用 uid")
		return
	}
	if nick == "" {
		nick = uid
	}
	a := &auth.Auth{
		Provider: auth.ProviderZCode,
		Mode:     auth.ModeJWT,
		// ready.token 就是 zcodejwt（Bearer 给 /api/v1/zcode-plan/anthropic/v1/messages）。
		AccessToken: ready.Token,
		UID:         uid,
		Username:    nick,
		Nickname:    nick,
		Region:      "cn",
		// Source 记录账号体系（zai / bigmodel）：同一 uid 在两套体系下额度独立，
		// 面板要靠它区分，否则"我明明开通了却没额度"。
		Source:      firstNonEmptyStr(sess.realm, "zai"),
		DeviceID:    randomUUID(),
		Remark:      "panel-login",
		LastRefresh: time.Now().Unix(),
		FilePath:    filepath.Join(p.cfg.AuthDir, fmt.Sprintf("zcode-%s.json", uid)),
	}
	// 业务 API Key（请求签名用）：走设备授权刚换到的业务 token 自助换钥。
	//
	// ready 里按家族分了两套载荷：zai -> data.zai.*，bigmodel -> data.bigmodel.*。
	// 官方桌面端两者都读；旧实现只读 zai，bigmodel 家族的号于是永远拿不到 key，
	// 只能回退去读「本机客户端 config.json」，才出现「电脑不开机就用不了」。
	if accessTok, _ := ready.ProviderTokenSet(sess.realm); accessTok != "" {
		if key, err := zcode.ResolveAPIKey(ctx, hc, sess.realm, accessTok); err != nil {
			log.Printf("panel: ZCode api_key 换取失败（不影响 JWT 模式）: %v", err)
		} else {
			a.SetSignCredential(key)
			a.Mode = auth.ModeJWT // 仍以 JWT 为主，api_key 只用于签名
		}
	} else {
		log.Printf("panel: ZCode 本轮授权未下发 %s 家族业务 token，跳过换钥", sess.realm)
	}
	// 兜底：只有在自助换钥失败时才读本机客户端配置（同机部署的便利来源）。
	// 服务端（无客户端）这条必为空，自助换钥才是主路径。
	if a.SignCredential() == "" {
		if key := auth.DiscoverZCodeSignCredential(); key != "" {
			a.SetSignCredential(key)
			log.Printf("panel: ZCode 签名凭据已从本机客户端配置兜底发现（uid=%s）", a.UID)
		}
	}
	if err := p.persistUpstream(a); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	p.delUpstreamLogin(state)
	log.Printf("panel: ZCode 新账号已热加载 uid=%s nickname=%q signed=%v", uid, nick, a.SignKeyID != "")

	// 授权已下发业务 token 但换钥没成功（网络抖动 / 上游接口一时不可用）时，
	// 在后台补一次，而不是把「未换出 key」的账号留给用户去手工粘贴。
	// 只在**本轮授权确实下发了业务 token** 时补：没下发说明是 zai 家族但服务端
	// 只给了 bigmodel 载荷一类的真实缺料，补一百次也没用，应当由前端提示兜底粘贴。
	if a.SignCredential() == "" && ready != nil {
		if accessTok, _ := ready.ProviderTokenSet(sess.realm); accessTok != "" {
			realm, ownerUID, signKeyID := sess.realm, uid, a.SignKeyID
			go func() {
				bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				key, err := rekeyWithRetry(bg, 4, 5*time.Second,
					func(c context.Context) (string, error) { return zcode.ResolveAPIKey(c, hc, realm, accessTok) })
				if key == "" {
					log.Printf("panel: ZCode 补换钥最终失败 uid=%s: %v（前端会提示兜底粘贴）", ownerUID, err)
					return
				}
				a.SetSignCredential(key)
				a.Mode = auth.ModeJWT
				if serr := a.SaveAtomic(); serr != nil {
					log.Printf("panel: ZCode 补换钥写盘失败 uid=%s: %v", ownerUID, serr)
					return
				}
				log.Printf("panel: ZCode 补换钥成功 uid=%s（原 keyId=%s，已随盘更新）", ownerUID, signKeyID)
			}()
		}
	}

	credits, total, checkinMsg := p.postLoginRefresh(ctx, a)
	// has_key 让前端能直接告诉用户「凭据已自动获取」还是「这个号还缺 key、需要兜底粘贴」——
	// 这是「登录 ZCode 不再手动获取凭证」这条需求的可见性闭环：用户不用去猜。
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "uid": uid, "nickname": nick, "provider": auth.ProviderZCode,
		"credits": credits, "credits_total": total, "checkin_message": checkinMsg,
		"has_key": a.SignCredential() != "",
	})
}

// ---- 共用 ----

// persistUpstream 落盘 + 热加载进池 + 清旧状态（与 CodeBuddy 登录后的口径一致）。
func (p *Panel) persistUpstream(a *auth.Auth) error {
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		return err
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID)
	return nil
}

// postLoginRefresh 登录后刷新额度（并尽力签到）。失败只体现在返回值里。
func (p *Panel) postLoginRefresh(ctx context.Context, a *auth.Auth) (int64, int64, string) {
	if p.upstream == nil {
		return -1, 0, ""
	}
	remain, total, err := p.upstream.Balance(ctx, a)
	if err != nil {
		return -1, 0, "余额刷新失败: " + err.Error()
	}
	p.cfg.Pool.SetCredits(a.UID, remain, total)
	msg := ""
	if _, m := p.upstream.Checkin(ctx, a); m != "" {
		msg = m
	}
	return remain, total, msg
}

// extractTraeAuthCode 从回调 query 里取出 AuthCode。
//
// 回调形如 ?authCodeInfo={"AuthCode":"..."}&userInfo={...}&...，Trae 客户端
// 先按 decodeURIComponent 整体 parse，失败再按原始 parse——这里同样两种都试。
func extractTraeAuthCode(rawQuery string) (string, error) {
	candidates := []string{rawQuery}
	if dec, err := url.QueryUnescape(rawQuery); err == nil && dec != rawQuery {
		candidates = append(candidates, dec)
	}
	for _, c := range candidates {
		vals, err := url.ParseQuery(c)
		if err != nil {
			continue
		}
		if v := vals.Get("authCodeInfo"); v != "" {
			var info struct {
				AuthCode string `json:"AuthCode"`
			}
			if json.Unmarshal([]byte(v), &info) == nil && info.AuthCode != "" {
				return info.AuthCode, nil
			}
		}
		if v := vals.Get("code"); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("回调中没有 authCodeInfo.AuthCode / code")
}

// deviceUIDFromToken 无 GetUserInfo 时的兜底 uid：从 JWT payload 取 id/sub。
func deviceUIDFromToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := decodeJWTPayload(parts[1])
	if err != nil {
		return ""
	}
	var payload struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if payload.Data.ID != "" {
		return payload.Data.ID
	}
	return payload.Sub
}

var _ = hex.EncodeToString
var _ = rand.Read

// ---- 小工具 ----

// randomHexToken 生成 n 字节随机 hex 串。
func randomHexToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randomDigits 生成 n 位十进制数字串（Trae device_id 形态）。
func randomDigits(n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = digits[int(b[i])%10]
	}
	return string(b)
}

// randomUUID 生成 v4 UUID（ZCode 的 X-Device-Mid 要求合法 UUID）。
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomUint32 返回一个均匀分布的 uint32（端口随机化用）。
func randomUint32() uint32 {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// firstNonEmptyStr 返回第一个非空字符串（空值回落用）。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// hostname 返回本机主机名（machine_id 派生种子）。
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "workbuddy2api"
	}
	return h
}

// decodeJWTPayload 解 JWT 第二段（无 padding base64url），返回原始 JSON 字节。
func decodeJWTPayload(seg string) ([]byte, error) {
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(seg)
}
