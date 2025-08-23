package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
	"n42-test/internal/beaconext" // ← 改成你的 module 路径
)

func main() {
	// 若有 .env 则加载（其中可写 RPC_URL 或 rpcurl）
	_ = godotenv.Load()

	rpc, err := beaconext.GetRPCURL()
	if err != nil {
		panic(err)
	}

	state, eth1Hash, beaconBlockHash, err := beaconext.ChainGetBeaconState(context.Background(), rpc, "", 12*time.Second)
	if err != nil {
		panic(err)
	}

	// ---- 格式化 JSON 并落盘 ----
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, state.Raw, "", "  "); err != nil {
		panic(err)
	}
	// 结尾加个换行，方便命令行查看
	pretty.WriteByte('\n')

	if err := os.WriteFile("beacon_state.json", pretty.Bytes(), 0644); err != nil {
		panic(err)
	}

	fmt.Println("OK")
	fmt.Println("Eth1 Block Hash   :", eth1Hash)
	fmt.Println("Beacon Block Hash :", beaconBlockHash)
	fmt.Println("Saved             : beacon_state.json")
}
