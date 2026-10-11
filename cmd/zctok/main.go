package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

func main() {
	prov, tok := auth.DiscoverZCodeCredentialsFile()
	info := auth.DiscoverZCodeLocalClient()
	out := map[string]any{
		"provider":       prov,
		"has_oauth":      tok != "",
		"oauth_preview":  red(tok),
		"client_api_key": red(info.APIKey),
		"uid":            info.UID,
		"sources":        info.Sources,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	io.WriteString(os.Stderr, "")
	// ?? token ???? stderr ??????????????????
	if tok != "" {
		_ = os.WriteFile(os.Getenv("TEMP")+"/_zctok.txt", []byte(prov+"\n"+tok+"\n"), 0o600)
	}
}

func red(s string) string {
	if len(s) <= 14 {
		return s
	}
	return s[:10] + "..." + s[len(s)-4:]
}
