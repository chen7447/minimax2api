// probe-signin 一次性诊断工具：在 VPS 上用仓库自己的 minimax 包签名逻辑，
// 直接拉官方 /signin/status 原始报文，侦查签到周期的真实机制。
// 用法: probe-signin -cred /tmp/probe1.json [-claim]
// 用完即删，不留凭证。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"minimax2api/internal/config"
	"minimax2api/internal/minimax"
)

type credFile struct {
	Token    string `json:"token"`
	UserID   string `json:"userID"`
	UUID     string `json:"uuid"`
	DeviceID string `json:"deviceID"`
	ScreenW  int    `json:"screenW"`
	ScreenH  int    `json:"screenH"`
}

func main() {
	path := flag.String("cred", "", "credential json file")
	doClaim := flag.Bool("claim", false, "also POST the claim endpoint")
	doRenew := flag.Bool("renew", false, "also call POST /v1/api/user/renewal and print the new token length")
	flag.Parse()

	raw, err := os.ReadFile(*path)
	if err != nil {
		panic(err)
	}
	var cf credFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		panic(err)
	}

	settings := config.DefaultSettings("/tmp/probe-data")
	client := minimax.New(func() config.Settings { return settings })
	cred := minimax.Credential{
		Region:      "global",
		Token:       cf.Token,
		UserID:      cf.UserID,
		UUID:        cf.UUID,
		DeviceID:    cf.DeviceID,
		ScreenWidth: cf.ScreenW, ScreenHeight: cf.ScreenH,
	}

	panel, err := client.SigninStatus(context.Background(), cred)
	if err != nil {
		fmt.Println("status error:", err)
	} else {
		b, _ := json.MarshalIndent(panel, "", "  ")
		fmt.Println("=== status panel ===")
		fmt.Println(string(b))
	}

	// -renew drives the discovery: POST /v1/api/user/renewal with the current
	// token returns a fresh 30-day JWT. Verified 2026-10-16 by hand; this hook
	// lets the probe re-check it through the same signing stack.
	if *doRenew {
		newTok, err := client.RenewToken(context.Background(), cred)
		if err != nil {
			fmt.Println("renew error:", err)
		} else {
			fmt.Println("=== renewed token ===")
			fmt.Println(len(newTok), "chars, head", newTok[:16])
		}
	}

	if *doClaim {
		claim, err := client.SigninClaim(context.Background(), cred)
		if err != nil {
			fmt.Println("claim error:", err)
		} else {
			b, _ := json.MarshalIndent(claim, "", "  ")
			fmt.Println("=== claim result ===")
			fmt.Println(string(b))
		}
	}

	// Raw mode is how the cycle questions get answered: the typed struct only
	// keeps what the gateway stores, and the envelope may carry cycle fields
	// (total cycles, next-cycle preview) that parsePanel drops.
	fmt.Println("=== raw status envelope ===")
	fmt.Println(client.DebugSigninStatus(context.Background(), cred))
}
