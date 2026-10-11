package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// zcodeCredentialAuto 处理「ZCode 凭据自动获取 / 批量写入 / 自动接入」。
//
// 这是面板上被问最多的那个问题的答案：「每加一个 ZCode 号都要手抄一次
// <keyId>.<secret> 吗？」——不必。本端点有三种来源，优先级由高到低：
//
//  1. 用户粘的文本           body.text   任意层级内嵌 "<keyId>.<secret>" 都能认
//  2. 服务端本机自动获取      body.auto   读本机 ZCode 客户端：
//     通道A 服务端换 key：getCustomerInfo -> api_keys -> api_keys/copy
//     通道B 读客户端成品： %USERPROFILE%\.zcode\v2\config.json 的 apiKey
//  3. 自动接入新账号         body.adopt  用本机客户端解出的 uid 建一个 ZCode 号
//     （等价于点一次「ZCode 浏览器登录」，无需打开浏览器）
//
// 全部请求体（都可省略）：
//
//	{
//	  "text":     "<整份 config.json / 裸 <keyId>.<secret> / 日志片段>",
//	  "uids":     ["<只写这几个号>"],     // 省略=池里所有 ZCode 号
//	  "dry_run":  true,                    // 只识别不写入
//	  "auto":     true,                    // 强制走本机客户端自动获取
//	  "adopt":    true,                    // 本机遇到新账号时自动建号入池
//	  "validate": true,                    // 写入后用真上游对话校验一次
//	  "scope":    "all"                    // 强制写满全池（默认：凭据归属 uid 已知时只写该号，防串号）
//	}
//
// 返回里 origin 说明凭据来自哪条通道，results 逐号给出 ok/msg。
func (p *Panel) zcodeCredentialAuto(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string   `json:"text"`
		UIDs     []string `json:"uids"`
		DryRun   bool     `json:"dry_run"`
		Auto     bool     `json:"auto"`
		Adopt    bool     `json:"adopt"`
		Validate bool     `json:"validate"`
		// Scope="all" 强制写满池里所有缺凭据的号（单账号场景的便利）。
		// 留空时：凭据归属 uid 已知 → 只写该号；未知（裸 <keyId>.<secret>）→ 写满缺口。
		Scope string `json:"scope"`
		// Force=true 用最新文本覆盖已有凭据（供「本机→服务器」推送在 secret 轮换后使用）。
		Force bool `json:"force"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	}

	// 1) 先解析用户粘的文本
	credFromText := auth.ExtractSignCredentialFromText(body.Text)
	origin := "text"
	if credFromText == "" {
		origin = ""
	}

	// 2) 走本机客户端自动获取的时机：
	//    - 用户**没粘文本**（最常见：新号/一键补齐，直接调本端点即可），或
	//    - 明确传了 "auto": true
	//    粘了文本但没认出来时不偷偷回退到本机客户端——那样会让
	//    「我粘错了」被静默吞掉；这种情况应当明确回 error+hint。
	var autoInfo *zcode.LocalKeyResult
	autoErr := ""
	wantAuto := body.Auto || strings.TrimSpace(body.Text) == ""
	if wantAuto {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		hc := &http.Client{Timeout: 45 * time.Second}
		res, err := zcode.EnsureLocalClientAPIKey(ctx, hc)
		if err == nil && res != nil {
			if res.Credential != "" && credFromText == "" {
				credFromText = res.Credential
				origin = "client:" + res.Source
			}
			autoInfo = res
		} else if err != nil {
			autoErr = err.Error()
		}
	}

	autoPayload := map[string]any{}
	if autoInfo != nil {
		autoPayload = map[string]any{
			"local_available": auth.ZCodeLocalClientAvailable(),
			"has_jwt":         autoInfo.Info.JWToken != "",
			"uid":             autoInfo.Info.UID,
			"provider":        autoInfo.Info.Provider,
			"sources":         autoInfo.Info.Sources,
			"channel_err":     autoInfo.ChannelErr,
		}
	} else if autoErr != "" {
		autoPayload = map[string]any{"error": autoErr}
	}

	// 归属 uid：本机客户端解出的，或调用方显式指定。
	ownerUID := ""
	if autoInfo != nil {
		ownerUID = strings.TrimSpace(autoInfo.Info.UID)
	}
	if len(body.UIDs) == 1 {
		ownerUID = strings.TrimSpace(body.UIDs[0])
	}

	// 目标账号计算（含「防串号」）：
	//   - 显式 uids  ：只写这些号（最高优先，调用方自负）
	//   - scope=all  ：写满池里所有缺凭据的号（单账号便利，显式要求）
	//   - 归属 uid 已知（本机客户端/单 uid）：只写该号 —— 绝不把甲的 key 写到乙身上
	//   - 归属未知（裸 <keyId>.<secret>）：写满缺口（此时 key 无归属信息，补全即预期）
	targets := p.zcodeCredentialTargets(body.UIDs)
	scopedToOwner := false
	if len(body.UIDs) == 0 && !strings.EqualFold(strings.TrimSpace(body.Scope), "all") && ownerUID != "" {
		scopedToOwner = true
		if a := p.cfg.Pool.AuthByUID(ownerUID); a != nil &&
			auth.NormalizeProvider(a.ProviderValue()) == auth.ProviderZCode {
			targets = []*auth.Auth{a}
		} else {
			targets = nil // 归属号还没入池：交给 adopt / 显式 uids 处理，不乱写他人
		}
	}

	// 3) 干跑：只识别不写入
	if body.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"dry_run":        true,
			"parsed":         credFromText != "",
			"origin":         origin,
			"key_id":         keyIDOf(credFromText),
			"preview":        previewCredential(credFromText),
			"targets":        len(targets),
			"would_apply_to": uidList(targets),
			"auto":           autoPayload,
		})
		return
	}

	// 4) 一个字都没有：给出可操作提示（并带上本机客户端诊断）
	if credFromText == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "没有识别到 ZCode 凭据",
			"hint": "三条路任选：① 服务端与 ZCode 客户端同机时直接调 {\"auto\":true}；" +
				"② 把 %USERPROFILE%\\.zcode\\v2\\config.json 整个粘到输入框；" +
				"③ 直接粘 <keyId>.<secret>",
			"targets": len(targets),
			"auto":    autoPayload,
			"results": []any{},
		})
		return
	}

	// 5) 逐号写入
	keyID := keyIDOf(credFromText)
	// force：来自本机客户端的凭据总是最新（含 secret 轮换）→ 覆盖；
	//       用户手粘的文本不确定时效 → 不覆盖已有凭据。
	force := strings.HasPrefix(origin, "client:") || body.Force
	results := p.zcodeCredentialWrite(targets, credFromText, keyID, origin == "text", force)

	// 6) 可选：写入后用真上游对话校验一次（验签链路走不通时能立刻发现）
	validated := ""
	if body.Validate {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		hc := &http.Client{Timeout: 45 * time.Second}
		if err := zcode.ValidateAPIKey(ctx, hc, credFromText, ""); err != nil {
			validated = "FAIL: " + err.Error()
		} else {
			validated = "OK"
		}
	}

	// 7) 可选：把本机客户端遇到的新账号自动建号入池
	adopted := map[string]any{}
	if body.Adopt && autoInfo != nil && autoInfo.Info.UID != "" {
		if a, created := p.zcodeCredentialAdopt(autoInfo.Info, credFromText, keyID); created {
			adopted = map[string]any{"uid": a.UID, "created": true, "source": a.Source}
		} else {
			adopted = map[string]any{"uid": autoInfo.Info.UID, "created": false, "reason": "账号已在池里"}
		}
	}

	done, wrote, updated := 0, 0, 0
	for _, it := range results {
		if it.OK {
			done++
		}
		if it.Wrote {
			wrote++
			if it.HadCred {
				updated++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(results), "succeeded": done,
		"wrote": wrote, "updated": updated,
		"origin": origin, "key_id": keyID, "preview": previewCredential(credFromText),
		"from_local_client": strings.HasPrefix(origin, "client:"),
		"scoped_to_owner":   scopedToOwner, "owner_uid": ownerUID,
		"validated": validated,
		"adopted":   adopted,
		"auto":      autoPayload,
		"results":   results,
	})
}

// zcodeCredentialItem 是逐号写入结果。
type zcodeCredentialItem struct {
	UID      string `json:"uid"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	KeyID    string `json:"key_id,omitempty"`
	Wrote    bool   `json:"wrote"`
	HadCred  bool   `json:"had_credential"`
	FromText bool   `json:"from_text"`
}

