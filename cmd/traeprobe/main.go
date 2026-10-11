package main

import (
	"bytes"
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

func main() {
	dir := os.Args[1]
	list, err := auth.LoadDir(dir)
	if err != nil {
		fmt.Println("loaddir:", err)
		os.Exit(1)
	}
	ctx := context.Background()
	for _, a := range list {
		if a.ProviderValue() != auth.ProviderTrae {
			continue
		}
		fmt.Println("=== ACCOUNT", a.UID)
		hc := &http.Client{Timeout: 30 * time.Second}
		tok := a.TokenValue()
		dev := a.DeviceID
		if dev == "" {
			dev = a.UID
		}
		post := func(path string, body map[string]any) (int, string) {
			raw, _ := json.Marshal(body)
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.trae.cn"+path, bytes.NewReader(raw))
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "Trae/"+trae.DefaultIDEVersion+" (win32; x64)")
			req.Header.Set("x-ide-version-code", trae.DefaultIDEVersion)
			req.Header.Set("x-app-id", trae.AppID)
			req.Header.Set("x-device-id", dev)
			req.Header.Set("x-device-brand", "pc")
			req.Header.Set("x-device-type", "windows")
			req.Header.Set("x-os-version", "10.0.19045")
			req.Header.Set("Authorization", "Cloud-IDE-JWT "+tok)
			req.Header.Set("x-cloudide-token", tok)
			r, e := hc.Do(req)
			if e != nil {
				return 0, "ERR " + e.Error()
			}
			defer r.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			return r.StatusCode, string(b)
		}
		show := func(label, path string, body map[string]any) {
			st, bd := post(path, body)
			n := len(bd)
			if n > 260 {
				n = 260
			}
			fmt.Printf("  %-56s http=%-4d %s\n", label, st, bd[:n])
			time.Sleep(400 * time.Millisecond)
		}

		fmt.Println("--- A) pay/grant_work_benefit (无参, trae_client=Lite) ---")
		show("benefit{Lite,device}", "/trae/api/v2/pay/grant_work_benefit", map[string]any{"trae_client": "Lite", "device_id": dev})
		show("benefit{IDE,device}", "/trae/api/v2/pay/grant_work_benefit", map[string]any{"trae_client": "IDE", "device_id": dev})
		show("benefit{device only}", "/trae/api/v2/pay/grant_work_benefit", map[string]any{"device_id": dev})
		show("benefit{empty}", "/trae/api/v2/pay/grant_work_benefit", map[string]any{})

		fmt.Println("--- B) ug/work_fission/grant (纯 activity_id) ---")
		for _, id := range []string{"code_fission_202609", "work_fission_202607"} {
			show("fission{activity_id="+id+"}", "/trae/api/v2/ug/work_fission/grant", map[string]any{"activity_id": id})
		}

		fmt.Println("--- C) activity/action (逐活动) ---")
		for _, id := range []string{"checkin_credits", "new_user_credits", "send_message_reward", "student_auth_reward", "work_fission_202607"} {
			show("action{activity_id="+id+"}", "/trae/api/v2/ug/activity/action", map[string]any{"activity_id": id})
		}

		fmt.Println("--- D) 复核活动状态 ---")
		info, err := trae.New(a, trae.Options{}).QueryActivityInfo(ctx, 1)
		if err == nil {
			for _, act := range info.Activities {
				b, _ := json.Marshal(act["work_extra"])
				fmt.Printf("  %-26s %s\n", act["activity_id"], string(b))
			}
		}
		fmt.Println("--- E) 额度 ---")
		if u, err := trae.New(a, trae.Options{}).QueryUsage(ctx, 1); err == nil {
			fmt.Printf("  remaining=%d total=%d consumed=%.2f\n", u.Remaining, u.Total, u.Summary.ConsumedAmount)
		}
	}
}
