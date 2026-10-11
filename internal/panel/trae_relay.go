package panel

// Trae 常驻回环中继：把授权页甩到 127.0.0.1 的那一跳接住。
//
// 【为什么必须这么做】
//
// Trae 授权页对 auth_callback_url 做**字符串级硬校验**（网页版与插件版各一处，
// 见 lf-cdn.trae.com.cn 的 static/js/async/authorization/page.*.js）：
//
//	if (!/^http:\/\/127\.0\.0\.1:(\d+)\/authorize$/.test(W)) return void ee(3);
//	// W = decodeURIComponent(auth_callback_url)
//
// 公网地址（含本网关域名）一律被判为 invalidUrl 并停在错误态；另一条
// vscode_open_uri 分支只认 vscode: / bytedanceide: / cursor: / windsurf: 四种
// scheme。也就是说 —— 回调注定落在 127.0.0.1:<port>，改地址这条路是死的。
//
// 本机没人监听该端口时，用户登录完只能看到 ERR_CONNECTION_REFUSED，还得手动把
// 地址栏那串 URL 粘回面板。这就是"多余的一步"的来源。
//
// 【解法】把端口固定下来（默认 17654），给出一条**只需执行一次**的安装命令：在本机
// 装一个常驻小中继。之后每次登录，授权页跳到 127.0.0.1:17654 时由它接住、原样转发
// 给网关，并回一个"授权成功"页。用户全程看不到任何报错，也不需要任何额外操作。
//
// 中继每 15s 向 /panel/api/login/trae/relay/ping 打一次心跳，面板据此显示
// "常驻中继：在线/离线"，并在在线时把回调端口固定为 17654。
import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultTraeRelayPort 常驻中继默认端口。
//
// 为什么选 17654：避开 Trae 客户端自身的调试端口段（它用 30000+ 的随机高位），
// 也避开旧文档里的示例端口 33398/30001，避免与一次性中继撞车。
const defaultTraeRelayPort = 17654

// relayOnlineWindow 多久没心跳算离线。中继 15s 一次心跳，45s 容忍三次丢失
// （本机短暂休眠/卡顿不至于误判离线，误差方向也是"宁可报在线"更友好）。
const relayOnlineWindow = 45 * time.Second

// traeRelayState 常驻中继的最近一次心跳。
type traeRelayState struct {
	port     int
	host     string
	lastSeen time.Time
}

// relayPort 常驻中继的固定端口（配置优先，非法值回落默认）。
func (p *Panel) relayPort() int {
	if v := p.cfg.TraeRelayPort; v >= 1024 && v <= 65535 {
		return v
	}
	return defaultTraeRelayPort
}

// relayStatus 返回中继状态与是否在线。
//
// 未收到过心跳时仍返回配置端口（而非 0），这样调用方拿到的永远是"该用哪个端口"，
// 不必区分"没装"和"装了但离线"。
func (p *Panel) relayStatus() (traeRelayState, bool) {
	p.relayMu.Lock()
	st := traeRelayState{port: p.relayPort()}
	if p.relay != nil {
		st = *p.relay
	}
	p.relayMu.Unlock()
	if st.port == 0 {
		st.port = p.relayPort()
	}
	return st, !st.lastSeen.IsZero() && time.Since(st.lastSeen) < relayOnlineWindow
}

// relayOnline 常驻中继是否在线。
func (p *Panel) relayOnline() bool {
	_, on := p.relayStatus()
	return on
}

