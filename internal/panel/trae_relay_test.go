package panel

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newRelayTestPanel 造一个只挂了中继相关路由的最小面板。
func newRelayTestPanel() (*Panel, http.Handler) {
	p := New(Config{APIKey: "k", PublicBaseURL: "http://example.test:7888"})
	return p, p.mux
}

func doRelayJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// 未收到心跳时仍返回配置端口，并给出非空安装命令。
func TestTraeRelayStatusDefaultsToConfiguredPort(t *testing.T) {
	_, h := newRelayTestPanel()
	w := doRelayJSON(t, h, http.MethodGet, "/panel/api/login/trae/relay/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"port":17654`) {
		t.Fatalf("expected default port in %s", body)
	}
	if !strings.Contains(body, `"online":false`) {
		t.Fatalf("expected online false in %s", body)
	}
	if !strings.Contains(body, "FromBase64String") {
		t.Fatalf("install_ps missing/empty in %s", body)
	}
}

// TraeRelayPort 的越界回落。
func TestTraeRelayPortFallback(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, 17654}, {-1, 17654}, {80, 17654}, {70000, 17654},
		{1024, 1024}, {17654, 17654}, {65535, 65535},
	} {
		p := New(Config{TraeRelayPort: tc.in})
		if got := p.relayPort(); got != tc.want {
			t.Errorf("relayPort(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// 心跳端口是权威值：脚本在端口被占时会自己换端口，status 与 start 都要跟着走。
func TestTraeRelayPingPortWins(t *testing.T) {
	_, h := newRelayTestPanel()
	w := doRelayJSON(t, h, http.MethodPost, "/panel/api/login/trae/relay/ping",
		`{"port":47123,"host":"box"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ping status = %d, body=%s", w.Code, w.Body.String())
	}

	st := doRelayJSON(t, h, http.MethodGet, "/panel/api/login/trae/relay/status", "")
	sb := st.Body.String()
	if !strings.Contains(sb, `"port":47123`) {
		t.Fatalf("status should follow reported port: %s", sb)
	}
	if !strings.Contains(sb, `"online":true`) {
		t.Fatalf("expected online true: %s", sb)
	}
	// 安装命令仍按**配置端口**生成（脚本体是 base64 内联，解开再断言）：
	// 它描述的是"装完之后监听哪个端口"，与"这次回调用哪个端口"是两回事。
	got := decodeInstallPSPort(t, sb)
	if got != 17654 {
		t.Fatalf("install_ps should use configured port, got %d", got)
	}

	start := doRelayJSON(t, h, http.MethodPost, "/panel/api/login/trae/start", "{}")
	tb := start.Body.String()
	if !strings.Contains(tb, `"callback_port":47123`) {
		t.Fatalf("start should use reported port: %s", tb)
	}
	if !strings.Contains(tb, `"relay_online":true`) {
		t.Fatalf("start should report relay_online: %s", tb)
	}
	if !strings.Contains(tb, "127.0.0.1%3A47123%2Fauthorize") {
		t.Fatalf("auth url should carry the relay callback: %s", tb)
	}
	if !strings.Contains(tb, `"relay_ps":""`) {
		t.Fatalf("relay_ps should be empty while relay is online: %s", tb)
	}
}

// decodeInstallPSPort 从 status 响应的 install_ps 里解出脚本体，并取出 $port。
func decodeInstallPSPort(t *testing.T, statusBody string) int {
	t.Helper()
	const tag = "FromBase64String('"
	i := strings.Index(statusBody, tag)
	if i < 0 {
		t.Fatalf("install_ps has no base64 payload")
	}
	rest := statusBody[i+len(tag):]
	j := strings.Index(rest, "'")
	if j < 0 {
		t.Fatalf("install_ps base64 payload not terminated")
	}
	// JSON 里 \\/ 之类的转义与本串无关（base64 只用 A-Za-z0-9+/=），可直接解码。
	raw, err := base64.StdEncoding.DecodeString(rest[:j])
	if err != nil {
		t.Fatalf("decode install_ps: %v", err)
	}
	body := string(raw)
	k := strings.Index(body, "$port   = ")
	if k < 0 {
		t.Fatalf("script has no $port line")
	}
	tail := body[k+len("$port   = "):]
	n := 0
	for _, c := range tail {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// 心跳端口的越界值回落配置端口。
func TestTraeRelayPingRejectsBadPort(t *testing.T) {
	_, h := newRelayTestPanel()
	doRelayJSON(t, h, http.MethodPost, "/panel/api/login/trae/relay/ping", `{"port":7}`)
	st := doRelayJSON(t, h, http.MethodGet, "/panel/api/login/trae/relay/status", "")
	if !strings.Contains(st.Body.String(), `"port":17654`) {
		t.Fatalf("bad port should fall back: %s", st.Body.String())
	}
}

// 脚本与安装命令必须是纯 ASCII：PS 5.1 按 ANSI 码页解析无 BOM 的 .ps1，
// 混入中文会把解析器带崩（实测 Unexpected token / missing the terminator）。
func TestTraeRelayScriptsAreASCII(t *testing.T) {
	ps := traeRelayScriptPS1(17654, "http://t/cb", "http://t/ping", "k")
	if strings.ContainsFunc(ps, func(r rune) bool { return r > 127 }) {
		t.Fatalf("ps1 script must be ASCII-only")
	}
	if !strings.Contains(ps, "= 'PCFkb2N0eXBl") {
		t.Fatalf("ps1 should embed the reply page as base64")
	}
	inst := traeRelayInstallPS1(17654, "http://t/cb", "http://t/ping", "k")
	if strings.ContainsFunc(inst, func(r rune) bool { return r > 127 }) {
		t.Fatalf("ps install command must be ASCII-only")
	}
	sh := traeRelayInstallSH(17654, "http://t/cb", "http://t/ping", "k")
	if strings.ContainsFunc(sh, func(r rune) bool { return r > 127 }) {
		t.Fatalf("sh install command must be ASCII-only")
	}
}

// 备用下载通道：?os=unix 走 python 版，target 用 %q 引用。
func TestTraeRelayScriptHandler(t *testing.T) {
	_, h := newRelayTestPanel()
	w := doRelayJSON(t, h, http.MethodGet, "/panel/api/login/trae/relay/script?os=unix", "")
	if w.Code != http.StatusOK {
		t.Fatalf("script status = %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "wb2api-trae-relay.py") {
		t.Fatalf("unexpected disposition: %s", w.Header().Get("Content-Disposition"))
	}
	if !strings.Contains(w.Body.String(), "http.server") {
		t.Fatalf("expected python relay body")
	}
	py := traeRelayScriptPY(1, "http://t/a'b", "http://t/ping", "k")
	if !strings.Contains(py, "TARGET = ") {
		t.Fatalf("python target should be quoted")
	}
}
