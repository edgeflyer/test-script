package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	// 改成你的真实模块路径
	"n42-test/internal/deposit"
)

// ======= 测试用常量（请按你的本地链替换）=======
const (
	RPC       = "http://127.0.0.1:8545"
	CONTRACT  = "0x5FbDB2315678afecb367f032d93F642f64180aa3"                         // 本地/测试链 Deposit 合约地址
	SENDER_SK = "0xeee5683d17a906cbea293688296ccaf6f25bc1837165e8a73f48d2f33d07da7f" // 用于发交易（EOA），非BLS

	// BLS材料（来自你的验证者生成工具/本地数据）
	BLS_SK      = "0x27c327507f888866867cc1178b9c28ecfd29b778abcc898de5374cfed577c1b1"
	PUBKEY_HEX  = "0x83b63b4aea531b66903d6dccd4b909dea84ea9ddeaa300d63f34da021621684a0e61ecb711001f00a41f1ba1aef1f22b" // 48B
	WC_HEX      = "0x010000000000000000000000CcC20d447F9196eB009D808BD99A4cc0Ab38eF08"                                 // 32B(ETH1提款)
	AMOUNT_GWEI = uint64(32_000_000_000)                                                                               // 32 ETH
)

// 轻度篡改签名：翻转最后一个字节（保持长度96B不变）
func tamperSignatureHex(sig string) string {
	if len(sig) < 4 {
		return sig
	} // 太短就不动
	// 简单替换最后一个hex字符
	last := sig[len(sig)-1]
	if last != '0' {
		return sig[:len(sig)-1] + "0"
	}
	return sig[:len(sig)-1] + "1"
}

func main() {
	// 1) 计算“正确”的 BLS 签名 和 root（仅展示，不发送）
	correctSigHex, correctRootHex, err := deposit.ComputeDepositSignatureAndRoot(PUBKEY_HEX, WC_HEX, AMOUNT_GWEI, BLS_SK)
	if err != nil {
		log.Fatalf("计算正确签名失败: %v", err)
	}
	fmt.Println("=== 基准（正确）===")
	fmt.Println("signature:", correctSigHex)
	fmt.Println("root     :", correctRootHex)

	// 2) 篡改签名，并“按篡改后的签名”重算 root
	tamperedSig := tamperSignatureHex(correctSigHex)
	tamperedRoot, err := deposit.ComputeDepositDataRoot(PUBKEY_HEX, WC_HEX, AMOUNT_GWEI, tamperedSig)
	if err != nil {
		log.Fatalf("重算root失败: %v", err)
	}
	fmt.Println("\n=== 篡改用案（预期链上成功，Beacon拒绝）===")
	fmt.Println("tampered signature:", tamperedSig)
	fmt.Println("recomputed root   :", tamperedRoot)

	// 3) 组装交易参数（注意：AmountWei = Gwei * 1e9）
	amountWei := new(big.Int).Mul(big.NewInt(int64(AMOUNT_GWEI)), big.NewInt(1_000_000_000))
	params := &deposit.DepositParams{
		Contract:             CONTRACT,
		PrivateKeyHex:        SENDER_SK,
		RPC:                  RPC,
		PubkeyHex:            PUBKEY_HEX,
		WCHex:                WC_HEX,
		SignatureHex:         tamperedSig,  // 用篡改后的签名
		RootHex:              tamperedRoot, // 用新的root与之匹配
		AmountWei:            amountWei,
		Nonce:                -1, // 自动
		GasLimit:             0,  // 自动估算（会成功，因为root匹配）
		MaxPriorityFeePerGas: nil,
		MaxFeePerGas:         nil,
	}

	// 4) 发送交易
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := deposit.NewClient(ctx, RPC, SENDER_SK)
	if err != nil {
		log.Fatalf("NewClient失败: %v", err)
	}
	defer cli.Close()

	cli.DebugPrintAccountState(ctx)

	txRes, err := cli.SendDeposit(ctx, params)
	if err != nil {
		log.Fatalf("发送失败: %v", err)
	}
	fmt.Println("\n=== 交易结果 ===")
	fmt.Printf("TxHash=%s\nNonce=%d\nEstGas=%d\nUsedGas=%d\nBlockNumber=%d\nEth1BlockHash=%s\n", txRes.TxHash, txRes.Nonce, txRes.EstimatedGas, txRes.UsedGas, txRes.BlockNumber, txRes.BlockHash)
	fmt.Println("\n[说明] 该交易会在合约层成功写入存款树，但由于签名被篡改，Beacon/共识层后续不会激活该验证者。")
}
