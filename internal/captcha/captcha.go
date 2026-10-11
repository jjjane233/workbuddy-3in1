// Package captcha 求解 ZCode 的阿里云无痕验证参数（X-Aliyun-Captcha-Verify-Param）。
//
// 阿里云无痕验证 SDK 在 jsdom 纯 Node 环境下会被判定为异常流量（返回 F001），
// 因此本实现通过 CDP 驱动真实 Chrome（headed，可运行在 Xvfb 虚拟显示上）执行
// 页面脚本，取得 VERIFY_PARAM。
//
//	node  captcha_node/solver_chrome.js  <sceneId> <region> <prefix>
//
// 求解结果按 TTL 缓存（阿里云 verifyParam 短时效），并发请求合并为单次求解；
// 上游 403/captcha 失败时调用 Invalidate 立刻作废重取。
package captcha

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config 求解器配置。
type Config struct {
	NodePath   string        // node 可执行文件（默认 "node"）
	ScriptPath string        // solver_chrome.js 路径
	SceneID    string        // 阿里云场景 id（默认 11xygtvd）
	Region     string        // 阿里云 region（默认 cn）
	Prefix     string        // 阿里云前缀（默认 no8xfe）
	TTL        time.Duration // 缓存有效期（默认 45s）
	Timeout    time.Duration // 单次求解超时（默认 60s）
	Retries    int           // 求解失败重试次数（默认 3）
	// CmdPrefix 是 node 之前的命令前缀（如 ["xvfb-run","-a","--server-args=-screen 0 1280x900x24"]，
	// 用于在无显示环境以虚拟显示运行有头 Chrome）。空则不加前缀。
	CmdPrefix []string

	// HomeDir 是解算子子进程的 HOME。
	//
	// 为什么要显式指定：主进程在宿主 shell / systemd 里拉起时
	// HOME 往往是 /root（权限 dr-xr-x---），而解算子以 www 降权跑 ——
	// Chrome 启动时要向 HOME 写 .config/.cache/.pki 等目录，写不进就会在
	// **不输出任何错误**的情况下几秒内退出，表现为
	// "no VERIFY_PARAM in solver output"。指到一个可写目录即可。
	// 留空时不改环境，行为与旧版一致。
	HomeDir string

	// ExtraEnv 是额外注入的环境变量（KEY=VALUE），会覆盖继承值。
	ExtraEnv []string
}

// Solver 无痕验证求解器（单例，内部串行化求解）。
type Solver struct {
	cfg Config

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
	solving  bool
	wait     chan struct{}

	fetcher ConfigFetcher
}

// ConfigFetcher 从上游拉取验证码配置（可为 nil，使用静态默认值）。
type ConfigFetcher func(ctx context.Context) (scene, region, prefix string, err error)

// New 构造求解器。scriptPath 为空时按 exe 目录 / 当前目录下的 captcha_node/solver_chrome.js 查找。
func New(cfg Config, f ConfigFetcher) *Solver {
	if cfg.NodePath == "" {
		cfg.NodePath = "node"
	}
	if cfg.SceneID == "" {
		cfg.SceneID = "11xygtvd"
	}
	if cfg.Region == "" {
		cfg.Region = "cn"
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "no8xfe"
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 45 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Retries <= 0 {
		cfg.Retries = 3
	}
	if cfg.ScriptPath == "" {
		cfg.ScriptPath = locateSolver()
	}
	// ScriptPath 必须绝对化。
	//
	// 为什么：runOnce 把 cmd.Dir 设为脚本所在目录（captcha_node/），
	// 而 node 对相对路径参数是按 **子进程当前目录** 解析的，
	// 于是 "captcha_node/solver_chrome.js" 会变成 captcha_node/captcha_node/solver_chrome.js，
	// node 直接 Cannot find module，stdout 里没有 VERIFY_PARAM，
	// 外部只能看到没头没尾的 "no VERIFY_PARAM in solver output"。
	if abs, err := filepath.Abs(cfg.ScriptPath); err == nil {
		cfg.ScriptPath = abs
	}
	return &Solver{cfg: cfg, fetcher: f}
}

func locateSolver() string {
	cands := []string{}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "captcha_node", "solver_chrome.js"))
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "captcha_node", "solver_chrome.js"))
		cands = append(cands, filepath.Join(wd, "..", "captcha_node", "solver_chrome.js"))
	}
	cands = append(cands, filepath.Join("captcha_node", "solver_chrome.js"))
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "captcha_node/solver_chrome.js"
}

// Ready 报告求解器脚本是否存在。
func (s *Solver) Ready() bool {
	if s == nil {
		return false
	}
	_, err := os.Stat(s.cfg.ScriptPath)
	return err == nil
}

