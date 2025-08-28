package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"n42-test/internal/validator"
)

func main() {
	// 运行时输入 BLS 私钥
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("请输入 BLS 私钥 (hex): ")
	priv, _ := reader.ReadString('\n')
	priv = strings.TrimSpace(priv) // 去掉换行符

	if priv == "" {
		log.Fatal("必须输入私钥！")
	}

	// RPC URL 你可以写死，或者也提示输入
	rpcURL := "ws://127.0.0.1:8546"
	httpURL := "http://127.0.0.1:8545"

	if err := validator.ValidateStreamFiltered(context.Background(), priv, rpcURL, httpURL); err != nil {
		log.Fatalf("validate run error: %v", err)
	}
}
