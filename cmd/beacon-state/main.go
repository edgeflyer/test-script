package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"n42-test/internal/beaconext" // ← 按你的实际 module 路径修改
)

func main() {
	// 读模式参数
	mode := readMode()

	// RPC 地址
	rpc := os.Getenv("RPC_URL")
	if rpc == "" {
		rpc = "http://127.0.0.1:8545"
	}
	c := beaconext.NewClient(rpc)

	in := bufio.NewReader(os.Stdin)
	fmt.Printf("已连接执行层 RPC: %s\n", rpc)
	fmt.Println("输入 eth1 区块哈希（0x + 64位hex），回车查询；输入 q 回车退出。")

	for {
		fmt.Print("\n请输入 eth1 区块哈希：")
		line, _ := in.ReadString('\n')
		eth1Hash := strings.TrimSpace(line)

		if eth1Hash == "" {
			fmt.Println("⚠️ 不能为空，请重新输入。")
			continue
		}
		if eth1Hash == "q" || eth1Hash == "Q" {
			fmt.Println("已退出。")
			return
		}
		if !looksLikeHash(eth1Hash) {
			fmt.Println("⚠️ 似乎不是合法的 0x… 区块哈希（期望长度 66）。仍然尝试查询……")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		snap, err := c.ResolveBeaconByEth1Hash(ctx, eth1Hash)
		cancel()
		if err != nil {
			fmt.Printf("❌ 查询失败：%v\n", err)
			continue
		}

		// 通用头部
		fmt.Println("eth1 hash        :", snap.Eth1Hash)
		fmt.Println("beacon block hash:", snap.BeaconBlockHash)

		switch mode {
		case 0:
			// 全部输出
			beaconext.PrettyPrintJSON("Beacon Block", snap.BeaconBlockRaw)
			beaconext.PrettyPrintJSON("Beacon State", snap.BeaconStateRaw)
		case 1:
			// 仅输出 Beacon State 的 validators + balances
			var state struct {
				Validators []map[string]any `json:"validators"`
				Balances   []uint64         `json:"balances"`
			}
			if err := json.Unmarshal(snap.BeaconStateRaw, &state); err != nil {
				fmt.Printf("❌ 解析 Beacon State 失败：%v\n", err)
				continue
			}
			partial := map[string]any{
				"validators": state.Validators,
				"balances":   state.Balances,
			}
			bs, _ := json.MarshalIndent(partial, "", "  ")
			fmt.Println("Beacon State（仅 validators + balances）：")
			fmt.Println(string(bs))
		default:
			fmt.Println("⚠️ 未知模式，使用 0（全部）作为回退。")
			beaconext.PrettyPrintJSON("Beacon Block", snap.BeaconBlockRaw)
			beaconext.PrettyPrintJSON("Beacon State", snap.BeaconStateRaw)
		}
	}
}

// 读取模式：0=全部；1=仅 state.validators+balances
func readMode() int {
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("请选择输出模式（0=全部，1=仅state.validators+balances）：")
		line, _ := in.ReadString('\n')
		s := strings.TrimSpace(line)
		switch s {
		case "0":
			return 0
		case "1":
			return 1
		default:
			fmt.Println("⚠️ 只能输入 0 或 1")
		}
	}
}

// 粗略校验：0x + 64 hex
func looksLikeHash(s string) bool {
	if len(s) != 66 || !(strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X")) {
		return false
	}
	for _, ch := range s[2:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			return false
		}
	}
	return true
}
