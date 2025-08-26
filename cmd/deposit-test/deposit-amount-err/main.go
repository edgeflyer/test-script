package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"n42-test/internal/deposit"
	"time"
)

const (
	RPC       = "http://127.0.0.1:8545"
	CONTRACT  = "0x5FbDB2315678afecb367f032d93F642f64180aa3"                         // 本地/测试链 Deposit 合约地址
	SENDER_SK = "0xce450e7ca567cc0b2cea71d5115cdb7a8ef9a12cddd6675ef2a1eaa893f2e9ce" // 用于发交易（EOA），非BLS

	// BLS材料（来自你的验证者生成工具/本地数据）
	BLS_SK      = "0x3b66b65afd0ef2276dec8ae8573c7daf93bdf3ec53070b73dff2766b4b0d97b0"
	PUBKEY_HEX  = "0xa0b70382269e80251254dc3962c9afd3578f01d31b2b7169a5a4782566e77336d35e4a5e33704b1fba86b58e80e4b804" // 48B
	WC_HEX      = "0x010000000000000000000000A9e5f7F86A946BAfda9D98e1907f387c38950525"                                 // 32B(ETH1提款)
	AMOUNT_GWEI = uint64(32_000_000_000)                                                                               // 32 ETH
)

func main() {
	amount_gwei := uint64(32_000_000_000)
	// 1) 计算“正确”的 BLS 签名 和 root（仅展示，不发送）
	correctSigHex, _, err := deposit.ComputeDepositSignatureAndRoot(PUBKEY_HEX, WC_HEX, amount_gwei, BLS_SK)
	if err != nil {
		log.Fatalf("计算签名失败: %v", err)
	}

	// 修改交易金额
	amount_gwei = uint64(64_000_000_000)
	_, rightRootHex, err := deposit.ComputeDepositSignatureAndRoot(PUBKEY_HEX, WC_HEX, amount_gwei, BLS_SK)
	if err != nil {
		log.Fatalf("计算root失败: %v", err)
	}

	// 组装交易参数
	amountWei := new(big.Int).Mul(big.NewInt(int64(amount_gwei)), big.NewInt(1_000_000_000))
	params := &deposit.DepositParams{
		Contract:             CONTRACT,
		PrivateKeyHex:        SENDER_SK,
		RPC:                  RPC,
		PubkeyHex:            PUBKEY_HEX,
		WCHex:                WC_HEX,
		SignatureHex:         correctSigHex, // 用32ETH时的签名
		RootHex:              rightRootHex,  // 用新的root与之匹配
		AmountWei:            amountWei,
		Nonce:                -1, // 自动
		GasLimit:             0,  // 自动估算（会成功，因为root匹配）
		MaxPriorityFeePerGas: nil,
		MaxFeePerGas:         nil,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := deposit.NewClient(ctx, RPC, SENDER_SK)
	if err != nil {
		log.Fatalf("NewClient失败：%v", err)
	}

	cli.DebugPrintAccountState(ctx)

	txRes, err := cli.SendDeposit(ctx, params)
	if err != nil {
		log.Fatalf("发送失败：%v", err)
	}
	fmt.Println("\n=== 交易结果 ===")
	fmt.Printf("TxHash=%s\nNonce=%d\nEstGas=%d\nUsedGas=%d\nBlockNumber=%d\nEth1BlockHash=%s\n", txRes.TxHash, txRes.Nonce, txRes.EstimatedGas, txRes.UsedGas, txRes.BlockNumber, txRes.BlockHash)
}
