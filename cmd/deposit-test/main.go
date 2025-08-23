package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"time"

	"n42-test/internal/deposit" // ← 改成你的真实 module 路径
)

func main() {
	// 运行模式
	var (
		mode = flag.String("mode", "calc", "calc(只计算) | send(计算+上链发送) | send-bad(故意造错)")
		// 计算需要的输入
		blsSkHex   = flag.String("bls-sk", "", "BLS secret key hex (0x...)")
		pubkeyHex  = flag.String("pubkey", "", "BLS pubkey (48B, 0x...)")
		wcHex      = flag.String("wc", "", "withdrawal_credentials (32B, 0x...)")
		amountGwei = flag.Uint64("amount-gwei", 32_000_000_000, "deposit amount in Gwei (32 ETH = 32_000_000_000)")
		// 发送交易需要的输入
		rpc      = flag.String("rpc", "http://127.0.0.1:8545", "Ethereum RPC URL")
		senderPK = flag.String("sender-sk", "", "Sender secp256k1 private key (0x...)")
		contract = flag.String("contract", "", "Deposit contract address (0x...)")
		gasLimit = flag.Uint64("gas", 0, "custom gas limit (0 = estimate)")
	)
	flag.Parse()

	if *pubkeyHex == "" || *wcHex == "" || *blsSkHex == "" {
		log.Fatalf("缺少必要参数：-pubkey -wc -bls-sk（BLS）")
	}

	// 计算bls签名和root
	sigHex, rootHex, err := deposit.ComputeDepositSignatureAndRoot(*pubkeyHex, *wcHex, *amountGwei, *blsSkHex)
	if err != nil {
		log.Fatalf("计算签名/root失败： %v", err)
	}
	fmt.Println("===计算结果===")
	fmt.Println("签名: ", sigHex)
	fmt.Println("root: ", rootHex)

	if *mode == "calc" {
		// 只计算，不上链
	}

	if *senderPK == "" || *contract == "" {
		log.Fatalf("发送模式需要 -sender-sk（交易私钥）和 -contract（存款合约地址）")
	}

	// 组装depositparams
	params := &deposit.DepositParams{
		Contract:      *contract,
		PrivateKeyHex: *senderPK,
		RPC:           *rpc,
		PubkeyHex:     *pubkeyHex,
		WCHex:         *wcHex,
		SignatureHex:  sigHex,
		RootHex:       rootHex,
		AmountWei:     big.NewInt(0).Mul(big.NewInt(int64(*amountGwei)), big.NewInt(1_000_000_000)),
		Nonce:         -1,
		GasLimit:      *gasLimit,
	}

	// 3) 可选：故意造错，测试链是否正确revert
	if *mode == "send-bad" {
		// 改 root 的最后一个字节，制造 root 不匹配 → 预期 receipt.Status = 0
		if len(params.RootHex) >= 4 {
			// 简单翻转最后一位十六进制字符
			last := params.RootHex[len(params.RootHex)-1]
			if last != '0' {
				params.RootHex = params.RootHex[:len(params.RootHex)-1] + "0"
			} else {
				params.RootHex = params.RootHex[:len(params.RootHex)-1] + "1"
			}
			fmt.Println("[WARN] 已故意篡改 root，预期交易执行失败（合约 revert）")
		}
	}

	// 创建客户端并发送
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cli, err := deposit.NewClient(ctx, *rpc, *senderPK)
	if err != nil {
		log.Fatalf("创建客户端失败：%v", err)
	}
	defer cli.Close()

	cli.DebugPrintAccountState(ctx)

	txRes, err := cli.SendDeposit(ctx, params)
	if err != nil {
		log.Fatalf("发送质押请求失败：%v", err)
	}
	fmt.Println("=== 交易结果 ===")
	fmt.Printf("TxHash: %s\nNonce: %d\nEstimatedGas: %d\nUsedGas: %d\n",
		txRes.TxHash, txRes.Nonce, txRes.EstimatedGas, txRes.UsedGas)
}
