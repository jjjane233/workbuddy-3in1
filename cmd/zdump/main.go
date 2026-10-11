package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

func main() {
	for _, p := range auth.ZCodeCredentialsPaths() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		fmt.Println("=== ", p)
		for k, v := range doc {
			s, _ := v.(string)
			if s == "" {
				fmt.Printf("%-34s (len %d)\n", k, 0)
				continue
			}
			if len(s) > 6 && s[:6] == "enc:v1" {
				if plain, err := auth.DecryptZCodeCredential(s); err == nil {
					if len(plain) > 700 {
						plain = plain[:700] + "...(truncated)"
					}
					fmt.Printf("%-34s -> %s\n", k, plain)
					continue
				}
				fmt.Printf("%-34s (decrypt failed)\n", k)
				continue
			}
			if len(s) > 700 {
				s = s[:700] + "...(truncated)"
			}
			fmt.Printf("%-34s -> %s\n", k, s)
		}
	}
}