// traeRelayPing 中继心跳（走鉴权：安装命令里带的是面板 api_key）。
//
// 用 api_key 而不是另发一个 token：用户本就要拿 api_key 进面板，命令里多带一个
// 密钥只会再多一个可能泄露/过期的东西；api_key 轮换时重跑一次安装命令即可。
func (p *Panel) traeRelayPing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port int    `json:"port"`
		Host string `json:"host"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<13)).Decode(&body)
	}
	port := body.Port
	if port < 1024 || port > 65535 {
		port = p.relayPort()
	}
	p.relayMu.Lock()
	p.relay = &traeRelayState{port: port, host: strings.TrimSpace(body.Host), lastSeen: time.Now()}
	p.relayMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "port": port})
}

// traeRelayStatusHandler 返回中继状态 + 一键安装命令（面板"常驻中继"区块用）。
//
// 同时给出 Windows(PowerShell) 与 Linux/macOS(shell) 两套安装命令，前端按当前
// 平台默认选中一套、另一套折叠，避免用户拿到不匹配自己系统的命令。
func (p *Panel) traeRelayStatusHandler(w http.ResponseWriter, r *http.Request) {
	st, online := p.relayStatus()
	// 中继在线时以它**上报的**端口为准：端口被占时脚本会自己换一个高位端口
	// （见 traeRelayScriptPS1 的候选端口循环），面板必须跟着它走，否则展示的
	// 端口和 login/start 真正塞进 auth_callback_url 的端口会对不上。
	port := p.relayPort()
	if online && st.port >= 1024 && st.port <= 65535 {
		port = st.port
	}
	base := p.publicBase(r)
	target := base + "/panel/api/login/trae/callback"
	ping := base + "/panel/api/login/trae/relay/ping"
	key := p.apiKey()

	last := ""
	if !st.lastSeen.IsZero() {
		last = st.lastSeen.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"port":       port,
		"online":     online,
		"last_seen":  last,
		"relay_host": st.host,
		"install_ps": traeRelayInstallPS1(p.relayPort(), target, ping, key),
		"install_sh": traeRelayInstallSH(p.relayPort(), target, ping, key),
		"target":     target,
	})
}

// traeRelayScriptHandler 下载中继脚本本体（备用通道：不想整条命令粘贴时直接下载跑）。
//
//	?os=unix → python3 版（Linux/macOS）
//	缺省     → PowerShell 版（Windows）
func (p *Panel) traeRelayScriptHandler(w http.ResponseWriter, r *http.Request) {
	base := p.publicBase(r)
	target := base + "/panel/api/login/trae/callback"
	ping := base + "/panel/api/login/trae/relay/ping"
	port, key := p.relayPort(), p.apiKey()

	name, body := "wb2api-trae-relay.ps1", traeRelayScriptPS1(port, target, ping, key)
	if strings.EqualFold(r.URL.Query().Get("os"), "unix") {
		name, body = "wb2api-trae-relay.py", traeRelayScriptPY(port, target, ping, key)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	_, _ = io.WriteString(w, body)
}

// ---- 脚本生成 ----
//
// 说明：两套脚本都刻意避开 HTTP 框架的监听类 API。Windows 上 HttpListener 监听
// http://127.0.0.1:PORT/ 在普通用户下会抛"拒绝访问"（要 netsh http add urlacl 或
// 管理员），而登录是日常操作，不该要求用户提权。直接开 TcpListener 不需要任何
// ACL 配置，普通权限即可，代价只是自己拼几十行 HTTP 响应头。
//
// 另一条同样的约束：脚本内容一律走 base64 内联，不做 here-string / 多行引号拼接。
// 安装命令必须"复制过去就能跑"，不能依赖不同终端对多行文本、反引号、$ 的转义差异。

// psQuote 转义 PowerShell 单引号字符串里的单引号。
func psQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }

// traeRelayScriptPS1 Windows 常驻中继（PowerShell + TcpListener）。
//
// 【为什么正文必须全 ASCII】
//
// PowerShell 5.1 在 .ps1 没有 BOM 时按 **ANSI 码页**读取源码：一段 UTF-8 中文
// 注释会被解码成一串乱码字节，进而把解析器带崩（实测报
// "relay.ps1:13 char:1 Unexpected token '}'"）。中继脚本是落盘后由
// powershell -File 加载的，所以脚本正文一律只写 ASCII；给用户看的"授权成功"
// 页面（含中文）改成 base64 内联，运行时 FromBase64String 还原。落盘那一侧再补
// UTF-8 BOM 双保险。
//
// 全程不使用反引号（避免与 Go 原始字符串字面量冲突），CRLF 用 [char]13+[char]10 拼。
func traeRelayScriptPS1(port int, target, ping, key string) string {
	pageb64 := base64.StdEncoding.EncodeToString([]byte(
		`<!doctype html><meta charset="utf-8"><title>登录完成</title>` +
			`<body style="font:15px system-ui;padding:48px;text-align:center">` +
			`<h2>授权成功</h2><p>可以关闭本页面，返回面板等待账号自动载入。</p></body>`))
	return fmt.Sprintf(`# wb2api Trae loopback relay (generated by the panel, port %d).
# Catches the callback that Trae's authorize page throws at 127.0.0.1:%d,
# forwards the raw query to the gateway and answers with a "done" page.
# Body is ASCII-only on purpose (see trae_relay.go); the reply page is base64.
$ErrorActionPreference = 'SilentlyContinue'
$port   = %d
$target = '%s'
$ping   = '%s'
$key    = '%s'
$page   = '%s'
$crlf   = [string][char]13 + [string][char]10