// zcodeCredentialTargets 解析要写入的目标账号：给 uids 就只写这些，
// 否则写池里所有 ZCode 号。
func (p *Panel) zcodeCredentialTargets(uids []string) []*auth.Auth {
	var targets []*auth.Auth
	appendIfZCode := func(a *auth.Auth) {
		if a == nil {
			return
		}
		if auth.NormalizeProvider(a.ProviderValue()) == auth.ProviderZCode {
			targets = append(targets, a)
		}
	}
	if len(uids) > 0 {
		for _, uid := range uids {
			appendIfZCode(p.cfg.Pool.AuthByUID(strings.TrimSpace(uid)))
		}
		return targets
	}
	for _, st := range p.cfg.Pool.List() {
		appendIfZCode(p.cfg.Pool.AuthByUID(st.UID))
	}
	return targets
}

// zcodeCredentialWrite 把凭据逐号写入并落盘。
//
// 已有凭据（SignCredential 与 APIKey 都在）的号跳过，不覆盖，
// 避免把甲号换成乙号的 key（多号场景的安全默认）。
func (p *Panel) zcodeCredentialWrite(targets []*auth.Auth, cred, keyID string, fromText, force bool) []zcodeCredentialItem {
	results := make([]zcodeCredentialItem, 0, len(targets))
	for _, a := range targets {
		it := zcodeCredentialItem{UID: a.UID, KeyID: keyID, FromText: fromText}
		cur := a.SignCredential()
		it.HadCred = cur != ""
		// force=true 用于 secret 轮换：即便已有凭据，也用最新值覆盖。
		// 非 force 时，已有凭据一律跳过（避免用旧文本覆盖更可信的实时值）。
		if !force && it.HadCred && a.APIKeyValue() != "" {
			it.OK = true
			it.Message = "已有凭据，跳过"
			p.cfg.Pool.Revive(a.UID)
			results = append(results, it)
			continue
		}
		if force && cur == cred && a.APIKeyValue() == cred {
			it.OK = true
			it.Message = "凭据一致，无需更新"
			// 凭据本来就对，但号可能因旧 401 仍在冷却/禁用 -> 同样解冻。
			p.cfg.Pool.Revive(a.UID)
			results = append(results, it)
			continue
		}
		a.SetSignCredential(cred)
		a.SetAPIKey(cred)
		if err := a.SaveAtomic(); err != nil {
			it.OK = false
			it.Message = "写入失败: " + err.Error()
			results = append(results, it)
			continue
		}
		it.OK = true
		it.Wrote = true
		if it.HadCred && force {
			it.Message = "已更新（secret 轮换）"
		} else {
			it.Message = "已写入"
		}
		// 换了新凭据后立刻解冻：账号可能因旧 key 401 已被冷却，
		// 不复活就会「key 已修好但对话仍 503」。
		p.cfg.Pool.Revive(a.UID)
		log.Printf("panel: ZCode 凭据已写入 uid=%s keyId=%s force=%v", a.UID, keyID, force)
		results = append(results, it)
	}
	return results
}

