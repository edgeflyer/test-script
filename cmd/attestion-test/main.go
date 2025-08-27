package main

import (
	"context"
	"log"

	"n42-test/internal/validator"
)

func main() {
	// 你的 BLS 私钥（hex）
	priv := "3b66b65afd0ef2276dec8ae8573c7daf93bdf3ec53070b73dff2766b4b0d97b0"

	// 若你的二进制需要 RPC_URL，就填上；否则传空字符串即可
	rpcURL := "ws://127.0.0.1:8546" // 或 "http://127.0.0.1:8545"，不需要就设 ""
	httpURL := "http://127.0.0.1:8545"

	if err := validator.ValidateStreamFiltered(context.Background(), priv, rpcURL, httpURL); err != nil {
		log.Fatalf("validate run error: %v", err)
	}
}