function Send-Beat {
  # Heartbeat: the panel uses it to tell whether this machine runs the relay,
  # and pins the Trae callback port to the port reported here.
  try {
    $b = '{"port":' + $port + ',"host":"' + $env:COMPUTERNAME + '"}'
    Invoke-RestMethod -Method POST -Uri $ping -Body $b -ContentType 'application/json' -Headers @{ Authorization = 'Bearer ' + $key } -TimeoutSec 8 | Out-Null
  } catch {}
}

# Bind: try the configured port first, then any free high port. Fallback keeps
# working when the configured port is already taken on this machine, because
# the panel follows whatever port the heartbeat reports.
$listener = $null
$cand = @($port)
$rnd = New-Object Random
for ($i = 0; $i -lt 20; $i++) { $cand += $rnd.Next(40000, 60000) }
foreach ($p in $cand) {
  try {
    $listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, $p)
    $listener.Start()
    $port = $p
    break
  } catch { $listener = $null; Start-Sleep -Milliseconds 200 }
}
if (-not $listener) { exit 1 }

Send-Beat
$lastBeat = Get-Date
while ($true) {
  try {
    if ($listener.Pending()) {
      $client = $listener.AcceptTcpClient()
      $stream = $client.GetStream()
      # Read timeout: Chrome opens preconnect sockets, and a connection that
      # never sends a request line must not block this single-threaded loop.
      $stream.ReadTimeout = 8000
      $reader = New-Object IO.StreamReader($stream)
      $line   = $reader.ReadLine()
      for ($h = 0; $h -lt 100; $h++) { $hd = $reader.ReadLine(); if (-not $hd) { break } }

      $path = ''
      if ($line) { $parts = $line.Split(' '); if ($parts.Length -ge 2) { $path = $parts[1] } }
      $q = ''
      $idx = $path.IndexOf('?')
      if ($idx -ge 0) { $q = $path.Substring($idx + 1) }

      if ($q) {
        # Forward the raw query verbatim: authCodeInfo is a percent-encoded JSON
        # blob, and re-encoding it once more is the classic way to corrupt it.
        try { Invoke-WebRequest -UseBasicParsing -Method POST -Uri $target -Body $q -ContentType 'text/plain' -TimeoutSec 20 | Out-Null } catch {}
      }

      $bytes = [Convert]::FromBase64String($page)
      $head  = 'HTTP/1.1 200 OK' + $crlf + 'Content-Type: text/html; charset=utf-8' + $crlf + 'Content-Length: ' + $bytes.Length + $crlf + 'Connection: close' + $crlf + $crlf
      $hb    = [Text.Encoding]::ASCII.GetBytes($head)
      $stream.Write($hb, 0, $hb.Length)
      $stream.Write($bytes, 0, $bytes.Length)
      $stream.Flush()
      $client.Close()
    } else {
      Start-Sleep -Milliseconds 300
      if (((Get-Date) - $lastBeat).TotalSeconds -ge 15) { Send-Beat; $lastBeat = Get-Date }
    }
  } catch { Start-Sleep -Milliseconds 500 }
}
`, port, port, port, psQuote(target), psQuote(ping), psQuote(key), pageb64)
}

// traeRelayInstallPS1 Windows 一键安装命令（用户复制到 PowerShell 执行一次）。
//
// 做三件事：脚本落盘到 %LOCALAPPDATA%、写一个开机自启的启动器到"启动"文件夹、
// 立刻在后台拉起一份。之后开机自动运行，用户不需要再管。
//
// 【同样必须全 ASCII】这条命令是**粘贴进终端**执行的，用户也可能先存成 .ps1。
// 终端粘贴走的是 UTF-16 内存字符串，但存文件再跑就会碰到 PS 5.1 的 ANSI 解码
// 问题（实测中文提示会变成一串乱码并把字符串引号吞掉，报
// "The string is missing the terminator"）。所以命令体一律 ASCII，
// 给用户看的中文提示用 base64 内联、运行时 UTF8 解码。
//
// 启动器 .cmd 用 -Encoding Default（ANSI）而不是 ASCII：%LOCALAPPDATA% 里
// 可能含中文用户名，ASCII 编码会把它写成 '?'，批处理就找不到脚本了。
func traeRelayInstallPS1(port int, target, ping, key string) string {
	// 落盘时前置 UTF-8 BOM（EF BB BF）：PowerShell 5.1 遇到无 BOM 的 .ps1 会按
	// 系统 ANSI 码页解析。脚本正文本身已是纯 ASCII，BOM 是双保险，也让用户在
	// 记事本/编辑器里打开时中文不乱码。
	raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte(traeRelayScriptPS1(port, target, ping, key))...)
	b64 := base64.StdEncoding.EncodeToString(raw)
	// relay.py 那一侧（Linux/macOS）的收尾提示，同样避开编码问题。
	msg := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(
		"wb2api Trae relay installed and started (port %d, autostart enabled). "+
			"Nothing else to do when logging into Trae from now on.", port)))
	return fmt.Sprintf(`$d="$env:LOCALAPPDATA\wb2api-trae-relay";New-Item -ItemType Directory -Force -Path $d|Out-Null;[IO.File]::WriteAllBytes("$d\relay.ps1",[Convert]::FromBase64String('%s'));$lnk=[Environment]::GetFolderPath('Startup')+'\wb2api-trae-relay.cmd';Set-Content -Encoding Default -Path $lnk -Value ('powershell -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "'+$d+'\relay.ps1"');Start-Process powershell -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-WindowStyle','Hidden','-File',"$d\relay.ps1" -WindowStyle Hidden;Start-Sleep -Seconds 2;Write-Host ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s')))`, b64, msg)
}

// traeRelayScriptPY Linux/macOS 常驻中继（python3 标准库，无第三方依赖）。
//
// 为什么用 python3 而不是 bash+nc：nc 各发行版参数不兼容（-l/-p/-k 五花八门），
// 也做不了心跳；python3 在主流发行版与 macOS 自带，稳定且能一并实现心跳与并发。
func traeRelayScriptPY(port int, target, ping, key string) string {
	return fmt.Sprintf(`#!/usr/bin/env python3
