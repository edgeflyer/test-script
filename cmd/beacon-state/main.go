package main

import (
	"context"
	"fmt"
	"time"

	"n42-test/internal/beaconext" // ← 改成你的 module 路径
)

func main() {
	c := beaconext.NewClient("http://127.0.0.1:8545")

	eth1Hash := "0xa3829090fd0022312949fefa8a3e24ac097c6c92bb27b6d780ad89b4467d8916"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	snap, err := c.ResolveBeaconByEth1Hash(ctx, eth1Hash)
	if err != nil {
		panic(err)
	}

	fmt.Println("eth1 hash        :", snap.Eth1Hash)
	fmt.Println("beacon block hash:", snap.BeaconBlockHash)

	// 格式化输出两个 JSON
	beaconext.PrettyPrintJSON("Beacon Block", snap.BeaconBlockRaw)
	beaconext.PrettyPrintJSON("Beacon State", snap.BeaconStateRaw)
}
