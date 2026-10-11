package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// soloHeaders 写入 DLL 模型链（llm_utils_chat）实际使用的头矩阵。
// 与 /api/ide/v1/chat 的 commonHeaders 不同：SOLO 链走独立的 IDE 版本
// （0.1.52/20260811）与完整的风控头（X-Uid / X-App-Version / X-OS-Version 等），
// 缺任一头上游都会拒绝（实测 400/401）。来源：SOLO 客户端抓包，
// 见 docs/TRAECODE_BUILTIN_MODELS_OPENAI_PROXY.md 的「DLL 对话请求头矩阵」。
func (c *Client) soloHeaders(h http.Header) {
	h.Set("Accept", "text/event-stream")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "Trae/"+SoloIDEVersion)
	tok := c.a.TokenValue()
	h.Set("Authorization", "Cloud-IDE-JWT "+tok)
	h.Set("X-Cloudide-Token", tok)
	h.Set("X-Ide-Token", tok)
	h.Set("X-Uid", c.a.UID)
	h.Set("X-App-Id", AppID)
	h.Set("X-App-Version", "default")
	h.Set("X-Ide-Version", SoloIDEVersion)
	h.Set("X-Ide-Version-Code", SoloIDEVersionCode)
	h.Set("X-App-Version-Code", SoloIDEVersionCode)
	h.Set("X-Ide-Version-Type", "stable")
	h.Set("X-Device-Type", "windows")
	h.Set("X-OS-Version", "Windows 11 Pro")
	h.Set("X-Device-Brand", "83DG")
	h.Set("Request-Traffic-Type", "prod")
	if c.a.DeviceID != "" {
		h.Set("X-Device-Id", c.a.DeviceID)
	}
	if mid := c.a.MachineIDValue(); mid != "" {
		h.Set("X-Machine-Id", mid)
	}
}

// SoloChatStream 发起 DLL 内置模型链的 SSE 请求（POST /api/agent/v3/llm_utils_chat）。
// 返回通道语义与 ChatStream 相同：dataCh 逐行送出 event:/data: 两行，errCh 报传输错误。
func (c *Client) SoloChatStream(ctx context.Context, body []byte, extraHeaders map[string]string) (<-chan string, <-chan error, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AgentDomain+PathSoloChat, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	c.soloHeaders(req.Header)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		resp.Body.Close()
		return nil, nil, fmt.Errorf("trae solo chat http %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	dataCh := make(chan string, 64)
	errCh := make(chan error, 1)
	go func() {
		defer close(dataCh)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "event:") {
				dataCh <- line
			}
			if ctx.Err() != nil {
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			errCh <- err
		}
	}()
	return dataCh, errCh, nil
}

// doSoloJSON 发起同一套 SOLO 头矩阵的非流式请求。
// get_detail_param 与 llm_utils_chat 属于同一条 DLL 链；使用通用
// 3.3.96 头会得到 400，而不会返回可解析的模型目录。
func (c *Client) doSoloJSON(ctx context.Context, method, url string, body any) (int, []byte, error) {
	rawBody, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(rawBody))
	if err != nil {
		return 0, nil, err
	}
	c.soloHeaders(req.Header)
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// DetailParam 拉取 DLL 模型配置清单（POST /api/ide/v1/get_detail_param）。
// 响应 config_info_list[].config_name 是当前账号可用的 DLL 内置模型名全集
// （glm-5.3-flashx / kimi-k3 / qwen3.8-max 等），与 /api/ide/v1/model_list
// 的四个平台模型互不重叠。请求体为固定形状（实测缺字段会 400）。
func (c *Client) DetailParam(ctx context.Context) (json.RawMessage, error) {
	body := map[string]any{
		"function":            SoloFunction,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	st, raw, err := c.doSoloJSON(ctx, http.MethodPost, AgentDomain+PathDetailParam, body)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("trae detail_param http %d: %s", st, truncate(string(raw), 300))
	}
	return json.RawMessage(raw), nil
}