// zcodeCredentialAdopt 用本机客户端解出的信息，为一个尚未入池的 ZCode 账号建号。
//
// 这就是「自动接入」——等同于走一遍面板的「ZCode 浏览器登录」，但不需要浏览器：
// uid 来自本机客户端，jwt 来自本机客户端，签名凭据来自刚解出的 key。
// 返回 (账号, 是否新建)。
func (p *Panel) zcodeCredentialAdopt(info auth.LocalClientInfo, cred, keyID string) (*auth.Auth, bool) {
	uid := strings.TrimSpace(info.UID)
	if uid == "" {
		return nil, false
	}
	if p.cfg.Pool.AuthByUID(uid) != nil {
		return nil, false
	}
	source := strings.TrimSpace(info.Provider)
	if source == "" {
		source = "bigmodel"
	}
	a := &auth.Auth{
		Provider:    auth.ProviderZCode,
		Mode:        auth.ModeJWT,
		AccessToken: info.JWToken,
		UID:         uid,
		Username:    uid,
		Nickname:    uid,
		Region:      "cn",
		Source:      source,
		DeviceID:    randomUUID(),
		Remark:      "panel-auto-adopt",
		LastRefresh: time.Now().Unix(),
		FilePath:    filepath.Join(p.cfg.AuthDir, fmt.Sprintf("zcode-%s.json", uid)),
	}
	a.SetSignCredential(cred)
	a.SetAPIKey(cred)
	if err := p.persistUpstream(a); err != nil {
		log.Printf("panel: ZCode 自动接入失败 uid=%s: %v", uid, err)
		return nil, false
	}
	log.Printf("panel: ZCode 账号已自动接入 uid=%s source=%s keyId=%s", uid, source, keyID)
	return a, true
}