"""wb2api Trae 常驻回环中继（由面板生成，端口 %d）。

接住 Trae 授权页甩到 127.0.0.1:%d 的回调，原样转发给网关并回"授权成功"页。
"""
import http.server
import json
import socket
import threading
import time
import urllib.request

PORT = %d
TARGET = %q
PING = %q
KEY = %q


def beat():
    """每 15s 上报一次，面板据此把回调端口自动固定为本端口。"""
    while True:
        try:
            data = json.dumps({"port": PORT, "host": socket.gethostname()}).encode()
            req = urllib.request.Request(
                PING, data=data, method="POST",
                headers={"Content-Type": "application/json",
                         "Authorization": "Bearer " + KEY})
            urllib.request.urlopen(req, timeout=8).read()
        except Exception:
            pass
        time.sleep(15)


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        i = self.path.find("?")
        q = self.path[i + 1:] if i >= 0 else ""
        if q:
            # 原样 POST 原始 query，避免对 percent-encoded 的 authCodeInfo 二次转义。
            try:
                req = urllib.request.Request(
                    TARGET, data=q.encode(), method="POST",
                    headers={"Content-Type": "text/plain"})
                urllib.request.urlopen(req, timeout=15).read()
            except Exception:
                pass
        body = ('<!doctype html><meta charset="utf-8"><title>登录完成</title>'
                '<body style="font:15px system-ui;padding:48px;text-align:center">'
                '<h2>授权成功</h2><p>可以关闭本页面，返回面板等待账号自动载入。</p>'
                '</body>').encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    threading.Thread(target=beat, daemon=True).start()
    http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
`, port, port, port, target, ping, key)
}

// traeRelayInstallSH Linux/macOS 一键安装命令。
//
// 落盘到 ~/.local/share/wb2api-trae-relay/relay.py，写 @reboot 的 crontab 保证
// 开机自启（不依赖 systemd —— 桌面发行版 / 容器 / 无 systemd 环境同样可用），
// 然后立即在后台拉起一份。
func traeRelayInstallSH(port int, target, ping, key string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(traeRelayScriptPY(port, target, ping, key)))
	msg := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(
		"wb2api Trae relay installed and started (port %d, @reboot enabled). "+
			"Nothing else to do when logging into Trae from now on.", port)))
	return fmt.Sprintf(`mkdir -p "$HOME/.local/share/wb2api-trae-relay";printf '%%s' '%s' | base64 -d > "$HOME/.local/share/wb2api-trae-relay/relay.py";chmod +x "$HOME/.local/share/wb2api-trae-relay/relay.py";(crontab -l 2>/dev/null | grep -v wb2api-trae-relay; echo '@reboot nohup python3 "$HOME/.local/share/wb2api-trae-relay/relay.py" >/dev/null 2>&1 &') | crontab -;nohup python3 "$HOME/.local/share/wb2api-trae-relay/relay.py" >/dev/null 2>&1 & sleep 1;printf '%%s' '%s' | base64 -d;echo`, b64, msg)
}
