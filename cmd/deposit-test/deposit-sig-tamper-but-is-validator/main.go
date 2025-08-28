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
	RPC      = "http://127.0.0.1:8545"
	CONTRACT = "0x5FbDB2315678afecb367f032d93F642f64180aa3" // 本地/测试链 Deposit 合约地址

	// 发送交易的 EOA（secp256k1），非 BLS
	SENDER_SK = "0x85ae33b0b62f27cd3b04799e3ffa1122063bdc90890c4b17f979664b95029787"

	// BLS 材料（48B 公钥、32B 提款凭证）
	BLS_SK     = "0x3a422633e750193e2e4482e712edbcea1a1143a387b4907d80d69004740411b2"
	PUBKEY_HEX = "0xae206611b0c428bf7e2c6e8d852e99cb2304a14f590747bb6c7e70f2a58e871c0bddf76e0e3a4598b474c392fa75509b" // 48B
	WC_HEX     = "0x0100000000000000000000007DC625D73347a5778de982b6Ee37e98d416Ef859"                                 // 32B (ETH1提款)
)

// Gwei -> Wei
func gweiToWei(g uint64) *big.Int {
	return new(big.Int).Mul(big.NewInt(int64(g)), big.NewInt(1_000_000_000))
}

// 轻度篡改签名：翻最后一个 hex 字符，保持长度96B不变（仅示例）
func tamperSignatureHex(sig string) string {
	if len(sig) < 4 {
		return sig
	}
	last := sig[len(sig)-1]
	if last != '0' {
		return sig[:len(sig)-1] + "0"
	}
	return sig[:len(sig)-1] + "1"
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := deposit.NewClient(ctx, RPC, SENDER_SK)
	if err != nil {
		log.Fatalf("NewClient失败: %v", err)
	}
	defer cli.Close()

	cli.DebugPrintAccountState(ctx)

	// =========================
	// 1) 正常质押 31 ETH（签名与 root 均正确）
	// =========================
	const amount31Gwei = uint64(31_000_000_000) // 31 ETH
	fmt.Println("=== [Tx1] 31 ETH：计算“正确”的签名与 root ===")
	sig31, root31, err := deposit.ComputeDepositSignatureAndRoot(PUBKEY_HEX, WC_HEX, amount31Gwei, BLS_SK)
	if err != nil {
		log.Fatalf("计算 31ETH 签名/Root 失败: %v", err)
	}
	fmt.Println("signature(31):", sig31)
	fmt.Println("root(31)     :", root31)

	params31 := &deposit.DepositParams{
		Contract:             CONTRACT,
		PrivateKeyHex:        SENDER_SK,
		RPC:                  RPC,
		PubkeyHex:            PUBKEY_HEX,
		WCHex:                WC_HEX,
		SignatureHex:         sig31,  // 正确签名
		RootHex:              root31, // 与之匹配的“正确 root”
		AmountWei:            gweiToWei(amount31Gwei),
		Nonce:                -1, // 自动
		GasLimit:             0,  // 自动估算
		MaxPriorityFeePerGas: nil,
		MaxFeePerGas:         nil,
	}

	fmt.Println("\n>>> 发送 Tx1（期望成功）")
	res31, err := cli.SendDeposit(ctx, params31)
	if err != nil {
		log.Fatalf("Tx1 发送失败: %v", err)
	}
	fmt.Printf("Tx1: hash=%s, nonce=%d, usedGas=%d, block=%d\n",
		res31.TxHash, res31.Nonce, res31.UsedGas, res31.BlockNumber)

	// =========================
	// 2) 异常场景：1 ETH
	//    “签名被篡改，但用篡改后的签名重新计算 root”
	//    => 合约层校验通过（因为 root 与四元组匹配），
	//       但共识层不会激活该验证者（签名无效）。
	// =========================
	const amount1Gwei = uint64(1_000_000_000) // 1 ETH
	fmt.Println("\n=== [Tx2] 1 ETH：先得到‘正确签名’，再篡改签名，并用篡改签名重算 root ===")
	sig1_correct, _, err := deposit.ComputeDepositSignatureAndRoot(PUBKEY_HEX, WC_HEX, amount1Gwei, BLS_SK)
	if err != nil {
		log.Fatalf("计算 1ETH 正确签名失败: %v", err)
	}
	badSig1 := tamperSignatureHex(sig1_correct) // 篡改签名
	// **关键**：用“篡改后的签名”重算 root，使 (pubkey, wc, amount, badSig1) 与 root 匹配 => 合约通过
	root1_with_badSig, err := deposit.ComputeDepositDataRoot(PUBKEY_HEX, WC_HEX, amount1Gwei, badSig1)
	if err != nil {
		log.Fatalf("按篡改签名重算 root 失败: %v", err)
	}

	fmt.Println("bad signature(1):", badSig1)
	fmt.Println("root(badSig,1)  :", root1_with_badSig)

	params1 := &deposit.DepositParams{
		Contract:             CONTRACT,
		PrivateKeyHex:        SENDER_SK,
		RPC:                  RPC,
		PubkeyHex:            PUBKEY_HEX,
		WCHex:                WC_HEX,
		SignatureHex:         badSig1,           // 篡改的签名
		RootHex:              root1_with_badSig, // 按篡改签名重算得到的 root（与四元组匹配）
		AmountWei:            gweiToWei(amount1Gwei),
		Nonce:                -1, // 自动
		GasLimit:             0,  // 可自动估算（这次应可通过）
		MaxPriorityFeePerGas: nil,
		MaxFeePerGas:         nil,
	}

	fmt.Println("\n>>> 发送 Tx2（预期：合约成功，但共识层不会激活）")
	res1, err := cli.SendDeposit(ctx, params1)
	if err != nil {
		log.Fatalf("Tx2 发送失败: %v", err)
	}
	fmt.Printf("Tx2: hash=%s, nonce=%d, usedGas=%d, block=%d\n",
		res1.TxHash, res1.Nonce, res1.UsedGas, res1.BlockNumber)

	fmt.Println("\n[说明]")
	fmt.Println("- Tx1：31 ETH，签名与 root 匹配 => 合约成功写入存款树。")
	fmt.Println("- Tx2：1 ETH，签名被篡改，但 root 按‘篡改签名’重算，仍与四元组匹配 => 合约层通过；")
	fmt.Println("       随后在 Beacon/共识层验签时会失败，因此该存款无法激活验证者。")
}
