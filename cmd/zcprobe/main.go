package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

func red(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:10] + "..." + s[len(s)-4:]
}

func main() {
	info := auth.DiscoverZCodeLocalClient()
	out := map[string]any{
		"available":       auth.ZCodeLocalClientAvailable(),
		"has_api_key":     info.APIKey != "",
		"api_key_preview": red(info.APIKey),
		"has_jwt":         info.JWToken != "",
		"uid":             info.UID,
		"provider":        info.Provider,
		"sources":         info.Sources,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hc := &http.Client{Timeout: 45 * time.Second}

	res, err := zcode.EnsureLocalClientAPIKey(ctx, hc)
	if err != nil {
		out["ensure_err"] = err.Error()
	} else {
		out["ensure_source"] = res.Source
		out["ensure_cred_preview"] = red(res.Credential)
		out["ensure_channel_err"] = res.ChannelErr
		if res.Credential != "" {
			if verr := zcode.ValidateAPIKey(ctx, hc, res.Credential, "glm-5.3-flash"); verr != nil {
				out["validate"] = "FAIL: " + verr.Error()
			} else {
				out["validate"] = "OK"
			}
		}
	}

	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
