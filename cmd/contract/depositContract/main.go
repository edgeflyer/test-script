package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	gethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"
)

const artifactPath = "./build/DepositContract.json" // 固定路径：把 artifact 放到这里即可

type artifact struct {
	ABI      json.RawMessage `json:"abi"`
	Bytecode string          `json:"bytecode"`
}

func main() {
	// 1) 读取 .env
	_ = godotenv.Load()
	rpcURL := mustEnv("RPC_URL")
	privHex := mustEnv("PRIVATE_KEY")

	// 2) 读取 artifact（含 abi + bytecode）
	abiJSON, bytecode := loadArtifact(artifactPath)

	// 3) 解析 ABI
	parsedABI, err := abi.JSON(strings.NewReader(string(abiJSON)))
	if err != nil {
		log.Fatalf("解析 ABI 失败: %v", err)
	}

	// 4) 连接 RPC
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}
	defer client.Close()

	// 5) 私钥 & from 地址
	privHex = strings.TrimPrefix(strings.TrimPrefix(privHex, "0x"), "0X")
	privateKey, err := crypto.HexToECDSA(privHex)
	if err != nil {
		log.Fatalf("解析 PRIVATE_KEY 失败: %v", err)
	}
	pub := privateKey.Public().(*ecdsa.PublicKey)
	from := crypto.PubkeyToAddress(*pub)
	fmt.Println("部署账户:", from.Hex())

	// 6) 构造签名器（EIP-155）
	ctx := context.Background()
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		log.Fatalf("获取 chainID 失败: %v", err)
	}
	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, chainID)
	if err != nil {
		log.Fatalf("创建 TransactOpts 失败: %v", err)
	}

	// 7) Gas 设置（兼容性强：优先用 legacy GasPrice；本地链足够）
	gp, err := client.SuggestGasPrice(ctx)
	if err != nil {
		log.Fatalf("获取 GasPrice 失败: %v", err)
	}
	auth.GasPrice = gp
	// 留空 GasLimit 让后端估算
	auth.GasLimit = 0
	auth.From = from
	auth.Context = ctx

	// 8) 部署合约（无构造函数参数）
	addr, tx, _, err := bind.DeployContract(auth, parsedABI, bytecode, client)
	if err != nil {
		log.Fatalf("部署失败: %v", err)
	}
	fmt.Println("部署交易哈希:", tx.Hash().Hex())
	fmt.Println("合约地址:", addr.Hex())

	// 9) 等待上链
	receipt, err := bind.WaitMined(ctx, client, tx)
	if err != nil {
		log.Fatalf("等待上链失败: %v", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		log.Fatalf("部署失败，交易状态=%d", receipt.Status)
	}
	fmt.Printf("✅ 部署成功，区块号=%d\n", receipt.BlockNumber.Uint64())
}

// ===== 工具函数 =====

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("环境变量 %s 未设置", key)
	}
	return v
}

func loadArtifact(path string) (abiJSON []byte, bytecode []byte) {
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("读取 artifact 失败 %s: %v", path, err)
	}
	var a artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		log.Fatalf("解析 artifact JSON 失败: %v", err)
	}
	if len(a.ABI) == 0 {
		log.Fatalf("artifact 缺少 abi 字段")
	}
	if a.Bytecode == "" {
		log.Fatalf("artifact 缺少 bytecode 字段（Hardhat/Foundry 产物应包含）")
	}
	abiJSON = a.ABI

	bc := strings.TrimPrefix(strings.TrimPrefix(a.Bytecode, "0x"), "0X")
	b, err := hex.DecodeString(bc)
	if err != nil {
		log.Fatalf("解析 bytecode 失败: %v", err)
	}
	bytecode = b
	return
}

// （仅为导入别名占位，避免未使用错误）
var _ = gethCommon.Address{}
