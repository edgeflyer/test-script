package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/herumi/bls-eth-go-binary/bls"
	"n42-test/internal/deposit" // ← 改成你的真实 module 路径
)

func main() {
	var (
		rpc        = flag.String("rpc", "http://127.0.0.1:8545", "执行层 RPC")
		contract   = flag.String("contract", "", "Deposit 合约地址 (0x...)")
		senderSK   = flag.String("sender-sk", "", "发送交易的私钥 secp256k1 (0x...)")
		blsSK      = flag.String("bls-sk", "", "BLS 私钥 (0x...) 用于自动计算 pubkey/签名/root")
		wcHex      = flag.String("wc", "", "withdrawal_credentials (可选，32B 0x...)")
		withdrawEA = flag.String("withdraw-addr", "", "提款执行层地址 (可选，20B 0x...)，与 -wc 二选一")
		amountETH  = flag.Uint64("amount-eth", 32, "质押金额（ETH），默认 32")
		gasLimit   = flag.Uint64("gas", 0, "GasLimit；0=自动估算")
	)
	flag.Parse()

	// 基本参数校验
	if *contract == "" || *senderSK == "" || *blsSK == "" {
		log.Fatalf("缺少必填：-contract -sender-sk -bls-sk")
	}
	// 处理 WC：优先 -wc；否则用 -withdraw-addr 生成
	var wc string
	if *wcHex != "" {
		wc = *wcHex
	} else if *withdrawEA != "" {
		gen, err := deposit.ComputeWithdrawalCredentialsFromEth1(*withdrawEA)
		if err != nil {
			log.Fatalf("根据执行层地址生成 wc 失败: %v", err)
		}
		wc = gen
	} else {
		log.Fatalf("请提供 -wc 或 -withdraw-addr（二选一）")
	}

	// === 1) 由 BLS 私钥自动得到 BLS 公钥（48B 压缩） ===
	bls.Init(bls.BLS12_381)
	var sk bls.SecretKey
	if err := sk.SetHexString(strings.TrimPrefix(*blsSK, "0x")); err != nil {
		log.Fatalf("BLS 私钥解析失败: %v", err)
	}
	pk := sk.GetPublicKey()
	pkBytes := pk.Serialize() // 48B
	pubkeyHex := "0x" + fmt.Sprintf("%x", pkBytes)

	// === 2) 自动计算 BLS 签名 与 deposit_data_root ===
	amountGwei := *amountETH * 1_000_000_000
	sigHex, rootHex, err := deposit.ComputeDepositSignatureAndRoot(pubkeyHex, wc, amountGwei, *blsSK)
	if err != nil {
		log.Fatalf("计算签名/根失败: %v", err)
	}

	fmt.Println("=== 自动计算结果 ===")
	fmt.Println("pubkey (48B):", pubkeyHex)
	fmt.Println("wc     (32B):", wc)
	fmt.Println("signature  :", sigHex)
	fmt.Println("root       :", rootHex)

	// === 3) 组装并发送交易 ===
	amountWei := new(big.Int).Mul(big.NewInt(int64(amountGwei)), big.NewInt(1_000_000_000)) // Gwei->Wei

	params := &deposit.DepositParams{
		Contract:      *contract,
		PrivateKeyHex: *senderSK, // 交易签名（secp256k1）
		RPC:           *rpc,

		PubkeyHex:    pubkeyHex,
		WCHex:        wc,
		SignatureHex: sigHex,
		RootHex:      rootHex,

		AmountWei: amountWei,
		Nonce:     -1,        // 自动
		GasLimit:  *gasLimit, // 0 = 自动估算
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cli, err := deposit.NewClient(ctx, *rpc, *senderSK)
	if err != nil {
		log.Fatalf("NewClient 失败: %v", err)
	}
	defer cli.Close()

	cli.DebugPrintAccountState(ctx)

	res, err := cli.SendDeposit(ctx, params)
	if err != nil {
		log.Fatalf("发送质押请求失败: %v", err)
	}
	fmt.Println("=== 交易结果 ===")
	fmt.Printf("TxHash=%s Nonce=%d EstGas=%d UsedGas=%d\n",
		res.TxHash, res.Nonce, res.EstimatedGas, res.UsedGas)
}