// errNoLocalClientCredential 保留旧的错误名（其它文件/注释仍可能引用语义）。
var errNoLocalClientCredential = errors.New("panel: no local ZCode client credential")

func keyIDOf(cred string) string {
	if i := strings.IndexByte(cred, '.'); i > 0 {
		return cred[:i]
	}
	return ""
}

func previewCredential(cred string) string {
	if cred == "" {
		return ""
	}
	i := strings.IndexByte(cred, '.')
	if i <= 0 {
		return ""
	}
	head := cred[:i]
	if len(head) > 8 {
		head = head[:8]
	}
	tail := cred[i+1:]
	if len(tail) > 4 {
		tail = tail[:4]
	}
	return head + "..." + tail
}

func uidList(as []*auth.Auth) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.UID)
	}
	return out
}

// zcodeCredentialStatus 列出池里所有 ZCode 号的凭据状态。
//
// 为什么需要它：ZCode 的 anthropic 与 completions 两条端点都只认
// <keyId>.<secret>，只有 zcodejwt 的号对话不可用（zcode-plan 实测 405/3012）。
// 这里让面板一眼看到缺口，并额外报告服务端本机客户端是否可作为自动来源。
func (p *Panel) zcodeCredentialStatus(w http.ResponseWriter, r *http.Request) {
	type row struct {
		UID       string `json:"uid"`
		Nickname  string `json:"nickname"`
		Source    string `json:"source"`
		HasKey    bool   `json:"has_key"`
		KeyID     string `json:"key_id,omitempty"`
		Preview   string `json:"preview,omitempty"`
		HasJWT    bool   `json:"has_jwt"`
		Upstream  string `json:"upstream"`
		Transport string `json:"transport"`
	}
	var rows []row
	missing := 0
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || auth.NormalizeProvider(a.ProviderValue()) != auth.ProviderZCode {
			continue
		}
		cred := a.SignCredential()
		if cred == "" {
			cred = a.APIKeyValue()
		}
		rw := row{
			UID: a.UID, Nickname: a.Nickname, Source: a.Source,
			HasJWT: a.TokenValue() != "",
		}
		if cred != "" {
			rw.HasKey = true
			rw.KeyID = keyIDOf(cred)
			rw.Preview = previewCredential(cred)
			rw.Upstream = "bigmodel/z.ai"
			rw.Transport = "api-key"
		} else {
			missing++
			rw.Upstream = "zcode.z.ai"
			rw.Transport = "jwt(zcode-plan, 实测 405/3012)"
		}
		rows = append(rows, rw)
	}

	// 服务端本机客户端诊断：同机部署时这就是「一键自动」的来源。
	local := map[string]any{"available": auth.ZCodeLocalClientAvailable()}
	if local["available"].(bool) {
		info := auth.DiscoverZCodeLocalClient()
		local["has_api_key"] = info.APIKey != ""
		local["key_id"] = keyIDOf(info.APIKey)
		local["has_jwt"] = info.JWToken != ""
		local["uid"] = info.UID
		local["provider"] = info.Provider
		local["sources"] = info.Sources
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(rows), "missing_key": missing, "accounts": rows,
		"local_client": local,
		"note": "ZCode 的 anthropic 与 completions 两条端点都只认 <keyId>.<secret>；" +
			"zcodejwt 只对 zcode.z.ai 的 zcode-plan 有效，而那条实测 405/3012。" +
			"本端点 local_client 为真时，POST /panel/api/zcode/credential/auto {\"auto\":true,\"adopt\":true} 即可全自动补齐。",
	})
}
