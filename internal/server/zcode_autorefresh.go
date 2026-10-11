package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// ZCode 签名凭据的后台自动同步。
//
// 解决的场景：
//
//	ZCode 客户端每次刷新都会重解 <keyId>.<secret>（上游会轮换 secret），
//	所以「一次性粘进去」的 key 会**过期**。手工时代的表现是：网关某天突然
//	全部 ZCode 号对话 401/签名失败，用户要重新手抄一遍。
//
// 自动化做法（本文件）：后台循环带冷却地读本机 ZCode 客户端的
// config.json / credentials.json，
//
//	1. uid 与客户端一致的号  → 用最新凭据覆盖（处理 secret 轮换）
//	2. 缺凭据的号            → 补上
//	3. 客户端 uid 还没入池   → 自动建号（「装一次即零步骤」）
//
// 注意：本机没装 ZCode 客户端时（典型：机房服务器）这里什么都不做，
// 由面板的「粘贴一次 / 一次性 auto」负责，互不冲突。

// zcodeCredSyncMinutes 自动同步周期（分钟）。与 ZCodeBalanceMinutes 同级。
//
// 取 5 分钟的理由：够快追上 secret 轮换，同时读的是本机文件、不打上游，
// 成本几乎为零。
const zcodeCredSyncMinutes = 5

// zcodeAutoRefresh 是同步器的进程内状态（由 Handler.zcodeCredMu 保护）。
type zcodeAutoRefresh struct {
	lastCred string
	lastUID  string
	lastRun  time.Time
}

// zcodeAutoSync 执行一次自动同步。force 忽略冷却。
//
// 返回人类可读的结果摘要（无变化时为空串）。调用方负责日志。
func (h *Handler) zcodeAutoSync(ctx context.Context, force bool) string {
	if !h.cfg.ZCodeCredAutoSync {
		return ""
	}
	if !auth.ZCodeLocalClientAvailable() {
		return ""
	}
	cd := time.Duration(zcodeCredSyncMinutes) * time.Minute
	h.zcodeCredMu.Lock()
	due := force || h.zcodeCredSync.lastRun.IsZero() || time.Since(h.zcodeCredSync.lastRun) >= cd
	if due {
		h.zcodeCredSync.lastRun = time.Now()
	}
	h.zcodeCredMu.Unlock()
	if !due {
		return ""
	}

	// 通道 A（服务端换 key）成本高且多半受服务端置空限制；后台循环只要
	// 通道 B（读客户端成品）即可 —— 客户端自己会去换。这里用短超时兜一下 A，
	// 失败不影响 B。
	hc := &http.Client{Timeout: 20 * time.Second}
	var res *zcode.LocalKeyResult
	var err error
	if force {
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		res, err = zcode.EnsureLocalClientAPIKey(cctx, hc)
		cancel()
	} else {
		res = &zcode.LocalKeyResult{Info: auth.DiscoverZCodeLocalClient()}
		res.Credential = auth.ReadSignCredentialFromLocalClient()
		res.Source = "client-config"
		if res.Credential == "" {
			err = zcode.ErrNoLocalCredential
		}
	}
	if res == nil || res.Credential == "" {
		if err != nil {
			return ""
		}
		return ""
	}

	h.zcodeCredMu.Lock()
	same := h.zcodeCredSync.lastCred == res.Credential && h.zcodeCredSync.lastUID == res.Info.UID
	if !same {
		h.zcodeCredSync.lastCred = res.Credential
		h.zcodeCredSync.lastUID = res.Info.UID
	}
	h.zcodeCredMu.Unlock()
	if same && !force {
		return ""
	}

	updated, filled, adopted := h.zcodeApplyLocalCredential(res)
	if updated == 0 && filled == 0 && adopted == 0 && !force {
		return ""
	}
	return fmt.Sprintf("凭据同步：更新 %d 补齐 %d 新增 %d（来源 %s，uid=%s）",
		updated, filled, adopted, res.Source, res.Info.UID)
}

// zcodeApplyLocalCredential 把本机凭据落到池里。返回 (更新数, 补齐数, 新增数)。
func (h *Handler) zcodeApplyLocalCredential(res *zcode.LocalKeyResult) (updated, filled, adopted int) {
	cred := res.Credential
	keyID := cred
	if i := strings.IndexByte(cred, '.'); i > 0 {
		keyID = cred[:i]
	}
	clientUID := res.Info.UID

	for _, st := range h.cfg.Pool.List() {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.ProviderOf() != auth.ProviderZCode {
			continue
		}
		cur := a.SignCredential()
		if cur == cred && a.APIKeyValue() == cred {
			continue
		}
		// 有客户端 uid 时只动匹配的号；没有则只补空缺，不猜。
		matches := clientUID != "" && a.UID == clientUID
		if cur != "" && !matches {
			continue
		}
		a.SetSignCredential(cred)
		a.SetAPIKey(cred)
		if err := a.SaveAtomic(); err != nil {
			logUpstream("zcode 凭据同步失败 uid=%s: %v", a.UID, err)
			continue
		}
		if cur == "" {
			filled++
		} else {
			updated++
		}
		h.cfg.Pool.Revive(a.UID)
	}

	// 客户端账号还没入池 → 自动建号（等价于面板的 ZCode 浏览器登录）
	if clientUID != "" && h.cfg.Pool.AuthByUID(clientUID) == nil {
		source := res.Info.Provider
		if source == "" {
			source = "bigmodel"
		}
		a := &auth.Auth{
			Provider:    auth.ProviderZCode,
			Mode:        auth.ModeJWT,
			AccessToken: res.Info.JWToken,
			UID:         clientUID,
			Username:    clientUID,
			Nickname:    clientUID,
			Region:      "cn",
			Source:      source,
			Remark:      "auto-sync",
			LastRefresh: time.Now().Unix(),
			FilePath:    h.zcodeAuthFilePath(clientUID),
		}
		a.SetSignCredential(cred)
		a.SetAPIKey(cred)
		if err := a.SaveAtomic(); err != nil {
			logUpstream("zcode 自动建号失败 uid=%s: %v", clientUID, err)
		} else {
			h.cfg.Pool.Add(a)
			h.cfg.Pool.Revive(clientUID)
			adopted++
		}
	}
	if keyID != "" && (updated+filled+adopted) > 0 {
		logUpstream("zcode 凭据已同步 keyId=%s", keyID)
	}
	return updated, filled, adopted
}

// zcodeAuthFilePath 与面板登录口径一致的落盘路径（auths/zcode-<uid>.json）。
func (h *Handler) zcodeAuthFilePath(uid string) string {
	dir := h.cfg.AuthDir
	if dir == "" {
		dir = "auths"
	}
	return filepath.Join(dir, "zcode-"+uid+".json")
}
