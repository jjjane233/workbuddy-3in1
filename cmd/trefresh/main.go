package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/trae"
)

// 一次性探针：验证 Trae refresh_token 换票 + model_list 实际返回。
func main() {
	dir := os.Args[1]
	list, err := auth.LoadDir(dir)
	if err != nil {
		fmt.Println("loaddir:", err)
		os.Exit(1)
	}
	ctx := context.Background()
	hc := &http.Client{Timeout: 40 * time.Second}
	for _, a := range list {
		if a.ProviderValue() != auth.ProviderTrae {
			continue
		}
		fmt.Println("=== ACCOUNT", a.UID, "expire_at_ms:", a.TokenExpireAtValue())
		c := trae.New(a, trae.Options{HTTPClient: hc})
		if ok, err := c.Ping(ctx); err != nil {
			fmt.Println("  ping:", err)
		} else {
			fmt.Println("  ping:", ok)
		}
		if _, err := c.RefreshAccessToken(ctx, a); err != nil {
			fmt.Println("  refresh:", err)
		} else {
			fmt.Println("  refresh OK new_exp_ms:", a.TokenExpireAtValue())
		}
		for _, typ := range []string{"chat", "llm_raw_chat"} {
			raw, err := c.ModelList(ctx, typ)
			if err != nil {
				fmt.Printf("  model_list %s: %v\n", typ, err)
				continue
			}
			var r struct {
				ModelConfigs []map[string]any `json:"model_configs"`
			}
			_ = json.Unmarshal(raw, &r)
			fmt.Printf("  model_list %s: %d entries\n", typ, len(r.ModelConfigs))
			for _, m := range r.ModelConfigs {
				b, _ := json.Marshal(map[string]any{
					"name": m["name"], "display_name": m["display_name"],
					"provider": m["provider"], "status": m["status"],
					"max_tokens": m["max_tokens"], "max_turn": m["max_turn"],
					"context_window_size": m["context_window_size"],
					"reasoning_effort_config": m["reasoning_effort_config"],
					"consumption_rate": func() any {
						f, _ := m["features"].(map[string]any)
						return f["consumption_rate"]
					}(),
				})
				fmt.Println("    ", string(b))
			}
			_ = io.Discard
		}
	}
}
