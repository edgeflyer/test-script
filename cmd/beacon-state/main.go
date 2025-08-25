package main

import (
	"context"
	"fmt"
	"time"

	"n42-test/internal/beaconext" // ← 改成你的 module 路径
)

func main() {
	c := beaconext.NewClient("http://127.0.0.1:8545")

	eth1Hash := "0x9e6fdcb71d4d56a7df5a5f3703a918ec5f5abb0c7c13479731954ab032a3539a"

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