// Get 返回可用的 verifyParam。命中缓存则直接返回，否则发起一次求解。
func (s *Solver) Get(ctx context.Context) (string, error) {
	if s == nil {
		return "", fmt.Errorf("captcha: solver disabled")
	}
	s.mu.Lock()
	if s.cached != "" && time.Since(s.cachedAt) < s.cfg.TTL {
		v := s.cached
		s.mu.Unlock()
		return v, nil
	}
	if s.solving {
		ch := s.wait
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		s.mu.Lock()
		v := s.cached
		fresh := v != "" && time.Since(s.cachedAt) < s.cfg.TTL
		s.mu.Unlock()
		if fresh {
			return v, nil
		}
		return "", fmt.Errorf("captcha: concurrent solve failed")
	}
	s.solving = true
	s.wait = make(chan struct{})
	waitCh := s.wait
	s.mu.Unlock()

	param, err := s.solveWithRetry(ctx)

	s.mu.Lock()
	if err == nil {
		s.cached = param
		s.cachedAt = time.Now()
	}
	s.solving = false
	close(waitCh)
	s.mu.Unlock()
	return param, err
}

// Invalidate 作废当前缓存（上游报告 captcha 失效时调用）。
func (s *Solver) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cached = ""
	s.cachedAt = time.Time{}
	s.mu.Unlock()
}

// Cached 返回当前缓存值与剩余有效期（面板展示用）。
func (s *Solver) Cached() (string, time.Duration, bool) {
	if s == nil {
		return "", 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == "" {
		return "", 0, false
	}
	left := s.cfg.TTL - time.Since(s.cachedAt)
	if left <= 0 {
		return "", 0, false
	}
	return s.cached, left, true
}

// Config 返回当前生效配置（面板展示用）。
func (s *Solver) Config() Config { return s.cfg }

func (s *Solver) solveWithRetry(ctx context.Context) (string, error) {
	scene, region, prefix := s.cfg.SceneID, s.cfg.Region, s.cfg.Prefix
	if s.fetcher != nil {
		if sc, rg, pf, err := s.fetcher(ctx); err == nil {
			if sc != "" {
				scene = sc
			}
			if rg != "" {
				region = rg
			}
			if pf != "" {
				prefix = pf
			}
		}
	}
	var lastErr error
	for i := 0; i < s.cfg.Retries; i++ {
		p, err := s.runOnce(ctx, scene, region, prefix)
		if err == nil && p != "" {
			return p, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("solver produced empty result")
		}
		select {
		case <-time.After(time.Duration(i+1) * 1200 * time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("captcha: solve failed after %d attempts: %w", s.cfg.Retries, lastErr)
}

func (s *Solver) runOnce(ctx context.Context, scene, region, prefix string) (string, error) {
	if !s.Ready() {
		return "", fmt.Errorf("captcha: solver script not found at %s", s.cfg.ScriptPath)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	args := append([]string{}, s.cfg.CmdPrefix...)
	args = append(args, s.cfg.NodePath, s.cfg.ScriptPath, scene, region, prefix)
	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	cmd.Dir = filepath.Dir(s.cfg.ScriptPath)
	// Chrome 在降权进程里启动时需要可写的 HOME/XDG 目录。
	// 不设的话它继承主进程的 HOME（常常是只读的 /root）而静默退出。
	if env := solverEnv(s.cfg); len(env) > 0 {
		cmd.Env = env
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("captcha: start node: %w", err)
	}
	param := ""
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "VERIFY_PARAM=") {
			param = line[len("VERIFY_PARAM="):]
			param = strings.Trim(param, `"' `)
		}
	}
	_ = cmd.Wait()
	if param == "" {
		if cctx.Err() != nil {
			return "", fmt.Errorf("captcha: solver timeout after %s", s.cfg.Timeout)
		}
		return "", fmt.Errorf("captcha: no VERIFY_PARAM in solver output")
	}
	return param, nil
}

// solverEnv 拼解算子子进程的环境变量。
//
// 只在配置了 HomeDir 时生效；否则返回 nil，让 exec 按原样继承父进程环境。
func solverEnv(cfg Config) []string {
	if cfg.HomeDir == "" {
		return nil
	}
	home := cfg.HomeDir
	env := os.Environ()
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
	)
	env = append(env, cfg.ExtraEnv...)
	return env
}

// FetchUpstreamConfig 从 ZCode /api/v1/client/configs 拉取验证码配置。
func FetchUpstreamConfig(hc *http.Client) ConfigFetcher {
	return func(ctx context.Context) (string, string, string, error) {
		if hc == nil {
			hc = http.DefaultClient
		}
		req, err := http.NewRequestWithContext(ctx, "GET",
			"https://zcode.z.ai/api/v1/client/configs?app_version=3.14.3&platform=win32", nil)
		if err != nil {
			return "", "", "", err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return "", "", "", err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var env struct {
			Data struct {
				Configs struct {
					Captcha struct {
						Enabled bool   `json:"enabled"`
						Prefix  string `json:"prefix"`
						Region  string `json:"region"`
						SceneID string `json:"sceneId"`
					} `json:"captcha"`
				} `json:"configs"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return "", "", "", err
		}
		c := env.Data.Configs.Captcha
		return c.SceneID, c.Region, c.Prefix, nil
	}
}
